package driver

import (
	"context"

	"github.com/maleolabs/eka-steward/v2/internal/escapehatch"
)

// ExecRunner is the production Runner for driver probing. It is a thin
// adapter over internal/escapehatch — the ONE audited subprocess package
// (docs/boundary.md, rule 2). It never spawns a process itself, and it can
// only run executables whose base name is on its AllowList.
//
// This is the ONLY file in internal/driver that imports escapehatch, so the
// pure core (registry.go) and the probe parse (probe.go) stay free of any
// subprocess dependency and remain unit-testable with a fake Runner.
type ExecRunner struct {
	allow escapehatch.AllowList
}

// NewExecRunner builds an ExecRunner whose AllowList is exactly the given
// executable base names — no default, no wildcard (escapehatch policy).
func NewExecRunner(cmds ...string) ExecRunner {
	return ExecRunner{allow: escapehatch.NewAllowList(cmds...)}
}

// Run executes spec (Cmd + Args) through the escape hatch and returns the
// captured stdout. A disallowed command fails with escapehatch.ErrNotAllowed.
// Note: escapehatch has no stdin pipe, so this Runner suits CLI (`--version`)
// probes today; the ACP initialize round-trip needs the future streaming
// client (docs/drivers.md, "deferred").
func (r ExecRunner) Run(ctx context.Context, spec Spec) ([]byte, error) {
	res, err := r.allow.Run(ctx, escapehatch.Spec{Name: spec.Cmd, Args: spec.Args})
	if err != nil {
		return nil, err
	}
	return res.Stdout, nil
}

// Runner builds the production Runner for this registry: an escapehatch
// AllowList of exactly the declared driver commands. The allow-list is a
// deterministic function of the config, so the set of executables the
// steward may probe is auditable from the config alone.
func (r *Registry) Runner() ExecRunner {
	cmds := make([]string, len(r.specs))
	for i, s := range r.specs {
		cmds[i] = s.Cmd
	}
	return NewExecRunner(cmds...)
}
