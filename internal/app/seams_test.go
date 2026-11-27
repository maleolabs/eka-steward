package app

import (
	"context"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
)

// TestProtocolSeams exercises the thin protocol-facing methods on an assembled
// app: Plan, Step, Status, Cancel and the bounded RunSteps driver all delegate
// to the owned loop and never duplicate it.
func TestProtocolSeams(t *testing.T) {
	if testing.Short() {
		t.Skip("assembles a real EKA runtime")
	}
	t.Setenv("EKA_HOME", t.TempDir())
	ctx := context.Background()

	disp := &fakeDispatcher{}
	a, err := Assemble(Options{
		Project:    "p",
		StateDir:   t.TempDir(),
		Source:     fakeSource{ids: []string{"p/sto:a", "p/sto:b"}},
		Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer a.Close()

	steps, err := a.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("Plan = %d steps, want 2", len(steps))
	}

	if _, ok, err := a.Step(ctx); err != nil || !ok {
		t.Fatalf("Step = (ok=%v, err=%v), want one dispatched step", ok, err)
	}
	if got := a.Status().Snapshot.Steps; got != 1 {
		t.Fatalf("Status steps = %d, want 1", got)
	}

	// Cancel with no batch in progress is a safe no-op.
	a.Cancel()

	// The bounded driver dispatches the remaining candidate.
	if err := a.RunSteps(ctx, 1); err != nil {
		t.Fatalf("RunSteps(1): %v", err)
	}
	remaining, err := a.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan after RunSteps: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("Plan after RunSteps = %d steps, want quiescent", len(remaining))
	}
	if got := len(disp.calls()); got != 2 {
		t.Fatalf("dispatch count = %d, want 2", got)
	}
}

// TestApproveWritesChannel proves Approve materializes a decision into the
// file/queue approval channel the loop polls.
func TestApproveWritesChannel(t *testing.T) {
	dir := t.TempDir()
	a := &App{stateDir: dir}

	if err := a.Approve("req-key-1", "approve"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	dec, err := (approval.FileApprover{Dir: dir}).Ask(context.Background(), approval.Request{Key: "req-key-1"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !dec.Approved() {
		t.Fatalf("Ask verdict = %q, want approve", dec.Verdict)
	}

	if err := a.Approve("", "approve"); err == nil {
		t.Fatal("Approve with empty request id must fail")
	}
	if err := a.Approve("k", "maybe"); err == nil {
		t.Fatal("Approve with unknown decision must fail")
	}
}

// TestAppSubscribeEmitsStepEvents proves the assembled app exposes the loop's
// step as ordered stream events (step, dispatch, metrics).
func TestAppSubscribeEmitsStepEvents(t *testing.T) {
	t.Setenv("EKA_HOME", t.TempDir())
	a, err := Assemble(Options{
		Project:    "p",
		StateDir:   t.TempDir(),
		Source:     fakeSource{ids: []string{"p/sto:a"}},
		Dispatcher: &fakeDispatcher{},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer a.Close()

	ch, cancel, err := a.Subscribe(nil, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if _, ok, err := a.Step(context.Background()); err != nil || !ok {
		t.Fatalf("Step = (ok=%v, err=%v), want one dispatched step", ok, err)
	}

	want := []string{obs.EventStep, obs.EventDispatch, obs.EventMetrics}
	for i, typ := range want {
		select {
		case ev := <-ch:
			if ev.Type != typ {
				t.Fatalf("event %d type = %q, want %q", i, ev.Type, typ)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d (%s)", i, typ)
		}
	}
}

// TestAppApproveEmitsApprovalEvent proves Approve emits one approval event on
// the stream after it materializes the decision.
func TestAppApproveEmitsApprovalEvent(t *testing.T) {
	a := &App{stateDir: t.TempDir(), stream: obs.NewStream(nil, nil)}
	ch, cancel := a.stream.Subscribe(nil)
	defer cancel()

	if err := a.Approve("req-key-1", "approve"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Type != obs.EventApproval {
			t.Fatalf("event type = %q, want approval", ev.Type)
		}
		d, ok := ev.Data.(obs.ApprovalData)
		if !ok || d.RequestID != "req-key-1" || d.Decision != "approve" {
			t.Fatalf("approval data = %#v", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the approval event")
	}
}
