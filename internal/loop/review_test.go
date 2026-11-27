package loop

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/role"
)

// fixtureRoles is a deterministic role set: one non-review author role and two
// review roles (sorted: reviewer, reviewer2).
const fixtureRoles = `
roles:
  - name: implementer
    driver: claude
    template: prompts/implementer.md
    review: false
  - name: reviewer
    driver: claude
    template: prompts/reviewer.md
    review: true
  - name: reviewer2
    driver: claude
    template: prompts/reviewer2.md
    review: true
`

func mustRoles(t *testing.T) *role.Set {
	t.Helper()
	s, err := role.Parse([]byte(fixtureRoles))
	if err != nil {
		t.Fatalf("parse roles: %v", err)
	}
	return s
}

// reviewDisp is a fake worker seam: Dispatch is the author call, DispatchReview
// the review call. It records both and returns a fixed verdict.
type reviewDisp struct {
	verdict     string
	notes       []string
	authorCalls int
	reviewCalls []ReviewRequest
}

func (d *reviewDisp) Dispatch(_ context.Context, c decision.Candidate) (DispatchResult, error) {
	d.authorCalls++
	return DispatchResult{CandidateID: c.ID, Output: "author-output"}, nil
}

func (d *reviewDisp) DispatchReview(_ context.Context, req ReviewRequest) (ReviewOutcome, error) {
	d.reviewCalls = append(d.reviewCalls, req)
	return ReviewOutcome{Verdict: d.verdict, Notes: d.notes}, nil
}

// reviewWriter is a fake Writer + ReviewRecorder. It records advances and
// review records and never touches EKA.
type reviewWriter struct {
	advanced []string
	reviews  []ReviewRecord
}

func (w *reviewWriter) Advance(_ context.Context, c decision.Candidate, _ string) error {
	w.advanced = append(w.advanced, c.ID)
	return nil
}

func (w *reviewWriter) Reconcile(_ context.Context, _ Intent) (ReconcileResult, error) {
	return ReconcileResult{}, nil
}

func (w *reviewWriter) Review(_ context.Context, _ decision.Candidate, rec ReviewRecord) error {
	w.reviews = append(w.reviews, rec)
	return nil
}

func oneCandidate() []decision.Candidate {
	return []decision.Candidate{{ID: "proj/sto:item-1", Priority: 10, State: "todo", CreatedSeq: 1}}
}

func reviewLoop(t *testing.T, disp Dispatcher, w Writer, policy ReviewPolicy) (*Loop, *Store) {
	t.Helper()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	return New(fixtureSource{oneCandidate()}, disp, w, store, Config{
		MaxWorkers:    8,
		DispatchState: "in-review",
		Review:        policy,
	}), store
}

// TestReviewApproveCompletes is AC1+AC2+AC3: the policy requires review, the
// reviewer is selected (deterministic, distinct from the author), the review
// approves, the outcome is recorded, and the item completes.
func TestReviewApproveCompletes(t *testing.T) {
	ctx := context.Background()
	disp := &reviewDisp{verdict: VerdictApprove, notes: []string{"lgtm"}}
	w := &reviewWriter{}
	l, store := reviewLoop(t, disp, w, ReviewPolicy{Roles: mustRoles(t), Author: "implementer"})

	step, ok, err := l.Step(ctx)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !ok || step.CandidateID != "proj/sto:item-1" {
		t.Fatalf("Step = %+v ok=%v, want the candidate", step, ok)
	}

	// The author ran once, the review ran once with the selected reviewer.
	if disp.authorCalls != 1 {
		t.Errorf("author dispatches = %d, want 1", disp.authorCalls)
	}
	if len(disp.reviewCalls) != 1 {
		t.Fatalf("review dispatches = %d, want 1", len(disp.reviewCalls))
	}
	rr := disp.reviewCalls[0]
	if rr.Author != "implementer" || rr.Reviewer != "reviewer" {
		t.Errorf("review request author/reviewer = %q/%q, want implementer/reviewer", rr.Author, rr.Reviewer)
	}
	if rr.Output != "author-output" {
		t.Errorf("review request output = %q, want the author output", rr.Output)
	}

	// AC3: the outcome is recorded, with the reviewer and verdict.
	if len(w.reviews) != 1 {
		t.Fatalf("recorded reviews = %d, want 1", len(w.reviews))
	}
	rec := w.reviews[0]
	if rec.Reviewer != "reviewer" || rec.Verdict != VerdictApprove || rec.Author != "implementer" {
		t.Errorf("recorded review = %+v", rec)
	}

	// Completion gate released: advanced once and recorded done.
	if len(w.advanced) != 1 || w.advanced[0] != "proj/sto:item-1" {
		t.Errorf("advances = %v, want the candidate", w.advanced)
	}
	st, _ := store.Load()
	if !st.Has("proj/sto:item-1") {
		t.Errorf("candidate is not recorded done after an approving review")
	}
}

