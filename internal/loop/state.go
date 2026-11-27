package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// stateVersion is the on-disk schema version of the loop state. A file with a
// different version is refused deterministically rather than guessed at.
const stateVersion = 1

// State is the loop's persisted resume state: the identities already
// dispatched and an optional cursor. It is versioned and written atomically
// by Store.
//
// This is a MINIMAL persist+reload: it records completion so a restart skips
// finished work. Crash-safe reconciliation of a partially applied step (the
// checkpoint/resume hardening) is a separate work item
// (sto:checkpoint-resume) and is deliberately not implemented here.
type State struct {
	Version int      `json:"version"`
	Done    []string `json:"done"`
	Cursor  string   `json:"cursor,omitempty"`
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

// Load reads and validates the state file. A missing file yields a fresh
// Version-1 state (empty Done); an unsupported Version is an error.
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
	if st.Version != stateVersion {
		return State{}, fmt.Errorf("loop: state %s has version %d, want %d", s.Path, st.Version, stateVersion)
	}
	return st, nil
}

// Save writes st atomically. Done is sorted and de-duplicated first so the
// file is byte-stable for the same completed set.
func (s *Store) Save(st State) error {
	st.Version = stateVersion
	st.Done = normalizeDone(st.Done)
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
