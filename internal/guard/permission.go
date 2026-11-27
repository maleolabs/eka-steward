package guard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// OptionID is an ACP permission option id (v1: allow_once/allow_always/
// reject_once/reject_always). An id the steward does not recognise is
// NEVER approval (docs/acp-spike.md §11).
type OptionID string

const (
	// OptionAllowOnce grants the single request.
	OptionAllowOnce OptionID = "allow_once"
	// OptionAllowAlways grants all future requests; the built-in policy
	// never auto-selects it.
	OptionAllowAlways OptionID = "allow_always"
	// OptionRejectOnce refuses the single request.
	OptionRejectOnce OptionID = "reject_once"
	// OptionRejectAlways refuses all future requests.
	OptionRejectAlways OptionID = "reject_always"
)

// Outcome is the steward's permission decision. The zero value is Deny, so
// an unset Outcome is safe.
type Outcome int

const (
	// Deny refuses the request (the zero value).
	Deny Outcome = iota
	// Allow permits the request.
	Allow
)

// Tool classes. Only ToolRead is ever allowable by the built-in policy.
const (
	ToolRead    = "read"
	ToolEdit    = "edit"
	ToolExecute = "execute"
	ToolDelete  = "delete"
	ToolMove    = "move"
)

// DefaultPermissionTimeout bounds every permission wait when Policy.Timeout
// is unset (<= 0). See docs/acp-spike.md §11: bounded wait, never hang.
const DefaultPermissionTimeout = 120 * time.Second

// PermissionRequest is one ACP permission request the steward must answer.
type PermissionRequest struct {
	// ToolKind is the requested tool class (read/edit/execute/...).
	ToolKind string
	// Path is the requested path, if any.
	Path string
	// WorkspaceRoot is the session workspace root a read must stay inside.
	WorkspaceRoot string
	// OptionID is the option under consideration (Decide) or returned by
	// the responder (AwaitAnswer). An unknown id is never Allow.
	OptionID OptionID
}

// Policy is the steward's permission policy. Its zero value is
// DEFAULT-DENY: it allows no read and applies the default wait bound.
type Policy struct {
	// AllowReadWithinRoot permits read-class requests inside
	// WorkspaceRoot. The zero value (false) allows nothing.
	AllowReadWithinRoot bool
	// Timeout bounds AwaitAnswer. <= 0 applies DefaultPermissionTimeout.
	Timeout time.Duration
}

// Decide is the pure, deterministic decision for req. The same input always
// yields the same answer: no clock, randomness, I/O or state.
//
// From the zero Policy, every request is Deny. A request is Allow ONLY when
// its OptionID is allow_once, its class is read, the policy allows reads,
// and Path is inside WorkspaceRoot. Everything else — execute/edit/delete/
// move, any other class, an unknown OptionID, allow_always, or a path that
// escapes the root — is Deny.
func (p Policy) Decide(req PermissionRequest) Outcome {
	if resolveOption(req.OptionID) != Allow {
		return Deny
	}
	if req.ToolKind != ToolRead {
		return Deny
	}
	if !p.AllowReadWithinRoot {
		return Deny
	}
	if !withinRoot(req.Path, req.WorkspaceRoot) {
		return Deny
	}
	return Allow
}

// AwaitAnswer waits for an external answer and maps it to an Outcome. The
// wait is BOUNDED by Policy.Timeout (DefaultPermissionTimeout when unset):
// on timeout or a cancelled ctx it returns (Deny, ctx.Err()) and NEVER
// hangs. A responder error, a panic inside ask, and an unknown option id
// all resolve to Deny. Only allow_once maps to Allow (allow_always, the
// reject ids and any unknown id are Deny), so an unrecognised answer is
// never approval.
func (p Policy) AwaitAnswer(ctx context.Context, ask func(context.Context) (OptionID, error)) (Outcome, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultPermissionTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type answer struct {
		id  OptionID
		err error
	}
	// Buffered so the ask goroutine can never block on send: if the wait
	// has already timed out, the send still completes and the goroutine
	// exits.
	ch := make(chan answer, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- answer{err: fmt.Errorf("guard: permission ask panicked: %v", r)}
			}
		}()
		id, err := ask(ctx)
		ch <- answer{id: id, err: err}
	}()

	select {
	case a := <-ch:
		if a.err != nil {
			return Deny, a.err
		}
		return resolveOption(a.id), nil
	case <-ctx.Done():
		return Deny, ctx.Err()
	}
}

// resolveOption maps an option id to the outcome it alone can grant. Only
// allow_once yields Allow; every other id — including allow_always and any
// unknown value — yields Deny.
func resolveOption(id OptionID) Outcome {
	if id == OptionAllowOnce {
		return Allow
	}
	return Deny
}

// withinRoot reports whether path, after cleaning, is inside root. Both must
// be absolute; a path that escapes root via ".." is not within it.
func withinRoot(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if !filepath.IsAbs(path) || !filepath.IsAbs(root) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
