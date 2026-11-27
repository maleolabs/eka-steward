package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/plan"
)

// fixtureCandidates is the deterministic fixture candidate set. It deliberately
// contains ties at every tie-break level so the golden sequence proves the
// pinned total order (priority desc, state rank asc, created-seq asc, ID asc):
//
//   - "b" and "c": equal priority, state and created-seq → ID breaks the tie.
//   - "a", "b", "c": equal priority and state, differing created-seq → FIFO.
//   - "f" vs "a","b","c": equal priority, planned ranks before todo.
//   - "e" (in-progress) and "g" (done) are NOT dispatchable and must never
//     appear in a step sequence.
func fixtureCandidates() []decision.Candidate {
	return []decision.Candidate{
		{ID: "a", Priority: 10, State: "todo", CreatedSeq: 1},
		{ID: "b", Priority: 10, State: "todo", CreatedSeq: 2},
		{ID: "c", Priority: 10, State: "todo", CreatedSeq: 2},
		{ID: "d", Priority: 5, State: "planned", CreatedSeq: 3},
		{ID: "e", Priority: 20, State: "in-progress", CreatedSeq: 0},
		{ID: "f", Priority: 10, State: "planned", CreatedSeq: 5},
		{ID: "g", Priority: 1, State: "done", CreatedSeq: 1},
	}
}

// fixtureSource is a fixed Source.
type fixtureSource struct{ cs []decision.Candidate }

func (f fixtureSource) Candidates(context.Context) ([]decision.Candidate, error) {
	return f.cs, nil
}

// spyDispatcher records every dispatch and fails loudly if it is ever called
// while the loop is planning (the determinism guarantee: Plan never touches
// the worker/LLM seam).
type spyDispatcher struct {
	calls      int
	mustNotRun bool
}

func (s *spyDispatcher) Dispatch(_ context.Context, c decision.Candidate) (DispatchResult, error) {
	if s.mustNotRun {
		panic("dispatcher called during planning — Plan must never touch the LLM seam")
	}
	s.calls++
	return DispatchResult{CandidateID: c.ID}, nil
}

// newTestLoop wires a Loop over the fixture, a fresh state file, and the given
// dispatcher/writer.
func newTestLoop(t *testing.T, src Source, d Dispatcher, w Writer) (*Loop, *Store) {
	t.Helper()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "state.json")}
	return New(src, d, w, store, Config{MaxWorkers: 8}), store
}

// planJSON marshals a step sequence exactly as the golden file stores it.
func planJSON(t *testing.T, steps []Step) []byte {
	t.Helper()
	b, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	return append(b, '\n')
}

