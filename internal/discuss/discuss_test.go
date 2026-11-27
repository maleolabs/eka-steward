package discuss

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
	"github.com/maleolabs/eka-steward/v2/internal/role"
)

// --- fakes modelling a PERSISTENT EKA store (survives a simulated kill) ---

// fakeStore is the persistent backing of the fake EKA: the same map survives a
// restart, so a fresh Discussion observes exactly the decisions a crashed run
// recorded.
type fakeStore struct {
	subject   Subject
	decisions map[string]Decision
	applies   int
	applyLog  []string
	reconcile int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		subject:   fixtureSubject(),
		decisions: map[string]Decision{},
	}
}

// fakeEKA is the read seam AND the write seam (Recorder) over a fakeStore. A
// fresh wrapper over the same store models a process restart.
type fakeEKA struct{ store *fakeStore }

func (f *fakeEKA) Subject(context.Context, string) (Subject, error) {
	return f.store.subject, nil
}

func (f *fakeEKA) Decisions(context.Context, string) ([]Decision, error) {
	out := make([]Decision, 0, len(f.store.decisions))
	for _, d := range f.store.decisions {
		out = append(out, d)
	}
	sortDecisions(out)
	return out, nil
}

// Reconcile models the write path's deterministic read-then-write replay gate:
// an already-recorded decision id is a no-op, never a second instance.
func (f *fakeEKA) Reconcile(_ context.Context, in loop.Intent) (loop.ReconcileResult, error) {
	f.store.reconcile++
	if in.Kind != loop.IntentPublish || in.Publish == nil {
		return loop.ReconcileResult{}, errNotPublish
	}
	id := in.Publish.ID
	if _, ok := f.store.decisions[id]; ok {
		return loop.ReconcileResult{Applied: false, Status: "no-op"}, nil
	}
	c := in.Publish.Content
	d := Decision{
		ID:           id,
		Round:        int(in.Step.CreatedSeq),
		Context:      asString(c["context"]),
		Decision:     asString(c["decision"]),
		Consequences: asString(c["consequences"]),
	}
	f.store.decisions[id] = d
	f.store.applies++
	f.store.applyLog = append(f.store.applyLog, id)
	return loop.ReconcileResult{Applied: true, Status: "applied"}, nil
}

var errNotPublish = &fakeError{"fakeEKA: expected a publish intent"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// fakeDispatcher records the rendered context it received and returns a
// deterministic output keyed by the candidate id.
type fakeDispatcher struct {
	calls    int
	payloads [][]byte
	out      func(decision.Candidate) string
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, c decision.Candidate) (loop.DispatchResult, error) {
	return f.DispatchContext(ctx, c, nil)
}

func (f *fakeDispatcher) DispatchContext(_ context.Context, c decision.Candidate, payload []byte) (loop.DispatchResult, error) {
	f.calls++
	f.payloads = append(f.payloads, append([]byte(nil), payload...))
	out := "decision " + c.ID
	if f.out != nil {
		out = f.out(c)
	}
	return loop.DispatchResult{CandidateID: c.ID, Output: out}, nil
}

func fixtureSubject() Subject {
	return Subject{
		Line:      "p/sto:i-1",
		Namespace: "p",
		Type:      "sto",
		ID:        "i-1",
		Title:     "context renderer",
		Brief:     "Render a bounded, deterministic context payload for the worker.",
	}
}

// newCheckpoint returns a fresh loop checkpoint store.
func newCheckpoint(t *testing.T) *loop.Store {
	t.Helper()
	return &loop.Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
}

// newDiscussion wires a Discussion over the fakes with a test config.
func newDiscussion(fk *fakeEKA, d *fakeDispatcher, store *loop.Store, cfg Config) *Discussion {
	cfg.Project = "p"
	cfg.Subject = "p/sto:i-1"
	return New(fk, d, fk, store, cfg)
}

// --- crash helpers (the same pattern as internal/loop) ---

type crashSignal struct{ boundary string }

func runAndRecover(fn func()) (crashed string) {
	defer func() {
		if r := recover(); r != nil {
			if cs, ok := r.(crashSignal); ok {
				crashed = cs.boundary
				return
			}
			panic(r)
		}
	}()
	fn()
	return ""
}

func crashAt(boundary string) loop.CrashPoint {
	return func(got string) {
		if got == boundary {
			panic(crashSignal{boundary: got})
		}
	}
}

func roundJSON(t *testing.T, rs []Round) []byte {
	t.Helper()
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		t.Fatalf("marshal rounds: %v", err)
	}
	return append(b, '\n')
}

func decisionsJSON(t *testing.T, store *fakeStore) []byte {
	t.Helper()
	ds := make([]Decision, 0, len(store.decisions))
	for _, d := range store.decisions {
		ds = append(ds, d)
	}
	sortDecisions(ds)
	b, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	return append(b, '\n')
}

