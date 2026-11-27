package obs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// recvEvent receives one event or fails on a bounded timeout.
func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event within the timeout budget")
		return Event{}
	}
}

// TestStreamExactSequence pins the exact, monotonic event sequence a fake run
// produces: step, dispatch, metrics, write, approval — seq 1..5 gap-free.
func TestStreamExactSequence(t *testing.T) {
	rec := NewRecorder(Config{}, nil)
	s := NewStream(NewRedactor(), rec.Snapshot)
	ch, cancel := s.Subscribe(nil)
	defer cancel()

	// Wire the composite exactly as the app does: the Recorder sees the step
	// (so its snapshot counts it) and the Stream then samples the snapshot.
	obsr := Composite{rec, s}
	obsr.Step(StepEvent{
		CandidateID: "p/sto:i-1", Kind: "dispatch", Outcome: OutcomeOK,
		Attempts: 2, Duration: 7 * time.Millisecond,
	})
	obsr.Write(WriteEvent{Line: "p/sto:i-1", Kind: "transition", Result: "applied"})
	s.EmitApproval("req-1", "approve")

	want := []string{EventStep, EventDispatch, EventMetrics, EventWrite, EventApproval}
	for i, typ := range want {
		ev := recvEvent(t, ch)
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (gap-free order)", i, ev.Seq, i+1)
		}
		if ev.Type != typ {
			t.Fatalf("event %d type = %q, want %q", i, ev.Type, typ)
		}
		switch d := ev.Data.(type) {
		case StepData:
			if d.CandidateID != "p/sto:i-1" || d.Kind != "dispatch" || d.Outcome != OutcomeOK || d.DurationMs != 7 {
				t.Fatalf("step data = %+v", d)
			}
		case DispatchData:
			if d.CandidateID != "p/sto:i-1" || d.Attempts != 2 || d.Outcome != OutcomeOK {
				t.Fatalf("dispatch data = %+v", d)
			}
		case Snapshot:
			if d.Steps != 1 {
				t.Fatalf("metrics steps = %d, want 1", d.Steps)
			}
		case WriteData:
			if d.Line != "p/sto:i-1" || d.Kind != "transition" || d.Result != "applied" {
				t.Fatalf("write data = %+v", d)
			}
		case ApprovalData:
			if d.RequestID != "req-1" || d.Decision != "approve" {
				t.Fatalf("approval data = %+v", d)
			}
		default:
			t.Fatalf("event %d has unexpected data type %T", i, ev.Data)
		}
	}
}

// TestStreamRedaction proves a secret-shaped token and a deny-list literal are
// masked in every emitted event payload.
func TestStreamRedaction(t *testing.T) {
	s := NewStream(NewRedactor("deny-value"), nil)
	ch, cancel := s.Subscribe(nil)
	defer cancel()

	s.Step(StepEvent{
		CandidateID: "p/sto:ghp_ABCDEFGH1234",
		Kind:        "Bearer abc.def.ghi",
		Outcome:     OutcomeOK,
		Attempts:    1,
	})
	s.Write(WriteEvent{Line: "p/sto:AIzaABCDEFGH12345", Kind: "transition", Result: "deny-value"})
	s.EmitApproval("sk-ABCDEFGH12345678", "approve")

	secrets := []string{"ghp_ABCDEFGH1234", "abc.def.ghi", "AIzaABCDEFGH12345", "deny-value", "sk-ABCDEFGH12345678"}
	var all []byte
	for i := 0; i < 4; i++ { // step, dispatch, write, approval (no metrics provider)
		ev := recvEvent(t, ch)
		b, err := json.Marshal(ev.Data)
		if err != nil {
			t.Fatalf("marshal event data: %v", err)
		}
		all = append(all, b...)
	}
	for _, secret := range secrets {
		if strings.Contains(string(all), secret) {
			t.Fatalf("secret %q leaked into stream events:\n%s", secret, all)
		}
	}
	if !strings.Contains(string(all), Mask) {
		t.Fatalf("events have no mask marker; redaction did not run:\n%s", all)
	}
}

// TestStreamBackpressureGap proves a slow subscriber never blocks the emitter:
// an overflow drops events and yields exactly ONE deterministic gap marker
// recording the dropped seq range.
func TestStreamBackpressureGap(t *testing.T) {
	s := NewStream(NewRedactor(), nil)
	ch, cancel := s.Subscribe(nil)
	defer cancel()

	// 100 steps x (step+dispatch) = 200 events against a 64-deep buffer.
	for i := 0; i < 100; i++ {
		s.Step(StepEvent{CandidateID: "c", Kind: "dispatch", Outcome: OutcomeOK})
	}

	// The buffer holds the first defaultBuffer events (seq 1..64); drain them.
	for i := 0; i < defaultBuffer; i++ {
		ev := recvEvent(t, ch)
		if ev.Type == EventGap {
			t.Fatalf("unexpected gap before the buffer drained: %+v", ev)
		}
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, i+1)
		}
	}

	// One more emit frees a slot and flushes the accumulated gap [65, 201].
	s.Write(WriteEvent{Line: "l", Kind: "k", Result: "r"})

	gap := recvEvent(t, ch)
	if gap.Type != EventGap {
		t.Fatalf("expected a gap marker, got %+v", gap)
	}
	g, ok := gap.Data.(GapData)
	if !ok {
		t.Fatalf("gap data type = %T, want GapData", gap.Data)
	}
	if g.FromSeq != uint64(defaultBuffer+1) || g.ToSeq != 201 {
		t.Fatalf("gap range = [%d,%d], want [%d,201]", g.FromSeq, g.ToSeq, defaultBuffer+1)
	}

	// The channel is empty again: no unbounded growth.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected trailing event: %+v", ev)
	default:
	}
}

// TestStreamCancelClosesChannel proves cancel stops delivery and closes the
// channel (so a forwarding goroutine exits), and is idempotent.
func TestStreamCancelClosesChannel(t *testing.T) {
	s := NewStream(nil, nil)
	ch, cancel := s.Subscribe(nil)

	s.EmitApproval("r", "approve")
	if ev := recvEvent(t, ch); ev.Type != EventApproval {
		t.Fatalf("event type = %q, want approval", ev.Type)
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after cancel")
	}
	cancel()                    // idempotent: must not panic
	s.EmitApproval("r", "deny") // no subscriber: must not panic
}

// TestStreamReplayFromSeq proves SubscribeFrom replays buffered events with
// seq >= fromSeq before any live event.
func TestStreamReplayFromSeq(t *testing.T) {
	s := NewStream(nil, nil)
	s.EmitApproval("a", "approve") // seq 1
	s.EmitApproval("b", "approve") // seq 2
	s.EmitApproval("c", "deny")    // seq 3

	ch, cancel := s.SubscribeFrom(nil, 2)
	defer cancel()

	for want := uint64(2); want <= 3; want++ {
		ev := recvEvent(t, ch)
		if ev.Seq != want {
			t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
		}
	}
}
