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

	"github.com/maleolabs/eka-steward/v2/internal/decision"
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
	// Metrics is the pool saturation seam (internal/pool.Metrics). nil uses a
	// no-op default. It is only consulted on the pooled path.
	Metrics pool.Metrics
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
	step, err := l.stepOne(ctx, c)
	if err != nil {
		return step, true, err
	}
	return step, true, nil
}

// stepOne runs the checkpointed transaction for one already-selected
// candidate: intent → dispatch → apply → done. Step selects the candidate (in
// the sequential path) and the pool workers call it directly (in the pooled
// path). It serializes every Store mutation (stateMu) and every Writer
// mutation (writeMu) but never the dispatch, so workers overlap on the LLM
// seam while the checkpoint stays consistent.
func (l *Loop) stepOne(ctx context.Context, c decision.Candidate) (Step, error) {
	step := stepFor(c)

	// No write path configured: the run is plan/dispatch only and carries no
	// checkpoint, so there is nothing to reconcile on resume.
	if l.writer == nil || l.cfg.DispatchState == "" {
		if _, err := l.dispatcher.Dispatch(ctx, c); err != nil {
			return step, fmt.Errorf("loop: dispatch %s: %w", c.ID, err)
		}
		if err := l.markDone(c.ID); err != nil {
			return step, err
		}
		l.observe(step)
		return step, nil
	}

	in := Intent{
		CandidateID: c.ID,
		Kind:        IntentTransition,
		Target:      c.ID,
		To:          l.cfg.DispatchState,
		Step:        step,
	}
	// Write-ahead: persist the intent BEFORE the worker runs.
	if err := l.beginIntent(in); err != nil {
		return step, err
	}
	l.crash(BoundaryAfterIntent)

	if _, err := l.dispatcher.Dispatch(ctx, c); err != nil {
		// A normal dispatch failure (not a crash) is rolled back so the item
		// is re-dispatched on the next run; only a crash leaves the intent for
		// resume to reconcile.
		_ = l.rollbackIntent(c.ID)
		return step, fmt.Errorf("loop: dispatch %s: %w", c.ID, err)
	}
	l.crash(BoundaryAfterDispatch)

	if err := l.advance(ctx, c); err != nil {
		return step, fmt.Errorf("loop: record %s: %w", c.ID, err)
	}
	l.crash(BoundaryAfterApply)

	if err := l.completeIntent(c.ID); err != nil {
		return step, err
	}
	l.observe(step)
	return step, nil
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
		if _, err := l.writer.Reconcile(ctx, in); err != nil {
			return fmt.Errorf("loop: reconcile %s: %w", in.CandidateID, err)
		}
		if err := l.completeIntent(in.CandidateID); err != nil {
			return err
		}
		l.observe(in.Step)
	}
	return nil
}

// Run reconciles any in-flight intent (resume) and then drives the loop until
// nothing is dispatchable or ctx is cancelled, returning nil (nothing left) or
// the cancellation error.
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
	if err := l.Resume(ctx); err != nil {
		return err
	}
	if l.cfg.QueueLimit > 0 {
		return l.runPooled(ctx)
	}
	return l.runSequential(ctx)
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
			_, err := l.stepOne(ctx, c)
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