// TestReviewRejectDoesNotComplete is the reject path: the item is NOT done, not
// advanced, the outcome is recorded, and the refusal is surfaced.
func TestReviewRejectDoesNotComplete(t *testing.T) {
	ctx := context.Background()
	disp := &reviewDisp{verdict: VerdictReject, notes: []string{"missing tests"}}
	w := &reviewWriter{}
	l, store := reviewLoop(t, disp, w, ReviewPolicy{Roles: mustRoles(t), Author: "implementer"})

	_, ok, err := l.Step(ctx)
	if !ok {
		t.Fatalf("Step ok=false, want the candidate was selected")
	}
	var refusal *ReviewRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Step error = %v, want *ReviewRefusal", err)
	}
	if refusal.Verdict != VerdictReject || refusal.Reviewer != "reviewer" {
		t.Errorf("refusal = %+v", refusal)
	}

	if len(w.advanced) != 0 {
		t.Errorf("advances = %v, want none on rejection", w.advanced)
	}
	st, _ := store.Load()
	if st.Has("proj/sto:item-1") {
		t.Errorf("candidate is recorded done despite a rejected review")
	}
	if len(w.reviews) != 1 || w.reviews[0].Verdict != VerdictReject {
		t.Errorf("recorded reviews = %+v, want one reject outcome", w.reviews)
	}
}

// TestReviewStepSequenceDeterministic asserts the approve and reject step
// sequences are stable across repeated runs.
func TestReviewStepSequenceDeterministic(t *testing.T) {
	ctx := context.Background()
	for _, verdict := range []string{VerdictApprove, VerdictReject} {
		var first Step
		for i := 0; i < 20; i++ {
			disp := &reviewDisp{verdict: verdict}
			l, _ := reviewLoop(t, disp, &reviewWriter{}, ReviewPolicy{Roles: mustRoles(t), Author: "implementer"})
			step, _, _ := l.Step(ctx)
			if i == 0 {
				first = step
				continue
			}
			if step != first {
				t.Fatalf("%s run %d step = %+v, want %+v", verdict, i, step, first)
			}
		}
	}
}

// TestSelectReviewerDeterministicDistinct is AC2: selection is deterministic,
// distinct from the author, and errors when no eligible reviewer exists.
func TestSelectReviewerDeterministicDistinct(t *testing.T) {
	roles := mustRoles(t)
	for i := 0; i < 20; i++ {
		got, err := SelectReviewer("implementer", roles)
		if err != nil {
			t.Fatalf("SelectReviewer run %d: %v", i, err)
		}
		if got.Name != "reviewer" {
			t.Fatalf("SelectReviewer run %d = %q, want reviewer (sorted-first, deterministic)", i, got.Name)
		}
	}

	// Excluding the author role: an author that IS review role skips itself.
	got, err := SelectReviewer("reviewer", roles)
	if err != nil {
		t.Fatalf("SelectReviewer(reviewer): %v", err)
	}
	if got.Name != "reviewer2" {
		t.Fatalf("SelectReviewer(reviewer) = %q, want reviewer2 (never self-review)", got.Name)
	}

	// No eligible reviewer: only the author is a review role.
	single, err := role.Parse([]byte(`
roles:
  - name: solo
    driver: claude
    template: p/solo.md
    review: true
`))
	if err != nil {
		t.Fatalf("parse single role: %v", err)
	}
	if _, err := SelectReviewer("solo", single); !errors.Is(err, ErrNoReviewer) {
		t.Fatalf("SelectReviewer(solo) error = %v, want ErrNoReviewer", err)
	}
	if _, err := SelectReviewer("x", nil); !errors.Is(err, ErrNoReviewer) {
		t.Fatalf("SelectReviewer(nil set) error = %v, want ErrNoReviewer", err)
	}
}

// TestReviewPolicyOffUnchanged is the policy-off path: no review dispatch, no
// record, completion unchanged.
func TestReviewPolicyOffUnchanged(t *testing.T) {
	ctx := context.Background()
	disp := &reviewDisp{verdict: VerdictApprove}
	w := &reviewWriter{}
	l, store := reviewLoop(t, disp, w, ReviewPolicy{}) // zero value = off

	if _, _, err := l.Step(ctx); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(disp.reviewCalls) != 0 {
		t.Errorf("review dispatched %d times with the policy off, want 0", len(disp.reviewCalls))
	}
	if len(w.reviews) != 0 {
		t.Errorf("review recorded %d times with the policy off, want 0", len(w.reviews))
	}
	st, _ := store.Load()
	if !st.Has("proj/sto:item-1") {
		t.Errorf("policy-off candidate is not done")
	}
}

// TestReviewRequiresRecordingWriter asserts review-enabled + a writer that
// cannot record a review is a deterministic error, not a silent pass.
func TestReviewRequiresRecordingWriter(t *testing.T) {
	ctx := context.Background()
	// A Writer that is not a ReviewRecorder.
	plain := &plainWriter{}
	l, _ := reviewLoop(t, &reviewDisp{verdict: VerdictApprove}, plain, ReviewPolicy{Roles: mustRoles(t), Author: "implementer"})
	if _, _, err := l.Step(ctx); err == nil {
		t.Fatalf("Step err = nil, want an error when the writer cannot record a review")
	}
}

type plainWriter struct{}

func (plainWriter) Advance(context.Context, decision.Candidate, string) error { return nil }
func (plainWriter) Reconcile(context.Context, Intent) (ReconcileResult, error) {
	return ReconcileResult{}, nil
}
