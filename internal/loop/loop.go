// Package loop is the steward's deterministic control loop: the thin
// orchestrator that binds the pure decision kernel (internal/decision), the
// EKA read/write boundary (internal/ekawrite), the cgroup-backed worker
// sizing (internal/sizing) and the persisted resume state into one
// step-at-a-time run.
//
// The loop keeps the steward's core split (docs/determinism.md): CODE decides
// WHICH candidate runs next (Plan, via internal/decision) and the LLM/worker
// only runs inside Dispatcher.Dispatch. Plan is a pure selection over the
// source candidate set and the persisted state; it NEVER calls the
// Dispatcher. Dispatch is the only place the worker/LLM seam runs, and it is
// never consulted to make a control decision.
//
// # Seams
//
//   - Source reads EKA READ-ONLY and yields dispatchable candidates.
//   - Dispatcher is the worker/LLM seam; a fake + call spy in tests.
//   - Writer is the single EKA write path (internal/ekawrite); the loop names
//     no api.Authoring method itself.
//
// # Read-only reads, one write path
//
// Candidates come from Source (a store-backed Source reads through
// api.Knowledge, the read-only contract). Every mutation the loop performs
// goes through Writer, whose production implementation routes to
// internal/ekawrite — the ONE audited EKA write path (docs/write-path.md).
// The loop never imports an eka-core kernel-internal package.
package loop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
	"github.com/maleolabs/eka-steward/v2/internal/pool"
	"github.com/maleolabs/eka-steward/v2/internal/sizing"
)

// Source reads EKA READ-ONLY and yields the candidates the loop may dispatch.
// A Source must not mutate EKA; the store-backed implementation
// (StoreSource) reads through api.Knowledge only.
type Source interface {
	Candidates(ctx context.Context) ([]decision.Candidate, error)
}

// DispatchResult is the opaque outcome of one dispatch. Its Output is treated
// as data the steward may record; it never feeds back into a control
// decision.
type DispatchResult struct {
	// CandidateID echoes the dispatched candidate identity.
	CandidateID string
	// Output is the worker/LLM output (opaque; never a control input).
	Output string
}

// Dispatcher is the worker/LLM seam. Production wires the (deferred) ACP
// worker dispatch here; tests inject a fake that records calls. Dispatch is
// the ONLY place the loop touches a worker/LLM — Plan never calls it.
type Dispatcher interface {
	Dispatch(ctx context.Context, c decision.Candidate) (DispatchResult, error)
}

// Writer is the loop's view of the single EKA write path (internal/ekawrite).
// The loop names no api.Authoring method itself: a mutation is expressed as
// one Advance (the forward apply) or one Reconcile (the resume apply of a
// persisted intent), whose production implementations route through ekawrite.
type Writer interface {
	// Advance records the loop's decision for one dispatched candidate by
	// moving it to the state `to`. It is the forward mutation of a step.
	Advance(ctx context.Context, c decision.Candidate, to string) error
	// Reconcile re-issues one in-flight intent idempotently. It is the ONLY
	// write the resume pass performs: for a transition it is a no-op when the
	// line is already at/after the target state; for a publish it goes through
	// ekawrite's deterministic replay gate (a same-payload re-issue is a
	// no-op). It never dispatches.
	Reconcile(ctx context.Context, in Intent) (ReconcileResult, error)
}

// KindDispatch is the only step kind this cut emits: one unit of work chosen
// for dispatch.
const KindDispatch = "dispatch"

// Step is one deterministic dispatch decision: a candidate, in the pinned
// total order, chosen by Plan or carried out by Step. It is JSON-able so a
// golden file can freeze the exact sequence for a fixture store.
type Step struct {
	CandidateID string `json:"candidateId"`
	Kind        string `json:"kind"`
	Priority    int    `json:"priority"`
	State       string `json:"state"`
	CreatedSeq  uint64 `json:"createdSeq"`
}

