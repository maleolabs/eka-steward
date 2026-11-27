// Package plan is the steward's pure, deterministic work-selection planner:
// given a dependency graph of work units it decides the ORDER in which they
// may be dispatched and WHICH one comes next, honouring `depends-on`
// prerequisites.
//
// It is deliberately inert, like internal/decision: it imports only the
// standard library and internal/decision (itself effect-free), and calls
// nothing that observes the outside world — no I/O, no clock, no randomness,
// no subprocess and no LLM / agent client. A selection is therefore a pure
// function of the graph: the same nodes and edges in, the same order out,
// byte for byte. See docs/selection.md.
//
// # The selection order
//
// The planner layers two rules:
//
//  1. Topological over `depends-on`: an edge {From: P, To: D} means D
//     depends-on P, so P is ordered before D. A unit whose prerequisite is
//     not in a terminal state (done / cancelled) is BLOCKED and is never
//     selected.
//  2. A deterministic tie-break between units with no remaining
//     prerequisite: container order ascending, then the pinned
//     internal/decision total order (priority desc, state rank asc,
//     created-seq asc, id asc). This makes selection a total order, and
//     when every unit sits in one container (order 0) with no edges, Order
//     is byte-identical to decision.Order — the current contract.
//
// See Order, SelectNext, Selectable and Blocked below.
package plan

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
)

// Node is one unit of work the planner may order and select. Its fields are
// the ONLY inputs the ordering and selection decisions read, so every such
// decision is a pure function of the node set.
type Node struct {
	// ID uniquely identifies the work unit; the final tie-break key. It must
	// be unique within a node set for the order to be total.
	ID string
	// State is the unit's lifecycle execution-state. Only the ready states
	// planned / todo are dispatchable (see decision.Dispatchable).
	State string
	// ContainerID is the container the unit belongs to. It groups units for
	// the container-order key; empty means no container.
	ContainerID string
	// ContainerOrder is the container's rank: lower orders dispatch first.
	// Units with no container (or an unranked container) use 0.
	ContainerOrder int
	// Priority is the decision total-order primary key (higher first).
	Priority int
	// CreatedSeq is the decision total-order FIFO key (ascending).
	CreatedSeq uint64
}

// Edge is a directed dependency: To depends-on From. From must be ordered —
// and, to unblock To, driven terminal — before To may be dispatched.
type Edge struct {
	From string
	To   string
}

// terminal reports whether a state satisfies a prerequisite: done and
// cancelled are terminal; every other state (planned, todo, in-progress,
// blocked, unknown) is not.
func terminal(state string) bool {
	return state == decision.StateDone || state == decision.StateCancelled
}

// candidate projects a Node into the decision kernel's view so the pinned
// total order can be reused unchanged as the tie-break beneath container
// order. ContainerID / ContainerOrder are plan-only keys.
func candidate(n Node) decision.Candidate {
	return decision.Candidate{ID: n.ID, Priority: n.Priority, State: n.State, CreatedSeq: n.CreatedSeq}
}

// less is the pinned total-order key: container order ascending, then the
// decision total order (priority desc, state rank asc, created-seq asc,
// id asc). It is negative when a sorts before b. It is a total order because
// decision.ID is unique in a node set.
func less(a, b Node) int {
	if c := cmp.Compare(a.ContainerOrder, b.ContainerOrder); c != 0 {
		return c
	}
	return decision.Compare(candidate(a), candidate(b))
}

// indexNodes returns the nodes keyed by ID, erroring deterministically on a
// duplicate ID (which would make the order ambiguous rather than total).
func indexNodes(nodes []Node) (map[string]Node, error) {
	idx := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if _, dup := idx[n.ID]; dup {
			return nil, fmt.Errorf("plan: duplicate node id %q", n.ID)
		}
		idx[n.ID] = n
	}
	return idx, nil
}

// adjacency builds the deduplicated depends-on adjacency (From -> [To...])
// and the in-degree per node. Edges whose From or To is not in the node set
// are ignored: an out-of-scope prerequisite cannot be ordered or driven
// terminal, so it is not a planning input. Adjacency lists are sorted so the
// traversal is deterministic.
func adjacency(nodes []Node, edges []Edge) (map[string][]string, map[string]int) {
	known := make(map[string]bool, len(nodes))
	indeg := make(map[string]int, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
		indeg[n.ID] = 0
	}
	adj := make(map[string][]string)
	seen := make(map[[2]string]bool, len(edges))
	for _, e := range edges {
		if !known[e.From] || !known[e.To] || e.From == e.To {
			continue
		}
		key := [2]string{e.From, e.To}
		if seen[key] {
			continue
		}
		seen[key] = true
		adj[e.From] = append(adj[e.From], e.To)
		indeg[e.To]++
	}
	for from := range adj {
		slices.Sort(adj[from])
	}
	return adj, indeg
}

