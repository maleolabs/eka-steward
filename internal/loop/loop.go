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
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
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
// one Advance, whose production implementation routes through ekawrite.
type Writer interface {
	// Advance records the loop's decision for one dispatched candidate by
	// moving it to the state `to`. It is the only mutation the loop performs.
	Advance(ctx context.Context, c decision.Candidate, to string) error
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
	// Writer. Empty disables the write (plan/dispatch only). The loop does
	// not re-derive transition legality; the candidate must be in a state
	// from which the engine permits `to` (docs/control-loop.md).
	DispatchState string
}

// Loop is the deterministic control loop. Construct it with New; it is not
// safe for concurrent use (the run is sequential by design — concurrency and
// backpressure are a separate work item).
type Loop struct {
	source     Source
	dispatcher Dispatcher
	writer     Writer
	state      *Store
	cfg        Config
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
// when nothing is dispatchable. On a successful dispatch it records the work
// as done in the state store so the next Step/Run resumes past it.
func (l *Loop) Step(ctx context.Context) (Step, bool, error) {
	cs, err := l.selectable(ctx)
	if err != nil {
		return Step{}, false, err
	}
	c, ok := decision.SelectNext(cs)
	if !ok {
		return Step{}, false, nil
	}
	step := stepFor(c)

	if _, err := l.dispatcher.Dispatch(ctx, c); err != nil {
		return step, true, fmt.Errorf("loop: dispatch %s: %w", c.ID, err)
	}
	if l.writer != nil && l.cfg.DispatchState != "" {
		if err := l.writer.Advance(ctx, c, l.cfg.DispatchState); err != nil {
			return step, true, fmt.Errorf("loop: record %s: %w", c.ID, err)
		}
	}
	if err := l.markDone(c.ID); err != nil {
		return step, true, err
	}
	return step, true, nil
}

// Run drives Step until nothing is dispatchable or ctx is cancelled, then
// returns nil (nothing left) or the cancellation error. The sequence is
// deterministic for a fixed Source/Dispatcher/state.
func (l *Loop) Run(ctx context.Context) error {
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