// Config is the loop's operator-owned configuration.
type Config struct {
	// MaxWorkers is the sizing hard upper bound (sizing requires >= 1).
	MaxWorkers int
	// CostCeiling optionally caps the worker count further (0 = no ceiling).
	CostCeiling int
	// CgroupPrefix overrides the cgroup v2 mount root (tests point this at a
	// fixture directory). Empty means DefaultCgroupPrefix.
	CgroupPrefix string
	// DispatchState is the state a dispatched candidate moves to through the
	// Writer. Empty disables the write (plan/dispatch only) and with it the
	// checkpoint intent log — a run with no write path has nothing to
	// reconcile (docs/checkpoint.md). The loop does not re-derive transition
	// legality; the candidate must be in a state from which the engine permits
	// `to`.
	DispatchState string
	// Crash is the injectable crash-injection seam. Production leaves it nil.
	// A test panics at a chosen boundary (BoundaryAfterIntent / Dispatch /
	// Apply) to simulate a kill deterministically.
	Crash CrashPoint
	// OnStep, when set, observes every step the loop completes — both a
	// forward dispatch and a resume reconcile — exactly once. In the
	// sequential path (QueueLimit == 0) the order is the deterministic plan
	// order; in the pooled path (QueueLimit > 0) OnStep fires from worker
	// goroutines, so its order is the completion order and the callback must
	// be safe for concurrent use. It is an observability seam; it never feeds
	// a control decision.
	OnStep func(Step)
	// QueueLimit, when > 0, enables the bounded worker pool (internal/pool):
	// Run admits at most QueueLimit candidates in the deterministic
	// decision.Order and drains them with up to sizing.Workers members, and a
	// further candidate is refused with pool.ErrQueueFull (the deterministic
	// overflow rule, docs/backpressure.md). 0 (the default) keeps the
	// unchanged sequential path — one step at a time, byte-identical to the
	// pre-pool loop.
	QueueLimit int
	// Metrics is the control-plane observability seam (internal/pool.Metrics):
	// on the pooled path the pool reports saturation through it, and on every
	// path the loop reports its bounded-retry and cancel counters (Retries,
	// Cancellations). nil uses a no-op default.
	Metrics pool.Metrics
	// Retry bounds the loop's re-dispatch of one candidate (docs/recovery.md).
	// The zero value ({0,0}) clamps to MaxAttempts 1: no retry, exactly the
	// pre-retry loop.
	Retry RetryPolicy
	// AttemptTimeout, when > 0, bounds each dispatch attempt with
	// context.WithTimeout: a timed-out attempt is a retryable failure. 0 means
	// no per-attempt deadline (the attempt is bounded only by the caller ctx).
	AttemptTimeout time.Duration
	// Sleep is the injectable backoff-delay seam: the loop calls it between
	// retry attempts. nil uses time.Sleep (production). A test injects a
	// recorder that returns immediately, so a retry run never waits on the
	// wall clock while still asserting the exact delay sequence.
	Sleep func(time.Duration)
	// Review configures the review stage. Its zero value disables review, so
	// the loop is byte-identical to the pre-review loop; enabling it routes a
	// worker's output to a deterministic review role and gates completion on
	// the verdict (docs/review.md).
	Review ReviewPolicy
	// Observer, when set, is notified of every completed or failed step and
	// every applied EKA write, so a run can be logged and reported without the
	// loop naming a logging stack (internal/obs, docs/observability.md). nil
	// disables it. It is an observability seam: it never feeds a control
	// decision and must be safe for concurrent use (the pooled path calls it
	// from worker goroutines).
	Observer obs.Observer
	// Now is the injectable clock the loop uses to stamp each step's duration
	// in the observer's step event. nil uses time.Now. A test installs a
	// deterministic clock so a run report is byte-stable.
	Now func() time.Time
}

