package ekawrite

import (
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
)

// TestReconcilePublishAppliedThenNoOp drives the unit-creation reconcile: a
// fresh payload applies once, an identical re-issue is a no-op, and the line
// holds exactly one instance.
func TestReconcilePublishAppliedThenNoOp(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)

	in := PublishInput{Project: "p", Namespace: "feather", Type: "sto", ID: "rec", Content: stoContent("d")}

	first, err := w.Reconcile(ReconcileRequest{Publish: &in})
	if err != nil {
		t.Fatalf("Reconcile (fresh): %v", err)
	}
	if first.Status != ReconcileApplied {
		t.Fatalf("fresh reconcile = %+v, want applied", first)
	}

	second, err := w.Reconcile(ReconcileRequest{Publish: &in})
	if err != nil {
		t.Fatalf("Reconcile (replay): %v", err)
	}
	if second.Status != ReconcileNoOp {
		t.Fatalf("replay reconcile = %+v, want no-op (replay gate)", second)
	}
	if second.InstanceVersion != first.InstanceVersion {
		t.Fatalf("replay changed the instance version: %d -> %d", first.InstanceVersion, second.InstanceVersion)
	}

	units, err := rt.Resolver().ResolveLine("feather", "sto", "rec")
	if err != nil || len(units) != 1 {
		t.Fatalf("instances = %d, %v; want exactly 1", len(units), err)
	}
}

// TestReconcilePublishDigestMismatchRefused proves a corrupted intent digest is
// a deterministic error, never a silent mis-write.
func TestReconcilePublishDigestMismatchRefused(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	in := PublishInput{Project: "p", Namespace: "feather", Type: "sto", ID: "bad", Content: stoContent("d")}
	if _, err := w.Reconcile(ReconcileRequest{Publish: &in, Digest: "not-the-digest"}); err == nil {
		t.Fatalf("Reconcile with a mismatched digest succeeded, want a deterministic refusal")
	}
}

// TestReconcileTransitionAppliedThenNoOp drives the transition reconcile: a
// not-yet-applied transition is applied, an identical re-issue is a no-op (the
// line is at the target), and a later target still applies.
func TestReconcileTransitionAppliedThenNoOp(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repo := testRepo(t, "p", "p-repo", "p")
	if _, err := w.Sync(repo, api.SyncOptions{}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if _, err := w.Publish(PublishInput{Project: "p", Namespace: "p", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	target := "p/sto:i-1"
	first, err := w.Reconcile(ReconcileRequest{RepoPath: repo, Target: target, To: "todo"})
	if err != nil {
		t.Fatalf("reconcile todo: %v", err)
	}
	if first.Status != ReconcileApplied || first.From != "planned" || first.To != "todo" {
		t.Fatalf("reconcile todo = %+v, want applied planned->todo", first)
	}

	// Replay: the line is already at todo -> no-op, no second transition.
	again, err := w.Reconcile(ReconcileRequest{RepoPath: repo, Target: target, To: "todo"})
	if err != nil {
		t.Fatalf("reconcile todo (replay): %v", err)
	}
	if again.Status != ReconcileNoOp {
		t.Fatalf("replay reconcile = %+v, want no-op", again)
	}

	// A forward target from the current state still applies.
	fwd, err := w.Reconcile(ReconcileRequest{RepoPath: repo, Target: target, To: "in-progress"})
	if err != nil {
		t.Fatalf("reconcile in-progress: %v", err)
	}
	if fwd.Status != ReconcileApplied {
		t.Fatalf("reconcile in-progress = %+v, want applied", fwd)
	}

	// A backward target from a later state is "at/after" -> no-op.
	back, err := w.Reconcile(ReconcileRequest{RepoPath: repo, Target: target, To: "todo"})
	if err != nil {
		t.Fatalf("reconcile todo (past): %v", err)
	}
	if back.Status != ReconcileNoOp {
		t.Fatalf("reconcile at/after target = %+v, want no-op", back)
	}

	units, _ := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if len(units) != 1 || units[0].StateVector.ExecutionState != "in-progress" {
		t.Fatalf("line state = %+v, want a single instance at in-progress", units)
	}
}

// TestReconcileTransitionNotFoundRefused pins the missing-line refusal.
func TestReconcileTransitionNotFoundRefused(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	if _, err := w.Reconcile(ReconcileRequest{RepoPath: t.TempDir(), Target: "p/sto:none", To: "todo"}); err == nil {
		t.Fatalf("reconcile of a missing line succeeded, want a refusal")
	}
}
