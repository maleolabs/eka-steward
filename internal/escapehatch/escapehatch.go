// Package escapehatch is the single, audited adapter for the steward's
// subprocess "escape hatch".
//
// eka-steward is a CONSUMER of eka-core and never writes the EKA store
// itself. The one legitimate reason to leave the process — shelling out
// to another CLI — is funnelled through this package so the whole
// surface is small, deterministic and reviewable in one place. See
// docs/boundary.md.
//
// Hard rules enforced here:
//   - No shell. Callers hand in an argv; the executable is exec'd
//     directly (never via "sh -c"), so there is no interpolation.
//   - An executable is refused deterministically unless its base name is
//     in the AllowList; an unknown command yields an error naming it.
//   - Only the passed Env reaches the child; an empty Env means an empty
//     environment, never the parent's.
//   - Cancellation is honoured: a cancelled context returns its error.
//
// Run captures whole streams; Start exposes them as pipes for a streaming
// protocol (the ACP client) with exactly the same allow-list, env and
// no-shell guarantees.
package escapehatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// ErrNotAllowed is returned (wrapped) when Spec.Name's base name is not
// in the AllowList. Test with errors.Is.
var ErrNotAllowed = errors.New("escapehatch: command not in allow-list")

// Spec is one subprocess invocation request.
type Spec struct {
	// Name is the executable. Only its base name is checked against the
	// AllowList; a full path is accepted but the base must be allowed.
	Name string
	// Args are passed to the executable verbatim — there is no shell.
	Args []string
	// Dir is the working directory ("" = the caller's).
	Dir string
	// Env is the child environment. A nil Env means an EMPTY
	// environment, not the parent's (no leakage).
	Env []string
}

// Result carries the outcome of an allowed run.
type Result struct {
	// ExitCode is the process exit code; 0 on success and the real code
	// on a clean non-zero exit (a non-zero exit is not an error).
	ExitCode int
	// Stdout and Stderr are the captured streams.
	Stdout []byte
	Stderr []byte
}

// AllowList is the explicit set of permitted executable base names. It
// has no zero-value meaning: construct it with NewAllowList naming every
// command on purpose.
type AllowList struct {
	names map[string]struct{}
}

// NewAllowList builds an AllowList from executable base names.
func NewAllowList(names ...string) AllowList {
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return AllowList{names: m}
}

// Allows reports whether base is in the allow-list.
func (a AllowList) Allows(base string) bool {
	_, ok := a.names[base]
	return ok
}

// command validates s and builds the direct-exec *exec.Cmd shared by Run and
// Start. The base name must be in the allow-list, a cancelled ctx is refused
// before any spawn, and the child environment is exactly s.Env (nil = empty),
// so no parent variable ever leaks.
func (a AllowList) command(ctx context.Context, s Spec) (*exec.Cmd, error) {
	base := filepath.Base(s.Name)
	if !a.Allows(base) {
		return nil, fmt.Errorf("%w: %q", ErrNotAllowed, base)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Dir = s.Dir
	if s.Env != nil {
		cmd.Env = s.Env
	} else {
		cmd.Env = []string{}
	}
	return cmd, nil
}

// Run executes s if its executable base name is allowed. A disallowed or
// unknown command returns a *deterministic* error wrapping ErrNotAllowed
// that names the command. The argv is exec'd directly (no shell), only
// the passed Env is given to the child, and a cancelled ctx returns
// ctx.Err().
func (a AllowList) Run(ctx context.Context, s Spec) (Result, error) {
	cmd, err := a.command(ctx, s)
	if err != nil {
		return Result{}, err
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil // a clean non-zero exit is data, not an error
	}
	return res, err // could not start the process
}

// Proc is a running child process started through Start. Its stdin is
// writable and its stdout/stderr are readable, so a caller can speak a
// streaming protocol (ACP) over the pipes. Call Wait (or Kill) to reap it.
type Proc struct {
	ctx    context.Context
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// Stdin returns the child's writable standard input.
func (p *Proc) Stdin() io.WriteCloser { return p.stdin }

// Stdout returns the child's readable standard output.
func (p *Proc) Stdout() io.ReadCloser { return p.stdout }

// Stderr returns the child's readable standard error.
func (p *Proc) Stderr() io.ReadCloser { return p.stderr }

// Start launches s exactly like Run — same allow-list, no shell, exactly
// s.Env — but wired to pipes instead of captured buffers, so the caller can
// stream over stdio. A cancelled ctx (or a later cancel) terminates the
// child; Wait then returns ctx.Err(). A disallowed command returns the same
// deterministic ErrNotAllowed refusal Run does.
func (a AllowList) Start(ctx context.Context, s Spec) (*Proc, error) {
	cmd, err := a.command(ctx, s)
	if err != nil {
		return nil, err
	}
	configureSysProcAttr(cmd)
	// Kill the whole child process group (unix) so an agent's own children
	// cannot outlive a cancel and hold the pipes open.
	cmd.Cancel = func() error { return killProcess(cmd) }

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Proc{ctx: ctx, cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// Wait reaps the child. It returns nil on a clean (zero) exit, ctx.Err()
// when the context was cancelled or the process was killed by it, and the
// underlying error (e.g. *exec.ExitError) otherwise.
func (p *Proc) Wait() error {
	err := p.cmd.Wait()
	if ctxErr := p.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// Kill terminates the child (and, on unix, its process group). It is
// idempotent and safe after the process has already exited.
func (p *Proc) Kill() error { return killProcess(p.cmd) }
