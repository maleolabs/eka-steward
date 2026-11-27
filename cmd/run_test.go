package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunCommandHelp proves `run` is wired into the root tree and advertises
// its flags without requiring a TTY or a project.
func TestRunCommandHelp(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Execute([]string{"run", "--help"}, nil, &out, &errBuf); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	help := out.String()
	for _, flag := range []string{"--project", "--config", "--driver", "--role", "--repo", "--state-dir", "--dispatch-state"} {
		if !strings.Contains(help, flag) {
			t.Fatalf("run help missing %s:\n%s", flag, help)
		}
	}
}

// TestRunCommandRequiresProject proves the required-flag contract is
// deterministic: no --project is a non-zero exit with a single error line.
func TestRunCommandRequiresProject(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := Execute([]string{"run"}, nil, &out, &errBuf); code == 0 {
		t.Fatalf("exit code = 0, want non-zero (stdout: %s)", out.String())
	}
	if !strings.Contains(errBuf.String(), "project") {
		t.Fatalf("stderr = %q, want it to name the missing project flag", errBuf.String())
	}
}
