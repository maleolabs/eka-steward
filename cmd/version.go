package cmd

import (
	"fmt"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/spf13/cobra"
)

// version is the steward build version. It defaults to "dev" and is
// overridable at build time:
//
//	go build -ldflags "-X github.com/maleolabs/eka-steward/v2/cmd.version=2.0.0" ./cmd/eka-steward
//
// It identifies the steward implementation, never the eka-core contract
// (that axis is api.ContractVersion).
var version = "dev"

// newVersionCommand builds `eka-steward version`: it prints the build
// version plus the eka-core public-contract version the steward was
// compiled against. The contract version is read from its single source
// (api.ContractVersion), never restated here.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long: `Print the steward build version (ldflags cmd.version, default "dev")
and the eka-core public-contract version it was compiled against.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			out := c.OutOrStdout()
			fmt.Fprintf(out, "eka-steward %s\n", version)
			fmt.Fprintf(out, "eka-core api %s\n", api.ContractVersion)
			return nil
		},
	}
}
