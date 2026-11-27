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
// the InFlight write-ahead intent log. Load upgrades a version-1 file (Done
// only, no intent log) to version 2 in memory; any other version is refused
// deterministically rather than guessed at (docs/checkpoint.md).
const stateVersion = 2

// previousStateVersion is the last version Load accepts and upgrades.
const previousStateVersion = 1

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
// Version-2 state (empty Done, empty InFlight); a version-1 file is upgraded to
// version 2 in memory (it has no intent log); any other version is a
// deterministic error.
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
	switch st.Version {
	case stateVersion:
		// current
	case previousStateVersion:
		// Upgrade: version 1 carried no intent log.
		st.Version = stateVersion
	default:
		return State{}, fmt.Errorf("loop: state %s has version %d, want %d (or the upgradeable %d)", s.Path, st.Version, stateVersion, previousStateVersion)
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
