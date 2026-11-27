package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
)

// --- fake Writer (models the persistent EKA store across a simulated kill) ---

// fakeWriter is an in-memory stand-in for internal/ekawrite. It models the
// PERSISTENT EKA store: the same instance survives a simulated process kill,
// so a restart observes exactly the writes the crashed run applied. It counts
// DISTINCT applies (a replay of an already-applied write is a no-op), which is
// the exactly-once invariant the checkpoint must preserve.
type fakeWriter struct {
	state     map[string]string // transition target -> current execution state
	seen      map[string]bool   // distinct applied write keys
	applies   int               // distinct applies
	applyLog  []string          // keys in apply order (determinism)
	reconcile int               // Reconcile calls
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{state: map[string]string{}, seen: map[string]bool{}}
}

var fakeRanks = map[string]int{"planned": 0, "todo": 1, "in-progress": 2, "in-review": 3, "done": 4}

// atOrAfter mirrors the write path's forward rank rule for the fake.
func atOrAfter(cur, to string) bool {
	if cur == to {
		return true
	}
	c, okC := fakeRanks[cur]
	t, okT := fakeRanks[to]
	return okC && okT && c >= t
}

func (f *fakeWriter) apply(key, target, to string) {
	if f.seen[key] {
		return
	}
	f.seen[key] = true
	f.applies++
	f.applyLog = append(f.applyLog, key)
	if target != "" {
		f.state[target] = to
	}
}

func (f *fakeWriter) Advance(_ context.Context, c decision.Candidate, to string) error {
	if atOrAfter(f.state[c.ID], to) {
		return nil
	}
	f.apply("t:"+c.ID+"->"+to, c.ID, to)
	return nil
}

func (f *fakeWriter) Reconcile(_ context.Context, in Intent) (ReconcileResult, error) {
	f.reconcile++
	if in.Kind == IntentPublish {
		key := "p:" + in.Digest
		if f.seen[key] {
			return ReconcileResult{Applied: false, Status: "no-op"}, nil
		}
		f.apply(key, "", "")
		return ReconcileResult{Applied: true, Status: "applied"}, nil
	}
	if atOrAfter(f.state[in.Target], in.To) {
		return ReconcileResult{Applied: false, Status: "no-op"}, nil
	}
	f.apply("t:"+in.Target+"->"+in.To, in.Target, in.To)
	return ReconcileResult{Applied: true, Status: "applied"}, nil
}

// countingDispatcher records per-candidate dispatch counts.
type countingDispatcher struct {
	per map[string]int
}

func newCountingDispatcher() *countingDispatcher {
	return &countingDispatcher{per: map[string]int{}}
}

func (d *countingDispatcher) Dispatch(_ context.Context, c decision.Candidate) (DispatchResult, error) {
	d.per[c.ID]++
	return DispatchResult{CandidateID: c.ID}, nil
}

func (d *countingDispatcher) total() int {
	n := 0
	for _, v := range d.per {
		n += v
	}
	return n
}

// crashSignal is the panic payload a crash hook raises to emulate a kill.
type crashSignal struct{ boundary string }

// runAndRecover runs fn, turning an emulated crash into a normal return.
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

// crashLoop wires a Loop over a fake store, writer and dispatcher, with a crash
// hook that fires once at the given boundary.
func crashLoop(src Source, fw Writer, d Dispatcher, store *Store, boundary string, onStep func(Step)) *Loop {
	return New(src, d, fw, store, Config{
		MaxWorkers:    8,
		DispatchState: "todo",
		OnStep:        onStep,
		Crash: func(got string) {
			if got == boundary {
				panic(crashSignal{boundary: got})
			}
		},
	})
}

// doneFixtureIDs is the dispatchable fixture subset (planned/todo), in plan
// order: f, a, b, c, d.
var doneFixtureIDs = []string{"f", "a", "b", "c", "d"}

