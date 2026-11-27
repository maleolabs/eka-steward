package cmd

import (
	"github.com/maleolabs/eka-steward/v2/internal/app"
	"github.com/spf13/cobra"
)

// newRunCommand builds `eka-steward run`: it assembles the production control
// loop (config → runtime → Source/Writer/Dispatcher/Recorder/Approver) and
// drives a project to quiescence under the single-instance lock, then prints
// the deterministic run report to stdout. Every flag maps to one app.Options
// field; the exit code is the process-wide contract (0 success, 1 any error),
// and no flag or path reads a terminal — the approval channel is the TTY-free
// file/queue approver, so `run` never blocks on a TTY.
func newRunCommand() *cobra.Command {
	var opts app.Options

	c := &cobra.Command{
		Use:   "run",
		Short: "Drive an EKA project to quiescence along the deterministic control loop",
		Long: `Drive an EKA project to quiescence: read its dispatchable work items
through api.Knowledge, plan them in the deterministic order, dispatch each
through the configured ACP driver, apply the single audited EKA write
(internal/ekawrite), and print a deterministic run report.

The run is checkpointed: a killed run resumes from its checkpoint on the next
run without re-dispatching or double-applying a write (the checkpoint intent
log plus the ekawrite replay gate). Exit code is 0 on success and 1 on any
error; no approval path requires a TTY.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			opts.LogWriter = c.ErrOrStderr()
			a, err := app.Assemble(opts)
			if err != nil {
				return err
			}
			defer a.Close()
			if err := a.Run(c.Context()); err != nil {
				return err
			}
			return a.WriteReport(c.OutOrStdout())
		},
	}

	f := c.Flags()
	f.StringVar(&opts.Project, "project", "", "EKA project id to drive (required)")
	f.StringVar(&opts.ConfigPath, "config", "", "path to the steward config file (required in production)")
	f.StringVar(&opts.Driver, "driver", "", "driver tool name (overrides the role's driver)")
	f.StringVar(&opts.Role, "role", "", "worker role name to dispatch as (required)")
	f.StringVar(&opts.Repo, "repo", "", "repository root the transitions are addressed from (walks up to its eka.yaml)")
	f.StringVar(&opts.StateDir, "state-dir", "", "steward state directory (default: $EKA_STEWARD_STATE_DIR, $EKA_HOME/steward, ~/.eka/steward)")
	f.StringVar(&opts.DispatchState, "dispatch-state", "todo", "state a dispatched item moves to (empty disables the write path)")
	_ = c.MarkFlagRequired("project")

	return c
}
