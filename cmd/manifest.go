package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/maleolabs/eka-steward/v2/internal/plugin"
	"github.com/spf13/cobra"
)

// flagJSON is the machine-readable output flag shared by the plugin
// subcommands (`manifest --json`, `install --json`).
const flagJSON = "json"

// newManifestCommand builds `eka-steward manifest --json`: it prints the
// plugin contract-v1 manifest (internal/plugin) using the same build
// version `version` reports. Exactly `--json` is accepted; any other
// invocation is a usage error.
func newManifestCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "manifest",
		Short: "Print the eka-cli plugin manifest as JSON",
		Long: `Print the eka-cli plugin contract (v1) self-description as indented
JSON on stdout. The CLI reads this with "eka-steward manifest --json" to
discover the plugin and register the "steward" command. Exactly --json is
accepted; the output is deterministic.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !jsonFlagSet(c) {
				return fmt.Errorf("usage: eka-steward manifest --json")
			}
			return writeJSON(c.OutOrStdout(), plugin.BuildManifest(version))
		},
	}
	c.Flags().Bool(flagJSON, false, "print the manifest as JSON (required)")
	return c
}

// jsonFlagSet reports whether the --json flag was passed. The plugin
// contract is JSON-only, so the flag is a required marker, not a toggle.
func jsonFlagSet(c *cobra.Command) bool {
	v, _ := c.Flags().GetBool(flagJSON)
	return v
}

// writeJSON writes v as indented JSON with a trailing newline — the
// deterministic machine-readable output of the plugin subcommands.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
