package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/plan"
)

// dependsOn is the EKA canonical `depends-on` relationship type: the unit on
// the referring side depends on the target.
const dependsOn = "depends-on"

// containerType is the EKA type token of a container (ctr-) unit. A `depends-on`
// edge to a container records membership, not a blocking prerequisite.
const containerType = "ctr"

// StoreSource is a READ-ONLY Source backed by the EKA public API. It reads the
// engineering knowledge of one project through api.Knowledge (the read-only
// contract) and projects each work item into a decision.Candidate.
//
// Mapping: the candidate identity is the qualified line form
// "<namespace>/<type>:<id>"; the State is the unit's execution-state. A unit
// that owns no execution-state (e.g. a knowledge artifact) yields an empty
// State, which the decision kernel ranks as non-dispatchable. Priority and
// CreatedSeq stay 0 in this cut — EKA work items carry no such field — so the
// order is by state rank then ID, still a deterministic total order. A richer
// priority/sequence source is a later concern.
type StoreSource struct {
	// Knowledge is the read-only EKA knowledge service (rt.Knowledge()).
	Knowledge api.Knowledge
	// Project is the project scope to read.
	Project string
}

// Candidates returns the project's work items as decision candidates. It never
// writes EKA.
func (s StoreSource) Candidates(_ context.Context) ([]decision.Candidate, error) {
	units, err := s.Knowledge.UnitsByProject(s.Project)
	if err != nil {
		return nil, fmt.Errorf("loop: read knowledge of project %q: %w", s.Project, err)
	}
	out := make([]decision.Candidate, 0, len(units))
	for _, u := range units {
		out = append(out, decision.Candidate{
			ID:    lineForm(u.Identity.Namespace, u.Identity.Type, u.Identity.ID),
			State: u.StateVector.ExecutionState,
		})
	}
	return out, nil
}

// Graph returns the project's work items as plan nodes plus their `depends-on`
// edges, read-only through api.Knowledge. A `depends-on` relationship whose
// target is a container (type `ctr`) records CONTAINER membership (sets the
// node's ContainerID); any other `depends-on` target is a dependency edge
// {From: target, To: unit}, so the target is ordered before the unit and, until
// it reaches a terminal state, blocks it.
//
// Targets are normalized to the qualified line form "<namespace>/<type>:<id>"
// (a trailing instance-version is dropped) so an edge endpoint matches a node's
// ID. ContainerOrder stays 0 in this cut: EKA carries no container ordinal, and
// the planner treats 0 as unranked. An edge whose endpoint is not a unit of
// this project is emitted as-is; the planner ignores it (an out-of-scope
// prerequisite is not a planning input).
func (s StoreSource) Graph(_ context.Context) ([]plan.Node, []plan.Edge, error) {
	units, err := s.Knowledge.UnitsByProject(s.Project)
	if err != nil {
		return nil, nil, fmt.Errorf("loop: read knowledge of project %q: %w", s.Project, err)
	}
	nodes := make([]plan.Node, 0, len(units))
	var edges []plan.Edge
	for _, u := range units {
		id := lineForm(u.Identity.Namespace, u.Identity.Type, u.Identity.ID)
		n := plan.Node{ID: id, State: u.StateVector.ExecutionState}
		for _, rel := range u.Relationships {
			if rel.Type != dependsOn {
				continue
			}
			ns, typ, target, ok := parseTarget(rel.Target, u.Identity.Namespace)
			if !ok {
				continue
			}
			if typ == containerType {
				n.ContainerID = lineForm(ns, typ, target)
				continue
			}
			edges = append(edges, plan.Edge{From: lineForm(ns, typ, target), To: id})
		}
		nodes = append(nodes, n)
	}
	return nodes, edges, nil
}

// lineForm renders the qualified line form "<namespace>/<type>:<id>".
func lineForm(ns, typ, id string) string {
	return ns + "/" + typ + ":" + id
}

// parseTarget resolves an EKA relationship target into (namespace, type, id).
// It accepts "type:id", "ns/type:id" and either with a trailing
// instance-version ("ns/type:id:1"); the version is dropped because a node ID
// is versionless. A missing namespace defaults to defaultNS.
func parseTarget(target, defaultNS string) (ns, typ, id string, ok bool) {
	s := strings.TrimSpace(target)
	if s == "" {
		return "", "", "", false
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			s = s[:i]
		}
	}
	ns, rest := defaultNS, s
	if i := strings.Index(s, "/"); i >= 0 {
		ns, rest = s[:i], s[i+1:]
	}
	j := strings.Index(rest, ":")
	if j < 0 {
		return "", "", "", false
	}
	typ, id = rest[:j], rest[j+1:]
	if ns == "" || typ == "" || id == "" {
		return "", "", "", false
	}
	return ns, typ, id, true
}