// TestFullDiscussionTurn records decisions and derives the next-phase input
// from the store.
func TestFullDiscussionTurn(t *testing.T) {
	ctx := context.Background()
	fk := &fakeEKA{store: newFakeStore()}
	dis := &fakeDispatcher{}
	store := newCheckpoint(t)

	var rounds []Round
	d := newDiscussion(fk, dis, store, Config{
		Rounds:   2,
		MaxBytes: 1024,
		OnRound:  func(r Round) { rounds = append(rounds, r) },
	})
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Decision 1: the session recorded one dec- decision per round.
	if len(fk.store.decisions) != 2 {
		t.Fatalf("recorded decisions = %d, want 2", len(fk.store.decisions))
	}
	if fk.store.applies != 2 {
		t.Fatalf("distinct applies = %d, want 2", fk.store.applies)
	}
	if len(rounds) != 2 || rounds[0].Index != 1 || rounds[1].Index != 2 {
		t.Fatalf("observed rounds = %+v, want rounds 1 and 2", rounds)
	}
	if dis.calls != 2 {
		t.Fatalf("dispatch calls = %d, want 2", dis.calls)
	}

	// The worker received a non-empty, byte-bounded context that names the
	// subject.
	for i, p := range dis.payloads {
		if len(p) == 0 || len(p) > 1024 {
			t.Fatalf("round %d context payload = %d bytes, want 1..1024", i+1, len(p))
		}
		if !strings.Contains(string(p), "p/sto:i-1") {
			t.Fatalf("round %d context does not name the subject:\n%s", i+1, p)
		}
	}

	// Decision 3: the next-phase input is built from the store only, and
	// contains the recorded decisions.
	exec, err := BuildExecuteInput(ctx, fk, "p/sto:i-1", 4096)
	if err != nil {
		t.Fatalf("BuildExecuteInput: %v", err)
	}
	for _, want := range []string{"decision i-1-dec-r01", "decision i-1-dec-r02"} {
		if !strings.Contains(string(exec), want) {
			t.Fatalf("next-phase input missing recorded decision %q:\n%s", want, exec)
		}
	}
}

// TestNextPhaseInputSurvivesRestart proves the execute input is reconstructed
// from the store: a fresh read seam over the same persistent store, with no
// Discussion value in scope, still yields the recorded decisions.
func TestNextPhaseInputSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	backing := newFakeStore()
	store := newCheckpoint(t)
	if err := newDiscussion(&fakeEKA{store: backing}, &fakeDispatcher{}, store, Config{Rounds: 3}).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Simulated restart: a brand-new wrapper over the same persisted store,
	// and no Discussion instance.
	fresh := &fakeEKA{store: backing}
	exec, err := BuildExecuteInput(ctx, fresh, "p/sto:i-1", 8192)
	if err != nil {
		t.Fatalf("BuildExecuteInput: %v", err)
	}
	for _, want := range []string{"decision i-1-dec-r01", "decision i-1-dec-r02", "decision i-1-dec-r03"} {
		if !strings.Contains(string(exec), want) {
			t.Fatalf("restart-built execute input missing %q:\n%s", want, exec)
		}
	}
}

// TestResumeCrashMatrix kills the discussion at each boundary, restarts, and
// asserts exactly-once recording and a deterministic round sequence.
func TestResumeCrashMatrix(t *testing.T) {
	ctx := context.Background()

	// Uninterrupted reference sequence.
	refBacking := newFakeStore()
	var want []Round
	if err := newDiscussion(&fakeEKA{store: refBacking}, &fakeDispatcher{}, newCheckpoint(t), Config{
		Rounds:  3,
		OnRound: func(r Round) { want = append(want, r) },
	}).Run(ctx); err != nil {
		t.Fatalf("reference Run: %v", err)
	}

	for _, b := range []string{loop.BoundaryAfterIntent, loop.BoundaryAfterDispatch, loop.BoundaryAfterApply} {
		t.Run(b, func(t *testing.T) {
			backing := newFakeStore()
			dis := &fakeDispatcher{}
			store := newCheckpoint(t)
			var got []Round
			collect := func(r Round) { got = append(got, r) }

			crashed := runAndRecover(func() {
				_ = newDiscussion(&fakeEKA{store: backing}, dis, store, Config{
					Rounds:  3,
					Crash:   crashAt(b),
					OnRound: collect,
				}).Run(ctx)
			})
			if crashed != b {
				t.Fatalf("expected a simulated crash at %q, got %q", b, crashed)
			}

			// Restart: a FRESH Discussion over the persisted store/state.
			restarted := newDiscussion(&fakeEKA{store: backing}, dis, store, Config{
				Rounds:  3,
				OnRound: collect,
			})
			if err := restarted.Run(ctx); err != nil {
				t.Fatalf("resume Run: %v", err)
			}

			// Exactly once: one decision per round, one apply per round.
			if len(backing.decisions) != 3 {
				t.Fatalf("recorded decisions = %d, want 3 (no duplicates)", len(backing.decisions))
			}
			if backing.applies != 3 {
				t.Fatalf("distinct applies = %d, want 3", backing.applies)
			}
			// No in-flight intent left.
			st, err := store.Load()
			if err != nil {
				t.Fatalf("load state: %v", err)
			}
			if len(st.InFlight) != 0 {
				t.Fatalf("final InFlight = %+v, want empty", st.InFlight)
			}
			// Deterministic round sequence.
			if string(roundJSON(t, got)) != string(roundJSON(t, want)) {
				t.Fatalf("[%s] resumed round sequence differs:\n got:\n%s\n want:\n%s", b, roundJSON(t, got), roundJSON(t, want))
			}
		})
	}
}