// cycleError renders a deterministic error naming the nodes that could not be
// ordered (the cycle), so a cyclic graph never loops and always reports the
// same message for the same input.
func cycleError(nodes []Node, placed map[string]bool) error {
	var cyc []string
	for _, n := range nodes {
		if !placed[n.ID] {
			cyc = append(cyc, n.ID)
		}
	}
	slices.Sort(cyc)
	return fmt.Errorf("plan: depends-on cycle among %s", strings.Join(cyc, ", "))
}

// Order returns the nodes in the deterministic selection order: topological
// over depends-on (prerequisites first), ties broken by less (container
// order, then the decision total order). It never mutates its inputs. When
// the graph has no edges and every node shares a container order, the result
// is byte-identical to decision.Order over the same nodes.
//
// A dependency cycle is a deterministic error (never an infinite loop): the
// message names the unordered nodes.
func Order(nodes []Node, edges []Edge) ([]Node, error) {
	if _, err := indexNodes(nodes); err != nil {
		return nil, err
	}
	adj, indeg := adjacency(nodes, edges)
	out := make([]Node, 0, len(nodes))
	placed := make(map[string]bool, len(nodes))
	for len(out) < len(nodes) {
		best := -1
		for i := range nodes {
			n := nodes[i]
			if placed[n.ID] || indeg[n.ID] != 0 {
				continue
			}
			if best < 0 || less(n, nodes[best]) < 0 {
				best = i
			}
		}
		if best < 0 {
			return nil, cycleError(nodes, placed)
		}
		n := nodes[best]
		placed[n.ID] = true
		out = append(out, n)
		for _, to := range adj[n.ID] {
			indeg[to]--
		}
	}
	return out, nil
}

// blockedSet returns the set of node IDs that have at least one prerequisite
// (depends-on From) whose From is present in the set and not terminal. A
// blocked node is never selected.
func blockedSet(nodes []Node, edges []Edge) map[string]bool {
	known := make(map[string]bool, len(nodes))
	term := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
		term[n.ID] = terminal(n.State)
	}
	blocked := map[string]bool{}
	for _, e := range edges {
		if !known[e.From] || !known[e.To] {
			continue
		}
		if !term[e.From] {
			blocked[e.To] = true
		}
	}
	return blocked
}

// Selectable returns the ordered nodes that may be dispatched now: state is
// dispatchable (decision.Dispatchable) and no prerequisite is incomplete.
// It is the sequence Plan emits; SelectNext is its head. A cycle is the same
// deterministic error as Order.
func Selectable(nodes []Node, edges []Edge) ([]Node, error) {
	ordered, err := Order(nodes, edges)
	if err != nil {
		return nil, err
	}
	blocked := blockedSet(nodes, edges)
	out := make([]Node, 0, len(ordered))
	for _, n := range ordered {
		if !decision.Dispatchable(n.State) || blocked[n.ID] {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// SelectNext returns the next dispatchable, unblocked node — the head of
// Selectable — and false when there is none. A cyclic graph selects nothing
// (false): the order is undefined, so nothing is safely dispatchable.
func SelectNext(nodes []Node, edges []Edge) (Node, bool) {
	sel, err := Selectable(nodes, edges)
	if err != nil || len(sel) == 0 {
		return Node{}, false
	}
	return sel[0], true
}

// Blocked returns the non-terminal nodes that have at least one incomplete
// prerequisite (see blockedSet), in the deterministic order (Order's order,
// or the pinned key order when the graph is cyclic). These are the nodes
// Selectable/SelectNext refuse: a blocked unit is never selected.
func Blocked(nodes []Node, edges []Edge) []Node {
	blocked := blockedSet(nodes, edges)
	eligible := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if blocked[n.ID] && !terminal(n.State) {
			eligible = append(eligible, n)
		}
	}
	ordered, err := Order(nodes, edges)
	if err != nil {
		slices.SortStableFunc(eligible, func(a, b Node) int { return less(a, b) })
		return eligible
	}
	out := make([]Node, 0, len(eligible))
	for _, n := range ordered {
		if blocked[n.ID] && !terminal(n.State) {
			out = append(out, n)
		}
	}
	return out
}
