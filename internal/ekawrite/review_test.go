package ekawrite

import (
	"encoding/json"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
)

// publishSto publishes one work item so a review note has a resolvable subject.
func publishSto(t *testing.T, w *Writer, project, ns, id string) {
	t.Helper()
	if _, err := w.Publish(PublishInput{Project: project, Namespace: ns, Type: "sto", ID: id, Content: stoContent("work")}); err != nil {
		t.Fatalf("Publish sto: %v", err)
	}
}

// TestReviewRecordsResolvedNote is the store-backed approve path (AC3): the
// outcome is persisted as a RESOLVED review note discussing the work item.
func TestReviewRecordsResolvedNote(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "item-1")

	res, err := w.Review(ReviewInput{
		RepoPath: repoDir,
		Target:   "sto:item-1",
		Verdict:  VerdictApprove,
		Notes:    []string{"looks good"},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.NoteID != "item-1-review" || res.Verdict != VerdictApprove || res.Form == "" {
		t.Fatalf("Review result = %+v", res)
	}

	units, err := rt.Resolver().ResolveLine("steward", "cmt", res.NoteID)
	if err != nil {
		t.Fatalf("ResolveLine note: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("note instances = %d, want exactly 1", len(units))
	}
	u := units[0]
	if u.StateVector.NoteState != "resolved" {
		t.Errorf("note-state = %q, want resolved (the done gate is released only by resolved)", u.StateVector.NoteState)
	}

	var content map[string]any
	if err := json.Unmarshal(u.ContentPayload, &content); err != nil {
		t.Fatalf("decode note content: %v", err)
	}
	if content["role"] != "review" || content["verdict"] != VerdictApprove {
		t.Errorf("note content = %v, want role=review verdict=approve", content)
	}

	var discusses string
	for _, rel := range u.Relationships {
		if rel.Type == "discusses" {
			discusses = rel.Target
		}
	}
	if discusses != "steward/sto:item-1" {
		t.Errorf("discusses = %q, want steward/sto:item-1", discusses)
	}

	report, err := rt.Integrity().Verify()
	if err != nil {
		t.Fatalf("Integrity.Verify: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Fatalf("integrity violations = %+v, want none", report.Violations)
	}
}

// TestReviewRejectNormalizesVerdict covers the reject path: the verdict is
// recorded as changes-requested (the note schema's non-approval value) and the
// note is still resolved (the outcome is recorded, the gate is not "passed" by
// the loop because it leaves the item un-done).
func TestReviewRejectNormalizesVerdict(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "item-2")

	res, err := w.Review(ReviewInput{
		RepoPath: repoDir,
		Target:   "sto:item-2",
		Verdict:  VerdictReject,
		Notes:    []string{"missing tests"},
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Verdict != VerdictChangesRequested {
		t.Fatalf("normalized verdict = %q, want %q", res.Verdict, VerdictChangesRequested)
	}

	units, err := rt.Resolver().ResolveLine("steward", "cmt", res.NoteID)
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine note: units=%d err=%v", len(units), err)
	}
	var content map[string]any
	if err := json.Unmarshal(units[0].ContentPayload, &content); err != nil {
		t.Fatalf("decode note content: %v", err)
	}
	if content["verdict"] != VerdictChangesRequested {
		t.Errorf("note verdict = %v, want changes-requested", content["verdict"])
	}
}

// TestReviewRejectsUnknownVerdict asserts an unknown verdict is refused before
// any draft is scaffolded.
func TestReviewRejectsUnknownVerdict(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	publishSto(t, w, "steward", "steward", "item-3")
	if _, err := w.Review(ReviewInput{RepoPath: repoDir, Target: "sto:item-3", Verdict: "bogus"}); err == nil {
		t.Fatalf("Review(bogus verdict) err = nil, want a refusal")
	}
}