// Loop is the deterministic control loop. Construct it with New. The sequential
// path (QueueLimit == 0) is single-threaded. The pooled path (QueueLimit > 0)
// may run several steps concurrently; it serializes every Store mutation and
// every Writer mutation internally, so a single Loop is safe for one Run at a
// time.
type Loop struct {
	source     Source
	dispatcher Dispatcher
	writer     Writer
	state      *Store
	cfg        Config

	// stateMu serializes every access to the persisted Store so concurrent
	// pool workers can never interleave a Load/modify/Save and lose an intent
	// or a Done entry (docs/backpressure.md). It is held only around Store
	// work, never around a dispatch.
	stateMu sync.Mutex
	// writeMu serializes Writer mutations so the single EKA write path is
	// never entered concurrently by two workers.
	writeMu sync.Mutex

	// cancelMu guards batchCancel, the cancel func of the Run in progress.
	// Cancel reads it to cancel the current batch deterministically.
	cancelMu    sync.Mutex
	batchCancel context.CancelFunc

	// metricMu guards the loop's own cumulative retry/cancel counters, which
	// are reported to the Config.Metrics seam (pooled path: concurrent).
	metricMu sync.Mutex
	retries  int
	cancels  int
}

// New wires a Loop from its seams. state must be non-nil: the loop records
// completed work there so a restart resumes deterministically.
func New(source Source, dispatcher Dispatcher, writer Writer, state *Store, cfg Config) *Loop {
	return &Loop{
		source:     source,
		dispatcher: dispatcher,
		writer:     writer,
		state:      state,
		cfg:        cfg,
	}
}

// Plan returns the deterministic step sequence the loop would emit from the
// current source set, skipping work already recorded done. It is a PURE
// selection: it reads the Source and the persisted state, orders the
// candidates with internal/decision.Order, and never calls the Dispatcher.
// The same source set + state yields byte-identical Steps over any number of
// calls and any permutation of the input.
func (l *Loop) Plan(ctx context.Context) ([]Step, error) {
	cs, err := l.selectable(ctx)
	if err != nil {
		return nil, err
	}
	ordered := decision.Order(cs)
	steps := make([]Step, 0, len(ordered))
	for _, c := range ordered {
		if !ready(c) {
			continue
		}
		steps = append(steps, stepFor(c))
	}
	return steps, nil
}

// Step plans and dispatches ONE candidate. It returns false (and a zero Step)
// when nothing is dispatchable. The step is a checkpointed write-ahead
// transaction: the intent is persisted, the worker runs, the mutation is
// applied through the Writer, and only then is the intent cleared and the id
// recorded done — each transition saved atomically. A crash at any boundary is
// recovered by the next Run's resume pass (docs/checkpoint.md).
func (l *Loop) Step(ctx context.Context) (Step, bool, error) {
	cs, err := l.selectable(ctx)
	if err != nil {
		return Step{}, false, err
	}
	c, ok := decision.SelectNext(cs)
	if !ok {
		return Step{}, false, nil
	}
	step, err := l.runStep(ctx, c)
	if err != nil {
		return step, true, err
	}
	return step, true, nil
}

// runStep times one checkpointed transaction and reports it to the observer,
// so every step — dispatch, resume reconcile and the no-write path — yields
// exactly one structured record with its outcome and the number of attempts.
func (l *Loop) runStep(ctx context.Context, c decision.Candidate) (Step, error) {
	start := l.now()
	step, attempts, err := l.stepOne(ctx, c)
	l.emitStep(c, step, attempts, err, l.now().Sub(start))
	return step, err
}

