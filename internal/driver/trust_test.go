package driver

import (
	"errors"
	"strings"
	"testing"
)

// TestSpecTrustClassifiesKnownVsUnknown pins the KNOWN rule: a driver is
// KNOWN iff it carries BOTH an exact version and a checksum.
func TestSpecTrustClassifiesKnownVsUnknown(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want Trust
	}{
		{"pinned", Spec{Tool: "claude", Version: "1.0.0", Checksum: "abc"}, TrustKnown},
		{"no version", Spec{Tool: "claude", Checksum: "abc"}, TrustUnknown},
		{"no checksum", Spec{Tool: "claude", Version: "1.0.0"}, TrustUnknown},
		{"neither", Spec{Tool: "claude"}, TrustUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Trust(); got != tc.want {
				t.Fatalf("Trust() = %v (%s), want %v (%s)", got, got, tc.want, tc.want)
			}
		})
	}
}

// TestRequireKnownRefusesUnpinned is AC1 at the registry-build acceptance
// point: an unpinned driver is refused with a stable error that names it and
// wraps driver.ErrUntrusted. The primitive's pure classification alone is not
// enough — this asserts the gate.
func TestRequireKnownRefusesUnpinned(t *testing.T) {
	specs := []Spec{
		{Tool: "claude", Cmd: "claude", Version: "1.0.0", Checksum: "aa"}, // known
		{Tool: "codex", Cmd: "codex"},                                     // unpinned
	}
	err := RequireKnown(specs)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("err = %v, want ErrUntrusted", err)
	}
	if !strings.Contains(err.Error(), `"codex"`) {
		t.Fatalf("error %q must name the unpinned tool", err)
	}
}

// TestRequireKnownAcceptsPinned pins the accept side at the same gate.
func TestRequireKnownAcceptsPinned(t *testing.T) {
	if err := RequireKnown([]Spec{{Tool: "claude", Cmd: "claude", Version: "1.0.0", Checksum: "aa"}}); err != nil {
		t.Fatalf("RequireKnown refused a pinned driver: %v", err)
	}
	if err := RequireKnown(nil); err != nil {
		t.Fatalf("empty set must be vacuously known, got %v", err)
	}
}

// TestRequireKnownEmptyToolIsRefused pins the fail-closed handling of an
// empty tool at the gate.
func TestRequireKnownEmptyToolIsRefused(t *testing.T) {
	if err := RequireKnown([]Spec{{Tool: "", Version: "1.0.0", Checksum: "aa"}}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("err = %v, want ErrUntrusted for an empty tool", err)
	}
}

// TestParseTrustedIsFailClosed is AC1 at the registry-build entry point: the
// trusted parse refuses an unpinned driver, while the permissive Parse still
// accepts it (pinning is an acceptance gate, not a parse rule).
func TestParseTrustedIsFailClosed(t *testing.T) {
	pinned := "drivers:\n  - tool: claude\n    cmd: claude\n    version: 1.0.0\n    checksum: aa\n"
	unpinned := "drivers:\n  - tool: claude\n    cmd: claude\n"

	if _, err := ParseTrusted([]byte(pinned)); err != nil {
		t.Fatalf("ParseTrusted refused a pinned driver: %v", err)
	}
	if _, err := Parse([]byte(unpinned)); err != nil {
		t.Fatalf("Parse must still accept an unpinned driver: %v", err)
	}
	_, err := ParseTrusted([]byte(unpinned))
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), `"claude"`) {
		t.Fatalf("ParseTrusted(unpinned) = %v, want ErrUntrusted naming claude", err)
	}
}

// TestUnknownToolErrorIsStable is AC4: an unknown tool fails with a
// deterministic message that names it, stable across parse, resolve and probe.
func TestUnknownToolErrorIsStable(t *testing.T) {
	const want = `driver: unknown tool "ghost"`

	_, perr := Parse([]byte("drivers:\n  - tool: ghost\n    cmd: ghost\n"))
	if perr == nil || !errors.Is(perr, ErrUnknownTool) || perr.Error() != want {
		t.Fatalf("Parse(ghost) = %q, want exactly %q", perr, want)
	}

	r := mustParse(t, validConfig)
	_, rerr := r.Resolve("ghost")
	if rerr == nil || !errors.Is(rerr, ErrUnknownTool) || rerr.Error() != want {
		t.Fatalf("Resolve(ghost) = %q, want exactly %q", rerr, want)
	}

	// Stability: the identical input yields the identical message.
	_, again := r.Resolve("ghost")
	if again.Error() != rerr.Error() {
		t.Fatalf("unknown-tool error not stable: %q vs %q", rerr, again)
	}
}
