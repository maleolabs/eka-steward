package driver

import "testing"

// TestVendorAdapterTableIsPresentAndSorted is AC2: the pin/audit table is
// present for exactly the two vendor CLI adapters, in deterministic order.
func TestVendorAdapterTableIsPresentAndSorted(t *testing.T) {
	got := VendorAdapters()
	if len(got) != 2 {
		t.Fatalf("VendorAdapters() has %d entries, want 2", len(got))
	}
	if got[0].Tool != "claude" || got[1].Tool != "codex" {
		t.Fatalf("VendorAdapters() order = [%s %s], want [claude codex]", got[0].Tool, got[1].Tool)
	}
}

// TestVendorAdaptersAreFullyPinned is AC2: every adapter pins an exact CLI
// version AND a non-empty dependency set whose every dep is itself pinned
// (non-empty module + version). No ranges, no empty entries.
func TestVendorAdaptersAreFullyPinned(t *testing.T) {
	for _, a := range VendorAdapters() {
		if a.Tool == "" || a.CLI == "" || a.Cmd == "" {
			t.Fatalf("adapter %+v has an empty identity field", a)
		}
		if a.CLIVersion == "" {
			t.Fatalf("adapter %q has no pinned CLI version", a.Tool)
		}
		if len(a.Deps) == 0 {
			t.Fatalf("adapter %q declares no dependencies", a.Tool)
		}
		for _, d := range a.Deps {
			if d.Module == "" || d.Version == "" {
				t.Fatalf("adapter %q has an unpinned dep %+v", a.Tool, d)
			}
		}
	}
}

// TestVendorAdaptersCoverTheCLIAdapters is AC2: the vendor pin table covers
// exactly the CLI adapters of this package (claude, codex) — a drift between
// the two tables fails.
func TestVendorAdaptersCoverTheCLIAdapters(t *testing.T) {
	var cliAdapters []string
	for tool, ad := range adapters {
		if ad.kind == KindCLI {
			cliAdapters = append(cliAdapters, tool)
		}
	}
	if len(cliAdapters) != 2 {
		t.Fatalf("adapters has %d CLI adapters, want 2", len(cliAdapters))
	}
	for _, tool := range cliAdapters {
		if _, ok := vendorAdapter(tool); !ok {
			t.Fatalf("CLI adapter %q has no vendor pin record", tool)
		}
	}
}

// TestVendorAdaptersAreHonestlyMarked is the honesty pin: a placeholder pin
// (Verified == false) MUST NOT be claimed as network-verified. This test
// documents the current state — both adapters are offline placeholders — and
// fails the moment a pin is flipped to Verified without the reviewer also
// recording the provenance in the same change.
func TestVendorAdaptersAreHonestlyMarked(t *testing.T) {
	for _, a := range VendorAdapters() {
		if a.CLIVersion == vendorPlaceholderVersion && a.Verified {
			t.Fatalf("adapter %q pins the placeholder version but claims Verified — do not fake a verified pin", a.Tool)
		}
		if !a.Verified && a.Note == "" {
			t.Fatalf("adapter %q is an unverified placeholder but carries no provenance note", a.Tool)
		}
	}
}

// TestVendorAdaptersReturnACopy pins that the accessor cannot leak the
// package table: mutating a returned record leaves the next call unchanged.
func TestVendorAdaptersReturnACopy(t *testing.T) {
	a := VendorAdapters()
	a[0].CLIVersion = "tampered"
	a[0].Deps[0].Version = "tampered"
	again := VendorAdapters()
	if again[0].CLIVersion == "tampered" || again[0].Deps[0].Version == "tampered" {
		t.Fatal("VendorAdapters() leaked the package table to the caller")
	}
}
