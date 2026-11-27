package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/maleolabs/eka-core/v2/api"
)

// TestVersionFlag proves the --version contract: the flag prints the bare
// build version (one line, no decoration) and exits 0.
func TestVersionFlag(t *testing.T) {
	old := version
	version = "2.0.0"
	t.Cleanup(func() { version = old })

	var out, errBuf bytes.Buffer
	code := Execute([]string{"--version"}, nil, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	if got := strings.TrimSpace(out.String()); got != "2.0.0" {
		t.Fatalf("--version output = %q, want %q", got, "2.0.0")
	}
}

// TestVersionCommand proves the `version` subcommand reports both axes
// from their single sources: the ldflags build version and the eka-core
// public-contract version.
func TestVersionCommand(t *testing.T) {
	old := version
	version = "2.0.0"
	t.Cleanup(func() { version = old })

	var out bytes.Buffer
	if code := Execute([]string{"version"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	text := out.String()
	if !strings.Contains(text, "eka-steward 2.0.0") {
		t.Fatalf("version output missing build version:\n%s", text)
	}
	if !strings.Contains(text, "eka-core api "+api.ContractVersion) {
		t.Fatalf("version output missing contract version:\n%s", text)
	}
}
