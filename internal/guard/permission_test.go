package guard

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDecideTable(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	inside := filepath.Join(root, "sub", "a.txt")
	escape := filepath.Join(root, "..", "etc", "passwd")
	outside := filepath.Join(string(filepath.Separator), "other", "a.txt")

	allowRead := Policy{AllowReadWithinRoot: true}
	denyRead := Policy{}
	// denyUndecidable opts out of escalation: the same undecidable requests
	// that default-escalate are then refused (the clearly-deniable fallback).
	denyUndecidable := Policy{AllowReadWithinRoot: true, DenyUndecidable: true}

	tests := []struct {
		name string
		p    Policy
		req  PermissionRequest
		want Outcome
	}{
		// Clearly-deniable: a non-allow_once option is Deny, never escalate.
		{"zero request denied", denyRead, PermissionRequest{}, Deny},
		{"read unknown option denied", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: "allow_now"}, Deny},
		{"read allow_always denied", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowAlways}, Deny},
		{"read reject denied", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: OptionRejectOnce}, Deny},

		// Decided by the policy alone (inside the root).
		{"read inside root allowed", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Allow},
		{"read at root allowed", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: root, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Allow},
		{"read denied when policy forbids", denyRead,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Deny},

		// Undecidable: escalate by default (default-escalate), deny on opt-out.
		{"read escaping via .. escalated", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: escape, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"read outside root escalated", allowRead,
			PermissionRequest{ToolKind: ToolRead, Path: outside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"execute escalated", allowRead,
			PermissionRequest{ToolKind: ToolExecute, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"edit escalated", allowRead,
			PermissionRequest{ToolKind: ToolEdit, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"delete escalated", allowRead,
			PermissionRequest{ToolKind: ToolDelete, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"move escalated", allowRead,
			PermissionRequest{ToolKind: ToolMove, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},
		{"unknown class escalated", allowRead,
			PermissionRequest{ToolKind: "network", Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Escalate},

		// Opt-out: undecidable → Deny, the clearly-deniable classes unchanged.
		{"read outside root denied on opt-out", denyUndecidable,
			PermissionRequest{ToolKind: ToolRead, Path: outside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Deny},
		{"execute denied on opt-out", denyUndecidable,
			PermissionRequest{ToolKind: ToolExecute, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Deny},
		{"read inside root still allowed on opt-out", denyUndecidable,
			PermissionRequest{ToolKind: ToolRead, Path: inside, WorkspaceRoot: root, OptionID: OptionAllowOnce}, Allow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Decide(tc.req); got != tc.want {
				t.Fatalf("Decide = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideIsDeterministic(t *testing.T) {
	p := Policy{AllowReadWithinRoot: true}
	req := PermissionRequest{ToolKind: ToolRead, Path: "/ws/a", WorkspaceRoot: "/ws", OptionID: OptionAllowOnce}
	want := p.Decide(req)
	for i := 0; i < 100; i++ {
		if got := p.Decide(req); got != want {
			t.Fatalf("Decide drifted: %v vs %v", got, want)
		}
	}
}

// TestEscalationIsNotApproval asserts the escalation branch never turns a
// clearly-deniable request into Escalate, and that the default-escalate policy
// never silently Allows an undecidable one.
func TestEscalationIsNotApproval(t *testing.T) {
	p := Policy{AllowReadWithinRoot: true}
	// A bad option id is Deny regardless of the class/path.
	for _, id := range []OptionID{OptionRejectOnce, OptionRejectAlways, OptionAllowAlways, "bogus"} {
		if got := p.Decide(PermissionRequest{ToolKind: ToolExecute, Path: "/etc/x", WorkspaceRoot: "/ws", OptionID: id}); got != Deny {
			t.Fatalf("Decide(option %q) = %v, want Deny (escalation must not rescue a refusal)", id, got)
		}
	}
	// Default-escalate never yields Allow for an undecidable request.
	req := PermissionRequest{ToolKind: ToolRead, Path: "/etc/passwd", WorkspaceRoot: "/ws", OptionID: OptionAllowOnce}
	for i := 0; i < 50; i++ {
		if got := p.Decide(req); got != Escalate {
			t.Fatalf("Decide(undecidable) = %v, want Escalate (never Allow)", got)
		}
	}
}

func TestAwaitAnswerBranches(t *testing.T) {
	p := Policy{Timeout: time.Second}
	tests := []struct {
		name    string
		ask     func(context.Context) (OptionID, error)
		want    Outcome
		wantErr bool
	}{
		{"allow_once -> Allow", func(context.Context) (OptionID, error) { return OptionAllowOnce, nil }, Allow, false},
		{"reject_once -> Deny", func(context.Context) (OptionID, error) { return OptionRejectOnce, nil }, Deny, false},
		{"allow_always -> Deny", func(context.Context) (OptionID, error) { return OptionAllowAlways, nil }, Deny, false},
		{"unknown option -> Deny", func(context.Context) (OptionID, error) { return "bogus", nil }, Deny, false},
		{"responder error -> Deny", func(context.Context) (OptionID, error) {
			return "", errors.New("boom")
		}, Deny, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.AwaitAnswer(context.Background(), tc.ask)
			if got != tc.want {
				t.Fatalf("AwaitAnswer = %v, want %v", got, tc.want)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("AwaitAnswer err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestAwaitAnswerTimeoutNeverHangs: a responder that blocks until its ctx is
// done must not hang the steward; the wait is bounded and the outcome Deny.
func TestAwaitAnswerTimeoutNeverHangs(t *testing.T) {
	p := Policy{Timeout: 50 * time.Millisecond}
	ask := func(ctx context.Context) (OptionID, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	got, err := p.AwaitAnswer(context.Background(), ask)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("AwaitAnswer took %v; must be bounded by Timeout", elapsed)
	}
	if got != Deny {
		t.Fatalf("outcome = %v, want Deny", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestAwaitAnswerResponderIgnoresCtx proves the wait returns even when the
// responder never observes cancellation.
func TestAwaitAnswerResponderIgnoresCtx(t *testing.T) {
	p := Policy{Timeout: 30 * time.Millisecond}
	blocked := make(chan struct{})
	defer close(blocked) // release the responder at test end
	ask := func(context.Context) (OptionID, error) {
		<-blocked
		return OptionAllowOnce, nil
	}
	start := time.Now()
	got, err := p.AwaitAnswer(context.Background(), ask)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("AwaitAnswer hung %v on a ctx-ignoring responder", elapsed)
	}
	if got != Deny || err == nil {
		t.Fatalf("AwaitAnswer = (%v, %v), want (Deny, non-nil)", got, err)
	}
}

// TestAwaitAnswerPanicIsContained: a panicking responder must not crash the
// steward; the bounded wait returns Deny with an error.
func TestAwaitAnswerPanicIsContained(t *testing.T) {
	p := Policy{Timeout: time.Second}
	got, err := p.AwaitAnswer(context.Background(), func(context.Context) (OptionID, error) {
		panic("responder exploded")
	})
	if got != Deny || err == nil {
		t.Fatalf("AwaitAnswer = (%v, %v), want (Deny, non-nil)", got, err)
	}
}

func TestAwaitAnswerZeroTimeoutUsesDefault(t *testing.T) {
	if DefaultPermissionTimeout <= 0 {
		t.Fatal("DefaultPermissionTimeout must be positive")
	}
	// A zero-policy wait on an immediate answer still returns promptly.
	got, err := (Policy{}).AwaitAnswer(context.Background(), func(context.Context) (OptionID, error) {
		return OptionRejectOnce, nil
	})
	if err != nil || got != Deny {
		t.Fatalf("AwaitAnswer = (%v, %v), want (Deny, nil)", got, err)
	}
}