// stepOne runs the checkpointed transaction for one already-selected
// candidate: intent → dispatch → apply → done. Step selects the candidate (in
// the sequential path) and the pool workers call it directly (in the pooled
// path). It serializes every Store mutation (stateMu) and every Writer
// mutation (writeMu) but never the dispatch, so workers overlap on the LLM
// seam while the checkpoint stays consistent.
//
// The second result is the number of dispatch attempts the step made (0 when
// it failed before dispatching).
//
// The dispatch is retried at most Config.Retry.MaxAttempts times
// (docs/recovery.md). Each attempt is its own write-ahead transaction: the
// intent is persisted before the attempt and rolled back the moment the
// attempt fails, so an in-flight intent only ever covers the attempt currently
// running and a failed candidate leaves NO partial EKA write behind. After
// MaxAttempts exhausted attempts the candidate is left un-Done (deferred, never
// dropped) and a deterministic outcome is recorded.
func (l *Loop) stepOne(ctx context.Context, c decision.Candidate) (Step, int, error) {
	step := stepFor(c)

	// Review-required path: dispatch the author, run the review stage, and
	// only on approval persist the transition intent and apply. The intent is
	// deferred until AFTER approval, so a crash during the author dispatch or
	// the review leaves NO in-flight intent — resume never advances an
	// un-reviewed item (docs/review.md).
	if required, err := l.cfg.Review.requires(); err != nil {
		return step, 0, err
	} else if required {
		attempts, err := l.reviewedStep(ctx, c, step)
		return step, attempts, err
	}

	// No write path configured: the run is plan/dispatch only and carries no
	// checkpoint, so there is nothing to reconcile on resume.
	if l.writer == nil || l.cfg.DispatchState == "" {
		_, attempts, err := l.dispatchWithRetry(ctx, c, nil, nil)
		if err != nil {
			return step, attempts, err
		}
		if err := l.markDone(c.ID); err != nil {
			return step, attempts, err
		}
		l.observe(step)
		return step, attempts, nil
	}

	in := Intent{
		CandidateID: c.ID,
		Kind:        IntentTransition,
		Target:      c.ID,
		To:          l.cfg.DispatchState,
		Step:        step,
	}
	begin := func() error { return l.beginIntent(in) }
	rollback := func() error { return l.rollbackIntent(c.ID) }

	// Write-ahead: persist the intent BEFORE each attempt and roll it back on
	// failure, so a normal dispatch (or timeout) failure leaves no intent and
	// no partial write; only a crash leaves the intent for resume.
	_, attempts, err := l.dispatchWithRetry(ctx, c, begin, rollback)
	if err != nil {
		return step, attempts, err
	}
	l.crash(BoundaryAfterDispatch)

	if err := l.advance(ctx, c); err != nil {
		return step, attempts, fmt.Errorf("loop: record %s: %w", c.ID, err)
	}
	l.observeWrite(c.ID, string(IntentTransition), "applied")
	l.crash(BoundaryAfterApply)

	if err := l.completeIntent(c.ID); err != nil {
		return step, attempts, err
	}
	l.observe(step)
	return step, attempts, nil
}

// reviewedStep runs one review-required step: dispatch the author, run the
// review stage, record the outcome, and either apply the transition (approved)
// or leave the candidate un-done (not approved). The transition intent is
// persisted only after approval, so resume never completes an un-reviewed item.
// It returns the number of author dispatch attempts.
func (l *Loop) reviewedStep(ctx context.Context, c decision.Candidate, step Step) (int, error) {
	rec, ok := l.writer.(ReviewRecorder)
	if l.writer == nil || !ok {
		return 0, fmt.Errorf("loop: review required for %s but the writer cannot record a review", c.ID)
	}

	res, attempts, err := l.dispatchWithRetry(ctx, c, nil, nil)
	if err != nil {
		return attempts, err
	}
	l.crash(BoundaryAfterDispatch)

	outcome, err := l.runReview(ctx, c, res.Output)
	if err != nil {
		return attempts, err
	}
	if err := l.recordReview(ctx, rec, c, outcome); err != nil {
		return attempts, fmt.Errorf("loop: record review of %s: %w", c.ID, err)
	}
	l.observeWrite(c.ID, WriteKindNote, outcome.Verdict)
	if !outcome.Approved() {
		return attempts, &ReviewRefusal{
			CandidateID: c.ID,
			Reviewer:    outcome.Reviewer,
			Verdict:     outcome.Verdict,
			Notes:       outcome.Notes,
		}
	}

	// Approved: apply the step. Without a write path the run is plan/dispatch
	// only; otherwise the transition runs as its own write-ahead transaction.
	if l.cfg.DispatchState == "" {
		if err := l.markDone(c.ID); err != nil {
			return attempts, err
		}
		l.observe(step)
		return attempts, nil
	}

	if err := l.beginIntent(Intent{
		CandidateID: c.ID,
		Kind:        IntentTransition,
		Target:      c.ID,
		To:          l.cfg.DispatchState,
		Step:        step,
	}); err != nil {
		return attempts, err
	}
	l.crash(BoundaryAfterIntent)

	if err := l.advance(ctx, c); err != nil {
		return attempts, fmt.Errorf("loop: record %s: %w", c.ID, err)
	}
	l.observeWrite(c.ID, string(IntentTransition), "applied")
	l.crash(BoundaryAfterApply)

	if err := l.completeIntent(c.ID); err != nil {
		return attempts, err
	}
	l.observe(step)
	return attempts, nil
}

