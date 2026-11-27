package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// These tests pin the checkpoint migration registry: a checkpoint written by
// release N is migrated/readable by release N+1 (state preserved), an
// unmigratable version fails with a typed, deterministic refusal (never silent
// corruption), and an in-flight intent logged before the version bump is
// reconciled by the current release's Resume. See docs/upgrade.md.

// writeStateFile marshals st and writes it to a fresh checkpoint file, returning
// its path.
func writeStateFile(t *testing.T, st State) string {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	p := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}
	return p
}

// TestCheckpointV1MigratesToCurrent proves a version-1 checkpoint loads and
// migrates to the current version with its state preserved exactly, and that
// the migration is byte-stable across load/save/load.
func TestCheckpointV1MigratesToCurrent(t *testing.T) {
	p := writeStateFile(t, State{Version: 1, Done: []string{"a", "b"}, Cursor: "c-1"})

	st, err := (&Store{Path: p}).Load()
	if err != nil {
		t.Fatalf("Load v1: %v", err)
	}
	want := State{Version: stateVersion, Done: []string{"a", "b"}, Cursor: "c-1"}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("migrated v1 = %+v, want %+v", st, want)
	}

	// Deterministic: a second load is deeply equal.
	again, err := (&Store{Path: p}).Load()
	if err != nil {
		t.Fatalf("second Load v1: %v", err)
	}
	if !reflect.DeepEqual(st, again) {
		t.Fatalf("Load is nondeterministic:\n%+v\n%+v", st, again)
	}

	// Byte-stable: save the migrated state, load it back and save again — the
	// committed bytes are identical (normalized, no map iteration).
	store := &Store{Path: p}
	if err := store.Save(st); err != nil {
		t.Fatalf("Save migrated: %v", err)
	}
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := store.Save(reloaded); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	second, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("migrated checkpoint not byte-stable across save/load/save")
	}
}

// TestCheckpointUnmigratableRefused proves every version the registry cannot
// reach is refused with the typed ErrCheckpointUnmigratable, naming the version
// and the supported range, deterministically across runs — never silent
// corruption.
func TestCheckpointUnmigratableRefused(t *testing.T) {
	cases := []struct {
		name    string
		version int
		want    string
	}{
		{"older than the registry floor", 0, "older than the oldest supported"},
		{"negative version", -1, "older than the oldest supported"},
		{"future version", stateVersion + 1, "newer than this steward supports"},
		{"far-future version", 99, "newer than this steward supports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeStateFile(t, State{Version: tc.version, Done: []string{"a"}})

			_, err := (&Store{Path: p}).Load()
			if err == nil {
				t.Fatalf("Load(version %d) succeeded, want a refusal", tc.version)
			}
			if !errors.Is(err, ErrCheckpointUnmigratable) {
				t.Fatalf("error %v is not ErrCheckpointUnmigratable", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.version)) {
				t.Fatalf("error %q does not name the offending version %d", err.Error(), tc.version)
			}

			// Deterministic across runs.
			_, err2 := (&Store{Path: p}).Load()
			if err2 == nil || err2.Error() != err.Error() {
				t.Fatalf("refusal not deterministic: %q vs %q", err, err2)
			}
		})
	}
}

