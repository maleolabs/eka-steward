package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// approvalWriter is a fake Writer that also records approvals, modelling the
// ekawrite replay gate: a re-issued identical decision is a no-op, so the
// resume path can never record the same approval twice.
type approvalWriter struct {
	*fakeWriter
	mu       sync.Mutex
	seen     map[string]bool
	records  []ApprovalRecord
	recorded int
}

func newApprovalWriter() *approvalWriter {
	return &approvalWriter{fakeWriter: newFakeWriter(), seen: map[string]bool{}}
}

func (w *approvalWriter) Approve(_ context.Context, c decision.Candidate, rec ApprovalRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := c.ID + "|" + rec.Key + "|" + rec.Verdict
	if w.seen[key] {
		return nil
	}
	w.seen[key] = true
	w.records = append(w.records, rec)
	w.recorded++
	return nil
}

// fixedApprover answers every request with a fixed decision.
type fixedApprover struct {
	d   approval.Decision
	err error
}

func (f fixedApprover) Ask(context.Context, approval.Request) (approval.Decision, error) {
	return f.d, f.err
}

// blockingApprover never answers and ignores ctx (the timeout case); release
// unblocks the parked goroutine after the test.
type blockingApprover struct{ release chan struct{} }

func (b blockingApprover) Ask(context.Context, approval.Request) (approval.Decision, error) {
	<-b.release
	return approval.Decision{Verdict: approval.Approve}, nil
}

// escalateFor asks the approval gate to escalate an execute request for every
// candidate — the pure policy cannot decide execute.
func escalateFor(decision.Candidate) (guard.PermissionRequest, bool) {
	return guard.PermissionRequest{
		ToolKind:      guard.ToolExecute,
		Path:          "/etc/hosts",
		WorkspaceRoot: "/ws",
		OptionID:      guard.OptionAllowOnce,
	}, true
}

func approvalConfig(a approval.Approver, timeout time.Duration) ApprovalPolicy {
	return ApprovalPolicy{
		Approver: a,
		Policy:   guard.Policy{Timeout: timeout},
		Request:  escalateFor,
	}
}

func approvalOneCandidate() Source {
	return fixtureSource{[]decision.Candidate{{ID: "a", State: "todo"}}}
}

// TestApprovalApproveProceeds is AC3-approve: an approved escalation is
// recorded once as an approve artifact and the step proceeds to dispatch.
func TestApprovalApproveProceeds(t *testing.T) {
	ctx := context.Background()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	w := newApprovalWriter()
	dis := newCountingDispatcher()
	l := New(approvalOneCandidate(), dis, w, store, Config{
		MaxWorkers:    1,
		DispatchState: "todo",
		Approval:      approvalConfig(fixedApprover{d: approval.Decision{Verdict: approval.Approve, By: "human"}}, time.Second),
	})

	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := dis.total(); got != 1 {
		t.Fatalf("dispatch count = %d, want 1 (approval proceeds)", got)
	}
	if w.recorded != 1 || w.records[0].Verdict != "approve" {
		t.Fatalf("approval records = %+v, want one approve", w.records)
	}
}

// TestApprovalDenyBlocks is AC3-deny: a denied escalation is recorded as a
// deny artifact and the step is blocked — the item is left un-done and the
// worker is never dispatched.
func TestApprovalDenyBlocks(t *testing.T) {
	ctx := context.Background()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	w := newApprovalWriter()
	dis := newCountingDispatcher()
	l := New(approvalOneCandidate(), dis, w, store, Config{
		MaxWorkers:    1,
		DispatchState: "todo",
		Approval:      approvalConfig(fixedApprover{d: approval.Decision{Verdict: approval.Deny, Reason: "untrusted"}}, time.Second),
	})

	err := l.Run(ctx)
	if err == nil {
		t.Fatal("Run err = nil, want an ApprovalRefusal")
	}
	var refusal *ApprovalRefusal
	if !asApprovalRefusal(err, &refusal) {
		t.Fatalf("Run err = %T %v, want *ApprovalRefusal", err, err)
	}
	if refusal.Verdict != "deny" {
		t.Fatalf("refusal verdict = %q, want deny", refusal.Verdict)
	}
	if got := dis.total(); got != 0 {
		t.Fatalf("dispatch count = %d, want 0 (deny must not dispatch)", got)
	}
	if w.recorded != 1 || w.records[0].Verdict != "deny" {
		t.Fatalf("approval records = %+v, want one deny", w.records)
	}
	st, _ := store.Load()
	if st.Has("a") {
		t.Fatal("denied item must stay un-done")
	}
}