// TestCrashResumeExactlyOnce is the crash matrix: for each step boundary,
// simulate a kill, restart, and assert exactly-once application, no
// re-dispatch of completed work, and the correct final state.
func TestCrashResumeExactlyOnce(t *testing.T) {
	boundaries := []string{BoundaryAfterIntent, BoundaryAfterDispatch, BoundaryAfterApply}
	for _, b := range boundaries {
		t.Run(b, func(t *testing.T) {
			ctx := context.Background()
			store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
			fw := newFakeWriter()
			dis := newCountingDispatcher()

			crashed := runAndRecover(func() {
				_ = crashLoop(fixtureSource{fixtureCandidates()}, fw, dis, store, b, nil).Run(ctx)
			})
			if crashed != b {
				t.Fatalf("expected a simulated crash at %q, got %q", b, crashed)
			}

			// Restart: a FRESH Loop from the persisted state, no crash hook.
			restarted := New(fixtureSource{fixtureCandidates()}, dis, fw, store, Config{MaxWorkers: 8, DispatchState: "todo"})
			if err := restarted.Run(ctx); err != nil {
				t.Fatalf("resume Run: %v", err)
			}

			// (i) exactly-once application: every fixture candidate applied once.
			perTarget := map[string]int{}
			for _, k := range fw.applyLog {
				perTarget[k]++
			}
			for _, id := range doneFixtureIDs {
				key := "t:" + id + "->todo"
				if perTarget[key] != 1 {
					t.Errorf("[%s] write %s applied %d times, want exactly 1 (log %v)", b, key, perTarget[key], fw.applyLog)
				}
			}
			if fw.applies != len(doneFixtureIDs) {
				t.Errorf("[%s] distinct applies = %d, want %d", b, fw.applies, len(doneFixtureIDs))
			}

			// (ii) no candidate dispatched more than once; completed work is not
			// re-dispatched after resume.
			for id, n := range dis.per {
				if n > 1 {
					t.Errorf("[%s] candidate %s dispatched %d times, want at most 1", b, id, n)
				}
			}

			// (iii) correct final state: all done, no in-flight left, every
			// line at the dispatch state.
			st, err := store.Load()
			if err != nil {
				t.Fatalf("load final state: %v", err)
			}
			if len(st.InFlight) != 0 {
				t.Errorf("[%s] final InFlight = %+v, want empty", b, st.InFlight)
			}
			for _, id := range doneFixtureIDs {
				if !st.Has(id) {
					t.Errorf("[%s] final state missing done id %q", b, id)
				}
				if fw.state[id] != "todo" {
					t.Errorf("[%s] line %s state = %q, want todo", b, id, fw.state[id])
				}
			}
		})
	}
}

// TestResumeStepSequenceMatchesUninterrupted proves resume produces a
// byte-identical step sequence to an uninterrupted run, for every crash
// boundary (determinism; the intent log is sorted, no map iteration leaks).
func TestResumeStepSequenceMatchesUninterrupted(t *testing.T) {
	ctx := context.Background()

	uninterrupted := recordSteps(t, func(l *Loop) error { return l.Run(ctx) }, newFakeWriter())
	want := planJSON(t, uninterrupted)

	for _, b := range []string{BoundaryAfterIntent, BoundaryAfterDispatch, BoundaryAfterApply} {
		t.Run(b, func(t *testing.T) {
			fw := newFakeWriter()
			dis := newCountingDispatcher()
			var got []Step
			collect := func(s Step) { got = append(got, s) }
			store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}

			runAndRecover(func() {
				_ = crashLoop(fixtureSource{fixtureCandidates()}, fw, dis, store, b, collect).Run(ctx)
			})
			restarted := New(fixtureSource{fixtureCandidates()}, dis, fw, store, Config{MaxWorkers: 8, DispatchState: "todo", OnStep: collect})
			if err := restarted.Run(ctx); err != nil {
				t.Fatalf("resume Run: %v", err)
			}
			if string(planJSON(t, got)) != string(want) {
				t.Fatalf("[%s] resumed step sequence differs from the uninterrupted run:\n got:\n%s\n want:\n%s",
					b, planJSON(t, got), want)
			}
		})
	}
}

// recordSteps runs a fresh loop over the fixture and returns the observed steps.
func recordSteps(t *testing.T, run func(*Loop) error, fw *fakeWriter) []Step {
	t.Helper()
	store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
	var steps []Step
	collect := func(s Step) { steps = append(steps, s) }
	cfg := Config{MaxWorkers: 8, DispatchState: "todo", OnStep: collect}
	l := New(fixtureSource{fixtureCandidates()}, newCountingDispatcher(), fw, store, cfg)
	if err := run(l); err != nil {
		t.Fatalf("run: %v", err)
	}
	return steps
}

