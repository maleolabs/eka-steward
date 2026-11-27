// Package cmd implements the eka-steward binary as a thin Cobra command
// layer.
//
// Steward is a CONSUMER of eka-core (ADR-014, ADR-030): production code
// in this package may import the public contract
// github.com/maleolabs/eka-core/v2/api and nothing else from eka-core —
// the kernel-internal packages (runtime, store, workspace, sync) are
// private implementation details. cmd/boundary_arch_test.go enforces the
// boundary, and cmd/layout_test.go pins the top-level layout.
//
// Scope of this cut: a scaffold. The ACP control loop, the driver
// registry and the ACP client are separate work items and are
// deliberately absent — no abstraction is pre-built for them.
package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// flagVersion is the persistent root flag that prints the bare build
// version and exits — the same value `version` reports.
const flagVersion = "version"

// Execute runs the steward with the given arguments and streams and
// returns the process exit code. It is the single testable entry point:
// main delegates to it, tests call it directly.
//
// Exit codes: 0 on success, 1 on any error (the error is rendered once
// as a single "eka-steward: ..." line on stderr).
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if args == nil {
		args = []string{}
	}
	root := newRootCommand()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(stderr, "eka-steward: %s\n", err)
		return 1
	}
	return 0
}

// newRootCommand builds a fresh command tree per Execute call so
// SetArgs/SetIn/SetOut/SetErr never leak between invocations.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "eka-steward",
		Short: "Deterministic orchestrator that drives an EKA project over ACP",
		Long: `eka-steward orchestrates AI coding-agent CLIs over the Agent Client
Protocol (ACP) to drive an EKA project from discussion to execution.

It is a thin, deterministic standalone binary and a consumer of
eka-core's public contract (github.com/maleolabs/eka-core/v2/api): it
holds no domain logic of its own.

This is a scaffold release: the version surface is available; the ACP
control loop, the driver registry and the ACP client ship in later work
items.

Run 'eka-steward version' for the build and contract versions.`,
		// The binary owns all error output: SilenceErrors + SilenceUsage
		// suppress cobra's "Error: ..." prefix and automatic usage dumps;
		// Execute renders every error as one deterministic line.
		SilenceErrors: true,
		SilenceUsage:  true,
		// `eka-steward` with no subcommand prints help (exit 0).
		// `eka-steward --version` prints the bare build version instead.
		RunE: func(c *cobra.Command, _ []string) error {
			if show, _ := c.Flags().GetBool(flagVersion); show {
				fmt.Fprintln(c.OutOrStdout(), version)
				return nil
			}
			return c.Help()
		},
	}
	root.PersistentFlags().Bool(flagVersion, false,
		"print the steward build version and exit")
	root.AddCommand(newVersionCommand())
	root.AddCommand(newRunCommand())
	root.AddCommand(newServeCommand())
	root.AddCommand(newManifestCommand())
	root.AddCommand(newInstallCommand())
	return root
}
