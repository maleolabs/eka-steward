package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestInstallCommandJSON proves `install --json` prints a deterministic
// no-op result with an empty "installed" list.
func TestInstallCommandJSON(t *testing.T) {
	run := func() (int, string, string) {
		var out, errBuf bytes.Buffer
		code := Execute([]string{"install", "--json"}, nil, &out, &errBuf)
		return code, out.String(), errBuf.String()
	}

	code, out, errStr := run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errStr)
	}
	var res struct {
		Installed []string `json:"installed"`
		Note      string   `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("install output is not valid JSON: %v\n%s", err, out)
	}
	if len(res.Installed) != 0 {
		t.Errorf("installed = %v, want empty", res.Installed)
	}
	if res.Note == "" {
		t.Error("note is empty")
	}

	// Deterministic: a second run yields byte-identical output.
	_, out2, _ := run()
	if out != out2 {
		t.Errorf("install output is not deterministic:\n%s\n--- vs ---\n%s", out, out2)
	}
}

// TestInstallCommandRequiresJSON proves the JSON-only contract.
func TestInstallCommandRequiresJSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Execute([]string{"install"}, nil, &out, &errBuf); code == 0 {
		t.Fatalf("exit code = 0, want non-zero for a missing --json")
	}
	if !strings.Contains(errBuf.String(), "--json") {
		t.Fatalf("stderr = %q, want it to name --json", errBuf.String())
	}
}

// TestInstallResultEmptyListShape pins the wire shape: "installed" is an
// empty JSON array, never null.
func TestInstallResultEmptyListShape(t *testing.T) {
	var out bytes.Buffer
	if code := Execute([]string{"install", "--json"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := strings.TrimSpace(string(raw["installed"])); got != "[]" {
		t.Errorf("installed = %s, want []", got)
	}
}
