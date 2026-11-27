package acp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// startPair wires a Client to an in-process fake agent over a net.Pipe pair.
func startPair(t *testing.T, fcfg fakeConfig, ccfg Config) (*Client, *fakeAgent) {
	t.Helper()
	cEnd, aEnd := net.Pipe()
	f := newFakeAgent(fcfg)
	go f.serve(aEnd, aEnd)
	c := NewClient(Conn{Reader: cEnd, Writer: cEnd}, ccfg)
	c.Start()
	t.Cleanup(func() {
		_ = c.Close()
		_ = cEnd.Close()
		_ = aEnd.Close()
	})
	return c, f
}

// openSession negotiates and opens a session, failing the test on error.
func openSession(t *testing.T, c *Client) string {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sid, err := c.SessionNew(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	return sid
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	tests := []struct {
		name  string
		max   int
		agent int
		want  int
	}{
		{"default-requests-v1", 0, 1, Version1},
		{"request-v2-agent-v1", 2, 1, Version1},
		{"both-v2", 2, 2, Version2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := startPair(t, fakeConfig{version: tc.agent}, Config{
				MaxVersion: tc.max,
				Info:       ClientInfo{Name: "test", Version: "0.0.1"},
			})
			hs, err := c.Initialize(context.Background())
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if hs.ProtocolVersion != tc.want {
				t.Fatalf("protocolVersion = %d, want %d", hs.ProtocolVersion, tc.want)
			}
			if c.NegotiatedVersion() != tc.want {
				t.Fatalf("NegotiatedVersion = %d, want %d", c.NegotiatedVersion(), tc.want)
			}
			if hs.AgentName != "fake-acp-agent" {
				t.Fatalf("agent name = %q", hs.AgentName)
			}
		})
	}
}

func TestSessionNewReturnsID(t *testing.T) {
	c, f := startPair(t, fakeConfig{}, Config{})
	sid := openSession(t, c)
	if sid != "sess_fake_1" {
		t.Fatalf("sessionId = %q, want sess_fake_1", sid)
	}
	if got := f.Prompts(); len(got) != 0 {
		t.Fatalf("prompts before Prompt = %v", got)
	}
}

func TestPromptStreamsUpdatesInOrder(t *testing.T) {
	tests := []struct {
		name      string
		agent     int
		max       int
		wantMsgID string
	}{
		{"v1", Version1, 0, ""},
		{"v2", Version2, Version2, "msg_user_1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var order []string
			c, _ := startPair(t, fakeConfig{version: tc.agent}, Config{
				MaxVersion: tc.max,
				OnUpdate: func(u SessionUpdate) {
					order = append(order, u.Kind+":"+u.text())
				},
			})
			sid := openSession(t, c)
			res, err := c.Prompt(context.Background(), sid, "hello")
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if res.Text != "ABC" {
				t.Fatalf("text = %q, want ABC", res.Text)
			}
			if res.StopReason != "end_turn" {
				t.Fatalf("stopReason = %q, want end_turn", res.StopReason)
			}
			if res.MessageID != tc.wantMsgID {
				t.Fatalf("messageId = %q, want %q", res.MessageID, tc.wantMsgID)
			}
			want := []string{"agent_message_chunk:A", "agent_message_chunk:B", "agent_message_chunk:C"}
			if tc.agent == Version2 {
				want = append(want, "state_update:")
			}
			if strings.Join(order, ",") != strings.Join(want, ",") {
				t.Fatalf("update order = %v, want %v", order, want)
			}
		})
	}
}

func TestPermissionDefaultDeny(t *testing.T) {
	tests := []struct {
		name  string
		agent int
		max   int
	}{
		{"v1-execute", Version1, 0},
		{"v2-execute", Version2, Version2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, f := startPair(t, fakeConfig{version: tc.agent, permission: true, permKind: "execute"}, Config{MaxVersion: tc.max})
			sid := openSession(t, c)
			if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if got := f.PermOutcome(); got != "reject-once" {
				t.Fatalf("permission outcome = %q, want reject-once (default deny)", got)
			}
		})
	}
}

