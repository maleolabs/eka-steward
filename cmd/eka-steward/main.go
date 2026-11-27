// Command eka-steward is the standalone EKA Steward binary: a thin,
// deterministic orchestrator that drives an EKA project from discussion
// to execution by orchestrating AI coding-agent CLIs over ACP.
//
// It is a CONSUMER of eka-core: the command tree lives in the cmd
// package, which imports only the public contract
// github.com/maleolabs/eka-core/v2/api.
package main

import (
	"os"

	"github.com/maleolabs/eka-steward/v2/cmd"
)

func main() {
	os.Exit(cmd.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
