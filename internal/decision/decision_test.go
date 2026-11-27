package decision

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// This file pins the ordering and selection contract of the decision
// package. The package imports no LLM / agent client (asserted below), so
// no test here can make an LLM call: determinism is guaranteed by
// construction, and these tests pin the exact order.

// encode renders a candidate slice to a stable string so two results can
// be compared byte for byte.
func encode(cs []Candidate) string {
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s|%d|%s|%d", c.ID, c.Priority, c.State, c.CreatedSeq)
	}
	return b.String()
}

// ids returns the ID field of each candidate in order.
func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// TestOrderPinsEveryTieBreakLevel is the table test: one case per key
// level of the pinned order (Priority desc, State rank asc, CreatedSeq
// asc, ID asc) plus the unknown-state fallback.
func TestOrderPinsEveryTieBreakLevel(t *testing.T) {
	tests := []struct {
		name string
		in   []Candidate
		want []string // expected IDs, in order
	}{
		{
			name: "priority dominates everything",
			in: []Candidate{
				{ID: "a", Priority: 1, State: StateTodo, CreatedSeq: 0},
				{ID: "b", Priority: 3, State: StateDone, CreatedSeq: 9},
				{ID: "c", Priority: 2, State: StatePlanned, CreatedSeq: 1},
			},
			want: []string{"b", "c", "a"},
		},
		{
			name: "state rank breaks equal priority (lifecycle order)",
			in: []Candidate{
				{ID: "done", Priority: 5, State: StateDone, CreatedSeq: 1},
				{ID: "planned", Priority: 5, State: StatePlanned, CreatedSeq: 2},
				{ID: "todo", Priority: 5, State: StateTodo, CreatedSeq: 3},
				{ID: "ip", Priority: 5, State: StateInProgress, CreatedSeq: 4},
				{ID: "blocked", Priority: 5, State: StateBlocked, CreatedSeq: 5},
				{ID: "cancelled", Priority: 5, State: StateCancelled, CreatedSeq: 6},
			},
			want: []string{"planned", "todo", "ip", "blocked", "done", "cancelled"},
		},
		{
			name: "createdSeq breaks equal priority and state",
			in: []Candidate{
				{ID: "c", Priority: 5, State: StateTodo, CreatedSeq: 300},
				{ID: "a", Priority: 5, State: StateTodo, CreatedSeq: 100},
				{ID: "b", Priority: 5, State: StateTodo, CreatedSeq: 200},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "ID breaks equal priority, state and createdSeq",
			in: []Candidate{
				{ID: "zulu", Priority: 5, State: StateTodo, CreatedSeq: 7},
				{ID: "alpha", Priority: 5, State: StateTodo, CreatedSeq: 7},
				{ID: "mike", Priority: 5, State: StateTodo, CreatedSeq: 7},
			},
			want: []string{"alpha", "mike", "zulu"},
		},
		{
			name: "unknown state ranks after every recognized state",
			in: []Candidate{
				{ID: "w", Priority: 5, State: "weird", CreatedSeq: 1},
				{ID: "c", Priority: 5, State: StateCancelled, CreatedSeq: 2},
				{ID: "p", Priority: 5, State: StatePlanned, CreatedSeq: 3},
			},
			want: []string{"p", "c", "w"},
		},
		{
			name: "all keys together",
			in: []Candidate{
				{ID: "x", Priority: 1, State: StatePlanned, CreatedSeq: 5},
				{ID: "y", Priority: 2, State: StateDone, CreatedSeq: 1},
				{ID: "z", Priority: 2, State: StatePlanned, CreatedSeq: 9},
				{ID: "w", Priority: 2, State: StatePlanned, CreatedSeq: 3},
			},
			want: []string{"w", "z", "y", "x"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(Order(tc.in))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("Order ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// baseSet is a fixed, ID-unique candidate multiset used by the property
// tests. It exercises every priority, state rank and seq relationship.
func baseSet() []Candidate {
	return []Candidate{
		{ID: "d1", Priority: 9, State: StatePlanned, CreatedSeq: 1},
		{ID: "d2", Priority: 9, State: StateTodo, CreatedSeq: 1},
		{ID: "d3", Priority: 9, State: StatePlanned, CreatedSeq: 2},
		{ID: "d4", Priority: 3, State: StateInProgress, CreatedSeq: 5},
		{ID: "d5", Priority: 3, State: StateBlocked, CreatedSeq: 5},
		{ID: "d6", Priority: 3, State: StateBlocked, CreatedSeq: 4},
		{ID: "d7", Priority: 100, State: StateDone, CreatedSeq: 0},
		{ID: "d8", Priority: 0, State: "mystery", CreatedSeq: 0},
		{ID: "d9", Priority: 9, State: StatePlanned, CreatedSeq: 1},
		{ID: "d10", Priority: 3, State: StateCancelled, CreatedSeq: 9},
	}
}

// TestOrderIsTotalOrderAndStableUnderShuffle proves Order is a total order
// (the output is a permutation, strictly increasing under compare) and
// that it is invariant under every shuffle of the same set — the core
// "same input multiset -> byte-identical output" guarantee.
func TestOrderIsTotalOrderAndStableUnderShuffle(t *testing.T) {
	base := baseSet()
	want := encode(Order(base))

	// Total order over the ordered output.
	out := Order(base)
	if len(out) != len(base) {
		t.Fatalf("Order dropped candidates: %d vs %d", len(out), len(base))
	}
	for i := 1; i < len(out); i++ {
		if c := compare(out[i-1], out[i]); c >= 0 {
			t.Fatalf("not strictly increasing at %d: compare(%q,%q)=%d", i, out[i-1].ID, out[i].ID, c)
		}
	}

	// Output is a permutation of the input (same multiset of IDs).
	gotIDs := append([]string(nil), ids(out)...)
	wantIDs := append([]string(nil), ids(base)...)
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("Order is not a permutation: got %v want %v", gotIDs, wantIDs)
	}

	// Invariant under shuffles (deterministic seed).
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 200; i++ {
		shuffled := slicesClone(base)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := encode(Order(shuffled)); got != want {
			t.Fatalf("Order not stable under shuffle %d:\n got %q\nwant %q", i, got, want)
		}
	}
}

// TestOrderIsByteIdenticalAcrossCalls pins that repeated calls on the same
// input return byte-identical output.
func TestOrderIsByteIdenticalAcrossCalls(t *testing.T) {
	in := baseSet()
	first := encode(Order(in))
	for i := 0; i < 50; i++ {
		if got := encode(Order(in)); got != first {
			t.Fatalf("call %d differs:\n got %q\nwant %q", i, got, first)
		}
	}
}

// TestOrderDoesNotMutateInput pins the copy-on-sort contract.
func TestOrderDoesNotMutateInput(t *testing.T) {
	in := []Candidate{
		{ID: "b", Priority: 1, State: StateTodo, CreatedSeq: 2},
		{ID: "a", Priority: 1, State: StateTodo, CreatedSeq: 1},
	}
	before := encode(in)
	_ = Order(in)
	if got := encode(in); got != before {
		t.Fatalf("Order mutated its input:\n got %q\nwant %q", got, before)
	}
}

// TestSelectNextPicksHighestPriorityDispatchable pins selection: the
// highest-priority dispatchable candidate wins and non-ready states are
// skipped even when they outrank the winner.
func TestSelectNextPicksHighestPriorityDispatchable(t *testing.T) {
	tests := []struct {
		name   string
		in     []Candidate
		wantID string
		wantOK bool
	}{
		{
			name: "skips non-dispatchable higher priority",
			in: []Candidate{
				{ID: "done-high", Priority: 100, State: StateDone, CreatedSeq: 0},
				{ID: "blocked-high", Priority: 50, State: StateBlocked, CreatedSeq: 0},
				{ID: "ready", Priority: 1, State: StatePlanned, CreatedSeq: 0},
			},
			wantID: "ready",
			wantOK: true,
		},
		{
			name: "state rank then seq among dispatchable",
			in: []Candidate{
				{ID: "todo-late", Priority: 5, State: StateTodo, CreatedSeq: 9},
				{ID: "planned-first", Priority: 5, State: StatePlanned, CreatedSeq: 9},
				{ID: "planned-earlier", Priority: 5, State: StatePlanned, CreatedSeq: 2},
			},
			wantID: "planned-earlier",
			wantOK: true,
		},
		{
			name: "none dispatchable",
			in: []Candidate{
				{ID: "a", Priority: 9, State: StateDone, CreatedSeq: 0},
				{ID: "b", Priority: 9, State: StateBlocked, CreatedSeq: 0},
				{ID: "c", Priority: 9, State: StateInProgress, CreatedSeq: 0},
			},
			wantOK: false,
		},
		{
			name:   "empty",
			in:     nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SelectNext(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("SelectNext ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.ID != tc.wantID {
				t.Fatalf("SelectNext id = %q, want %q", got.ID, tc.wantID)
			}
		})
	}
}

// TestSelectNextIsOrderIndependent pins that the selected candidate does
// not depend on the input order.
func TestSelectNextIsOrderIndependent(t *testing.T) {
	base := baseSet()
	first, ok := SelectNext(base)
	if !ok {
		t.Fatal("SelectNext found nothing in the base set")
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		shuffled := slicesClone(base)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, ok := SelectNext(shuffled)
		if !ok || got.ID != first.ID {
			t.Fatalf("shuffle %d: SelectNext = (%q,%v), want (%q,true)", i, got.ID, ok, first.ID)
		}
	}
}

// slicesClone is a tiny local copy helper (the standard library slices
// package is intentionally not imported by this test's subject; here we
// only need a shallow copy).
func slicesClone(cs []Candidate) []Candidate {
	out := make([]Candidate, len(cs))
	copy(out, cs)
	return out
}
