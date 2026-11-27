package obs

// Live event stream. Stream is a second Observer (alongside Recorder) plus the
// approval/metrics sources, fanned out to subscribers as ordered, monotonic,
// redacted Events. It is the source behind the protocol's events.subscribe.
//
// Ordering: every real event is assigned a strictly increasing, gap-free seq
// under one mutex; delivery to every subscriber happens under the same mutex,
// so a subscriber receives events in seq order. seq is GLOBAL (over all event
// types): a subscriber that filters by type may therefore observe seq gaps for
// the types it did not request. Only an overflow drop is signalled, with an
// explicit gap event (see backpressure).
//
// Backpressure: each subscriber has a bounded buffer (defaultBuffer). Delivery
// is NON-BLOCKING, so the emitter — the control loop — never blocks or hangs.
// When a subscriber's buffer is full the event is dropped and, once space
// frees, the subscriber receives exactly ONE synthetic gap event per dropped
// burst recording the dropped seq range [fromSeq, toSeq]. The gap event's own
// seq is fromSeq, so it stays ordered after the last delivered event and
// before the next. Gap events are always delivered, regardless of the
// subscriber's type filter (they describe that subscriber's own loss).

import (
	"sync"
)

// Event type names carried on the stream (and the initialize capabilities.events
// list).
const (
	// EventStep is one loop step (from StepEvent).
	EventStep = "step"
	// EventDispatch is the dispatch attempt/outcome detail derived from StepEvent.
	EventDispatch = "dispatch"
	// EventWrite is one applied EKA write (from WriteEvent).
	EventWrite = "write"
	// EventApproval is one materialized human decision (EmitApproval).
	EventApproval = "approval"
	// EventMetrics is a Recorder snapshot, emitted after each step.
	EventMetrics = "metrics"
	// EventGap is a synthetic marker recording a dropped seq range (overflow).
	EventGap = "gap"
)

// defaultBuffer is the per-subscriber bounded buffer depth. A full buffer drops
// (never blocks); see the package comment for the gap policy.
const defaultBuffer = 64

// ringCap bounds the replay ring used by SubscribeFrom / fromSeq.
const ringCap = 256

// Event is one notification on the stream: a monotonic Seq, a Type, and a
// redacted Data payload (a typed struct from this package, or a Snapshot).
type Event struct {
	Seq  uint64
	Type string
	Data any
}

// StepData is the redacted payload of an EventStep.
type StepData struct {
	CandidateID string `json:"candidateId"`
	Kind        string `json:"kind"`
	Outcome     string `json:"outcome"`
	DurationMs  int64  `json:"durationMs"`
}

// DispatchData is the redacted payload of an EventDispatch: the dispatch
// attempt/outcome detail derived from the step event.
type DispatchData struct {
	CandidateID string `json:"candidateId"`
	Attempts    int    `json:"attempts"`
	Outcome     string `json:"outcome"`
}

// WriteData is the redacted payload of an EventWrite.
type WriteData struct {
	Line            string `json:"line"`
	Kind            string `json:"kind"`
	Result          string `json:"result"`
	InstanceVersion int    `json:"instanceVersion"`
}

// ApprovalData is the redacted payload of an EventApproval.
type ApprovalData struct {
	RequestID string `json:"requestId"`
	Decision  string `json:"decision"`
}

// GapData is the payload of an EventGap: the inclusive range of dropped seqs.
type GapData struct {
	FromSeq uint64 `json:"fromSeq"`
	ToSeq   uint64 `json:"toSeq"`
}

// Stream fans the loop's observability events and the approval/metrics sources
// out to subscribers. It implements Observer and is safe for concurrent use
// (the pooled path calls Step/Write from worker goroutines). Construct it with
// NewStream.
type Stream struct {
	red     *Redactor
	metrics func() Snapshot

	mu     sync.Mutex
	seq    uint64
	nextID uint64
	subs   map[uint64]*subscription
	ring   []Event
}

// NewStream builds a Stream. red redacts every string payload (nil is a no-op);
// metrics, when non-nil, is sampled after each step to emit an EventMetrics.
func NewStream(red *Redactor, metrics func() Snapshot) *Stream {
	return &Stream{red: red, metrics: metrics, subs: map[uint64]*subscription{}}
}

// subscription is one subscriber's channel and overflow state. Its fields are
// guarded by the owning Stream's mutex.
type subscription struct {
	id    uint64
	ch    chan Event
	types map[string]bool // nil = all types

	dropping bool
	dropFrom uint64
	dropTo   uint64
}

// accepts reports whether the subscriber wants an event of type t. A nil filter
// accepts every type.
func (sub *subscription) accepts(t string) bool {
	return sub.types == nil || sub.types[t]
}

