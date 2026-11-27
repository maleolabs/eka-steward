package ekawrite

// Approval recording: a human decision on an escalated permission request is
// recorded as EKA knowledge, not in a parallel store. The note schema (ADR-019
// D7) fixes the `role` vocabulary at implementation|review|fix|reply, so an
// `approval` role cannot be invented; an approval is recorded with the EXISTING
// review-note convention (role=review, a RESOLVED cmt- note discussing the
// request target) whose `verdict` is approve|changes-requested and whose
// `notes` carry the request key, the raw decision (approve|deny|timeout) and
// the reason (docs/approval.md).
//
// Recording is REPLAY-SAFE. The note is only scaffolded after a read-then-write
// gate finds no existing resolved review note on the same target carrying the
// same author, verdict and notes, so a re-issued approval (a checkpoint resume)
// is a NO-OP: no second note, no bumped instance.

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-core/v2/conformance"
)

// Approval recording status.
type ApprovalStatus string

const (
	// ApprovalRecorded: a new approval note was published.
	ApprovalRecorded ApprovalStatus = "recorded"
	// ApprovalNoOp: an identical approval note already existed (the replay
	// gate matched); nothing was written.
	ApprovalNoOp ApprovalStatus = "no-op"
)

// ApprovalInput describes one human approval decision to record. Verdict is
// the RAW decision: approve | deny | timeout (deny and timeout both persist as
// the note schema's `changes-requested`, distinguished in `notes`).
type ApprovalInput struct {
	// RepoPath addresses the repository whose project the note lives in.
	RepoPath string
	// Target is the request target line ("<ns>/<type>:<id>" or "<type>:<id>").
	Target string
	// Verdict is the raw decision: approve | deny | timeout.
	Verdict string
	// RequestKey is the deterministic approval request key (approval.RequestFor).
	RequestKey string
	// ToolKind and Path are the requested tool class and path (documentation).
	ToolKind string
	Path     string
	// Reason is the human's or the timeout's explanation.
	Reason string
}

// ApprovalResult reports one recorded (or already-recorded) approval note.
type ApprovalResult struct {
	// NoteID is the cmt- note id.
	NoteID string
	// Form is the canonical published form of the note.
	Form string
	// Verdict is the NORMALIZED note verdict (approve|changes-requested).
	Verdict string
	// Status is "recorded" or "no-op".
	Status ApprovalStatus
}

// Approval records one human decision as a resolved review note with the
// deterministic replay gate described in the package comment.
func (w *Writer) Approval(in ApprovalInput) (ApprovalResult, error) {
	if err := w.ready(); err != nil {
		return ApprovalResult{}, err
	}
	verdict, ok := normalizeApprovalVerdict(in.Verdict)
	if !ok {
		return ApprovalResult{}, fmt.Errorf("ekawrite: approval verdict %q is not one of approve, deny, timeout", in.Verdict)
	}
	if in.Target == "" {
		return ApprovalResult{}, fmt.Errorf("ekawrite: approval needs a target line")
	}
	notes := approvalNotes(in)

	existing, found, err := w.findApproval(in.RepoPath, in.Target, verdict, notes)
	if err != nil {
		return ApprovalResult{}, err
	}
	if found {
		return ApprovalResult{
			NoteID:  existing.Identity.ID,
			Form:    existing.CanonicalIdentityForm,
			Verdict: verdict,
			Status:  ApprovalNoOp,
		}, nil
	}

	res, err := w.Review(ReviewInput{RepoPath: in.RepoPath, Target: in.Target, Verdict: verdict, Notes: notes})
	if err != nil {
		return ApprovalResult{}, err
	}
	return ApprovalResult{NoteID: res.NoteID, Form: res.Form, Verdict: res.Verdict, Status: ApprovalRecorded}, nil
}

