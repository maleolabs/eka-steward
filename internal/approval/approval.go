// Package approval is the steward's human-approval channel for permission
// requests the pure policy CANNOT decide (guard.Escalate). It defines the
// channel seam (Approver), a file/queue-based default that needs NO TTY, the
// deterministic request key, and a bounded wait whose timeout is ALWAYS a
// Deny (never a hang).
//
// The channel is deliberately NOT an EKA write path: this package only moves
// a request to a human and a decision back. Recording that decision as EKA
// knowledge is the loop's job, through internal/ekawrite (docs/approval.md).
// It imports nothing from eka-core and never touches os.Stdin / a terminal.
package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// Verdict is a human decision on an escalated permission request.
type Verdict string

const (
	// Approve grants the request.
	Approve Verdict = "approve"
	// Deny refuses it. A timed-out wait is recorded as Deny (TimedOut true).
	Deny Verdict = "deny"
)

// Decision is one human answer. The zero value is Deny, so an unset Decision
// is safe.
type Decision struct {
	Verdict Verdict `json:"verdict"`
	// By is the approver identity when known ("" for a timeout).
	By string `json:"by,omitempty"`
	// Reason is the approver's optional note.
	Reason string `json:"reason,omitempty"`
	// TimedOut records that the bounded wait elapsed without an answer; the
	// verdict is then Deny.
	TimedOut bool `json:"timedOut,omitempty"`
}

// Approved reports whether the decision grants the request.
func (d Decision) Approved() bool { return d.Verdict == Approve }

// Request is one escalated permission request handed to the human channel.
// Key is the deterministic identity of the request: the same permission
// request always yields the same Key, so a re-issued request (a resume) is
// the SAME approval, never a second one.
type Request struct {
	Key           string `json:"key"`
	ToolKind      string `json:"toolKind"`
	Path          string `json:"path,omitempty"`
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	OptionID      string `json:"optionId"`
	// Reason explains why the policy could not decide (documentation only;
	// it does not enter the Key).
	Reason string `json:"reason,omitempty"`
}

// Approver is the human channel seam. Ask sends one request and returns the
// decision. Implementations MUST honor ctx; the bounded wait in AskBounded
// additionally guarantees the steward never hangs even when an Approver
// ignores it.
type Approver interface {
	Ask(ctx context.Context, req Request) (Decision, error)
}

// RequestFor projects a guard permission request into an approval request,
// computing the deterministic Key. The Key covers the request's decision
// inputs only (tool kind, path, workspace root, option id) — never the clock,
// a random id or a map order — so it is stable across runs and restarts.
func RequestFor(req guard.PermissionRequest) Request {
	key := requestKey(string(req.OptionID), req.ToolKind, req.Path, req.WorkspaceRoot)
	return Request{
		Key:           key,
		ToolKind:      req.ToolKind,
		Path:          req.Path,
		WorkspaceRoot: req.WorkspaceRoot,
		OptionID:      string(req.OptionID),
	}
}

// requestKey is the SHA-256 over a NUL-delimited, deterministic projection of
// the request's decision inputs.
func requestKey(option, toolKind, path, root string) string {
	h := sha256.New()
	for _, f := range []string{"approval/1", option, toolKind, path, root} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AskBounded sends req to a and waits for the answer under the policy's
// timeout (guard.DefaultPermissionTimeout when Policy.Timeout <= 0). The wait
// is BOUNDED: on timeout or a cancelled ctx it returns (Deny{TimedOut:true},
// ctx.Err()) and NEVER hangs — even if the Approver ignores the context — and
// a panicking Approver is contained. This mirrors guard.Policy.AwaitAnswer's
// bounded-wait discipline; any error or non-approve answer is a Deny.
func AskBounded(ctx context.Context, p guard.Policy, a Approver, req Request) (Decision, error) {
	if a == nil {
		return Decision{Verdict: Deny}, fmt.Errorf("approval: nil approver")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = guard.DefaultPermissionTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type answer struct {
		d   Decision
		err error
	}
	// Buffered so the approver goroutine can never block on send: if the wait
	// has already timed out, the send still completes and the goroutine exits.
	ch := make(chan answer, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- answer{d: Decision{Verdict: Deny}, err: fmt.Errorf("approval: approver panicked: %v", r)}
			}
		}()
		d, err := a.Ask(cctx, req)
		ch <- answer{d: d, err: err}
	}()

	select {
	case a := <-ch:
		if a.err != nil {
			return Decision{Verdict: Deny}, a.err
		}
		if a.d.Verdict != Approve {
			if a.d.Verdict == "" {
				a.d.Verdict = Deny
			}
			return a.d, nil
		}
		return a.d, nil
	case <-cctx.Done():
		return Decision{Verdict: Deny, TimedOut: true}, cctx.Err()
	}
}

// TimedOut reports whether err is the bounded-wait timeout of AskBounded (as
// opposed to an approver error). The caller records a timeout as a deny.
func TimedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