// TestResumePublishIntentReplayIsNoOp seeds a publish intent twice (simulating a
// crash after apply, before markDone, twice) and asserts the replay is a no-op.
func TestResumePublishIntentReplayIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
	fw := newFakeWriter()
	in := Intent{
		CandidateID: "p/sto:i-1",
		Kind:        IntentPublish,
		Target:      "p/sto:i-1",
		Digest:      "digest-1",
		Publish:     &PublishIntent{Project: "p", Namespace: "p", Type: "sto", ID: "i-1"},
		Step:        Step{CandidateID: "p/sto:i-1", Kind: KindDispatch},
	}

	// First crash-before-markDone: intent in flight, write NOT yet applied.
	if err := store.Save(State{Version: stateVersion, InFlight: []Intent{in}}); err != nil {
		t.Fatal(err)
	}
	l := New(fixtureSource{}, newCountingDispatcher(), fw, store, Config{MaxWorkers: 8})
	if err := l.Resume(ctx); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	if fw.applies != 1 {
		t.Fatalf("first resume applies = %d, want 1", fw.applies)
	}

	// Second crash-before-markDone on the SAME publish: the write already
	// applied, so reconcile must be a no-op (no second application).
	if err := store.Save(State{Version: stateVersion, InFlight: []Intent{in}}); err != nil {
		t.Fatal(err)
	}
	if err := l.Resume(ctx); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if fw.applies != 1 {
		t.Fatalf("second resume applied the publish again: applies = %d, want 1 (replay must be a no-op)", fw.applies)
	}
	if fw.reconcile != 2 {
		t.Fatalf("reconcile calls = %d, want 2", fw.reconcile)
	}
}

// TestStateSaveIsAtomicAndDeterministic asserts Save leaves no partial/temp file
// and is byte-stable across save/load/save.
func TestCheckpointSaveIsAtomicAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	store := &Store{Path: path}
	if err := store.Save(State{Done: []string{"z", "a", "z", "m"}, InFlight: []Intent{{CandidateID: "b"}, {CandidateID: "a"}, {CandidateID: "b"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// No leftover temp file: the write is a temp+rename, never a partial write.
	leftovers, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("Save left temp files behind: %v", leftovers)
	}

	// The committed file is complete, valid, versioned JSON.
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(first, &raw); err != nil {
		t.Fatalf("committed state is not valid JSON (partial write?): %v", err)
	}
	if got := raw["version"]; got != float64(stateVersion) {
		t.Fatalf("version = %v, want %d", got, stateVersion)
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Fatalf("committed state does not end with a newline")
	}

	// Byte-stable across save/load/save, and normalized (sorted, de-duped).
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Join(st.Done, ",") != "a,m,z" {
		t.Fatalf("Done = %v, want sorted+deduped [a m z]", st.Done)
	}
	var ids []string
	for _, it := range st.InFlight {
		ids = append(ids, it.CandidateID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("InFlight = %v, want sorted+deduped [a b]", ids)
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

// TestStateVersionUpgradeAndRefusal pins the version handling: a version-1 file
// upgrades deterministically, an unknown version is refused.
func TestStateVersionUpgradeAndRefusal(t *testing.T) {
	dir := t.TempDir()

	v1 := filepath.Join(dir, "v1.json")
	if err := os.WriteFile(v1, []byte(`{"version":1,"done":["a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := (&Store{Path: v1}).Load()
	if err != nil {
		t.Fatalf("Load v1: %v (a v1 file must upgrade, not fail)", err)
	}
	if st.Version != stateVersion || len(st.Done) != 1 || len(st.InFlight) != 0 {
		t.Fatalf("upgraded v1 state = %+v, want version %d with done [a] and no intent log", st, stateVersion)
	}

	v9 := filepath.Join(dir, "v9.json")
	if err := os.WriteFile(v9, []byte(`{"version":9,"done":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Store{Path: v9}).Load(); err == nil {
		t.Fatalf("Load v9 succeeded, want a deterministic version error")
	}
}

// TestCheckpointPaths pins the documented checkpoint location precedence.
func TestCheckpointPaths(t *testing.T) {
	t.Setenv(StateDirEnv, "")
	t.Setenv("EKA_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := DefaultStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".eka", "steward", CheckpointFile); p != want {
		t.Fatalf("default path = %q, want %q", p, want)
	}

	eka := t.TempDir()
	t.Setenv("EKA_HOME", eka)
	p, _ = DefaultStatePath()
	if want := filepath.Join(eka, "steward", CheckpointFile); p != want {
		t.Fatalf("EKA_HOME path = %q, want %q", p, want)
	}

	over := t.TempDir()
	t.Setenv(StateDirEnv, over)
	p, _ = DefaultStatePath()
	if want := filepath.Join(over, CheckpointFile); p != want {
		t.Fatalf("override path = %q, want %q", p, want)
	}
}