// TestPlanGoldenSequence freezes the exact step sequence for the fixture and
// asserts the committed golden file matches byte for byte.
func TestPlanGoldenSequence(t *testing.T) {
	l, _ := newTestLoop(t, fixtureSource{fixtureCandidates()}, &spyDispatcher{}, nil)
	steps, err := l.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	got := planJSON(t, steps)
	want, err := os.ReadFile(filepath.Join("testdata", "steps.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("step sequence drifted from the golden:\n got:\n%s\n want:\n%s", got, want)
	}
}

// TestPlanIsByteIdenticalAcrossRuns runs Plan N times over the SAME source and
// requires byte-identical steps every time.
func TestPlanIsByteIdenticalAcrossRuns(t *testing.T) {
	const n = 20
	l, _ := newTestLoop(t, fixtureSource{fixtureCandidates()}, &spyDispatcher{}, nil)
	var first []byte
	for i := 0; i < n; i++ {
		steps, err := l.Plan(context.Background())
		if err != nil {
			t.Fatalf("Plan run %d: %v", i, err)
		}
		got := planJSON(t, steps)
		if i == 0 {
			first = got
			continue
		}
		if string(got) != string(first) {
			t.Fatalf("Plan run %d differs from run 0:\n%s\nvs\n%s", i, got, first)
		}
	}
}

// TestPlanIsIndependentOfInputOrder proves the order comes from
// internal/decision, not from the source slice order (no map iteration leak).
func TestPlanIsIndependentOfInputOrder(t *testing.T) {
	base := fixtureCandidates()
	l0, _ := newTestLoop(t, fixtureSource{base}, &spyDispatcher{}, nil)
	want, err := l0.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantJSON := planJSON(t, want)

	// Reverse and rotate the fixture; every permutation must plan identically.
	permutations := [][]decision.Candidate{
		reverse(base),
		append(append([]decision.Candidate{}, base[3:]...), base[:3]...),
	}
	for i, cs := range permutations {
		l, _ := newTestLoop(t, fixtureSource{cs}, &spyDispatcher{}, nil)
		got, err := l.Plan(context.Background())
		if err != nil {
			t.Fatalf("Plan permutation %d: %v", i, err)
		}
		if string(planJSON(t, got)) != string(wantJSON) {
			t.Fatalf("permutation %d changed the planned sequence:\n%s\nwant:\n%s", i, planJSON(t, got), wantJSON)
		}
	}
}

// TestPlanNeverCallsDispatcher is the LLM-call spy test: Plan must record zero
// dispatches, and a dispatch during planning panics.
func TestPlanNeverCallsDispatcher(t *testing.T) {
	spy := &spyDispatcher{mustNotRun: true}
	l, _ := newTestLoop(t, fixtureSource{fixtureCandidates()}, spy, nil)
	for i := 0; i < 5; i++ {
		if _, err := l.Plan(context.Background()); err != nil {
			t.Fatalf("Plan: %v", err)
		}
	}
	if spy.calls != 0 {
		t.Fatalf("Plan called the dispatcher %d times, want 0", spy.calls)
	}
}

// TestStepDispatchesInPlanOrder drives Step until exhausted and asserts the
// emitted sequence equals Plan's, one dispatch per step, then stops.
func TestStepDispatchesInPlanOrder(t *testing.T) {
	ctx := context.Background()
	spy := &spyDispatcher{}
	l, store := newTestLoop(t, fixtureSource{fixtureCandidates()}, spy, nil)

	planned, err := l.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var emitted []Step
	for {
		step, ok, err := l.Step(ctx)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if !ok {
			break
		}
		emitted = append(emitted, step)
		if len(emitted) > len(planned) {
			t.Fatalf("Step emitted more steps than Plan planned")
		}
	}
	if string(planJSON(t, emitted)) != string(planJSON(t, planned)) {
		t.Fatalf("Step sequence != Plan sequence:\n%s\nvs\n%s", planJSON(t, emitted), planJSON(t, planned))
	}
	if spy.calls != len(planned) {
		t.Fatalf("dispatch calls = %d, want %d", spy.calls, len(planned))
	}
	// Every planned id is now recorded done.
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load state: %v", err)
	}
	for _, s := range planned {
		if !st.Has(s.CandidateID) {
			t.Errorf("state missing completed id %q", s.CandidateID)
		}
	}
}

// TestResumeSkipsCompleted writes state, reloads it and asserts completed work
// is skipped deterministically.
func TestResumeSkipsCompleted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "state.json")}
	if err := store.Save(State{Version: 1, Done: []string{"f"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	l := New(fixtureSource{fixtureCandidates()}, &spyDispatcher{}, nil, store, Config{MaxWorkers: 8})
	steps, err := l.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, s := range steps {
		if s.CandidateID == "f" {
			t.Fatalf("resumed Plan still contains completed id %q", s.CandidateID)
		}
	}
	// The head of the resumed plan is the next item in order ("a").
	if len(steps) == 0 || steps[0].CandidateID != "a" {
		t.Fatalf("resumed plan head = %+v, want candidate a", steps)
	}
}

// TestConfigDefaultStateIsFresh asserts a missing state file loads as a fresh,
// versioned, empty state.
func TestConfigDefaultStateIsFresh(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "absent.json")}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Version != stateVersion || len(st.Done) != 0 {
		t.Fatalf("fresh state = %+v, want version %d with empty Done", st, stateVersion)
	}
}

