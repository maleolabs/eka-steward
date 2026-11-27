package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolRunsEveryJobExactlyOnce asserts a full queue drains with each job run
// exactly once.
func TestPoolRunsEveryJobExactlyOnce(t *testing.T) {
	const jobs = 50
	var n int64
	p := NewPool(4, 100, nil)
	for i := 0; i < jobs; i++ {
		if err := p.Submit(func(context.Context) error { atomic.AddInt64(&n, 1); return nil }); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	if err := p.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := atomic.LoadInt64(&n); got != jobs {
		t.Fatalf("ran %d jobs, want %d", got, jobs)
	}
}

// TestPoolRejectsWhenFullDeterministic pins the overflow rule: with QueueLimit
// 3 the first three submissions are accepted and the rest refused with
// ErrQueueFull, identically across 20 runs.
func TestPoolRejectsWhenFullDeterministic(t *testing.T) {
	const (
		runs          = 20
		queueLimit    = 3
		total         = 5
		maxWorkers    = 2
		wantAcceptedN = queueLimit
	)
	var first []int
	for r := 0; r < runs; r++ {
		p := NewPool(maxWorkers, queueLimit, nil)
		var accepted []int
		for i := 0; i < total; i++ {
			err := p.Submit(func(context.Context) error { return nil })
			switch {
			case err == nil:
				accepted = append(accepted, i)
			case errors.Is(err, ErrQueueFull):
				// expected overflow
			default:
				t.Fatalf("Submit %d: unexpected error %v", i, err)
			}
		}
		if len(accepted) != wantAcceptedN {
			t.Fatalf("run %d accepted %d jobs, want %d", r, len(accepted), wantAcceptedN)
		}
		if r == 0 {
			first = accepted
			continue
		}
		if !equalInts(first, accepted) {
			t.Fatalf("accepted set drifted: run %d = %v, want %v", r, accepted, first)
		}
	}
	if !equalInts(first, []int{0, 1, 2}) {
		t.Fatalf("accepted = %v, want the deterministic prefix [0 1 2]", first)
	}
}

// TestPoolPeakConcurrencyRespectsMaxWorkers blocks every job and asserts the
// observed peak never exceeds MaxWorkers and does reach it.
func TestPoolPeakConcurrencyRespectsMaxWorkers(t *testing.T) {
	const maxWorkers = 3
	d := &blocker{release: make(chan struct{})}
	p := NewPool(maxWorkers, 10, nil)
	for i := 0; i < 6; i++ {
		if err := p.Submit(d.run); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- p.Drain(context.Background()) }()

	waitForPeak(t, d.peak, maxWorkers)
	if got := d.peak(); got > maxWorkers {
		t.Fatalf("peak in-flight = %d, exceeds MaxWorkers %d", got, maxWorkers)
	}
	close(d.release)
	if err := <-done; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got := d.peak(); got != maxWorkers {
		t.Fatalf("peak in-flight = %d, want %d", got, maxWorkers)
	}
}

// TestPoolQueueDepthNeverExceedsLimit asserts the saturation seam reports a
// queue depth that never exceeds the limit and counts the one overflow event.
func TestPoolQueueDepthNeverExceedsLimit(t *testing.T) {
	const queueLimit = 4
	rec := &Recorder{}
	d := &blocker{release: make(chan struct{})}
	p := NewPool(2, queueLimit, rec)
	for i := 0; i < queueLimit; i++ {
		if err := p.Submit(d.run); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	if err := p.Submit(d.run); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("overflow Submit = %v, want ErrQueueFull", err)
	}
	snap := rec.Snapshot()
	if snap.QueueDepth > queueLimit || snap.PeakQueueDepth > queueLimit {
		t.Fatalf("queue depth %d (peak %d) exceeds limit %d", snap.QueueDepth, snap.PeakQueueDepth, queueLimit)
	}
	if snap.Rejected != 1 {
		t.Fatalf("Rejected = %d, want 1", snap.Rejected)
	}

	done := make(chan error, 1)
	go func() { done <- p.Drain(context.Background()) }()
	waitForPeak(t, d.peak, 2)
	close(d.release)
	if err := <-done; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if snap := rec.Snapshot(); snap.PeakInFlight > 2 {
		t.Fatalf("peak in-flight %d exceeds MaxWorkers 2", snap.PeakInFlight)
	}
}

// TestPoolDrainReturnsFirstError asserts a task error cancels the drain and is
// returned.
func TestPoolDrainReturnsFirstError(t *testing.T) {
	sentinel := errors.New("boom")
	p := NewPool(2, 4, nil)
	if err := p.Submit(func(context.Context) error { return sentinel }); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Drain = %v, want the task error", err)
	}
}

// blocker is a Task that parks until release is closed, recording the peak
// number of concurrent executions.
type blocker struct {
	release chan struct{}
	mu      sync.Mutex
	running int
	peakN   int
}

func (b *blocker) run(ctx context.Context) error {
	b.mu.Lock()
	b.running++
	if b.running > b.peakN {
		b.peakN = b.running
	}
	b.mu.Unlock()

	select {
	case <-b.release:
	case <-ctx.Done():
	}

	b.mu.Lock()
	b.running--
	b.mu.Unlock()
	return nil
}

func (b *blocker) peak() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peakN
}

func waitForPeak(t *testing.T, peak func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for peak() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func equalInts(a, b []int) bool {
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