// dispatchWithRetry runs the bounded, deterministic dispatch retry loop for one
// candidate. For each attempt it calls begin (persisting the write-ahead
// intent; nil when there is no write path), runs the worker under the
// per-attempt timeout, and on success returns nil with the intent left in
// flight for the caller to apply and complete. On failure it rolls the failed
// attempt's intent back via rollback (nil when there is no write path) and
// retries until MaxAttempts is reached or the context is cancelled.
//
// It returns the number of dispatch attempts actually made (0 when begin
// failed before the first dispatch), for the observer's step record.
//
// Cancellation (the caller's ctx, or Loop.Cancel) is never retried: the loop
// stops, the attempt's intent is already rolled back, and the candidate stays
// un-Done. A retry is reported to the Metrics seam exactly once per
// re-dispatch, so the count is deterministic: MaxAttempts attempts yield
// MaxAttempts-1 retries when every attempt fails.
func (l *Loop) dispatchWithRetry(ctx context.Context, c decision.Candidate, begin, rollback func() error) (DispatchResult, int, error) {
	max := l.cfg.Retry.MaxAttempts
	if max < 1 {
		max = 1
	}
	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= max; attempt++ {
		if begin != nil {
			if err := begin(); err != nil {
				return DispatchResult{}, attempts, err
			}
			l.crash(BoundaryAfterIntent)
		}

		attempts = attempt
		res, err := l.dispatch(ctx, c)
		if err == nil {
			return res, attempts, nil
		}
		lastErr = err
		if rollback != nil {
			_ = rollback()
		}

		// Stop without recording a retry when this was the last allowed
		// attempt or the context has been cancelled.
		if attempt == max || ctx.Err() != nil {
			break
		}
		l.recordRetry()
		l.sleep(l.cfg.Retry.Backoff)
	}
	return DispatchResult{}, attempts, lastErr
}

// dispatch runs ONE dispatch attempt, bounded by Config.AttemptTimeout when it
// is set. A timeout surfaces as a context.DeadlineExceeded error, which the
// retry loop treats as a retryable failure.
func (l *Loop) dispatch(ctx context.Context, c decision.Candidate) (DispatchResult, error) {
	attemptCtx := ctx
	if l.cfg.AttemptTimeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, l.cfg.AttemptTimeout)
		defer cancel()
	}
	res, err := l.dispatcher.Dispatch(attemptCtx, c)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("loop: dispatch %s: %w", c.ID, err)
	}
	return res, nil
}

// sleep waits for the deterministic backoff delay between retry attempts. A
// non-positive delay and the injected test seam both return immediately.
func (l *Loop) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if l.cfg.Sleep != nil {
		l.cfg.Sleep(d)
		return
	}
	time.Sleep(d)
}

// recordRetry increments the cumulative retry counter and reports it to the
// Metrics seam.
func (l *Loop) recordRetry() {
	l.metricMu.Lock()
	l.retries++
	n := l.retries
	l.metricMu.Unlock()
	l.metrics().Retries(n)
}

// recordCancellation increments the cumulative cancel counter and reports it to
// the Metrics seam.
func (l *Loop) recordCancellation() {
	l.metricMu.Lock()
	l.cancels++
	n := l.cancels
	l.metricMu.Unlock()
	l.metrics().Cancellations(n)
}

