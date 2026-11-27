// Package plugin implements the eka-steward side of the eka-cli plugin
// contract (v1): the machine-readable self-description the CLI reads
// with `eka-steward manifest --json`, and the deterministic no-op result
// of `eka-steward install --json`.
//
// The JSON tags here mirror github.com/maleolabs/eka-core/v2/plugin
// exactly — that package is the shared contract and the single source
// of truth. It is deliberately NOT imported: the steward's documented
// boundary is api-only (cmd/root.go), so the sibling shapes are restated
// here rather than coupled. A test proves the two stay compatible.
//
// The `commands` array is the eka-cli v1 additive extension (ADR-031):
// the CLI registers each entry as `eka <name>` and dispatches it to the
// plugin executable with the declared args followed by the user's args.
// The steward declares one command (`steward`, no args) so `eka steward
// <args>` reaches `eka-steward <args>` unchanged.
package plugin

// ContractVersion is the plugin contract version this manifest is
// written against. It mirrors eka-core's plugin.ContractVersion: a
// mismatch is a refusal on the CLI side, never a silent
// misinterpretation.
const ContractVersion = "v1"

// Manifest is the plugin self-description emitted for `manifest --json`.
// Its JSON keys are the contract: contract, name, version, description,
// artifacts, capabilities, source and commands (the eka-cli extension).
type Manifest struct {
	// Contract is the contract version (must equal ContractVersion).
	Contract string `json:"contract"`
	// Name is the stable plugin identity ("steward").
	Name string `json:"name"`
	// Version is the build version (the ldflags cmd.version value).
	Version string `json:"version"`
	// Description is a one-line human-readable summary.
	Description string `json:"description"`
	// Artifacts lists installable artifact families (none for steward).
	Artifacts []Artifact `json:"artifacts"`
	// Capabilities declares what the plugin can do ("install" and/or
	// "mcp"). The steward installs no agent artifacts and is not an MCP
	// server, so it declares none.
	Capabilities []string `json:"capabilities"`
	// Source is the canonical source repository of the plugin.
	Source string `json:"source"`
	// Commands are the CLI commands this plugin registers — the eka-cli
	// v1 additive extension. An empty/nil array registers nothing.
	Commands []Command `json:"commands"`
}

// Artifact is one installable family (mirrors eka-core's Artifact). The
// steward has none, but the type keeps the manifest shape contract-exact.
type Artifact struct {
	// Kind is the family name ("skills", "commands", ...).
	Kind string `json:"kind"`
	// Entries are the artifact names within the family.
	Entries []string `json:"entries"`
}

// Command is one command the CLI registers under this plugin (the
// eka-cli v1 dispatch-protocol extension).
type Command struct {
	// Name is the CLI command name (e.g. "steward").
	Name string `json:"name"`
	// Description is the one-line help text shown in `eka --help`.
	Description string `json:"description"`
	// Args are the fixed arguments inserted between the plugin
	// executable and the user's args (none: whole-binary passthrough).
	Args []string `json:"args"`
}

// InstallResult is the machine-readable result of `install --json`.
// The steward installs no agent artifacts, so the list is always empty
// and deterministic; Note explains the no-op.
type InstallResult struct {
	// Installed are the artifact names installed (always empty).
	Installed []string `json:"installed"`
	// Note is the deterministic human-readable explanation.
	Note string `json:"note"`
}

// stewardDescription is the one-line summary shared by the manifest and
// its command entry.
const stewardDescription = "Deterministic orchestrator that drives an EKA project over ACP"

// BuildManifest returns the steward's plugin manifest for the given
// build version. It is deterministic — no timestamps, no environment.
func BuildManifest(version string) Manifest {
	return Manifest{
		Contract:     ContractVersion,
		Name:         "steward",
		Version:      version,
		Description:  stewardDescription,
		Artifacts:    []Artifact{},
		Capabilities: []string{},
		Source:       "github.com/maleolabs/eka-steward",
		Commands: []Command{{
			Name:        "steward",
			Description: stewardDescription,
			Args:        []string{},
		}},
	}
}
