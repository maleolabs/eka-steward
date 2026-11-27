package guard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/escapehatch"
)

func TestWorktreePathIsDeterministic(t *testing.T) {
	w := NewWorktree("/repo", "/base")
	if w.Path("w-1") != w.Path("w-1") {
		t.Fatal("Path is not deterministic")
	}
	if w.Path("w-1") == w.Path("w-2") {
		t.Fatal("distinct workers share a path")
	}
	if !strings.HasPrefix(filepath.Base(w.Path("w-1")), "worker-") {
		t.Fatalf("path %q lacks the worker- prefix", w.Path("w-1"))
	}
	// A worker name carrying separators cannot escape the base directory.
	if dir := filepath.Dir(w.Path("../evil")); dir != "/base" {
		t.Fatalf("worker name escaped the base dir: %q", w.Path("../evil"))
	}
}

func TestWorktreeCreateThenRemove(t *testing.T) {
	if !hasGit() {
		t.Skip("git not installed; skipping worktree test")
	}
	repo := initRepo(t)
	base := t.TempDir()
	w := NewWorktree(repo, base)

	path, err := w.Add(context.Background(), "w-1")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if path != w.Path("w-1") {
		t.Fatalf("Add returned %q, want %q", path, w.Path("w-1"))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree path missing after Add: %v", err)
	}
	// A linked worktree carries a .git FILE (not a dir): this is what makes
	// it a git worktree rather than a plain copy.
	if fi, err := os.Stat(filepath.Join(path, ".git")); err != nil || fi.IsDir() {
		t.Fatalf("path is not a git worktree (.git file): err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "seed.txt")); err != nil {
		t.Fatalf("worktree checkout is missing the committed file: %v", err)
	}

	if err := w.Remove(context.Background(), "w-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Remove: err=%v", err)
	}
}

// initRepo creates a temp git repository with one commit, using the audited
// escape hatch (never os/exec directly).
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	allow := escapehatch.NewAllowList("git")
	run := func(args ...string) {
		t.Helper()
		res, err := allow.Run(context.Background(), escapehatch.Spec{
			Name: "git", Args: args, Dir: repo, Env: gitEnv(),
		})
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("git %v: exit %d: %s", args, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init")
	return repo
}

// hasGit reports whether a git executable is on PATH, without importing
// os/exec (mirrors internal/driver's lookPath helper).
func hasGit() bool {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if info, err := os.Stat(filepath.Join(dir, "git")); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}