// metrics returns the observability seam, defaulting to a no-op so the loop
// never needs a nil check on its path.
func (l *Loop) metrics() pool.Metrics {
	if l.cfg.Metrics == nil {
		return pool.NopMetrics{}
	}
	return l.cfg.Metrics
}

// advance applies the step's mutation through the Writer under writeMu, so the
// single EKA write path is never entered concurrently.
func (l *Loop) advance(ctx context.Context, c decision.Candidate) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.writer.Advance(ctx, c, l.cfg.DispatchState)
}

// Resume reconciles every in-flight intent persisted by a previous run BEFORE
// any new dispatch, then clears it. It never dispatches: a transition is
// re-issued only when the line is not yet at/after the target state; a publish
// is re-issued through ekawrite's replay gate (a same-payload re-issue is a
// no-op). It is deterministic: the intent log is processed in sorted candidate
// order. Run calls it first, so a restart skips completed work and completes
// any step a crash interrupted.
func (l *Loop) Resume(ctx context.Context) error {
	st, err := l.state.Load()
	if err != nil {
		return err
	}
	if len(st.InFlight) == 0 {
		return nil
	}
	if l.writer == nil {
		return fmt.Errorf("loop: %d in-flight intent(s) but no writer to reconcile", len(st.InFlight))
	}
	for _, in := range st.InFlight {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := l.writer.Reconcile(ctx, in)
		if err != nil {
			return fmt.Errorf("loop: reconcile %s: %w", in.CandidateID, err)
		}
		l.observeWrite(in.Target, string(in.Kind), res.Status)
		if err := l.completeIntent(in.CandidateID); err != nil {
			return err
		}
		l.observe(in.Step)
	}
	return nil
}

// Run reconciles any in-flight intent (resume) and then drives the loop until
// nothing is dispatchable or the batch is cancelled, returning nil (nothing
// left, or a clean cancel) or the error.
//
// Run installs a cancellable batch context; Loop.Cancel cancels it, which
// cancels every in-flight dispatch in this Run. A cancelled batch is a clean,
// deterministic stop: each in-flight worker returns, its intent is rolled back
// (no EKA write) and its candidate is left un-Done, so Run returns nil and the
// next Run re-selects the same candidates (docs/recovery.md).
//
// With the default QueueLimit == 0 the loop is sequential: Run drives Step
// until nothing is dispatchable, and for a fixed Source/Dispatcher/state an
// uninterrupted run and a crash-then-resume run emit a byte-identical step
// sequence (docs/checkpoint.md).
//
// With QueueLimit > 0 Run uses the bounded worker pool: it admits at most
// QueueLimit candidates in the deterministic decision.Order, drains them with
// up to sizing.Workers members, and returns when that batch is drained. A
// candidate past the queue limit is left undone for the next Run — the
// deterministic overflow rule in docs/backpressure.md, never a nondeterministic
// drop. The caller drives repeated Runs to quiescence.
func (l *Loop) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l.setBatch(cancel)
	defer func() {
		l.clearBatch()
		cancel()
	}()

	if err := l.Resume(ctx); err != nil {
		return l.dispatchErr(ctx, err)
	}
	var err error
	if l.cfg.QueueLimit > 0 {
		err = l.runPooled(ctx)
	} else {
		err = l.runSequential(ctx)
	}
	return l.dispatchErr(ctx, err)
}

// dispatchErr maps a run outcome to Run's return value. A cancelled batch (the
// caller's ctx or Loop.Cancel) is a clean, deterministic stop — the in-flight
// intents were rolled back and the candidates stay un-Done — so Run returns
// nil. Any other error is returned unchanged.
func (l *Loop) dispatchErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}

// setBatch records the cancel func of the Run in progress.
func (l *Loop) setBatch(c context.CancelFunc) {
	l.cancelMu.Lock()
	l.batchCancel = c
	l.cancelMu.Unlock()
}

