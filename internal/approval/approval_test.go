package approval

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// funcApprover adapts a function to the Approver seam (the test fake).
type funcApprover func(context.Context, Request) (Decision, error)

func (f funcApprover) Ask(ctx context.Context, req Request) (Decision, error) { return f(ctx, req) }

func escalatedRequest() Request {
	return RequestFor(guard.PermissionRequest{
		ToolKind:      guard.ToolExecute,
		Path:          "/etc/hosts",
		WorkspaceRoot: "/ws",
		OptionID:      guard.OptionAllowOnce,
	})
}

// TestRequestKeyIsDeterministic: the same permission request always yields the
// same Key (no map order, clock or randomness), and a changed input changes it.
func TestRequestKeyIsDeterministic(t *testing.T) {
	r := escalatedRequest()
	for i := 0; i < 100; i++ {
		if got := RequestFor(guard.PermissionRequest{
			ToolKind: guard.ToolExecute, Path: "/etc/hosts", WorkspaceRoot: "/ws", OptionID: guard.OptionAllowOnce,
		}); got.Key != r.Key {
			t.Fatalf("run %d Key = %q, want %q", i, got.Key, r.Key)
		}
	}
	other := RequestFor(guard.PermissionRequest{ToolKind: guard.ToolEdit, Path: "/etc/hosts", WorkspaceRoot: "/ws", OptionID: guard.OptionAllowOnce})
	if other.Key == r.Key {
		t.Fatal("different tool kind produced the same Key")
	}
	if len(r.Key) != 64 {
		t.Fatalf("Key = %q, want a 64-char sha256 hex", r.Key)
	}
}

// TestAskBoundedApproveAndDeny covers the two decided branches.
func TestAskBoundedApproveAndDeny(t *testing.T) {
	req := escalatedRequest()
	p := guard.Policy{Timeout: time.Second}

	ok := funcApprover(func(context.Context, Request) (Decision, error) {
		return Decision{Verdict: Approve, By: "human"}, nil
	})
	d, err := AskBounded(context.Background(), p, ok, req)
	if err != nil || !d.Approved() {
		t.Fatalf("approve = (%+v, %v), want approved", d, err)
	}

	no := funcApprover(func(context.Context, Request) (Decision, error) {
		return Decision{Verdict: Deny, Reason: "untrusted path"}, nil
	})
	d, err = AskBounded(context.Background(), p, no, req)
	if err != nil || d.Approved() || d.Verdict != Deny {
		t.Fatalf("deny = (%+v, %v), want denied", d, err)
	}
}

// TestAskBoundedTimeoutReturnsDenyWithinBound: an approver that never answers
// (and ignores ctx) must not hang the steward; the wait returns Deny bounded
// by Policy.Timeout.
func TestAskBoundedTimeoutReturnsDenyWithinBound(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	never := funcApprover(func(context.Context, Request) (Decision, error) {
		<-blocked // deliberately ignores ctx
		return Decision{Verdict: Approve}, nil
	})
	p := guard.Policy{Timeout: 50 * time.Millisecond}

	start := time.Now()
	d, err := AskBounded(context.Background(), p, never, escalatedRequest())
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("AskBounded took %v; must be bounded by Policy.Timeout", elapsed)
	}
	if d.Verdict != Deny || !d.TimedOut {
		t.Fatalf("timeout = %+v, want Deny{TimedOut:true}", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v, want context.DeadlineExceeded", err)
	}
}

// TestAskBoundedErrorAndPanicAreDeny asserts a failing or panicking approver
// resolves to Deny, never approval.
func TestAskBoundedErrorAndPanicAreDeny(t *testing.T) {
	p := guard.Policy{Timeout: time.Second}
	boom := funcApprover(func(context.Context, Request) (Decision, error) {
		return Decision{}, errors.New("channel down")
	})
	if d, err := AskBounded(context.Background(), p, boom, escalatedRequest()); err == nil || d.Approved() {
		t.Fatalf("error path = (%+v,%v), want Deny+err", d, err)
	}
	panics := funcApprover(func(context.Context, Request) (Decision, error) { panic("boom") })
	if d, err := AskBounded(context.Background(), p, panics, escalatedRequest()); err == nil || d.Approved() {
		t.Fatalf("panic path = (%+v,%v), want Deny+err", d, err)
	}
	if _, err := AskBounded(context.Background(), p, nil, escalatedRequest()); err == nil {
		t.Fatal("nil approver must be an error")
	}
}

