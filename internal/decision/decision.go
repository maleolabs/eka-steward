// Package decision holds the steward's pure, deterministic control
// decisions: given a set of work Candidates it decides the dispatch
// ORDER and WHICH candidate comes next.
//
// It is deliberately inert. The package imports only the standard
// library and is written so it can call nothing that observes the outside
// world: no I/O, no clock, no randomness, no subprocess and no LLM / agent
// client. A control decision is therefore a pure function of its input —
// the same candidates in, the same order out, byte for byte. This is the
// enforcement seam for steward's core split: CODE decides which / when /
// if, the LLM decides what (inside an ACP worker session). See
// docs/determinism.md.
//
// It is NOT a scheduler, queue, worker pool, retry engine or control
// loop. Those are separate work items and own their own concerns (timers,
// concurrency, persistence). This package is only the ordering / selection
// kernel they share.
package decision

import (
	"cmp"
	"slices"
)

// Candidate is one unit of work the steward may dispatch. The fields are
// the ONLY inputs the ordering and selection decisions may read, so every
// such decision is a pure function of the candidate set.
type Candidate struct {
	// ID uniquely identifies the work unit. It is the final tie-break key
	// and must be unique within a candidate set for the order to be total.
	ID string
	// Priority is the primary key: higher priority dispatches first.
	Priority int
	// State is the work unit's lifecycle state; its rank is the second key.
	// See the rank table below.
	State string
	// CreatedSeq is the monotonic creation sequence: the third key, giving
	// FIFO within equal priority and state.
	CreatedSeq uint64
}

// Recognized lifecycle states. Any other State is ranked after every
// recognized state (rank StateRankUnknown); ties still resolve through
// CreatedSeq then ID, so ordering stays total regardless.
const (
	StatePlanned    = "planned"
	StateTodo       = "todo"
	StateInProgress = "in-progress"
	StateBlocked    = "blocked"
	StateDone       = "done"
	StateCancelled  = "cancelled"
)

// StateRankUnknown is the rank given to any State not in the rank table.
// It is deliberately greater than every recognized rank.
const StateRankUnknown = 6

// stateRanks is the pinned State rank table (ascending: lower ranks sort
// first). It is the lifecycle order: planned -> todo -> in-progress ->
// blocked -> done -> cancelled. Any state missing here ranks
// StateRankUnknown. This table is a total-order key, so changing it is a
// determinism-breaking change and must update docs/determinism.md.
var stateRanks = map[string]int{
	StatePlanned:    0,
	StateTodo:       1,
	StateInProgress: 2,
	StateBlocked:    3,
	StateDone:       4,
	StateCancelled:  5,
}

// stateRank returns the rank of state (StateRankUnknown if unrecognized).
func stateRank(state string) int {
	if r, ok := stateRanks[state]; ok {
		return r
	}
	return StateRankUnknown
}

// dispatchable reports whether a candidate in this State may be dispatched
// now: only the ready states, planned and todo. in-progress is already
// dispatched; blocked / done / cancelled must not be (re)dispatched.
func dispatchable(state string) bool {
	return state == StatePlanned || state == StateTodo
}

// StateRank returns the pinned rank of state (StateRankUnknown when the
// state is not in the rank table). It is exported so an adjacent pure
// package (internal/plan) shares this ONE rank table instead of duplicating
// it: the lifecycle order is the pinned contract (docs/determinism.md).
func StateRank(state string) int { return stateRank(state) }

// Dispatchable reports whether a unit in this State may be dispatched now —
// the ready states planned and todo. Exported for the same reason as
// StateRank: internal/plan reuses the one dispatchability rule.
func Dispatchable(state string) bool { return dispatchable(state) }

// Compare orders two candidates by the pinned total-order key (priority
// desc, state rank asc, created-seq asc, id asc); <0 when a sorts before b.
// Exported so internal/plan can reuse it as the tie-break beneath its
// container-order key, keeping one coherent order contract.
func Compare(a, b Candidate) int { return compare(a, b) }

// compare orders two candidates by the pinned key: Priority desc, State
// rank asc, CreatedSeq asc, ID asc. It returns <0 if a sorts before b, >0
// if a sorts after b, and 0 only when the two are identical in every key
// (equal ID and all fields — the same work unit). Because ID is unique in
// a candidate set, the order is total.
func compare(a, b Candidate) int {
	if c := cmp.Compare(b.Priority, a.Priority); c != 0 { // Priority desc
		return c
	}
	if c := cmp.Compare(stateRank(a.State), stateRank(b.State)); c != 0 { // State rank asc
		return c
	}
	if c := cmp.Compare(a.CreatedSeq, b.CreatedSeq); c != 0 { // CreatedSeq asc
		return c
	}
	return cmp.Compare(a.ID, b.ID) // ID asc
}

// Order returns candidates in the deterministic dispatch order. It never
// mutates the input (it sorts a copy) and, because the key is a total
// order, the result is identical for any permutation of the same
// candidates — byte-stable for the same candidate multiset.
func Order(cs []Candidate) []Candidate {
	out := slices.Clone(cs)
	slices.SortStableFunc(out, compare)
	return out
}

// SelectNext returns the highest-priority dispatchable candidate (the
// minimum under the pinned order among candidates in a ready State) and
// false when none is dispatchable. The result is independent of the input
// order: it is the minimum of a total order over a fixed set.
func SelectNext(cs []Candidate) (Candidate, bool) {
	var best Candidate
	found := false
	for _, c := range cs {
		if !dispatchable(c.State) {
			continue
		}
		if !found || compare(c, best) < 0 {
			best = c
			found = true
		}
	}
	return best, found
}
