package loop

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/pool"
)

// This file covers the bounded worker pool + backpressure work item
// (sto:worker-pool-backpressure). See docs/backpressure.md for the model, the
// deterministic overflow rule and the bounded-resource guarantees.

// bpCandidates builds n dispatchable candidates ("c01".."cNN") with a distinct
// CreatedSeq so decision.Order is c01, c02, ... — a fixed total order the
// overflow tests can rely on.
func bpCandidates(n int) []decision.Candidate {
	cs := make([]decision.Candidate, 0, n)
	for i := 1; i <= n; i++ {
		cs = append(cs, decision.Candidate{
			ID:         fmt.Sprintf("c%02d", i),
			State:      decision.StateTodo,
			CreatedSeq: uint64(i),
		})
	}
	return cs
}

// pooledLoop wires a Loop with the bounded pool enabled (QueueLimit > 0) and a
// cgroup fixture whose sizing base is 4 (16 cores / 16 GiB), so l.Workers() is
// clamp(4, 1, cfgMax).
func pooledLoop(t *testing.T, cs []decision.Candidate, d Dispatcher, cfgMax, queueLimit int, rec pool.Metrics) (*Loop, *Store) {
	t.Helper()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	cg := writeCgroup(t, "17179869184\n", "1600000 100000\n")
	return New(fixtureSource{cs}, d, nil, store, Config{
		MaxWorkers:   cfgMax,
		CgroupPrefix: cg,
		QueueLimit:   queueLimit,
		Metrics:      rec,
	}), store
}

// TestPooledRunNeverExceedsSizingWorkers asserts the pool's concurrency comes
// from internal/sizing, not from the raw MaxWorkers config: with config max 8
// and a 16-core/16-GiB snapshot the sizing count is 4, and the observed peak
// in-flight is exactly 4, never 5..8.
func TestPooledRunNeverExceedsSizingWorkers(t *testing.T) {
	const sizingWorkers = 4
	d := newBlockingDisp()
	l, _ := pooledLoop(t, bpCandidates(10), d, 8, 10, nil)

	errc := make(chan error, 1)
	go func() { errc <- l.Run(context.Background()) }()

	waitForPeak(t, d.peak, sizingWorkers)
	if got := d.peak(); got > sizingWorkers {
		t.Fatalf("peak in-flight = %d, exceeds sizing.Workers = %d", got, sizingWorkers)
	}
	close(d.release)
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.peak(); got != sizingWorkers {
		t.Fatalf("peak in-flight = %d, want %d", got, sizingWorkers)
	}
	if got := d.count(); got != 10 {
		t.Fatalf("dispatched %d, want 10", got)
	}
}

// TestPooledRunDeterministicOverflow pins the overflow rule: with QueueLimit 3
// and 5 candidates, one Run admits exactly the first 3 in decision.Order and
// leaves the rest undone, identically across 20 runs with shuffled inputs.
func TestPooledRunDeterministicOverflow(t *testing.T) {
	const (
		queueLimit = 3
		runs       = 20
	)
	base := bpCandidates(5)
	wantAccepted := []string{"c01", "c02", "c03"}

	for r := 0; r < runs; r++ {
		cs := permute(base, r)
		d := newRecordingDisp()
		l, store := pooledLoop(t, cs, d, 4, queueLimit, nil)
		if err := l.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", r, err)
		}
		if got := d.sortedIDs(); !equalStrings(got, wantAccepted) {
			t.Fatalf("run %d accepted %v, want the deterministic prefix %v", r, got, wantAccepted)
		}
		st, err := store.Load()
		if err != nil {
			t.Fatalf("run %d: Load: %v", r, err)
		}
		if len(st.Done) != queueLimit {
			t.Fatalf("run %d Done = %v, want %d completed", r, st.Done, queueLimit)
		}
		if len(st.InFlight) != 0 {
			t.Fatalf("run %d InFlight = %v, want empty", r, st.InFlight)
		}
	}
}

// TestPooledRunMetricsShowBoundedSaturation asserts saturation is observable:
// the pool never queues beyond the limit nor runs more than the sizing count,
// and the overflow is recorded.
func TestPooledRunMetricsShowBoundedSaturation(t *testing.T) {
	const (
		queueLimit = 8
		total      = 50
		sizing     = 4
	)
	rec := &pool.Recorder{}
	d := newBlockingDisp()
	l, _ := pooledLoop(t, bpCandidates(total), d, 4, queueLimit, rec)

	errc := make(chan error, 1)
	go func() { errc <- l.Run(context.Background()) }()
	waitForPeak(t, d.peak, sizing)
	close(d.release)
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := rec.Snapshot()
	if snap.PeakInFlight > sizing {
		t.Fatalf("PeakInFlight = %d, exceeds sizing %d", snap.PeakInFlight, sizing)
	}
	if snap.PeakQueueDepth > queueLimit {
		t.Fatalf("PeakQueueDepth = %d, exceeds QueueLimit %d", snap.PeakQueueDepth, queueLimit)
	}
	if snap.Dispatched != queueLimit {
		t.Fatalf("Dispatched = %d, want %d (deterministic overflow admits exactly QueueLimit)", snap.Dispatched, queueLimit)
	}
	if snap.Rejected < 1 {
		t.Fatalf("Rejected = %d, want >= 1 (overflow must be observable)", snap.Rejected)
	}
}

