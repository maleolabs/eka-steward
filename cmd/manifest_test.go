package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	coreplugin "github.com/maleolabs/eka-core/v2/plugin"
)

// TestManifestCommandJSON proves `manifest --json` prints a valid
// contract-v1 manifest that parses into eka-core's plugin.Manifest and
// into the CLI's commands-extension shape, and reports the build
// version.
func TestManifestCommandJSON(t *testing.T) {
	old := version
	version = "2.0.0"
	t.Cleanup(func() { version = old })

	var out, errBuf bytes.Buffer
	if code := Execute([]string{"manifest", "--json"}, nil, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}

	var m coreplugin.Manifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("manifest output is not valid eka-core JSON: %v\n%s", err, out.String())
	}
	if m.Contract != coreplugin.ContractVersion {
		t.Errorf("contract = %q, want %q", m.Contract, coreplugin.ContractVersion)
	}
	if m.Name != "steward" {
		t.Errorf("name = %q, want %q", m.Name, "steward")
	}
	if m.Version != "2.0.0" {
		t.Errorf("version = %q, want %q", m.Version, "2.0.0")
	}
	if m.Source != "github.com/maleolabs/eka-steward" {
		t.Errorf("source = %q, want %q", m.Source, "github.com/maleolabs/eka-steward")
	}

	var ext struct {
		Commands []struct {
			Name string   `json:"name"`
			Args []string `json:"args"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &ext); err != nil {
		t.Fatalf("manifest output has no valid commands extension: %v", err)
	}
	if len(ext.Commands) != 1 || ext.Commands[0].Name != "steward" || len(ext.Commands[0].Args) != 0 {
		t.Fatalf("commands = %+v, want exactly [{steward, []}]", ext.Commands)
	}
}

// TestManifestCommandRequiresJSON proves the JSON-only contract: any
// invocation other than `manifest --json` is a non-zero usage error with
// no manifest on stdout.
func TestManifestCommandRequiresJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := Execute([]string{"manifest"}, nil, &out, &errBuf)
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero for a missing --json")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("stdout = %q, want empty on usage error", out.String())
	}
	if !strings.Contains(errBuf.String(), "--json") {
		t.Fatalf("stderr = %q, want it to name --json", errBuf.String())
	}
}
