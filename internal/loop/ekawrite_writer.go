package loop

import (
	"context"
	"fmt"

	"github.com/maleolabs/eka-core/v2/api"
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
