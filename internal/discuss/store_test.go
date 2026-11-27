package discuss

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
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

// TestStoreBackedDiscussionTurn is the end-to-end acceptance test against a
// real EKA store: a discussion publishes dec- knowledge through the single
// write path, the next-phase input is reconstructed from the store, and a
// re-run is a replay no-op (no duplicate instances). It is skipped under
// -short because it opens a real workspace.
func TestStoreBackedDiscussionTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed discussion test (open a real EKA workspace)")
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
	if _, err := w.Publish(ekawrite.PublishInput{
		Project:   "p",
		Namespace: "p",
		Type:      "sto",
		ID:        "i-1",
		Content:   map[string]any{"description": "context renderer", "acceptanceCriteria": "bounded deterministic payload"},
	}); err != nil {
		t.Fatalf("publish subject: %v", err)
	}

	read := StoreKnowledge{Knowledge: rt.Knowledge(), Resolver: rt.Resolver(), Project: "p"}
	recorder := loop.EkawriteWriter{Writer: w}
	dis := &fakeDispatcher{}

	run := func(checkpoint string) {
		d := New(read, dis, recorder, &loop.Store{Path: filepath.Join(t.TempDir(), checkpoint)}, Config{
			Project:  "p",
			Subject:  "p/sto:i-1",
			Rounds:   2,
			MaxBytes: 2048,
		})
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	run("checkpoint.json")

	// Each round recorded exactly one dec- instance in the store.
	for _, id := range []string{"i-1-dec-r01", "i-1-dec-r02"} {
		units, err := rt.Resolver().ResolveLine("p", DecisionType, id)
		if err != nil {
			t.Fatalf("ResolveLine(%s): %v", id, err)
		}
		if len(units) != 1 {
			t.Fatalf("dec %s instances = %d, want 1", id, len(units))
		}
		if !strings.Contains(string(units[0].ContentPayload), "decision i-1-dec-r01") &&
			!strings.Contains(string(units[0].ContentPayload), "decision i-1-dec-r02") {
			t.Fatalf("recorded dec %s content missing the worker decision:\n%s", id, units[0].ContentPayload)
		}
	}

	// AC3: build the next-phase input from a FRESH read seam (no Discussion
	// value in scope) after the session has finished.
	fresh := StoreKnowledge{Knowledge: rt.Knowledge(), Resolver: rt.Resolver(), Project: "p"}
	exec, err := BuildExecuteInput(ctx, fresh, "p/sto:i-1", 8192)
	if err != nil {
		t.Fatalf("BuildExecuteInput: %v", err)
	}
	for _, want := range []string{"decision i-1-dec-r01", "decision i-1-dec-r02"} {
		if !strings.Contains(string(exec), want) {
			t.Fatalf("store-derived execute input missing %q:\n%s", want, exec)
		}
	}

	// Replay gate: a second run with a FRESH checkpoint (state lost) re-issues
	// the identical payloads; ekawrite's read-then-write gate makes each a
	// no-op, so no decision gains a second instance.
	before := map[string]int{}
	for _, id := range []string{"i-1-dec-r01", "i-1-dec-r02"} {
		units, err := rt.Resolver().ResolveLine("p", DecisionType, id)
		if err != nil {
			t.Fatalf("ResolveLine(%s): %v", id, err)
		}
		before[id] = len(units)
	}
	run("checkpoint-2.json")
	for _, id := range []string{"i-1-dec-r01", "i-1-dec-r02"} {
		units, err := rt.Resolver().ResolveLine("p", DecisionType, id)
		if err != nil {
			t.Fatalf("ResolveLine(%s): %v", id, err)
		}
		if len(units) != before[id] {
			t.Fatalf("replay duplicated dec %s: %d instances, want %d", id, len(units), before[id])
		}
	}

	// Integrity holds after the discussion writes.
	report, err := rt.Integrity().Verify()
	if err != nil {
		t.Fatalf("Integrity.Verify: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Fatalf("integrity violations after discussion: %+v", report.Violations)
	}
}