// TestApprovalTimeoutDenies is AC3-timeout: an answer that never arrives is a
// bounded wait recorded as a timeout (deny); the loop never hangs.
func TestApprovalTimeoutDenies(t *testing.T) {
	ctx := context.Background()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	w := newApprovalWriter()
	dis := newCountingDispatcher()
	blocked := blockingApprover{release: make(chan struct{})}
	defer close(blocked.release)

	l := New(approvalOneCandidate(), dis, w, store, Config{
		MaxWorkers:    1,
		DispatchState: "todo",
		Approval:      approvalConfig(blocked, 40*time.Millisecond),
	})

	start := time.Now()
	err := l.Run(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run hung for %v; the approval wait must be bounded", elapsed)
	}
	if err == nil {
		t.Fatal("Run err = nil, want a timeout ApprovalRefusal")
	}
	if got := dis.total(); got != 0 {
		t.Fatalf("dispatch count = %d, want 0 (timeout must not dispatch)", got)
	}
	if w.recorded != 1 || w.records[0].Verdict != "timeout" {
		t.Fatalf("approval records = %+v, want one timeout", w.records)
	}
}

// asApprovalRefusal is errors.As without importing errors in every call site.
func asApprovalRefusal(err error, target **ApprovalRefusal) bool {
	r, ok := err.(*ApprovalRefusal)
	if ok {
		*target = r
	}
	return ok
}

// TestApprovalResumeNoDoubleRecord is the resumption guarantee: crash AFTER the
// approval was recorded but before its intent was cleared, restart, and assert
// the durable decision is re-read and the artifact is NOT recorded twice (the
// replay gate dedupes) while the step still proceeds exactly once.
func TestApprovalResumeNoDoubleRecord(t *testing.T) {
	ctx := context.Background()

	// The operator's durable answer, keyed to the deterministic request key.
	dir := t.TempDir()
	req := approval.RequestFor(guard.PermissionRequest{
		ToolKind: guard.ToolExecute, Path: "/etc/hosts", WorkspaceRoot: "/ws", OptionID: guard.OptionAllowOnce,
	})
	b, _ := json.Marshal(map[string]any{"key": req.Key, "verdict": "approve", "by": "human"})
	if err := os.WriteFile(filepath.Join(dir, approval.DefaultResponseFile), b, 0o644); err != nil {
		t.Fatalf("write response: %v", err)
	}
	approver := approval.FileApprover{Dir: dir, PollInterval: 5 * time.Millisecond}

	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	fw := newApprovalWriter()
	dis := newCountingDispatcher()

	crashed := runAndRecover(func() {
		_ = New(approvalOneCandidate(), dis, fw, store, Config{
			MaxWorkers:    1,
			DispatchState: "todo",
			Crash: func(got string) {
				if got == BoundaryAfterApply {
					panic(crashSignal{got})
				}
			},
			Approval: approvalConfig(approver, time.Second),
		}).Run(ctx)
	})
	if crashed != BoundaryAfterApply {
		t.Fatalf("expected a crash at %q, got %q", BoundaryAfterApply, crashed)
	}
	if fw.recorded != 1 {
		t.Fatalf("after crash: recorded %d, want 1", fw.recorded)
	}

	// Restart: resume re-reads the durable decision, re-records through the
	// replay gate (no second artifact), then the forward step runs once.
	restarted := New(approvalOneCandidate(), dis, fw, store, Config{
		MaxWorkers:    1,
		DispatchState: "todo",
		Approval:      approvalConfig(approver, time.Second),
	})
	if err := restarted.Run(ctx); err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if fw.recorded != 1 {
		t.Fatalf("resume recorded %d approvals, want exactly 1 (replay gate must dedupe)", fw.recorded)
	}
	if got := dis.total(); got != 1 {
		t.Fatalf("dispatch count = %d, want 1 (approved item proceeds once)", got)
	}
	st, _ := store.Load()
	if len(st.InFlight) != 0 {
		t.Fatalf("final InFlight = %+v, want empty", st.InFlight)
	}
	if !st.Has("a") {
		t.Fatal("approved item must be done after resume")
	}
}
