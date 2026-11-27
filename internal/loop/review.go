package loop

// Review stage: when the policy requires it, a worker's output is routed to a
// review role before the candidate is advanced/done. The review is a CODE
// decision (which role reviews) plus one worker/LLM call (the verdict); the
// outcome is recorded as a resolved review note through the single write path
// (internal/ekawrite). See docs/review.md.

import (
	"context"
	"errors"
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/role"
)

// Review verdicts. The EKA review-note schema (ADR-019 D7) allows exactly
// approve and changes-requested; VerdictReject is accepted as a loop-level
// alias for changes-requested and normalized when the note is recorded.
const (
	VerdictApprove          = "approve"
	VerdictChangesRequested = "changes-requested"
	VerdictReject           = "reject"
)

// ErrNoReviewer is returned by SelectReviewer when the role set carries no
// eligible review role (no review role at all, or only the author role).
var ErrNoReviewer = errors.New("loop: no eligible review role")

// ReviewPolicy decides whether a candidate's dispatch output must be reviewed
// before the candidate can be marked done, and supplies the roles used to pick
// a reviewer. The ZERO VALUE disables review: the loop is byte-identical to
// the pre-review loop. Review is enabled by setting Roles.
type ReviewPolicy struct {
	// Roles is the validated role set reviewer selection draws from. nil
	// disables the review stage entirely (the zero value).
	Roles *role.Set
	// Author is the role name the loop dispatches workers as. Required when
	// Roles is set; an empty Author with review enabled is a deterministic
	// error.
	Author string
	// RequireFor, when non-nil, is the explicit set of author role names
	// whose output requires review. When nil the documented default rule
	// applies: the author role requires review unless it is itself a review
	// role (Review=true). Maps are only LOOKED UP, never iterated, so the
	// decision stays deterministic.
	RequireFor map[string]bool
}

// enabled reports whether the review stage is configured.
func (p ReviewPolicy) enabled() bool { return p.Roles != nil }

// requires reports whether the configured author role requires review, plus a
// deterministic error when the policy is enabled but misconfigured (no author,
// or an author absent from the role set under the default rule).
func (p ReviewPolicy) requires() (bool, error) {
	if !p.enabled() {
		return false, nil
	}
	if p.Author == "" {
		return false, errors.New("loop: review policy requires an author role")
	}
	if p.RequireFor != nil {
		return p.RequireFor[p.Author], nil
	}
	r, err := p.Roles.Resolve(p.Author)
	if err != nil {
		return false, fmt.Errorf("loop: review policy author: %w", err)
	}
	return !r.Review, nil
}

// ReviewRequest is the review half of the worker seam: the author role's
// output for one candidate, handed to the selected reviewer role.
type ReviewRequest struct {
	// Candidate is the work item under review.
	Candidate decision.Candidate
	// Author is the role name that produced Output.
	Author string
	// Reviewer is the selected review role name.
	Reviewer string
	// Output is the author dispatcher's opaque output.
	Output string
}

// ReviewOutcome is the reviewer's verdict. Reviewer is filled by the loop from
// the deterministic selection, never by the reviewer itself.
type ReviewOutcome struct {
	Verdict  string   `json:"verdict"`
	Notes    []string `json:"notes,omitempty"`
	Reviewer string   `json:"reviewer,omitempty"`
}

// Approved reports whether the outcome releases the completion gate.
func (o ReviewOutcome) Approved() bool { return o.Verdict == VerdictApprove }

// ReviewDispatcher is a Dispatcher that can also run the review stage. It is
// the SAME worker seam (the loop's one dispatcher) extended with a review call,
// so a fake can drive the workflow deterministically. A loop with review
// required and a dispatcher that does not implement this is a deterministic
// error.
type ReviewDispatcher interface {
	Dispatcher
	DispatchReview(ctx context.Context, req ReviewRequest) (ReviewOutcome, error)
}

// ReviewRecord is one review outcome to persist. The loop's Writer records it
// as a resolved review note (role=review) discussing the candidate.
type ReviewRecord struct {
	Author   string
	Reviewer string
	Verdict  string
	Notes    []string
}

// ReviewRecorder is implemented by a Writer that can persist a review outcome
// (the ekawrite-backed Writer does). The loop requires it only when the review
// policy is enabled.
type ReviewRecorder interface {
	Review(ctx context.Context, c decision.Candidate, rec ReviewRecord) error
}

// ReviewRefusal is the deterministic, surfaced outcome of a review that did not
// approve. The candidate is left un-done (never advanced; re-work is out of
// scope) and the recorded review note carries the verdict.
type ReviewRefusal struct {
	CandidateID string
	Reviewer    string
	Verdict     string
	Notes       []string
}

// Error renders the deterministic refusal.
func (e *ReviewRefusal) Error() string {
	return fmt.Sprintf("loop: review of %s by %s did not approve (verdict %q); the item is left un-done",
		e.CandidateID, e.Reviewer, e.Verdict)
}

// SelectReviewer picks the reviewer for an author role deterministically: the
// FIRST review role in the set's sorted ReviewRoles() order that is not the
// author role. Same input always yields the same reviewer. No eligible review
// role (none configured, or only the author) is ErrNoReviewer — a deterministic
// error, never a fallback.
func SelectReviewer(author string, set *role.Set) (role.Role, error) {
	if set == nil {
		return role.Role{}, ErrNoReviewer
	}
	for _, r := range set.ReviewRoles() { // sorted by name
		if r.Name == author {
			continue // never self-review
		}
		return r, nil
	}
	return role.Role{}, ErrNoReviewer
}

// runReview selects the reviewer and dispatches the review through the same
// worker seam. It fills the reviewer name on the outcome.
func (l *Loop) runReview(ctx context.Context, c decision.Candidate, output string) (ReviewOutcome, error) {
	rd, ok := l.dispatcher.(ReviewDispatcher)
	if !ok {
		return ReviewOutcome{}, fmt.Errorf("loop: review required for %s but the dispatcher cannot review", c.ID)
	}
	reviewer, err := SelectReviewer(l.cfg.Review.Author, l.cfg.Review.Roles)
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("loop: review %s: %w", c.ID, err)
	}
	out, err := rd.DispatchReview(ctx, ReviewRequest{
		Candidate: c,
		Author:    l.cfg.Review.Author,
		Reviewer:  reviewer.Name,
		Output:    output,
	})
	if err != nil {
		return ReviewOutcome{}, fmt.Errorf("loop: review %s: %w", c.ID, err)
	}
	out.Reviewer = reviewer.Name
	return out, nil
}

// recordReview persists one review outcome through the single write path under
// writeMu, so the EKA write path is never entered concurrently.
func (l *Loop) recordReview(ctx context.Context, rec ReviewRecorder, c decision.Candidate, o ReviewOutcome) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return rec.Review(ctx, c, ReviewRecord{
		Author:   l.cfg.Review.Author,
		Reviewer: o.Reviewer,
		Verdict:  o.Verdict,
		Notes:    o.Notes,
	})
}