// TestDeterminismAcrossRuns requires byte-identical recorded decisions and
// next-phase input across >= 20 identical runs.
func TestDeterminismAcrossRuns(t *testing.T) {
	ctx := context.Background()
	const n = 20
	var firstDec, firstExec []byte
	for i := 0; i < n; i++ {
		backing := newFakeStore()
		if err := newDiscussion(&fakeEKA{store: backing}, &fakeDispatcher{}, newCheckpoint(t), Config{
			Rounds:   3,
			MaxBytes: 2048,
		}).Run(ctx); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		dec := decisionsJSON(t, backing)
		exec, err := BuildExecuteInput(ctx, &fakeEKA{store: backing}, "p/sto:i-1", 4096)
		if err != nil {
			t.Fatalf("run %d: BuildExecuteInput: %v", i, err)
		}
		if i == 0 {
			firstDec, firstExec = dec, exec
			continue
		}
		if string(dec) != string(firstDec) {
			t.Fatalf("run %d recorded decisions differ from run 0:\n%s\nvs\n%s", i, dec, firstDec)
		}
		if string(exec) != string(firstExec) {
			t.Fatalf("run %d execute input differs from run 0:\n%s\nvs\n%s", i, exec, firstExec)
		}
	}
}

// TestRoundsAreBoundedAndStopDeterministic pins the code-owned round count and
// its clamp.
func TestRoundsAreBoundedAndStopDeterministic(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ in, want int }{
		{0, 1},
		{-7, 1},
		{3, 3},
		{MaxRounds + 10, MaxRounds},
	}
	for _, c := range cases {
		backing := newFakeStore()
		dis := &fakeDispatcher{}
		d := newDiscussion(&fakeEKA{store: backing}, dis, newCheckpoint(t), Config{Rounds: c.in})
		if got := d.Rounds(); got != c.want {
			t.Fatalf("Rounds(%d) = %d, want %d", c.in, got, c.want)
		}
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run(%d): %v", c.in, err)
		}
		if dis.calls != c.want {
			t.Fatalf("Rounds(%d): dispatch calls = %d, want %d", c.in, dis.calls, c.want)
		}
		if len(backing.decisions) != c.want {
			t.Fatalf("Rounds(%d): recorded = %d, want %d", c.in, len(backing.decisions), c.want)
		}
	}
}

// TestUnknownRoleIsDeterministicError pins that a configured role set rejects
// an unknown role before any dispatch (code-owned role selection).
func TestUnknownRoleIsDeterministicError(t *testing.T) {
	set, err := role.Parse([]byte("roles:\n  - name: author\n    driver: opencode\n    template: t\n  - name: reviewer\n    driver: opencode\n    template: t\n    review: true\n"))
	if err != nil {
		t.Fatalf("role.Parse: %v", err)
	}
	backing := newFakeStore()
	dis := &fakeDispatcher{}
	d := newDiscussion(&fakeEKA{store: backing}, dis, newCheckpoint(t), Config{
		Rounds: 1,
		Role:   "ghost",
		Roles:  set,
	})
	err = d.Run(context.Background())
	if err == nil {
		t.Fatal("Run with an unknown role succeeded, want a deterministic error")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("error %q does not name the unknown role", err)
	}
	if dis.calls != 0 {
		t.Fatalf("dispatch calls = %d, want 0 (role must resolve before dispatch)", dis.calls)
	}
}

// TestSingleStepStopsWhenAllRoundsRecorded pins Step's bounded behaviour.
func TestSingleStepStopsWhenAllRoundsRecorded(t *testing.T) {
	ctx := context.Background()
	backing := newFakeStore()
	d := newDiscussion(&fakeEKA{store: backing}, &fakeDispatcher{}, newCheckpoint(t), Config{Rounds: 2})
	for i := 0; i < 2; i++ {
		if _, ok, err := d.Step(ctx); err != nil || !ok {
			t.Fatalf("Step %d = ok %v, err %v; want a round", i, ok, err)
		}
	}
	if _, ok, err := d.Step(ctx); err != nil || ok {
		t.Fatalf("Step past the last round = ok %v, err %v; want (false, nil)", ok, err)
	}
}
