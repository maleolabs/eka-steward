package plugin

import (
	"encoding/json"
	"reflect"
	"testing"

	coreplugin "github.com/maleolabs/eka-core/v2/plugin"
)

// TestBuildManifestMatchesCoreContract proves the steward manifest is a
// valid eka-core contract-v1 manifest: it unmarshals cleanly into
// coreplugin.Manifest (the shared contract) with the expected core
// fields. This test MAY import eka-core/plugin (tests are the documented
// boundary exemption); the production code must not.
func TestBuildManifestMatchesCoreContract(t *testing.T) {
	b, err := json.Marshal(BuildManifest("2.0.0"))
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	var m coreplugin.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("steward manifest does not parse into eka-core plugin.Manifest: %v", err)
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
	if m.Description == "" {
		t.Error("description is empty")
	}
	if m.Source != "github.com/maleolabs/eka-steward" {
		t.Errorf("source = %q, want %q", m.Source, "github.com/maleolabs/eka-steward")
	}
	if len(m.Artifacts) != 0 {
		t.Errorf("artifacts = %+v, want none", m.Artifacts)
	}
	if len(m.Capabilities) != 0 {
		t.Errorf("capabilities = %+v, want none (steward installs no artifacts and is not an MCP server)", m.Capabilities)
	}
}

// TestBuildManifestCommandsExtension proves the eka-cli v1 additive
// "commands" array (ADR-031) is emitted in the shape the CLI registers:
// one command named "steward" with an empty args list, so `eka steward
// <args>` dispatches to `eka-steward <args>`.
func TestBuildManifestCommandsExtension(t *testing.T) {
	// The pluginCommandManifest shape (eka-cli cmd/plugin_registration.go):
	// the core Manifest plus the additive commands array.
	var ext struct {
		coreplugin.Manifest
		Commands []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Args        []string `json:"args"`
		} `json:"commands"`
	}
	b, err := json.Marshal(BuildManifest("2.0.0"))
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := json.Unmarshal(b, &ext); err != nil {
		t.Fatalf("manifest does not parse into the CLI commands-extension shape: %v", err)
	}
	if len(ext.Commands) != 1 {
		t.Fatalf("commands = %+v, want exactly one", ext.Commands)
	}
	got := ext.Commands[0]
	if got.Name != "steward" {
		t.Errorf("commands[0].name = %q, want %q", got.Name, "steward")
	}
	if got.Description == "" {
		t.Error("commands[0].description is empty")
	}
	if len(got.Args) != 0 {
		t.Errorf("commands[0].args = %v, want empty (whole-binary passthrough)", got.Args)
	}
}

// TestBuildManifestDeterministic proves the manifest carries no
// timestamps or other run-to-run variation.
func TestBuildManifestDeterministic(t *testing.T) {
	if !reflect.DeepEqual(BuildManifest("2.0.0"), BuildManifest("2.0.0")) {
		t.Fatal("BuildManifest is not deterministic for the same version")
	}
}
