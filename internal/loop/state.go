package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// stateVersion is the on-disk schema version of the loop state. Version 2 adds
// the InFlight write-ahead intent log. Load upgrades an older file through the
// ordered migration registry (checkpointMigrations) up to this version; a
// version the registry cannot reach is refused with ErrCheckpointUnmigratable
// rather than guessed at (docs/checkpoint.md, docs/upgrade.md).
const stateVersion = 2

// ErrCheckpointUnmigratable is the typed refusal for a checkpoint the migration
// registry cannot bring to stateVersion: older than the registry floor, newer
// than this steward, or sitting on a gap with no registry step. A caller
// matches it with errors.Is; the message names the offending version and the
// supported range. It is never returned after a partial upgrade.
var ErrCheckpointUnmigratable = errors.New("loop: checkpoint version is not migratable")

// migration is one ordered step of the checkpoint schema-migration registry: it
// upgrades a parsed State at From to To. Apply rewrites the fields the step
// changes; the walk advances the version, so Apply never sets Version itself.
type migration struct {
	From  int
	To    int
	Apply func(*State)
}

// checkpointMigrations is the ORDERED checkpoint schema-migration registry,
// oldest first. Load walks it from the file's version up to stateVersion; the
// walk is deterministic (slice order, no map iteration) and never partial.
// Bumping the schema means appending {From: stateVersion, To: next} here and
// advancing stateVersion — never editing or removing an existing step.
// docs/upgrade.md.
var checkpointMigrations = []migration{
	{From: 1, To: 2, Apply: migrateV1toV2},
}

// migrateV1toV2 upgrades a version-1 state. Version 1 carried no intent log
// (Done only); the version-2 field set is otherwise identical — InFlight
// already defaults to empty — so the upgrade is exactly the version advance the
// walk applies. There is deliberately no field to rewrite and, critically, none
// is dropped: an InFlight entry the loader decoded (a v1 line that carried one)
// is preserved untouched.
func migrateV1toV2(*State) {}

// migrateState brings st in place from its version to `to` by walking regs, or
// returns ErrCheckpointUnmigratable. It never partially upgrades: the full step
// path is resolved BEFORE any Apply runs, so a version below the registry floor,
// above `to`, or on a gap with no step is refused with st left untouched.
func migrateState(st *State, to int, regs []migration) error {
	floor := migrationFloor(regs, to)
	switch {
	case st.Version > to:
		return fmt.Errorf("%w: version %d is newer than this steward supports (current %d)", ErrCheckpointUnmigratable, st.Version, to)
	case st.Version < floor:
		return fmt.Errorf("%w: version %d is older than the oldest supported %d (current %d)", ErrCheckpointUnmigratable, st.Version, floor, to)
	}
	path, err := migrationPath(regs, st.Version, to, floor)
	if err != nil {
		return err
	}
	for _, m := range path {
		m.Apply(st)
	}
	st.Version = to
	return nil
}

// migrationPath resolves the ordered steps from `from` to `to`, or returns the
// typed gap refusal before any step is applied (a clean two-pass, so a refusal
// is never a partial migration).
func migrationPath(regs []migration, from, to, floor int) ([]migration, error) {
	var path []migration
	for v := from; v < to; {
		m, ok := migrationFrom(regs, v)
		if !ok {
			return nil, fmt.Errorf("%w: no migration path from version %d to %d (supported %d..%d)", ErrCheckpointUnmigratable, v, to, floor, to)
		}
		path = append(path, m)
		v = m.To
	}
	return path, nil
}

// migrationFloor is the oldest version regs can upgrade from: the smallest From.
// With no registry step it is `to` itself, so anything older is refused.
func migrationFloor(regs []migration, to int) int {
	floor := to
	for _, m := range regs {
		if m.From < floor {
			floor = m.From
		}
	}
	return floor
}

// migrationFrom returns the registry step whose From is v.
func migrationFrom(regs []migration, v int) (migration, bool) {
	for _, m := range regs {
		if m.From == v {
			return m, true
		}
	}
	return migration{}, false
}

// State is the loop's persisted checkpoint: the identities already completed
// (Done) and the write-ahead intent log of steps that were persisted but not
// yet confirmed applied (InFlight). It is versioned and written atomically by
// Store. On resume every InFlight intent is reconciled BEFORE the loop
// dispatches anything, so a crash at any boundary of a step is recoverable
// without re-dispatching or double-applying.
type State struct {
	Version  int      `json:"version"`
	Done     []string `json:"done"`
	Cursor   string   `json:"cursor,omitempty"`
	InFlight []Intent `json:"inFlight,omitempty"`
}

// Has reports whether id is recorded done.
func (s State) Has(id string) bool {
	for _, d := range s.Done {
		if d == id {
			return true
		}
	}
	return false
}

// Store persists the loop State as a versioned JSON file. Save writes
// atomically (temp file in the same directory, then rename) so a torn write
// never leaves a half-written state file.
type Store struct {
	Path string
}

// Load reads and validates the checkpoint file. A missing file yields a fresh
// Version-2 state (empty Done, empty InFlight); an older file is upgraded to
// the current version through the ordered registry (checkpointMigrations); any
// version the registry cannot reach is a deterministic ErrCheckpointUnmigratable.
func (s *Store) Load() (State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: stateVersion}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("loop: read state %s: %w", s.Path, err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("loop: decode state %s: %w", s.Path, err)
	}
	if err := migrateState(&st, stateVersion, checkpointMigrations); err != nil {
		return State{}, fmt.Errorf("loop: state %s: %w", s.Path, err)
	}
	return st, nil
}

// Save writes st atomically. Done and InFlight are normalized first (sorted,
// de-duplicated) so the file is byte-stable for the same logical state.
func (s *Store) Save(st State) error {
	st.Version = stateVersion
	st.Done = normalizeDone(st.Done)
	st.InFlight = normalizeInFlight(st.InFlight)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("loop: encode state: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(s.Path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("loop: create temp state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("loop: write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("loop: close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("loop: commit state %s: %w", s.Path, err)
	}
	return nil
}

// normalizeDone sorts and de-duplicates ids (deterministic file bytes).
func normalizeDone(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// normalizeInFlight sorts the intent log by candidate identity and keeps the
// last intent per candidate, so the persisted bytes are deterministic and a
// candidate never carries two intents.
func normalizeInFlight(in []Intent) []Intent {
	if len(in) == 0 {
		return nil
	}
	byID := make(map[string]Intent, len(in))
	ids := make([]string, 0, len(in))
	for _, it := range in {
		if _, seen := byID[it.CandidateID]; !seen {
			ids = append(ids, it.CandidateID)
		}
		byID[it.CandidateID] = it
	}
	sort.Strings(ids)
	out := make([]Intent, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}
