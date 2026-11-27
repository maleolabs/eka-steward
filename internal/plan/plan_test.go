package plan

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
)

// fixture is the deterministic selection graph. It deliberately exercises
// every rule of the contract:
//
//   - diamond: A→B, A→C, B&C→D (Topo: A before B,C; B,C before D).
//   - container order: c1 (order 0) outranks c2 (order 1), so A,C,B,G,E come
//     before D,H regardless of their decision key.
//   - state adjacency: A and C (planned) precede B and G (todo) at equal
//     priority and container.
//   - blocked: E depends-on F (planned → incomplete), so E is never selected
//     even though it has the highest priority; B, C (depends-on A planned) and
//     D (depends-on B,C planned) are also blocked.
//   - non-dispatchable in order: Z is done (highest priority) and occupies the
//     head of Order but never a selection slot.
func fixture() ([]Node, []Edge) {
	nodes := []Node{
		{ID: "Z", State: decision.StateDone, ContainerID: "c1", ContainerOrder: 0, Priority: 200, CreatedSeq: 0},
		{ID: "A", State: decision.StatePlanned, ContainerID: "c1", ContainerOrder: 0, Priority: 10, CreatedSeq: 1},
		{ID: "B", State: decision.StateTodo, ContainerID: "c1", ContainerOrder: 0, Priority: 10, CreatedSeq: 2},
		{ID: "C", State: decision.StatePlanned, ContainerID: "c1", ContainerOrder: 0, Priority: 10, CreatedSeq: 3},
		{ID: "D", State: decision.StatePlanned, ContainerID: "c2", ContainerOrder: 1, Priority: 10, CreatedSeq: 4},
		{ID: "E", State: decision.StateTodo, ContainerID: "c1", ContainerOrder: 0, Priority: 5, CreatedSeq: 5},
		{ID: "F", State: decision.StatePlanned, ContainerID: "c1", ContainerOrder: 0, Priority: 50, CreatedSeq: 6},
		{ID: "G", State: decision.StateTodo, ContainerID: "c1", ContainerOrder: 0, Priority: 10, CreatedSeq: 7},
		{ID: "H", State: decision.StatePlanned, ContainerID: "c2", ContainerOrder: 1, Priority: 10, CreatedSeq: 8},
	}
	edges := []Edge{
		{From: "A", To: "B"}, // B depends-on A
		{From: "A", To: "C"}, // C depends-on A
		{From: "B", To: "D"}, // D depends-on B
		{From: "C", To: "D"}, // D depends-on C (diamond)
		{From: "F", To: "E"}, // E depends-on F (blocked: F planned)
	}
	return nodes, edges
}

// goldenView is the frozen selection outcome for the fixture graph.
type goldenView struct {
	Order     []string `json:"order"`
	Selection []string `json:"selection"`
	Blocked   []string `json:"blocked"`
	First     string   `json:"first"`
}

