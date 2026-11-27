package cmd

import (
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/plugin"
	"github.com/spf13/cobra"
)

// newInstallCommand builds `eka-steward install --json`: the steward
// installs no agent artifacts, so this always prints a deterministic
// no-op result (an empty "installed" list) with no network access and
// no writes. It exists to satisfy the plugin contract's install
// subcommand; the steward declares no "install" capability, so the CLI
// never delegates artifact installation to it. Exactly `--json` is
// accepted.
func newInstallCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "install",
		Short: "Report the (empty) artifact install result as JSON",
		Long: `Print the steward's artifact install result as JSON. The steward installs
no agent artifacts, so the result is always an empty "installed" list —
deterministic, with no network access and no filesystem writes. Exactly
--json is accepted.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !jsonFlagSet(c) {
				return fmt.Errorf("usage: eka-steward install --json")
			}
			return writeJSON(c.OutOrStdout(), plugin.InstallResult{
				Installed: []string{},
				Note:      "eka-steward installs no agent artifacts",
			})
		},
	}
	c.Flags().Bool(flagJSON, false, "print the install result as JSON (required)")
	return c
}