// TestPooledRunLoadBoundedAndExactlyOnce is the load test: 200 candidates with
// MaxWorkers=4 and QueueLimit=8, drained by repeated Runs. In-flight never
// exceeds the sizing count, the queue never exceeds its limit, and every
// candidate is dispatched and applied exactly once.
func TestPooledRunLoadBoundedAndExactlyOnce(t *testing.T) {
	const (
		total      = 200
		sizing     = 4
		queueLimit = 8
	)
	rec := &pool.Recorder{}
	d := newRecordingDisp()
	l, store := pooledLoop(t, bpCandidates(total), d, 4, queueLimit, rec)

	ctx := context.Background()
	runs := 0
	for {
		steps, err := l.Plan(ctx)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(steps) == 0 {
			break
		}
		if err := l.Run(ctx); err != nil {
			t.Fatalf("Run %d: %v", runs, err)
		}
		runs++
		if runs > total {
			t.Fatalf("load did not converge after %d runs", runs)
		}
	}

	if runs != total/queueLimit {
		t.Fatalf("runs = %d, want %d (one QueueLimit-sized batch per Run)", runs, total/queueLimit)
	}
	if got := d.count(); got != total {
		t.Fatalf("dispatched %d, want %d", got, total)
	}
	for _, id := range d.sortedIDs() {
		if n := d.countOf(id); n != 1 {
			t.Fatalf("candidate %s dispatched %d times, want exactly 1", id, n)
		}
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Done) != total {
		t.Fatalf("Done size = %d, want %d (each candidate applied exactly once)", len(st.Done), total)
	}
	if len(st.InFlight) != 0 {
		t.Fatalf("InFlight = %v, want empty", st.InFlight)
	}

	snap := rec.Snapshot()
	if snap.PeakInFlight > sizing {
		t.Fatalf("PeakInFlight = %d, exceeds sizing %d", snap.PeakInFlight, sizing)
	}
	if snap.PeakQueueDepth > queueLimit {
		t.Fatalf("PeakQueueDepth = %d, exceeds QueueLimit %d", snap.PeakQueueDepth, queueLimit)
	}
}

// TestPooledRunSingleWorkerMatchesPlan is the backward-compatibility check: with
// one worker (the sizing floor) the pooled Run emits the same step sequence as
// the pure Plan — byte-identical, so the golden contract holds.
func TestPooledRunSingleWorkerMatchesPlan(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	cg := writeCgroup(t, "17179869184\n", "1600000 100000\n")

	var mu sync.Mutex
	var got []Step
	collect := func(s Step) { mu.Lock(); got = append(got, s); mu.Unlock() }

	l := New(fixtureSource{fixtureCandidates()}, &spyDispatcher{}, nil, store, Config{
		MaxWorkers:   1,
		CgroupPrefix: cg,
		QueueLimit:   100,
		OnStep:       collect,
	})
	want, err := l.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(planJSON(t, got)) != string(planJSON(t, want)) {
		t.Fatalf("single-worker pooled Run sequence != Plan:\n got:\n%s\n want:\n%s", planJSON(t, got), planJSON(t, want))
	}
}

// --- test doubles ---

// blockingDisp blocks every dispatch until released, recording the peak number
// of concurrent dispatches.
type blockingDisp struct {
	release chan struct{}
	mu      sync.Mutex
	running int
	peakN   int
	total   int
}

func newBlockingDisp() *blockingDisp { return &blockingDisp{release: make(chan struct{})} }

func (d *blockingDisp) Dispatch(ctx context.Context, c decision.Candidate) (DispatchResult, error) {
	d.mu.Lock()
	d.running++
	d.total++
	if d.running > d.peakN {
		d.peakN = d.running
	}
	d.mu.Unlock()

	select {
	case <-d.release:
	case <-ctx.Done():
	}

	d.mu.Lock()
	d.running--
	d.mu.Unlock()
	return DispatchResult{CandidateID: c.ID}, nil
}

func (d *blockingDisp) peak() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.peakN
}

func (d *blockingDisp) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.total
}

// recordingDisp records every dispatched candidate id and how many times.
type recordingDisp struct {
	mu  sync.Mutex
	per map[string]int
}

func newRecordingDisp() *recordingDisp { return &recordingDisp{per: map[string]int{}} }

func (d *recordingDisp) Dispatch(_ context.Context, c decision.Candidate) (DispatchResult, error) {
	d.mu.Lock()
	d.per[c.ID]++
	d.mu.Unlock()
	return DispatchResult{CandidateID: c.ID}, nil
}

func (d *recordingDisp) sortedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]string, 0, len(d.per))
	for id := range d.per {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (d *recordingDisp) countOf(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.per[id]
}

func (d *recordingDisp) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, v := range d.per {
		n += v
	}
	return n
}

// --- helpers ---

// waitForPeak polls until the peak reaches want or a short deadline passes.
func waitForPeak(t *testing.T, peak func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for peak() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// permute returns a rotation (optionally reversed) of cs, so a suite of runs
// exercises different input orders of the same candidate multiset.
func permute(cs []decision.Candidate, r int) []decision.Candidate {
	out := append([]decision.Candidate(nil), cs...)
	if r%2 == 1 {
		out = reverse(out)
	}
	k := r % len(out)
	return append(append([]decision.Candidate(nil), out[k:]...), out[:k]...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
