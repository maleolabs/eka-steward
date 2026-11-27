package driver

import "sort"

// VendorDep is one pinned dependency of a vendor CLI adapter. Both fields
// are required: a dep with an empty Module or Version is not a pin.
type VendorDep struct {
	// Module is the dependency identifier (an npm package, or the vendor
	// CLI itself), e.g. "@agentclientprotocol/sdk".
	Module string
	// Version is the exact pinned version string.
	Version string
}

// VendorAdapter is the in-repo PIN/AUDIT record for one vendor CLI adapter
// (Claude Code, Codex). It pins the exact vendor CLI version and the
// adapter's dependency set so the reviewable surface is a single table, not
// a scattered set of loose ranges.
//
// Verified is the honesty bit: false means the pin is a DOCUMENTED
// PLACEHOLDER that was NOT resolved against the network in this change.
// Nothing in this repo claims a network-verified pin while Verified is
// false — a test asserts that (VendorAdaptersAreHonestlyMarked). See
// docs/drivers.md, "Vendor CLI adapters — pin and audit".
type VendorAdapter struct {
	// Tool is the adapter key (must match an adapters key, e.g. "claude").
	Tool string
	// CLI is the vendor CLI product name (for humans, e.g. "Claude Code").
	CLI string
	// Cmd is the executable base name (must match the driver cmd).
	Cmd string
	// CLIVersion is the exact pinned vendor CLI version.
	CLIVersion string
	// Deps is every pinned adapter dependency (declared set, no ranges).
	Deps []VendorDep
	// Verified reports whether the pin was resolved against the network.
	Verified bool
	// Note records the pin's provenance / upgrade path (honest).
	Note string
}

// ponytail: the two vendor CLI versions below are documented placeholders —
// this change is offline (GOPROXY=off) and could not resolve a real vendor
// release. Upgrade path: replace each placeholderCLI with the exact version
// printed by `<cmd> --version` for the pinned release and flip Verified to
// true in the same reviewed change. Until then Verified stays false and the
// docs say "asserted, not network-verified".
const vendorPlaceholderVersion = "0.0.0-unresolved"

// vendorAdapters is the fixed pin/audit table. It is a table (not a map) so
// ordering is explicit and the record is stable to review and diff.
var vendorAdapters = []VendorAdapter{
	{
		Tool:       "claude",
		CLI:        "Claude Code",
		Cmd:        "claude",
		CLIVersion: vendorPlaceholderVersion,
		Deps: []VendorDep{
			{Module: "claude", Version: vendorPlaceholderVersion},
			{Module: "@agentclientprotocol/sdk", Version: vendorPlaceholderVersion},
		},
		Verified: false,
		Note:     "placeholder; resolve `claude --version` for the pinned release and update the ACP bridge dep set",
	},
	{
		Tool:       "codex",
		CLI:        "Codex",
		Cmd:        "codex",
		CLIVersion: vendorPlaceholderVersion,
		Deps: []VendorDep{
			{Module: "codex", Version: vendorPlaceholderVersion},
			{Module: "@agentclientprotocol/sdk", Version: vendorPlaceholderVersion},
		},
		Verified: false,
		Note:     "placeholder; resolve `codex --version` for the pinned release and update the ACP bridge dep set",
	},
}

// VendorAdapters returns a copy of the pinned vendor-adapter table, sorted
// by Tool, each with a copy of its Deps slice (callers cannot mutate the
// package table). Deterministic.
func VendorAdapters() []VendorAdapter {
	out := make([]VendorAdapter, len(vendorAdapters))
	for i, a := range vendorAdapters {
		a.Deps = append([]VendorDep(nil), a.Deps...)
		out[i] = a
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// vendorAdapter returns the pinned record for tool (zero value + false when
// no such adapter is pinned).
func vendorAdapter(tool string) (VendorAdapter, bool) {
	for _, a := range vendorAdapters {
		if a.Tool == tool {
			return a, true
		}
	}
	return VendorAdapter{}, false
}