func TestPermissionAllowsReadInsideRoot(t *testing.T) {
	root := t.TempDir()
	c, f := startPair(t, fakeConfig{
		permission: true,
		permKind:   guard.ToolRead,
		permPath:   filepath.Join(root, "notes.md"),
	}, Config{
		Policy:        guard.Policy{AllowReadWithinRoot: true},
		WorkspaceRoot: root,
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.PermOutcome(); got != "allow-once" {
		t.Fatalf("permission outcome = %q, want allow-once", got)
	}
}

func TestPermissionEscalatesToApprover(t *testing.T) {
	c, f := startPair(t, fakeConfig{permission: true, permKind: "execute"}, Config{
		Approver: fakeApprover{decision: approval.Decision{Verdict: approval.Approve}},
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.PermOutcome(); got != "allow-once" {
		t.Fatalf("permission outcome = %q, want allow-once (human approved)", got)
	}
}

func TestCancelTerminatesDeterministically(t *testing.T) {
	started := make(chan struct{}, 1)
	c, _ := startPair(t, fakeConfig{hang: true}, Config{
		OnUpdate: func(SessionUpdate) {
			select {
			case started <- struct{}{}:
			default:
			}
		},
	})
	sid := openSession(t, c)

	type outcome struct {
		res TurnResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := c.Prompt(context.Background(), sid, "x")
		done <- outcome{res, err}
	}()

	<-started // the turn is in flight
	if err := c.Cancel(sid); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := <-done
	if !errors.Is(got.err, ErrCancelled) {
		t.Fatalf("Prompt err = %v, want ErrCancelled", got.err)
	}
}

func TestClientRefusesBadFrame(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		maxBytes int
		want     error
	}{
		{"malformed", "this is not json\n", 0, ErrMalformed},
		{"oversized", strings.Repeat("a", 64) + "\n", 16, ErrLineTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cEnd, aEnd := net.Pipe()
			go func() {
				br := bufio.NewReader(aEnd)
				_, _ = br.ReadString('\n') // consume the initialize request
				_, _ = io.WriteString(aEnd, tc.line)
			}()
			c := NewClient(Conn{Reader: cEnd, Writer: cEnd}, Config{MaxFrameBytes: tc.maxBytes})
			c.Start()
			defer func() { _ = c.Close(); _ = cEnd.Close(); _ = aEnd.Close() }()

			if _, err := c.Initialize(context.Background()); !errors.Is(err, tc.want) {
				t.Fatalf("Initialize err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPermissionBoundedWaitDeniesOnTimeout(t *testing.T) {
	c, f := startPair(t, fakeConfig{permission: true, permKind: "execute"}, Config{
		Policy:   guard.Policy{Timeout: 100 * time.Millisecond},
		Approver: blockingApprover{},
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.PermOutcome(); got != "reject-once" {
		t.Fatalf("permission outcome = %q, want reject-once (bounded wait timed out)", got)
	}
}

func TestReadTextFileServedInsideRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.md")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, f := startPair(t, fakeConfig{fsReadPath: path}, Config{
		Policy:        guard.Policy{AllowReadWithinRoot: true},
		WorkspaceRoot: root,
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.FSOutcome(); got != "hello" {
		t.Fatalf("fs outcome = %q, want hello", got)
	}
}

func TestReadTextFileOutsideRootRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c, f := startPair(t, fakeConfig{fsReadPath: outside}, Config{
		Policy:        guard.Policy{AllowReadWithinRoot: true},
		WorkspaceRoot: t.TempDir(),
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.FSOutcome(); got != "error:read refused" {
		t.Fatalf("fs outcome = %q, want error:read refused", got)
	}
}

func TestReadTextFileRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	c, f := startPair(t, fakeConfig{fsReadPath: link}, Config{
		Policy:        guard.Policy{AllowReadWithinRoot: true},
		WorkspaceRoot: root,
	})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.FSOutcome(); got != "error:read refused" {
		t.Fatalf("fs outcome = %q, want error:read refused (symlink escape)", got)
	}
}

func TestTerminalRequestRefused(t *testing.T) {
	c, f := startPair(t, fakeConfig{terminalMethod: "terminal/create"}, Config{})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.TerminalOutcome(); got != "error:terminal not supported" {
		t.Fatalf("terminal outcome = %q, want error:terminal not supported", got)
	}
}

func TestUnknownServerRequestRefused(t *testing.T) {
	c, f := startPair(t, fakeConfig{terminalMethod: "session/bogus"}, Config{})
	sid := openSession(t, c)
	if _, err := c.Prompt(context.Background(), sid, "x"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := f.TerminalOutcome(); got != "error:method not found" {
		t.Fatalf("unknown-method outcome = %q, want error:method not found", got)
	}
}

// fakeApprover is a canned human channel.
type fakeApprover struct {
	decision approval.Decision
}

// Ask returns the canned decision.
func (f fakeApprover) Ask(context.Context, approval.Request) (approval.Decision, error) {
	return f.decision, nil
}

// blockingApprover never answers: it waits for its (bounded) ctx to end. It
// models a human channel that does not respond in time.
type blockingApprover struct{}

// Ask blocks until ctx is done, then returns the ctx error.
func (blockingApprover) Ask(ctx context.Context, _ approval.Request) (approval.Decision, error) {
	<-ctx.Done()
	return approval.Decision{}, ctx.Err()
}
