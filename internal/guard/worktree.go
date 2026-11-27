package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/escapehatch"
)

// Worktree issues per-worker git worktrees. A worktree gives a worker its
// own checkout so concurrent workers do not fight over one working tree.
//
// It is VCS ISOLATION, NOT A SECURITY BOUNDARY: a worker process with
// filesystem access can still read or write outside its worktree (an
// absolute path, /tmp, the parent repository). The worktree only keeps git
// bookkeeping and the checkout separate. See docs/guardrails.md.
//
// git is run through internal/escapehatch — the ONE audited subprocess
// adapter — so neither this package nor any other imports os/exec.
type Worktree struct {
	repo  string
	base  string
	allow escapehatch.AllowList
}

// NewWorktree returns a Worktree manager rooted at repo, placing worker
// worktrees under base.
func NewWorktree(repo, base string) Worktree {
	return Worktree{repo: repo, base: base, allow: escapehatch.NewAllowList("git")}
}

// Path returns the deterministic worktree path for worker. The same worker
// name always yields the same path.
func (w Worktree) Path(worker string) string {
	return filepath.Join(w.base, "worker-"+sanitizeWorker(worker))
}

// Add creates the worker's worktree — detached at the repository's current
// HEAD — and returns its path. The base directory is created if missing.
func (w Worktree) Add(ctx context.Context, worker string) (string, error) {
	path := w.Path(worker)
	if err := os.MkdirAll(w.base, 0o755); err != nil {
		return "", fmt.Errorf("guard: worktree base %q: %w", w.base, err)
	}
	res, err := w.allow.Run(ctx, escapehatch.Spec{
		Name: "git",
		Args: []string{"worktree", "add", "--detach", path},
		Dir:  w.repo,
		Env:  gitEnv(),
	})
	if err != nil {
		return "", fmt.Errorf("guard: worktree add %q: %w", worker, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("guard: worktree add %q: git exit %d: %s",
			worker, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return path, nil
}

// Remove deletes the worker's worktree (forcing removal of a dirty tree).
func (w Worktree) Remove(ctx context.Context, worker string) error {
	path := w.Path(worker)
	res, err := w.allow.Run(ctx, escapehatch.Spec{
		Name: "git",
		Args: []string{"worktree", "remove", "--force", path},
		Dir:  w.repo,
		Env:  gitEnv(),
	})
	if err != nil {
		return fmt.Errorf("guard: worktree remove %q: %w", worker, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("guard: worktree remove %q: git exit %d: %s",
			worker, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// gitEnv is the environment git runs with: the never-duplicated WorkerEnv
// (no HOME, no EKA_HOME, no PATH, no credential variable) plus two static,
// non-secret git controls. The child inherits no host environment.
func gitEnv() []string {
	env := ekawrite.WorkerEnv()
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
}

// sanitizeWorker maps a worker name to a path-safe, deterministic token:
// every rune outside [A-Za-z0-9._-] becomes '_'.
func sanitizeWorker(worker string) string {
	if worker == "" {
		return "unnamed"
	}
	var b strings.Builder
	for _, r := range worker {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