// clearBatch drops the recorded batch cancel func.
func (l *Loop) clearBatch() {
	l.cancelMu.Lock()
	l.batchCancel = nil
	l.cancelMu.Unlock()
}

// Cancel cancels the current batch: every in-flight dispatch of the Run in
// progress has its context cancelled. Each cancelled worker's intent is rolled
// back (no EKA write) and its candidate is left un-Done, so it is re-selected
// by the next Run; Run itself returns nil (a clean cancel, not an error).
// Cancel is idempotent, safe to call concurrently with Run, and a no-op when no
// batch is active. A standalone Step caller cancels through its own context
// instead. See docs/recovery.md.
func (l *Loop) Cancel() {
	l.cancelMu.Lock()
	c := l.batchCancel
	l.cancelMu.Unlock()
	if c == nil {
		return
	}
	c()
	l.recordCancellation()
}

// runSequential is the original single-threaded driver: Step until nothing is
// dispatchable.
func (l *Loop) runSequential(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, ok, err := l.Step(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
}

// runPooled drives one admission window through the bounded worker pool. It
// selects the dispatchable candidates, orders them with the pinned decision
// order, and Submits them in that order: the first QueueLimit are admitted and
// the rest are refused with pool.ErrQueueFull, at which point the loop stops
// planning and drains (docs/backpressure.md). The accepted set is therefore a
// pure function of the candidate set, QueueLimit and the persisted state.
func (l *Loop) runPooled(ctx context.Context) error {
	cs, err := l.selectable(ctx)
	if err != nil {
		return err
	}
	var dispatchable []decision.Candidate
	for _, c := range decision.Order(cs) {
		if ready(c) {
			dispatchable = append(dispatchable, c)
		}
	}
	if len(dispatchable) == 0 {
		return nil
	}

	// MaxWorkers comes from the sizing policy, never from a raw config value:
	// the pool can never run more workers than internal/sizing chose.
	workers, err := l.Workers()
	if err != nil {
		return err
	}
	p := pool.NewPool(workers, l.cfg.QueueLimit, l.cfg.Metrics)

	for _, cand := range dispatchable {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := cand
		err := p.Submit(func(ctx context.Context) error {
			_, err := l.runStep(ctx, c)
			return err
		})
		if err == nil {
			continue
		}
		if errors.Is(err, pool.ErrQueueFull) {
			// Deterministic overflow: stop planning additional items and
			// drain the admitted ones. The refused candidates stay undone.
			break
		}
		return err
	}
	return p.Drain(ctx)
}

// Workers reads cgroup v2 resources under the configured prefix and applies
// the deterministic sizing formula (internal/sizing), clamped to MaxWorkers.
// Missing / "max" / unreadable cgroup files fall back conservatively to the
// 1-worker floor; host totals are never substituted (see CgroupReader).
func (l *Loop) Workers() (int, error) {
	prefix := l.cfg.CgroupPrefix
	if prefix == "" {
		prefix = DefaultCgroupPrefix
	}
	res := CgroupReader{Prefix: prefix}.Resources()
	return sizing.Workers(res, sizing.Config{
		MaxWorkers:  l.cfg.MaxWorkers,
		CostCeiling: l.cfg.CostCeiling,
	})
}

// selectable returns the source candidates minus those already recorded done.
func (l *Loop) selectable(ctx context.Context) ([]decision.Candidate, error) {
	cs, err := l.source.Candidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("loop: read candidates: %w", err)
	}
	st, err := l.state.Load()
	if err != nil {
		return nil, err
	}
	out := make([]decision.Candidate, 0, len(cs))
	for _, c := range cs {
		if !st.Has(c.ID) {
			out = append(out, c)
		}
	}
	return out, nil
}

// markDone records id as completed in the persisted state.
func (l *Loop) markDone(id string) error {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	st, err := l.state.Load()
	if err != nil {
		return err
	}
	if st.Has(id) {
		return nil
	}
	st.Done = append(st.Done, id)
	return l.state.Save(st)
}

