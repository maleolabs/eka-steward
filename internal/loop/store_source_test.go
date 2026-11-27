package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
)

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

// TestStoreSourceAndWritePath is the read-only + write-path smoke test: a
// store-backed Source reads published work items through api.Knowledge (no
// mutation), the loop dispatches one, and the ONLY mutation (planned→todo) goes
// through ekawrite. It is skipped under -short because it opens a real store.
func TestStoreSourceAndWritePath(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed smoke test (open a real EKA workspace)")
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
	for _, id := range []string{"i-1", "i-2"} {
		if _, err := w.Publish(ekawrite.PublishInput{
			Project:   "p",
			Namespace: "p",
			Type:      "sto",
			ID:        id,
			Content:   stoContent(id),
		}); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}

	src := StoreSource{Knowledge: rt.Knowledge(), Project: "p"}
	cs, err := src.Candidates(context.Background())
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cs))
	}
	for _, c := range cs {
		if c.State != decision.StatePlanned {
			t.Errorf("candidate %s state = %q, want planned (draft default)", c.ID, c.State)
		}
	}

	// Dispatch the head candidate and record planned→todo through ekawrite.
	spy := &spyDispatcher{}
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	l := New(src, spy, EkawriteWriter{Writer: w, RepoPath: repo}, store, Config{
		MaxWorkers:    4,
		DispatchState: "todo",
	})
	step, ok, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !ok {
		t.Fatal("Step reported nothing to do, want the head candidate")
	}
	if spy.calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1", spy.calls)
	}
	// The mutation is observable in the store: head is todo, the other planned.
	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine(sto:i-1) = %v, %v", units, err)
	}
	if got := units[0].StateVector.ExecutionState; got != "todo" {
		t.Fatalf("i-1 execution-state = %q, want todo (write through ekawrite)", got)
	}
	if step.CandidateID != "p/sto:i-1" {
		t.Fatalf("dispatched %q, want p/sto:i-1 (ID tie-break)", step.CandidateID)
	}
	// Resume: the dispatched item is now skipped.
	if steps, err := l.Plan(context.Background()); err != nil {
		t.Fatalf("Plan: %v", err)
	} else {
		for _, s := range steps {
			if s.CandidateID == "p/sto:i-1" {
				t.Fatalf("completed candidate still planned after Step")
			}
		}
	}
}
