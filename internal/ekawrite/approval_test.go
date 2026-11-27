package ekawrite

import (
	"encoding/json"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
)

// approvalInput builds a deterministic approval input for one subject.
func approvalInput(repoDir, target, verdict, reason string) ApprovalInput {
	return ApprovalInput{
		RepoPath:   repoDir,
		Target:     target,
		Verdict:    verdict,
		RequestKey: "key-abc",
		ToolKind:   "execute",
		Path:       "/etc/hosts",
		Reason:     reason,
	}
}

// TestApprovalRecordsResolvedReviewNote is the store-backed typed-artifact
// proof: an approval is a RESOLVED review note discussing the subject, with
// the request key and raw decision in its notes.
func TestApprovalRecordsResolvedReviewNote(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "appr-1")

	res, err := w.Approval(approvalInput(repoDir, "sto:appr-1", "approve", "trusted"))
	if err != nil {
		t.Fatalf("Approval: %v", err)
	}
	if res.Status != ApprovalRecorded || res.Verdict != VerdictApprove || res.NoteID == "" {
		t.Fatalf("Approval = %+v, want recorded approve", res)
	}

	units, err := rt.Resolver().ResolveLine("steward", "cmt", res.NoteID)
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine note = %d, %v; want 1", len(units), err)
	}
	u := units[0]
	if u.StateVector.NoteState != "resolved" {
		t.Errorf("note-state = %q, want resolved", u.StateVector.NoteState)
	}
	var content map[string]any
	if err := json.Unmarshal(u.ContentPayload, &content); err != nil {
		t.Fatalf("decode note: %v", err)
	}
	if content["role"] != "review" || content["verdict"] != VerdictApprove {
		t.Errorf("note content role/verdict = %v/%v, want review/approve", content["role"], content["verdict"])
	}
	notes, _ := content["notes"].([]any)
	if len(notes) < 2 || notes[0] != "approval request key-abc" || notes[len(notes)-1] != "trusted" {
		t.Errorf("note notes = %v, want the request key and the reason", notes)
	}
}

// TestApprovalReplayIsNoOp: recording the SAME approval twice produces exactly
// one note (the checkpoint-resume replay gate).
func TestApprovalReplayIsNoOp(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "appr-2")

	in := approvalInput(repoDir, "sto:appr-2", "deny", "untrusted")
	first, err := w.Approval(in)
	if err != nil {
		t.Fatalf("first Approval: %v", err)
	}
	if first.Status != ApprovalRecorded {
		t.Fatalf("first Approval = %+v, want recorded", first)
	}
	second, err := w.Approval(in)
	if err != nil {
		t.Fatalf("second Approval: %v", err)
	}
	if second.Status != ApprovalNoOp || second.NoteID != first.NoteID {
		t.Fatalf("second Approval = %+v, want no-op of %q", second, first.NoteID)
	}
	units, err := rt.Resolver().ResolveLine("steward", "cmt", first.NoteID)
	if err != nil || len(units) != 1 {
		t.Fatalf("note instances = %d, %v; want exactly 1 (replay must not allocate)", len(units), err)
	}

	// A DIFFERENT decision (timeout, not deny) is a distinct note.
	timed, err := w.Approval(approvalInput(repoDir, "sto:appr-2", "timeout", "no answer"))
	if err != nil {
		t.Fatalf("timeout Approval: %v", err)
	}
	if timed.Status != ApprovalRecorded || timed.NoteID == first.NoteID {
		t.Fatalf("timeout Approval = %+v, want a distinct recorded note", timed)
	}
	var content map[string]any
	units, _ = rt.Resolver().ResolveLine("steward", "cmt", timed.NoteID)
	_ = json.Unmarshal(units[0].ContentPayload, &content)
	if content["verdict"] != VerdictChangesRequested {
		t.Errorf("timeout note verdict = %v, want changes-requested", content["verdict"])
	}
}

// TestApprovalRejectsUnknownVerdict: an unknown decision is refused before any
// note is scaffolded.
func TestApprovalRejectsUnknownVerdict(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "appr-3")
	if _, err := w.Approval(approvalInput(repoDir, "sto:appr-3", "maybe", "")); err == nil {
		t.Fatal("Approval(maybe) err = nil, want a refusal")
	}
}