// beginIntent persists one write-ahead intent (atomically) before its mutation
// is attempted. A re-begin for the same candidate replaces the entry. The Store
// mutation is serialized under stateMu so concurrent workers never lose an
// intent.
func (l *Loop) beginIntent(in Intent) error {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	st, err := l.state.Load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range st.InFlight {
		if st.InFlight[i].CandidateID == in.CandidateID {
			st.InFlight[i] = in
			replaced = true
			break
		}
	}
	if !replaced {
		st.InFlight = append(st.InFlight, in)
	}
	return l.state.Save(st)
}

// rollbackIntent removes an intent whose mutation was abandoned (a normal
// dispatch failure), so the item is re-dispatched cleanly on the next run.
func (l *Loop) rollbackIntent(id string) error {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	st, err := l.state.Load()
	if err != nil {
		return err
	}
	st.InFlight = removeIntent(st.InFlight, id)
	return l.state.Save(st)
}

// completeIntent atomically moves an intent from the in-flight log to Done:
// the mutation is confirmed applied and the item is complete.
func (l *Loop) completeIntent(id string) error {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	st, err := l.state.Load()
	if err != nil {
		return err
	}
	st.InFlight = removeIntent(st.InFlight, id)
	if !st.Has(id) {
		st.Done = append(st.Done, id)
	}
	return l.state.Save(st)
}

// removeIntent returns in without the entry for id.
func removeIntent(in []Intent, id string) []Intent {
	out := in[:0:0]
	for _, it := range in {
		if it.CandidateID != id {
			out = append(out, it)
		}
	}
	return out
}

// crash invokes the configured crash-injection seam, if any.
func (l *Loop) crash(boundary string) {
	if l.cfg.Crash != nil {
		l.cfg.Crash(boundary)
	}
}

// observe reports one completed step to the observability seam, if any.
func (l *Loop) observe(s Step) {
	if l.cfg.OnStep != nil {
		l.cfg.OnStep(s)
	}
}

// WriteKindNote is the write class of a review outcome. It mirrors an EKA note
// written against the reviewed line.
const WriteKindNote = "note"

// emitStep reports one step to the observer: its outcome, the number of
// dispatch attempts and how long it took. A context cancellation/deadline is
// the deterministic cancel of docs/recovery.md, reported as OutcomeCancelled.
func (l *Loop) emitStep(c decision.Candidate, step Step, attempts int, err error, d time.Duration) {
	if l.cfg.Observer == nil {
		return
	}
	outcome := obs.OutcomeOK
	switch {
	case err == nil:
		// ok
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		outcome = obs.OutcomeCancelled
	default:
		outcome = obs.OutcomeFailed
	}
	l.cfg.Observer.Step(obs.StepEvent{
		CandidateID: c.ID,
		Kind:        step.Kind,
		Outcome:     outcome,
		Attempts:    attempts,
		Duration:    d,
	})
}

// observeWrite reports one applied EKA write to the observer, if any. The loop
// reports the affected line, the write class and the result; the true
// instance version of a publish is produced by internal/ekawrite, not here.
func (l *Loop) observeWrite(line, kind, result string) {
	if l.cfg.Observer != nil {
		l.cfg.Observer.Write(obs.WriteEvent{Line: line, Kind: kind, Result: result})
	}
}

// now returns the loop's clock (the injectable seam, or time.Now).
func (l *Loop) now() time.Time {
	if l.cfg.Now != nil {
		return l.cfg.Now()
	}
	return time.Now()
}

// ready reports whether a candidate is in a dispatchable state. It mirrors
// the decision kernel's unexported dispatchable rule: only the ready states,
// planned and todo.
func ready(c decision.Candidate) bool {
	return c.State == decision.StatePlanned || c.State == decision.StateTodo
}

// stepFor projects a candidate into a Step.
func stepFor(c decision.Candidate) Step {
	return Step{
		CandidateID: c.ID,
		Kind:        KindDispatch,
		Priority:    c.Priority,
		State:       c.State,
		CreatedSeq:  c.CreatedSeq,
	}
}
