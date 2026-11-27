package loop

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
)

// cand builds a minimal candidate carrying only the qualified line identity.
func cand(id string) decision.Candidate { return decision.Candidate{ID: id} }

// These tests drive the checkpoint/resume path against a REAL temp EKA
// workspace and the REAL write path (internal/ekawrite), reusing its replay
// gate. They are store-backed and therefore skipped under -short.

// TestResumeTransitionReconcileExactlyOnce applies a transition, simulates a
// crash after apply but before markDone, resumes, and asserts reconcile is a
// no-op and the line holds the target state exactly once.
func TestResumeTransitionReconcileExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed checkpoint integration (opens a real EKA workspace)")
	}
	ctx := context.Background()
	t.Setenv("EKA_HOME", t.TempDir())
	rt, err := api.Ensure()
	if err != nil {
		t.Fatalf("api.Ensure: %v", err)
	}
	defer rt.Close()

	w := ekawrite.NewSteward(rt)
	repo := testRepo(t, "p", "p-repo", "p")
	if _, err := w.Sync(repo, api.SyncOptions{}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if _, err := w.Publish(ekawrite.PublishInput{Project: "p", Namespace: "p", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	target := "p/sto:i-1"
	store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
	writer := EkawriteWriter{Writer: w, RepoPath: repo}

	// Simulate a crash AFTER the write applied but BEFORE markDone: the write
	// moved i-1 planned->todo, the intent is still in flight and Done is empty.
	if err := writer.Advance(ctx, cand(target), "todo"); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	in := Intent{CandidateID: target, Kind: IntentTransition, Target: target, To: "todo", Step: Step{CandidateID: target, Kind: KindDispatch}}
	if err := store.Save(State{InFlight: []Intent{in}}); err != nil {
		t.Fatal(err)
	}

	l := New(fixtureSource{}, newCountingDispatcher(), writer, store, Config{MaxWorkers: 4})
	if err := l.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.InFlight) != 0 || !st.Has(target) {
		t.Fatalf("post-resume state = %+v, want done and no in-flight", st)
	}
	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine = %d units, %v; want exactly 1", len(units), err)
	}
	if got := units[0].StateVector.ExecutionState; got != "todo" {
		t.Fatalf("i-1 execution-state = %q, want todo", got)
	}

	// Re-seed the SAME intent (crash again before markDone): reconcile must be
	// a no-op (the line is at/after the target) and must not error.
	if err := store.Save(State{InFlight: []Intent{in}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Resume(ctx); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	units, _ = rt.Resolver().ResolveLine("p", "sto", "i-1")
	if len(units) != 1 || units[0].StateVector.ExecutionState != "todo" {
		t.Fatalf("second resume changed the line: %+v", units)
	}
}

// TestReconcilePublishReplayGateIntegration publishes a unit, then reconciles
// the same payload twice: the first is an applied/no-op per state, the second
// is a no-op, and exactly one instance exists (the replay gate is reused, not
// reimplemented).
func TestReconcilePublishReplayGateIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed checkpoint integration (opens a real EKA workspace)")
	}
	t.Setenv("EKA_HOME", t.TempDir())
	rt, err := api.Ensure()
	if err != nil {
		t.Fatalf("api.Ensure: %v", err)
	}
	defer rt.Close()

	w := ekawrite.NewSteward(rt)
	repo := testRepo(t, "p", "p-repo", "p")
	if _, err := w.Sync(repo, api.SyncOptions{}); err != nil {
		t.Fatalf("register repo: %v", err)
	}

	in := PublishIntent{
		Project:   "p",
		Namespace: "p",
		Type:      "sto",
		ID:        "i-9",
		Content:   stoContent("reconcile"),
	}
	writer := EkawriteWriter{Writer: w, RepoPath: repo}

	// First reconcile: the unit does not exist yet -> applied.
	res, err := writer.Reconcile(context.Background(), Intent{Kind: IntentPublish, Target: "p/sto:i-9", Publish: &in})
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if !res.Applied {
		t.Fatalf("first Reconcile = %+v, want applied", res)
	}

	// Second reconcile of the identical payload: the replay gate matches the
	// current instance -> no-op, no max+1.
	res, err = writer.Reconcile(context.Background(), Intent{Kind: IntentPublish, Target: "p/sto:i-9", Publish: &in})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if res.Applied {
		t.Fatalf("second Reconcile = %+v, want no-op (replay gate)", res)
	}

	units, err := rt.Resolver().ResolveLine("p", "sto", "i-9")
	if err != nil || len(units) != 1 {
		t.Fatalf("instances = %d, %v; want exactly 1 (replay must not allocate a version)", len(units), err)
	}
}