// normalizeApprovalVerdict maps a raw approval decision to the review-note
// schema's verdict. approve stays approve; deny and timeout become
// changes-requested (the schema's only non-approval value).
func normalizeApprovalVerdict(v string) (string, bool) {
	switch v {
	case "approve":
		return VerdictApprove, true
	case "deny", "timeout":
		return VerdictChangesRequested, true
	}
	return "", false
}

// approvalNotes builds the deterministic notes list recorded on the note. It
// carries the request key and the raw decision, so two DIFFERENT approvals
// (and approve vs deny vs timeout) are distinct notes, while a re-issued
// IDENTICAL approval is byte-identical (the replay gate then no-ops).
func approvalNotes(in ApprovalInput) []string {
	notes := []string{"approval request " + in.RequestKey}
	if in.ToolKind != "" || in.Path != "" {
		notes = append(notes, "tool "+in.ToolKind+" path "+in.Path)
	}
	notes = append(notes, "decision "+in.Verdict)
	if in.Reason != "" {
		notes = append(notes, in.Reason)
	}
	return notes
}

// findApproval is the replay gate: it returns the existing resolved review
// note (authored by this Writer) that discusses target with the same verdict
// and notes, if any. The scan is over the project's units in the store's
// deterministic (canonical form) order — no map iteration.
func (w *Writer) findApproval(repoPath, target, verdict string, notes []string) (*api.KnowledgeUnit, bool, error) {
	repo, err := w.repo(repoPath)
	if err != nil {
		return nil, false, err
	}
	ref, err := conformance.ParseReference(target, repo.Namespace, "")
	if err != nil {
		return nil, false, fmt.Errorf("ekawrite: approval target %q: %w", target, err)
	}
	if ref.HasVersion {
		return nil, false, fmt.Errorf("ekawrite: approval target %q is a canonical form; address the subject line", target)
	}
	subject := ref.Namespace + "/" + ref.Type + ":" + ref.ID

	units, err := w.rt.Knowledge().UnitsByProject(repo.ProjectID)
	if err != nil {
		return nil, false, fmt.Errorf("ekawrite: cannot read project %s for the approval replay gate: %w", repo.ProjectID, err)
	}
	for _, u := range units {
		if u.Identity.Type != "cmt" || u.StateVector.NoteState != "resolved" || u.Author != w.author {
			continue
		}
		if !discusses(u, subject) {
			continue
		}
		content, ok := decodeNote(u)
		if !ok || content["role"] != "review" || content["verdict"] != verdict {
			continue
		}
		if !sameNotes(content["notes"], notes) {
			continue
		}
		return u, true, nil
	}
	return nil, false, nil
}

// repo resolves the registered repository at repoPath.
func (w *Writer) repo(repoPath string) (api.Repo, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return api.Repo{}, fmt.Errorf("ekawrite: cannot resolve %q: %w", repoPath, err)
	}
	repo, found, err := w.rt.Workspace().FindRepo(abs)
	if err != nil {
		return api.Repo{}, fmt.Errorf("ekawrite: cannot find repo %s: %w", abs, err)
	}
	if !found {
		return api.Repo{}, fmt.Errorf("ekawrite: repository %s is not registered in the workspace", abs)
	}
	return repo, nil
}

// discusses reports whether u carries a `discusses` edge to subject.
func discusses(u *api.KnowledgeUnit, subject string) bool {
	for _, r := range u.Relationships {
		if r.Type == "discusses" && r.Target == subject {
			return true
		}
	}
	return false
}

// decodeNote unmarshals a note's content payload.
func decodeNote(u *api.KnowledgeUnit) (map[string]any, bool) {
	var content map[string]any
	if err := json.Unmarshal(u.ContentPayload, &content); err != nil {
		return nil, false
	}
	return content, true
}

// sameNotes reports whether a decoded note `notes` field equals want.
func sameNotes(v any, want []string) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	if len(list) != len(want) {
		return false
	}
	for i := range want {
		s, ok := list[i].(string)
		if !ok || s != want[i] {
			return false
		}
	}
	return true
}
