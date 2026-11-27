package loop

import (
	"context"
	"fmt"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-core/v2/exchange"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
)

// EkawriteWriter is the production Writer: it routes the loop's one mutation
// through internal/ekawrite, the single audited EKA write path. The loop never
// names an api.Authoring method itself; this adapter is the only place the
// loop's write reaches the mutating surface, and it delegates to ekawrite.
type EkawriteWriter struct {
	// Writer is the ekawrite gateway (steward or worker author).
	Writer *ekawrite.Writer
	// RepoPath is the repository the transition is addressed from (the
	// engine walks up to its eka.yaml).
	RepoPath string
}

// Advance transitions the dispatched work item to `to` through ekawrite. The
// target is the candidate's qualified line form ("<ns>/<type>:<id>").
func (e EkawriteWriter) Advance(_ context.Context, c decision.Candidate, to string) error {
	if e.Writer == nil {
		return fmt.Errorf("loop: EkawriteWriter has no ekawrite.Writer")
	}
	_, err := e.Writer.Transition(api.TransitionRequest{
		RepoPath:  e.RepoPath,
		Target:    c.ID,
		To:        to,
		Confirmed: true,
	})
	return err
}

// Reconcile re-issues one in-flight intent through ekawrite, the single write
// path. A transition is a no-op when the line is already at/after the target
// state; a publish goes through ekawrite's deterministic replay gate. It never
// dispatches a worker.
func (e EkawriteWriter) Reconcile(_ context.Context, in Intent) (ReconcileResult, error) {
	if e.Writer == nil {
		return ReconcileResult{}, fmt.Errorf("loop: EkawriteWriter has no ekawrite.Writer")
	}
	req := ekawrite.ReconcileRequest{
		Target:   in.Target,
		To:       in.To,
		Digest:   in.Digest,
		RepoPath: e.RepoPath,
	}
	if in.Kind == IntentPublish && in.Publish != nil {
		req.Publish = &ekawrite.PublishInput{
			Project:       in.Publish.Project,
			Namespace:     in.Publish.Namespace,
			Type:          in.Publish.Type,
			ID:            in.Publish.ID,
			Content:       in.Publish.Content,
			Dimension:     in.Publish.Dimension,
			Domain:        in.Publish.Domain,
			Phase:         in.Publish.Phase,
			Relationships: toExchangeRelationships(in.Publish.Relationships),
		}
	}
	res, err := e.Writer.Reconcile(req)
	if err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{
		Applied: res.Status == ekawrite.ReconcileApplied,
		Status:  string(res.Status),
	}, nil
}

// toExchangeRelationships maps the loop's minimal edge to the exchange type
// ekawrite consumes.
func toExchangeRelationships(rels []Relationship) []exchange.Relationship {
	if len(rels) == 0 {
		return nil
	}
	out := make([]exchange.Relationship, 0, len(rels))
	for _, r := range rels {
		out = append(out, exchange.Relationship{Type: r.Type, Target: r.Target})
	}
	return out
}
