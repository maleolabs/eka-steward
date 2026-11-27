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
package escapehatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Run executes s if its executable base name is allowed. A disallowed or
// unknown command returns a *deterministic* error wrapping ErrNotAllowed
// that names the command. The argv is exec'd directly (no shell), only
// the passed Env is given to the child, and a cancelled ctx returns
// ctx.Err().
func (a AllowList) Run(ctx context.Context, s Spec) (Result, error) {
	base := filepath.Base(s.Name)
	if !a.Allows(base) {
		return Result{}, fmt.Errorf("%w: %q", ErrNotAllowed, base)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Dir = s.Dir
	if s.Env != nil {
		cmd.Env = s.Env
	} else {
		cmd.Env = []string{}
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
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