// TestFileApproverApprove: a response already on disk is returned immediately
// — the resume case (no second human answer needed).
func TestFileApproverApprove(t *testing.T) {
	dir := t.TempDir()
	req := escalatedRequest()
	resp := response{Key: req.Key, Verdict: Approve, By: "operator"}
	b, _ := json.Marshal(resp)
	if err := os.WriteFile(filepath.Join(dir, DefaultResponseFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	a := FileApprover{Dir: dir, PollInterval: 10 * time.Millisecond}
	d, err := AskBounded(context.Background(), guard.Policy{Timeout: time.Second}, a, req)
	if err != nil || !d.Approved() {
		t.Fatalf("file approve = (%+v,%v), want approved", d, err)
	}
	// The request file was published for the human to read.
	if _, err := os.Stat(filepath.Join(dir, DefaultRequestFile)); err != nil {
		t.Fatalf("request file missing: %v", err)
	}
}

// TestFileApproverDenyAfterDelay: the answer can arrive after the poll starts.
func TestFileApproverDenyAfterDelay(t *testing.T) {
	dir := t.TempDir()
	req := escalatedRequest()
	go func() {
		time.Sleep(40 * time.Millisecond)
		b, _ := json.Marshal(response{Key: req.Key, Verdict: Deny, Reason: "nope"})
		_ = os.WriteFile(filepath.Join(dir, DefaultResponseFile), b, 0o644)
	}()

	a := FileApprover{Dir: dir, PollInterval: 10 * time.Millisecond}
	d, err := AskBounded(context.Background(), guard.Policy{Timeout: 2 * time.Second}, a, req)
	if err != nil || d.Approved() || d.Verdict != Deny {
		t.Fatalf("file deny = (%+v,%v), want denied", d, err)
	}
}

// TestFileApproverIgnoresMismatchedResponse: a stale/other-request answer can
// only time out to Deny, never approve the wrong request.
func TestFileApproverIgnoresMismatchedResponse(t *testing.T) {
	dir := t.TempDir()
	req := escalatedRequest()
	b, _ := json.Marshal(response{Key: "some-other-key", Verdict: Approve})
	if err := os.WriteFile(filepath.Join(dir, DefaultResponseFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	a := FileApprover{Dir: dir, PollInterval: 10 * time.Millisecond}
	start := time.Now()
	d, err := AskBounded(context.Background(), guard.Policy{Timeout: 80 * time.Millisecond}, a, req)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("mismatched response hung for %v", elapsed)
	}
	if d.Approved() {
		t.Fatalf("mismatched response approved: %+v", d)
	}
	if err == nil || !d.TimedOut {
		t.Fatalf("mismatched response = (%+v,%v), want timeout deny", d, err)
	}
}

// TestFileApproverNeedsDir: the default channel refuses an empty directory
// rather than guessing one.
func TestFileApproverNeedsDir(t *testing.T) {
	if _, err := (FileApprover{}).Ask(context.Background(), escalatedRequest()); err == nil {
		t.Fatal("FileApprover with no Dir must error")
	}
}

// TestNoTTYInDefaultPath asserts the non-test approval source never reads a
// terminal: no os.Stdin/Stdout/Stderr selector, no isatty or x/term import.
// The file channel is the only default; a test may use os.Pipe, never the TTY.
func TestNoTTYInDefaultPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(p, "isatty") || p == "golang.org/x/term" || p == "golang.org/x/sys/unix" {
				t.Fatalf("%s imports a TTY package %q; the default approval path must be TTY-free", name, p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != "os" {
				return true
			}
			switch sel.Sel.Name {
			case "Stdin", "Stdout", "Stderr":
				t.Errorf("%s touches os.%s; the default approval path must not use the TTY", name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestOutcomeIsDeterministic: the same request + recorded decision yields the
// same outcome across repeated runs.
func TestOutcomeIsDeterministic(t *testing.T) {
	req := escalatedRequest()
	p := guard.Policy{Timeout: time.Second}
	approve := funcApprover(func(context.Context, Request) (Decision, error) {
		return Decision{Verdict: Approve}, nil
	})
	for i := 0; i < 50; i++ {
		d, err := AskBounded(context.Background(), p, approve, req)
		if err != nil || !d.Approved() {
			t.Fatalf("run %d = (%+v,%v), want approved", i, d, err)
		}
	}
}