// TestCheckpointInFlightSurvivesMigrationAndResumes proves the migration
// registry never drops the in-flight intent log, and that an intent logged
// before the version bump is reconciled (exactly once, then cleared) by the
// current release's Resume.
func TestCheckpointInFlightSurvivesMigrationAndResumes(t *testing.T) {
	in := Intent{
		CandidateID: "a",
		Kind:        IntentTransition,
		Target:      "a",
		To:          "todo",
		Step:        Step{CandidateID: "a", Kind: KindDispatch},
	}
	// A v1 checkpoint that carried an intent: a real v1 logged no intents (it
	// had no intent log), so this is the strongest form of the no-drop check —
	// whatever the loader decodes, the walk must preserve it untouched.
	p := writeStateFile(t, State{Version: 1, Done: []string{"z"}, InFlight: []Intent{in}})

	store := &Store{Path: p}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load v1 with intent: %v", err)
	}
	if st.Version != stateVersion {
		t.Fatalf("version = %d, want %d", st.Version, stateVersion)
	}
	if len(st.InFlight) != 1 || !reflect.DeepEqual(st.InFlight[0], in) {
		t.Fatalf("migration dropped/altered the in-flight intent: %+v, want %+v", st.InFlight, in)
	}
	if !st.Has("z") {
		t.Fatalf("migration dropped Done: %+v", st.Done)
	}

	// The pre-upgrade intent is reconciled by the current release's Resume:
	// applied exactly once, cleared, and the candidate marked done.
	fw := newFakeWriter()
	l := New(fixtureSource{}, newCountingDispatcher(), fw, store, Config{MaxWorkers: 8, DispatchState: "todo"})
	if err := l.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after migration: %v", err)
	}
	if fw.applies != 1 || fw.state["a"] != "todo" {
		t.Fatalf("post-migration resume: applies=%d state=%v, want 1 apply to todo", fw.applies, fw.state)
	}
	final, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.InFlight) != 0 || !final.Has("a") {
		t.Fatalf("final state = %+v, want done and no in-flight", final)
	}
}

// TestMigrateStateRegistryWalk proves the generic ordered-walk shape: an
// N -> N+1 -> N+2 chain applies every step in order, and a gap (a version with
// no registry step) is refused before any step past the gap runs.
func TestMigrateStateRegistryWalk(t *testing.T) {
	var applied []int
	step := func(n int) func(*State) { return func(*State) { applied = append(applied, n) } }
	regs := []migration{
		{From: 1, To: 2, Apply: step(1)},
		{From: 2, To: 3, Apply: step(2)},
	}

	st := State{Version: 1, Done: []string{"a"}}
	if err := migrateState(&st, 3, regs); err != nil {
		t.Fatalf("migrateState 1->3: %v", err)
	}
	if st.Version != 3 || !reflect.DeepEqual(applied, []int{1, 2}) {
		t.Fatalf("walk = version %d, applied %v; want version 3, applied [1 2]", st.Version, applied)
	}

	// A real version between the floor and the target but with no step is a gap.
	var gapped []int
	stepG := func(n int) func(*State) { return func(*State) { gapped = append(gapped, n) } }
	regsGap := []migration{
		{From: 1, To: 2, Apply: stepG(1)},
		{From: 3, To: 4, Apply: stepG(3)},
	}
	st2 := State{Version: 1, Done: []string{"a"}}
	err := migrateState(&st2, 4, regsGap)
	if !errors.Is(err, ErrCheckpointUnmigratable) || !strings.Contains(err.Error(), "no migration path") {
		t.Fatalf("gap walk err = %v, want a no-migration-path refusal", err)
	}
	// The walk is two-pass: a gap refuses BEFORE any step runs, so nothing was
	// applied and the state is untouched (never a partial migration).
	if len(gapped) != 0 || st2.Version != 1 || !reflect.DeepEqual(st2.Done, []string{"a"}) {
		t.Fatalf("gap refusal partially migrated: version %d, applied %v, done %v", st2.Version, gapped, st2.Done)
	}
}

// TestCheckpointDowngradeRefused pins the mechanism behind the rollback note: a
// checkpoint written by a NEWER steward (version > stateVersion) is refused
// deterministically, so an older steward never silently reads or downgrades a
// newer checkpoint.
func TestCheckpointDowngradeRefused(t *testing.T) {
	p := writeStateFile(t, State{Version: stateVersion + 1, Done: []string{"a"}})
	_, err := (&Store{Path: p}).Load()
	if !errors.Is(err, ErrCheckpointUnmigratable) || !strings.Contains(err.Error(), "newer than") {
		t.Fatalf("downgrade load err = %v, want a newer-than refusal", err)
	}
}