// TestStateSaveIsAtomicAndDeterministic asserts Save sorts/dedups and reloads
// identically.
func TestStateSaveIsAtomicAndDeterministic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := &Store{Path: path}
	if err := store.Save(State{Version: 1, Done: []string{"z", "a", "z", "m"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Join(st.Done, ",") != "a,m,z" {
		t.Fatalf("Done = %v, want sorted+deduped [a m z]", st.Done)
	}
	if err := store.Save(st); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read state: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("state file is not byte-stable across save/load/save")
	}
}

func reverse(cs []decision.Candidate) []decision.Candidate {
	out := make([]decision.Candidate, len(cs))
	for i, c := range cs {
		out[len(cs)-1-i] = c
	}
	return out
}

// graphSource is a fixed GraphSource: it implements Source.Candidates (so it
// can be wired into the loop) and the optional Graph capability, so the loop
// plans over its dependency graph.
type graphSource struct {
	nodes []plan.Node
	edges []plan.Edge
}

func (g graphSource) Candidates(context.Context) ([]decision.Candidate, error) {
	out := make([]decision.Candidate, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, decision.Candidate{ID: n.ID, State: n.State, Priority: n.Priority, CreatedSeq: n.CreatedSeq})
	}
	return out, nil
}

func (g graphSource) Graph(context.Context) ([]plan.Node, []plan.Edge, error) {
	return g.nodes, g.edges, nil
}

// TestGraphPlanOrdersTopologicallyAndSkipsBlocked proves the loop plans over
// the Source's graph: a dependency edge orders the prerequisite first, and a
// unit whose prerequisite is not terminal is blocked and never planned or
// selected. The plan stays pure (the dispatcher is never consulted).
func TestGraphPlanOrdersTopologicallyAndSkipsBlocked(t *testing.T) {
	src := graphSource{
		nodes: []plan.Node{
			{ID: "A", State: decision.StatePlanned, CreatedSeq: 1},
			{ID: "B", State: decision.StateTodo, CreatedSeq: 2},    // depends-on A (not terminal)
			{ID: "C", State: decision.StatePlanned, CreatedSeq: 3}, // independent
			{ID: "D", State: decision.StateTodo, CreatedSeq: 4},    // depends-on C (not terminal)
		},
		edges: []plan.Edge{{From: "A", To: "B"}, {From: "C", To: "D"}},
	}
	spy := &spyDispatcher{mustNotRun: true}
	l, _ := newTestLoop(t, src, spy, nil)

	steps, err := l.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// B and D are blocked (their prerequisites A, C are still planned), so
	// only A and C are planned, in the deterministic order (A before C).
	if got := stepIDs(steps); got != "A,C" {
		t.Fatalf("plan ids = %q, want %q (blocked B,D must never be planned)", got, "A,C")
	}
	if spy.calls != 0 {
		t.Fatalf("Plan called the dispatcher %d times, want 0", spy.calls)
	}

	// Step also refuses the blocked nodes: it dispatches A first.
	spy.mustNotRun = false
	step, ok, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !ok || step.CandidateID != "A" {
		t.Fatalf("Step = (%q,%v), want (A,true)", step.CandidateID, ok)
	}
}

// TestGraphPlanUnblocksWhenPrerequisiteTerminal proves a unit is planned once
// its prerequisite reaches a terminal state.
func TestGraphPlanUnblocksWhenPrerequisiteTerminal(t *testing.T) {
	src := graphSource{
		nodes: []plan.Node{
			{ID: "A", State: decision.StateDone, CreatedSeq: 1}, // terminal
			{ID: "B", State: decision.StateTodo, CreatedSeq: 2}, // depends-on A (terminal)
		},
		edges: []plan.Edge{{From: "A", To: "B"}},
	}
	l, _ := newTestLoop(t, src, &spyDispatcher{}, nil)
	steps, err := l.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stepIDs(steps); got != "B" {
		t.Fatalf("plan ids = %q, want %q", got, "B")
	}
}

// stepIDs joins the candidate IDs of a step sequence.
func stepIDs(steps []Step) string {
	ids := make([]string, len(steps))
	for i, s := range steps {
		ids[i] = s.CandidateID
	}
	return strings.Join(ids, ",")
}
