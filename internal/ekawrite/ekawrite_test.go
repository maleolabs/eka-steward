package ekawrite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-core/v2/exchange"
)

// These tests drive the write path end to end against a real (temp) EKA
// workspace and a real (temp) repository. Fixtures use the api contract only
// (no kernel-internal import): the workspace is a temp EKA_HOME, the
// repository is a temp dir carrying a minimal eka.yaml, registered through
// Authoring.Sync.

// testRuntime opens a temp EKA workspace and returns it.
func testRuntime(t *testing.T) api.Runtime {
	t.Helper()
	t.Setenv("EKA_HOME", t.TempDir())
	rt, err := api.Ensure()
	if err != nil {
		t.Fatalf("api.Ensure: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// testRepo writes a minimal eka.yaml into a fresh temp dir (the shape of a
// real repository's metadata) and returns the directory.
func testRepo(t *testing.T, project, name, namespace string) string {
	t.Helper()
	dir := t.TempDir()
	meta := "version: 1\nproject: " + project + "\nname: " + name + "\nnamespace: " + namespace + "\n"
	if err := os.WriteFile(filepath.Join(dir, "eka.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write eka.yaml: %v", err)
	}
	return dir
}

// stoContent is a publishable sto- body: the two required sections.
func stoContent(description string) map[string]any {
	return map[string]any{"description": description, "acceptanceCriteria": "ac " + description}
}

// --- Deterministic instance versions + replay safety (AC3) ---

func TestPublishReplayIsNoOp(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)

	in := PublishInput{
		Project:   "p",
		Namespace: "feather",
		Type:      "sto",
		ID:        "replay",
		Content:   stoContent("d"),
	}

	first, err := w.Publish(in)
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if first.Status != PublishPublished || first.InstanceVersion != 1 {
		t.Fatalf("first Publish = %+v, want published v1", first)
	}

	second, err := w.Publish(in)
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if second.Status != PublishNoOp {
		t.Errorf("second Publish status = %q, want %q (replay gate)", second.Status, PublishNoOp)
	}
	if second.InstanceVersion != 1 || second.Form != first.Form {
		t.Errorf("second Publish = %+v, want the SAME instance %s (no max+1)", second, first.Form)
	}

	units, err := rt.Resolver().ResolveLine("feather", "sto", "replay")
	if err != nil {
		t.Fatalf("ResolveLine: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("instances = %d, want exactly 1 (replay must not allocate a version)", len(units))
	}
}

func TestPublishChangedPayloadAddsOneInstance(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)

	in := PublishInput{Project: "p", Namespace: "feather", Type: "sto", ID: "change", Content: stoContent("d")}
	if _, err := w.Publish(in); err != nil {
		t.Fatalf("Publish v1: %v", err)
	}

	in.Content = stoContent("changed")
	second, err := w.Publish(in)
	if err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	if second.Status != PublishPublished || second.InstanceVersion != 2 {
		t.Fatalf("changed Publish = %+v, want published v2 (max 1 + 1)", second)
	}

	units, err := rt.Resolver().ResolveLine("feather", "sto", "change")
	if err != nil {
		t.Fatalf("ResolveLine: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("instances = %d, want exactly 2 (one additional instance)", len(units))
	}

	// Re-issuing the ORIGINAL payload as a third write is again a no-op:
	// the line's current instance (v2) carries a different digest, so it
	// publishes v3 — the gate compares against the current instance, not
	// any historical one.
	in.Content = stoContent("d")
	third, err := w.Publish(in)
	if err != nil {
		t.Fatalf("Publish v3: %v", err)
	}
	if third.Status != PublishPublished || third.InstanceVersion != 3 {
		t.Fatalf("reverted Publish = %+v, want published v3", third)
	}
}

// --- Attribution (AC5) ---

func TestAttributionKindIsDeterministic(t *testing.T) {
	rt := testRuntime(t)

	workerAuthor := NewWorker(rt).Author()
	agentAuthor := NewSteward(rt).Author()
	if workerAuthor.Kind != "worker" || agentAuthor.Kind != "agent" {
		t.Fatalf("author kinds = %q / %q, want worker / agent", workerAuthor.Kind, agentAuthor.Kind)
	}

	worker := NewWorker(rt)
	if _, err := worker.Publish(PublishInput{Project: "p", Namespace: "feather", Type: "sto", ID: "by-worker", Content: stoContent("w")}); err != nil {
		t.Fatalf("worker Publish: %v", err)
	}
	steward := NewSteward(rt)
	if _, err := steward.Publish(PublishInput{Project: "p", Namespace: "feather", Type: "sto", ID: "by-agent", Content: stoContent("a")}); err != nil {
		t.Fatalf("steward Publish: %v", err)
	}

	cases := []struct {
		id   string
		kind string
	}{
		{"by-worker", "worker"},
		{"by-agent", "agent"},
	}
	for _, c := range cases {
		units, err := rt.Resolver().ResolveLine("feather", "sto", c.id)
		if err != nil || len(units) != 1 {
			t.Fatalf("ResolveLine(%s) = %v, %v", c.id, units, err)
		}
		if got := units[0].Author.Kind; got != c.kind {
			t.Errorf("persisted Author.Kind of %s = %q, want %q", c.id, got, c.kind)
		}
	}
}

// --- ADR-032: provenance + explicit adopt (AC2) ---

func TestStewardAuthoredUnitTravelsAfterAdopt(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")

	// Registration (no pull/push): the repository identity is established.
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync (register): %v", err)
	}

	// Repo-context publish: the caller names the repository's project and
	// namespace and will adopt explicitly.
	pub, err := w.Publish(PublishInput{
		Project:   "steward",
		Namespace: "steward",
		Type:      "sto",
		ID:        "wp-1",
		Content:   stoContent("d"),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pub.Form != "steward/sto:wp-1:1" {
		t.Fatalf("published form = %q, want steward/sto:wp-1:1", pub.Form)
	}

	snapshotDir := filepath.Join(repoDir, "exchange", "snapshots")

	// Workspace-native publish alone never reaches a repository snapshot.
	if _, err := exchange.LoadSnapshot(snapshotDir); err == nil {
		t.Fatalf("publish alone must not seed a repository snapshot")
	}

	// Explicit adopt re-attributes the reference to the repository.
	adopted, err := w.Adopt(repoDir, nil, false)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted.Units != 1 {
		t.Fatalf("adopted units = %d, want 1", adopted.Units)
	}

	// The next push emits it into the repository snapshot.
	if _, err := w.Sync(repoDir, api.SyncOptions{Push: true}); err != nil {
		t.Fatalf("Sync (push): %v", err)
	}
	res, err := exchange.LoadSnapshot(snapshotDir)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	found := false
	for _, u := range res.Package.Units {
		if u.CanonicalIdentityForm == "steward/sto:wp-1:1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the steward-authored unit is absent from the repository snapshot: %+v", res.Package.Units)
	}

	// Store integrity holds after the run.
	report, err := rt.Integrity().Verify()
	if err != nil {
		t.Fatalf("Integrity.Verify: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Fatalf("integrity violations after the write path = %+v, want none", report.Violations)
	}
}

// TestIntegrityAfterRun exercises every write class once and asserts a clean
// store (AC6).
func TestIntegrityAfterRun(t *testing.T) {
	rt := testRuntime(t)
	w := NewSteward(rt)
	repoDir := testRepo(t, "steward", "steward-repo", "steward")
	if _, err := w.Sync(repoDir, api.SyncOptions{}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := w.Publish(PublishInput{Project: "steward", Namespace: "steward", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := w.Transition(api.TransitionRequest{RepoPath: repoDir, Target: "sto:i-1", To: "todo", Confirmed: true}); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if _, err := w.Note(api.NoteDraftRequest{RepoPath: repoDir, Target: "sto:i-1", Role: "implementation"}); err != nil {
		t.Fatalf("Note: %v", err)
	}

	report, err := rt.Integrity().Verify()
	if err != nil {
		t.Fatalf("Integrity.Verify: %v", err)
	}
	if len(report.Violations) != 0 {
		t.Fatalf("integrity violations = %+v, want none", report.Violations)
	}
}

// --- Worker environment (AC4) ---

func TestWorkerEnvBlocksStoreAccess(t *testing.T) {
	env := WorkerEnv()

	// No HOME, no EKA_HOME, no PATH, no credential-looking variable.
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "HOME", "EKA_HOME", "PATH":
			t.Errorf("WorkerEnv must not carry %s (%q)", key, kv)
		}
		upper := strings.ToUpper(key)
		for _, needle := range []string{"TOKEN", "KEY", "SECRET", "CREDENTIAL", "PASSWORD", "AUTH"} {
			if strings.Contains(upper, needle) {
				t.Errorf("WorkerEnv must not carry a credential variable (%q)", kv)
			}
		}
	}

	if !WorkerHomeUnresolvable(env) {
		t.Errorf("WorkerEnv must leave the EKA home unresolvable")
	}

	// Portable assertion: with no EKA_HOME and no HOME, the EKA workspace
	// root cannot be resolved, so opening the default store path fails.
	t.Setenv("HOME", "")
	t.Setenv("EKA_HOME", "")
	if _, err := api.HomeDir(); err == nil {
		t.Errorf("api.HomeDir must fail with no HOME/EKA_HOME")
	}
	if _, err := api.Open(); err == nil {
		t.Errorf("api.Open must fail when the EKA home is unresolvable")
	}

	// Portable assertion: a bare-name `eka` lookup cannot resolve under a
	// PATH-less environment (what the worker env provides).
	if p, ok := lookPath(env, "eka"); ok {
		t.Errorf("WorkerEnv resolved a bare-name eka at %s", p)
	}
	// Positive control: the same lookup DOES resolve when a PATH is present.
	dir := t.TempDir()
	fake := filepath.Join(dir, "eka")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookPath([]string{"PATH=" + dir}, "eka"); !ok {
		t.Errorf("lookPath positive control failed to resolve a fake eka under PATH")
	}
}

// lookPath resolves file (a base name) against the PATH carried by env,
// mirroring the process-env lookup a spawned child would perform. It is a
// test-only stand-in: Go's exec.LookPath reads the PROCESS environment, not
// a child's, so this is how the env-scoped lookup is asserted portably.
func lookPath(env []string, file string) (string, bool) {
	if strings.ContainsRune(file, os.PathSeparator) {
		if info, err := os.Stat(file); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return file, true
		}
		return "", false
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, file)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}
