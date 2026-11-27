package driver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenCapConfig is a fully pinned driver set for the golden capabilities
// artifact: an ACP adapter (opencode) and two CLI adapters (claude, codex).
const goldenCapConfig = `drivers:
  - tool: opencode
    cmd: opencode
    args: [acp]
    version: 2.4.1
    checksum: 1111111111111111111111111111111111111111111111111111111111111111
  - tool: claude
    cmd: claude
    version: 1.2.3
    checksum: 2222222222222222222222222222222222222222222222222222222222222222
  - tool: codex
    cmd: codex
    version: 0.9.0
    checksum: 3333333333333333333333333333333333333333333333333333333333333333
`

// goldenProbedAt is the fixed injected timestamp: the payload never reads a
// wall clock, so the golden bytes are stable.
const goldenProbedAt = "2026-01-01T00:00:00Z"

// buildGoldenCapabilities probes every driver in goldenCapConfig with a fake
// Runner and assembles the capabilities document.
func buildGoldenCapabilities(t *testing.T) CapabilitiesFile {
	t.Helper()
	reg, err := ParseTrusted([]byte(goldenCapConfig))
	if err != nil {
		t.Fatalf("ParseTrusted: %v", err)
	}
	outputs := map[string]string{
		"opencode": opencodeInitialize,
		"claude":   "1.2.3\n",
		"codex":    "codex-cli 0.9.0\n(rest)\n",
	}
	probed := make(map[string]Capabilities, len(outputs))
	for tool, out := range outputs {
		caps, err := reg.Probe(context.Background(), tool, &fakeRunner{out: []byte(out)})
		if err != nil {
			t.Fatalf("Probe(%s): %v", tool, err)
		}
		probed[tool] = caps
	}
	f, err := BuildCapabilities(reg, probed, goldenProbedAt)
	if err != nil {
		t.Fatalf("BuildCapabilities: %v", err)
	}
	return f
}

// TestCapabilitiesGolden is AC3: the serialized capabilities.json is
// byte-identical to the committed golden. Set UPDATE_GOLDEN=1 to regenerate.
func TestCapabilitiesGolden(t *testing.T) {
	f := buildGoldenCapabilities(t)
	got, err := WriteCapabilities(f)
	if err != nil {
		t.Fatalf("WriteCapabilities: %v", err)
	}

	golden := filepath.Join("testdata", "capabilities.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("capabilities.json drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestCapabilitiesSerializationIsDeterministic is AC3's stability property:
// the same inputs yield byte-identical output, every run (no map iteration,
// no clock).
func TestCapabilitiesSerializationIsDeterministic(t *testing.T) {
	first, err := WriteCapabilities(buildGoldenCapabilities(t))
	if err != nil {
		t.Fatalf("WriteCapabilities: %v", err)
	}
	for i := 0; i < 25; i++ {
		got, err := WriteCapabilities(buildGoldenCapabilities(t))
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("run %d drifted from first", i)
		}
	}
}

// TestCapabilitiesRoundTripAndValidation is AC3's schema check: a valid
// document reads back deeply-equal, and an invalid one (bad schema,
// duplicate tool, malformed entry, unknown field) is refused.
func TestCapabilitiesRoundTripAndValidation(t *testing.T) {
	f := buildGoldenCapabilities(t)
	data, err := WriteCapabilities(f)
	if err != nil {
		t.Fatalf("WriteCapabilities: %v", err)
	}
	back, err := ReadCapabilities(data)
	if err != nil {
		t.Fatalf("ReadCapabilities: %v", err)
	}
	if len(back.Drivers) != len(f.Drivers) || back.Schema != CapabilitiesSchema {
		t.Fatalf("round trip = %+v, want %+v", back, f)
	}

	bad := []struct {
		name string
		doc  string
		want string
	}{
		{"bad schema", `{"schema":99,"drivers":[]}`, "schema"},
		{"duplicate tool", `{"schema":1,"drivers":[{"tool":"a","protocol":"cli"},{"tool":"a","protocol":"cli"}]}`, "duplicate"},
		{"malformed entry", `{"schema":1,"drivers":[{"tool":"a","protocol":"cli","capabilities":["nope"]}]}`, "malformed"},
		{"bad protocol", `{"schema":1,"drivers":[{"tool":"a","protocol":"weird"}]}`, "invalid protocol"},
		{"unknown field", `{"schema":1,"drivers":[],"extra":true}`, "unknown field"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadCapabilities([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadCapabilities(%s) = %v, want error containing %q", tc.doc, err, tc.want)
			}
		})
	}
}

// TestBuildCapabilitiesRequiresEveryProbe pins the completeness rule: a
// declared driver with no probe result is an error naming the tool.
func TestBuildCapabilitiesRequiresEveryProbe(t *testing.T) {
	reg := mustParse(t, validConfig)
	probed := map[string]Capabilities{"claude": {Kind: KindCLI}, "codex": {Kind: KindCLI}}
	_, err := BuildCapabilities(reg, probed, "")
	if err == nil || !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("BuildCapabilities with a missing probe = %v, want error naming opencode", err)
	}
}