// ids returns the ID of each node, in order.
func ids(ns []Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

// simulate drives SelectNext to quiescence, marking each selected node done
// and re-planning, and returns the exact dispatch sequence. It proves the
// static selection is realizable end to end and that blocked nodes only ever
// appear once their prerequisites turn terminal.
func simulate(t *testing.T, nodes []Node, edges []Edge) []string {
	t.Helper()
	cur := make([]Node, len(nodes))
	copy(cur, nodes)
	var seq []string
	for {
		n, ok := SelectNext(cur, edges)
		if !ok {
			return seq
		}
		seq = append(seq, n.ID)
		for i := range cur {
			if cur[i].ID == n.ID {
				cur[i].State = decision.StateDone
			}
		}
	}
}

// TestSelectionGolden freezes the exact order, dispatch sequence, blocked set
// and head for the fixture graph.
func TestSelectionGolden(t *testing.T) {
	nodes, edges := fixture()
	ordered, err := Order(nodes, edges)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	first, ok := SelectNext(nodes, edges)
	if !ok {
		t.Fatal("SelectNext found nothing")
	}
	got := goldenView{
		Order:     ids(ordered),
		Selection: simulate(t, nodes, edges),
		Blocked:   ids(Blocked(nodes, edges)),
		First:     first.ID,
	}
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b = append(b, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "graph.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(b) != string(want) {
		t.Fatalf("selection drifted from the golden:\n got:\n%s\n want:\n%s", b, want)
	}
}

// TestBlockedNeverSelected asserts every blocked node is refused by
// Selectable and never appears in the simulated dispatch sequence while its
// prerequisite is incomplete.
func TestBlockedNeverSelected(t *testing.T) {
	nodes, edges := fixture()
	blocked := ids(Blocked(nodes, edges))
	if len(blocked) == 0 {
		t.Fatal("fixture must have at least one blocked node")
	}
	sel, err := Selectable(nodes, edges)
	if err != nil {
		t.Fatalf("Selectable: %v", err)
	}
	selected := map[string]bool{}
	for _, n := range sel {
		selected[n.ID] = true
	}
	for _, id := range blocked {
		if selected[id] {
			t.Errorf("blocked node %q was selected", id)
		}
	}
	// E has the highest selection priority among todo but is blocked by F
	// (planned); the head must be F, never E.
	first, ok := SelectNext(nodes, edges)
	if !ok || first.ID != "F" {
		t.Fatalf("SelectNext = (%q,%v), want (F,true): E is blocked by F", first.ID, ok)
	}
}

// TestCycleIsDeterministicError pins the cycle handling: Order fails with a
// stable message, SelectNext selects nothing, and it never loops.
func TestCycleIsDeterministicError(t *testing.T) {
	nodes := []Node{
		{ID: "A", State: decision.StateTodo},
		{ID: "B", State: decision.StateTodo},
		{ID: "C", State: decision.StateTodo},
	}
	edges := []Edge{{From: "A", To: "B"}, {From: "B", To: "C"}, {From: "C", To: "A"}}

	_, first := Order(nodes, edges)
	if first == nil {
		t.Fatal("Order accepted a cycle")
	}
	for i := 0; i < 20; i++ {
		if _, err := Order(nodes, edges); err == nil || err.Error() != first.Error() {
			t.Fatalf("cycle error not deterministic: %v vs %v", err, first)
		}
	}
	if _, ok := SelectNext(nodes, edges); ok {
		t.Fatal("SelectNext selected something in a cyclic graph")
	}
}

// TestOrderIsDeterministicAcrossRunsAndPermutations pins byte-identical order
// over ≥20 runs and every input permutation.
func TestOrderIsDeterministicAcrossRunsAndPermutations(t *testing.T) {
	nodes, edges := fixture()
	want, err := Order(nodes, edges)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	base := strings.Join(ids(want), ",")
	for i := 0; i < 20; i++ {
		got, err := Order(nodes, edges)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if strings.Join(ids(got), ",") != base {
			t.Fatalf("run %d order differs: %v vs %s", i, ids(got), base)
		}
	}
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 50; i++ {
		nn := append([]Node(nil), nodes...)
		ee := append([]Edge(nil), edges...)
		rng.Shuffle(len(nn), func(a, b int) { nn[a], nn[b] = nn[b], nn[a] })
		rng.Shuffle(len(ee), func(a, b int) { ee[a], ee[b] = ee[b], ee[a] })
		got, err := Order(nn, ee)
		if err != nil {
			t.Fatalf("permutation %d: %v", i, err)
		}
		if strings.Join(ids(got), ",") != base {
			t.Fatalf("permutation %d changed order: %v", i, ids(got))
		}
	}
}

// TestNoEdgesMatchesDecisionOrder pins backward compatibility: with no edges
// and one container order, Order is byte-identical to decision.Order, so the
// existing loop golden (which has no dependency graph) is unchanged.
func TestNoEdgesMatchesDecisionOrder(t *testing.T) {
	cs := []decision.Candidate{
		{ID: "a", Priority: 10, State: decision.StateTodo, CreatedSeq: 1},
		{ID: "b", Priority: 10, State: decision.StateTodo, CreatedSeq: 2},
		{ID: "c", Priority: 10, State: decision.StateTodo, CreatedSeq: 2},
		{ID: "d", Priority: 5, State: decision.StatePlanned, CreatedSeq: 3},
		{ID: "e", Priority: 20, State: decision.StateInProgress, CreatedSeq: 0},
		{ID: "f", Priority: 10, State: decision.StatePlanned, CreatedSeq: 5},
		{ID: "g", Priority: 1, State: decision.StateDone, CreatedSeq: 1},
	}
	nodes := make([]Node, len(cs))
	for i, c := range cs {
		nodes[i] = Node{ID: c.ID, State: c.State, Priority: c.Priority, CreatedSeq: c.CreatedSeq}
	}
	got, err := Order(nodes, nil)
	if err != nil {
		t.Fatalf("Order: %v", err)
	}
	if g, w := strings.Join(ids(got), ","), strings.Join(candidateIDs(decision.Order(cs)), ","); g != w {
		t.Fatalf("no-edge Order = %s, want decision.Order %s", g, w)
	}
}

// candidateIDs returns the ID of each candidate, in order.
func candidateIDs(cs []decision.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}
