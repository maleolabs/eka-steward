package loop

import (
	"context"
	"fmt"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
)

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
			ID:    u.Identity.Namespace + "/" + u.Identity.Type + ":" + u.Identity.ID,
			State: u.StateVector.ExecutionState,
		})
	}
	return out, nil
}