// send delivers ev non-blocking, applying the gap policy. It is called under
// the Stream mutex.
func (sub *subscription) send(ev Event) {
	if !sub.dropping {
		select {
		case sub.ch <- ev:
			return
		default:
			sub.dropping = true
			sub.dropFrom = ev.Seq
			sub.dropTo = ev.Seq
			return
		}
	}
	// In a drop burst: extend the range, then enqueue ONE gap event if space
	// has freed. The current ev stays dropped (it is inside the range).
	if ev.Seq > sub.dropTo {
		sub.dropTo = ev.Seq
	}
	gap := Event{Seq: sub.dropFrom, Type: EventGap, Data: GapData{FromSeq: sub.dropFrom, ToSeq: sub.dropTo}}
	select {
	case sub.ch <- gap:
		sub.dropping = false
	default:
		// Still full; keep accumulating the range.
	}
}

// Subscribe registers a subscriber for all event types. It returns the receive
// channel and an idempotent cancel func that closes the channel.
func (s *Stream) Subscribe(types []string) (<-chan Event, func()) {
	return s.SubscribeFrom(types, 0)
}

// SubscribeFrom is Subscribe with replay: events still in the bounded ring with
// seq >= fromSeq are delivered first (fromSeq <= 0 replays nothing). Replay and
// live delivery share the subscriber's bounded buffer, so a large replay may
// itself yield a gap event.
func (s *Stream) SubscribeFrom(types []string, fromSeq uint64) (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	sub := &subscription{id: s.nextID, ch: make(chan Event, defaultBuffer), types: typeSet(types)}
	if fromSeq > 0 {
		for _, ev := range s.ring {
			if ev.Seq >= fromSeq && sub.accepts(ev.Type) {
				sub.send(ev)
			}
		}
	}
	s.subs[sub.id] = sub
	id := sub.id
	return sub.ch, func() { s.cancel(id) }
}

// cancel removes a subscriber and closes its channel. Idempotent: a second call
// (or a call after the stream removed it) is a no-op.
func (s *Stream) cancel(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[id]
	if !ok {
		return
	}
	delete(s.subs, id)
	close(sub.ch)
}

// publish assigns the next seq and delivers ev to every accepting subscriber,
// all under one mutex so ordering is total and per-subscriber stable.
func (s *Stream) publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.Seq = s.seq
	s.ringAppend(ev)
	for _, sub := range s.subs {
		if sub.accepts(ev.Type) {
			sub.send(ev)
		}
	}
}

// ringAppend keeps the last ringCap events for fromSeq replay.
func (s *Stream) ringAppend(ev Event) {
	s.ring = append(s.ring, ev)
	if len(s.ring) > ringCap {
		s.ring = append(s.ring[:0], s.ring[len(s.ring)-ringCap:]...)
	}
}

// --- Observer ---

// Step emits a step event, a dispatch event (the attempt/outcome detail), and —
// when a metrics provider was configured — a metrics snapshot, in that order.
func (s *Stream) Step(e StepEvent) {
	rd := s.red
	s.publish(Event{Type: EventStep, Data: StepData{
		CandidateID: rd.Redact(e.CandidateID),
		Kind:        rd.Redact(e.Kind),
		Outcome:     rd.Redact(e.Outcome),
		DurationMs:  e.Duration.Milliseconds(),
	}})
	s.publish(Event{Type: EventDispatch, Data: DispatchData{
		CandidateID: rd.Redact(e.CandidateID),
		Attempts:    e.Attempts,
		Outcome:     rd.Redact(e.Outcome),
	}})
	if s.metrics != nil {
		s.publish(Event{Type: EventMetrics, Data: s.metrics()})
	}
}

// Write emits a write event for one applied EKA write.
func (s *Stream) Write(e WriteEvent) {
	rd := s.red
	s.publish(Event{Type: EventWrite, Data: WriteData{
		Line:            rd.Redact(e.Line),
		Kind:            rd.Redact(e.Kind),
		Result:          rd.Redact(e.Result),
		InstanceVersion: e.InstanceVersion,
	}})
}

// EmitApproval emits an approval event for one materialized human decision. The
// app calls it from Approve.
func (s *Stream) EmitApproval(requestID, decision string) {
	rd := s.red
	s.publish(Event{Type: EventApproval, Data: ApprovalData{
		RequestID: rd.Redact(requestID),
		Decision:  rd.Redact(decision),
	}})
}

// typeSet builds the type filter; an empty list means "all types" (nil set).
func typeSet(types []string) map[string]bool {
	if len(types) == 0 {
		return nil
	}
	m := make(map[string]bool, len(types))
	for _, t := range types {
		m[t] = true
	}
	return m
}
