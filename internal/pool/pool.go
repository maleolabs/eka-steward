// Package pool is the steward's bounded worker pool: the small, deterministic
// concurrency + backpressure primitive the control loop (internal/loop) uses to
// run worker dispatches concurrently without ever exceeding the count
// internal/sizing chose.
//
// It is deliberately minimal and owns exactly three things:
//
//   - a hard concurrency bound (MaxWorkers): at most this many jobs run at
//     once, so the pool can never dispatch more workers than the sizing policy
//     permits;
//   - a bounded queue (QueueLimit): Submit is NON-blocking and returns
//     ErrQueueFull once the queue is full, so admission is explicit and the
//     overflow is deterministic — the caller stops feeding and drains;
//   - a saturation metrics seam (Metrics) that the observability work item
//     (sto:steward-observability) will consume later.
//
// The pool holds no persistence, no clock and no randomness, and it knows
// nothing about EKA, the control loop's checkpoint, or the worker/LLM seam: it
// only runs the Task closures it is handed. Given the same tasks submitted in
// the same order it admits the same set (the first QueueLimit) and rejects the
// rest — see docs/backpressure.md.
package pool

import (
	"context"
	"errors"
	"sync"
)

// ErrQueueFull is returned by Submit when the bounded queue is full. It is the
// deterministic overflow signal: the caller stops submitting and drains. The
// pool never blocks a Submit and never drops a job silently.
var ErrQueueFull = errors.New("pool: queue full")

// Task is one unit of work the pool executes. The pool treats it as opaque: it
// measures queue depth and in-flight count around the call and returns the
// task's error. A Task must be safe to run concurrently with other tasks only
// to the extent the caller serializes shared state itself.
type Task func(ctx context.Context) error

// Metrics is the pool's minimal saturation seam. It is intentionally a small
// interface of plain int setters — no metrics stack, no labels, no timers —
// so the observability work item can adapt it to whatever backend it builds
// without the pool taking a dependency. Every method receives the current
// value; implementations must be safe for concurrent use because the pool
// calls them from its worker goroutines.
type Metrics interface {
	// QueueDepth is the CURRENT number of submitted-but-not-yet-started jobs.
	QueueDepth(n int)
	// InFlight is the CURRENT number of jobs executing right now.
	InFlight(n int)
	// Dispatched is the CUMULATIVE number of jobs that have started.
	Dispatched(n int)
	// Rejected is the CUMULATIVE number of Submit calls refused by the queue
	// limit (overflow events).
	Rejected(n int)
	// PeakInFlight is the highest InFlight value observed so far.
	PeakInFlight(n int)
}

// NopMetrics discards every observation. It is the default when no recorder is
// wired, so the pool never needs a nil check on its hot path.
type NopMetrics struct{}

func (NopMetrics) QueueDepth(int)   {}
func (NopMetrics) InFlight(int)     {}
func (NopMetrics) Dispatched(int)   {}
func (NopMetrics) Rejected(int)     {}
func (NopMetrics) PeakInFlight(int) {}

// Snapshot is an instantaneous reading of a Recorder.
type Snapshot struct {
	QueueDepth     int
	PeakQueueDepth int
	InFlight       int
	Dispatched     int
	Rejected       int
	PeakInFlight   int
}

// Recorder is an in-memory Metrics for tests and for a caller that wants to
// read the saturation numbers back. It is safe for concurrent use.
type Recorder struct {
	mu   sync.Mutex
	snap Snapshot
}

func (r *Recorder) record(apply func(*Snapshot)) {
	r.mu.Lock()
	apply(&r.snap)
	r.mu.Unlock()
}

func (r *Recorder) QueueDepth(n int) {
	r.record(func(s *Snapshot) {
		s.QueueDepth = n
		if n > s.PeakQueueDepth {
			s.PeakQueueDepth = n
		}
	})
}
func (r *Recorder) InFlight(n int)   { r.record(func(s *Snapshot) { s.InFlight = n }) }
func (r *Recorder) Dispatched(n int) { r.record(func(s *Snapshot) { s.Dispatched = n }) }
func (r *Recorder) Rejected(n int)   { r.record(func(s *Snapshot) { s.Rejected = n }) }

func (r *Recorder) PeakInFlight(n int) {
	r.record(func(s *Snapshot) {
		if n > s.PeakInFlight {
			s.PeakInFlight = n
		}
	})
}

// Snapshot returns the latest recorded values.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap
}

// Pool is a bounded worker pool. Create it with NewPool, Submit jobs, then
// call Drain exactly once. Submit must not be called concurrently with or
// after Drain.
type Pool struct {
	maxWorkers int
	metrics    Metrics
	jobs       chan Task

	mu         sync.Mutex
	closed     bool
	rejected   int
	dispatched int
	inFlight   int
	peak       int
}

// NewPool builds a pool that runs at most maxWorkers jobs at once and buffers
// at most queueLimit submitted jobs. A non-positive maxWorkers or queueLimit is
// clamped to 1 (a pool always runs at least one worker and always has a
// finite, non-empty queue). A nil metrics uses NopMetrics.
func NewPool(maxWorkers, queueLimit int, m Metrics) *Pool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if queueLimit < 1 {
		queueLimit = 1
	}
	if m == nil {
		m = NopMetrics{}
	}
	return &Pool{
		maxWorkers: maxWorkers,
		metrics:    m,
		jobs:       make(chan Task, queueLimit),
	}
}

// MaxWorkers reports the hard concurrency bound.
func (p *Pool) MaxWorkers() int { return p.maxWorkers }

// QueueLimit reports the bounded queue capacity.
func (p *Pool) QueueLimit() int { return cap(p.jobs) }

// Submit enqueues one job. It never blocks: if the queue is full it returns
// ErrQueueFull and records a rejection, so the caller can stop feeding and
// drain deterministically. Submit must be called before Drain.
func (p *Pool) Submit(t Task) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrQueueFull
	}
	select {
	case p.jobs <- t:
		depth := len(p.jobs)
		p.mu.Unlock()
		p.metrics.QueueDepth(depth)
		return nil
	default:
		p.rejected++
		rejected := p.rejected
		p.mu.Unlock()
		p.metrics.Rejected(rejected)
		return ErrQueueFull
	}
}

// Drain closes the queue and runs the buffered jobs with at most MaxWorkers
// workers, blocking until every runnable job has finished. It returns the first
// task error, cancelling the remaining work on that error. Drain runs every
// accepted job at most once.
func (p *Pool) Drain(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	close(p.jobs)
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := 0; i < p.maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-p.jobs:
					if !ok {
						return
					}
					if err := p.runOne(ctx, t); err != nil {
						once.Do(func() {
							firstErr = err
							cancel()
						})
					}
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// runOne executes one task while maintaining the in-flight counter and the
// saturation metrics. The metrics callbacks are made outside the pool mutex so
// a slow recorder can never stall admission.
func (p *Pool) runOne(ctx context.Context, t Task) error {
	p.mu.Lock()
	p.inFlight++
	p.dispatched++
	if p.inFlight > p.peak {
		p.peak = p.inFlight
	}
	inFlight, dispatched, peak := p.inFlight, p.dispatched, p.peak
	p.mu.Unlock()

	p.metrics.InFlight(inFlight)
	p.metrics.QueueDepth(len(p.jobs))
	p.metrics.Dispatched(dispatched)
	p.metrics.PeakInFlight(peak)

	err := t(ctx)

	p.mu.Lock()
	p.inFlight--
	inFlight = p.inFlight
	p.mu.Unlock()
	p.metrics.InFlight(inFlight)
	return err
}
