package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
)

// fakeSource is a store-free loop.Source: a fixed candidate set in the ready
// state, returned in the given order.
type fakeSource struct{ ids []string }

func (s fakeSource) Candidates(context.Context) ([]decision.Candidate, error) {
	out := make([]decision.Candidate, 0, len(s.ids))
	for _, id := range s.ids {
		out = append(out, decision.Candidate{ID: id, State: "planned"})
	}
	return out, nil
}

// fakeDispatcher records every dispatch so a test can prove each candidate ran
// exactly once. It never writes EKA.
type fakeDispatcher struct {
	mu   sync.Mutex
	seen []string
}

func (d *fakeDispatcher) Dispatch(_ context.Context, c decision.Candidate) (loop.DispatchResult, error) {
	d.mu.Lock()
	d.seen = append(d.seen, c.ID)
	d.mu.Unlock()
	return loop.DispatchResult{CandidateID: c.ID, Output: "ok"}, nil
}

func (d *fakeDispatcher) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// testRepo writes a minimal eka.yaml into a fresh temp dir.
func testRepo(t *testing.T, project, name, namespace string) string {
	t.Helper()
	dir := t.TempDir()
	meta := "version: 1\nproject: " + project + "\nname: " + name + "\nnamespace: " + namespace + "\n"
	if err := os.WriteFile(filepath.Join(dir, "eka.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write eka.yaml: %v", err)
	}
	return dir
}

func stoContent(description string) map[string]any {
	return map[string]any{"description": description, "acceptanceCriteria": "ac " + description}
}

// TestRunFakeDispatcherToQuiescence drives a store-free source/dispatcher pair
// to quiescence with the write path disabled (DispatchState == ""): every
// candidate is dispatched exactly once, the loop records them done, and Plan is
// empty afterwards.
func TestRunFakeDispatcherToQuiescence(t *testing.T) {
	t.Setenv("EKA_HOME", t.TempDir())

	disp := &fakeDispatcher{}
	a, err := Assemble(Options{
		Project:    "p",
		StateDir:   t.TempDir(),
		Source:     fakeSource{ids: []string{"p/sto:a", "p/sto:b", "p/sto:c"}},
		Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer a.Close()

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(disp.calls()); got != 3 {
		t.Fatalf("dispatch count = %d, want 3 (%v)", got, disp.calls())
	}
	if got := len(a.Report().Steps); got != 3 {
		t.Fatalf("report steps = %d, want 3", got)
	}
	steps, err := a.loop.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("post-run plan = %d steps, want quiescent", len(steps))
	}
}

// TestStoreBackedSmokeRun publishes one planned work item, drives it through
// the assembled run, and asserts the single write path moved it to todo exactly
// once.
func TestStoreBackedSmokeRun(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed smoke run (opens a real EKA workspace)")
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
	if _, err := w.Publish(ekawrite.PublishInput{Project: "p", Namespace: "p", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	a, err := Assemble(Options{
		Project:       "p",
		Repo:          repo,
		StateDir:      t.TempDir(),
		DispatchState: "todo",
		Dispatcher:    &fakeDispatcher{},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer a.Close()

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine = %d units, %v; want exactly 1", len(units), err)
	}
	if got := units[0].StateVector.ExecutionState; got != "todo" {
		t.Fatalf("i-1 execution-state = %q, want todo", got)
	}
}

// TestResumeAfterCrashNoDuplicateWrite crashes after the write applied but
// before the intent is cleared, then resumes: the replay gate makes reconcile a
// no-op, so the write is not duplicated and the item is completed once.
func TestResumeAfterCrashNoDuplicateWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed resume (opens a real EKA workspace)")
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
	if _, err := w.Publish(ekawrite.PublishInput{Project: "p", Namespace: "p", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	stateDir := t.TempDir()
	opts := Options{
		Project:       "p",
		Repo:          repo,
		StateDir:      stateDir,
		DispatchState: "todo",
		Dispatcher:    &fakeDispatcher{},
	}

	// First run: crash after apply, before the intent is cleared.
	crashed := false
	opts.Crash = func(boundary string) {
		if boundary == loop.BoundaryAfterApply {
			crashed = true
			panic("simulated kill")
		}
	}
	func() {
		defer func() { _ = recover() }()
		a, err := Assemble(opts)
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		defer a.Close()
		_ = a.Run(context.Background())
	}()
	if !crashed {
		t.Fatal("crash hook never fired at after-apply")
	}

	// Second run: no crash, resume reconciles the in-flight intent.
	opts.Crash = nil
	a, err := Assemble(opts)
	if err != nil {
		t.Fatalf("Assemble (resume): %v", err)
	}
	defer a.Close()
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("resume Run: %v", err)
	}

	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine = %d units, %v; want exactly 1", len(units), err)
	}
	if got := units[0].StateVector.ExecutionState; got != "todo" {
		t.Fatalf("i-1 execution-state = %q, want todo", got)
	}
	writes := a.Report().Writes
	if len(writes) != 1 {
		t.Fatalf("resume writes = %d, want 1 (the reconciled intent)", len(writes))
	}
	if writes[0].Result != "no-op" {
		t.Fatalf("resume write result = %q, want no-op (replay gate)", writes[0].Result)
	}
}
