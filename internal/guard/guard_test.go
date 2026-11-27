package guard

import (
	"reflect"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
)

// --- AC5: containment is default-on, no opt-out ---

func TestZeroConfigIsContained(t *testing.T) {
	var c Config
	if !c.Contained() {
		t.Fatal("zero Config is not contained; containment must be default-on")
	}
	// The zero Policy denies even a read-inside-root request: contained.
	got := c.Permission.Decide(PermissionRequest{
		ToolKind: ToolRead, Path: "/ws/a.txt", WorkspaceRoot: "/ws", OptionID: OptionAllowOnce,
	})
	if got != Deny {
		t.Fatalf("zero policy allowed a read (%v); zero value must be default-deny", got)
	}
}

func TestConfigHasNoOptOutField(t *testing.T) {
	bad := []string{"disable", "bypass", "optout", "opt_out", "insecure", "unsafe"}
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, b := range bad {
			if strings.Contains(name, b) {
				t.Fatalf("Config field %q looks like a containment opt-out; v1 must have none", rt.Field(i).Name)
			}
		}
	}
}

// --- AC1: workers run with the allow-listed env only ---

func TestWorkerEnvIsAllowListed(t *testing.T) {
	env, err := WorkerEnv()
	if err != nil {
		t.Fatalf("WorkerEnv(): %v", err)
	}
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		switch name {
		case "HOME", "EKA_HOME", "PATH":
			t.Errorf("worker env must not carry %s (%q)", name, kv)
		}
		upper := strings.ToUpper(name)
		for _, needle := range credentialNeedles {
			if strings.Contains(upper, needle) {
				t.Errorf("worker env must not carry a credential variable (%q)", kv)
			}
		}
	}
	// No HOME and no EKA_HOME => the home-derived default store path
	// (~/.eka) and host credential dotfiles (~/.ssh/...) are not derivable
	// from the ambient environment.
	if !ekawrite.WorkerHomeUnresolvable(env) {
		t.Error("worker env must leave the EKA home unresolvable")
	}
}

func TestWorkerEnvRefusesUnsafeExtensions(t *testing.T) {
	for _, kv := range []string{
		"HOME=/root",
		"EKA_HOME=/srv/eka",
		"PATH=/usr/bin",
		"GITHUB_TOKEN=x",
		"AWS_SECRET_ACCESS_KEY=x",
		"SSH_AUTH_SOCK=/tmp/agent",
		"OPENCODE_API_KEY=x",
	} {
		if _, err := WorkerEnv(kv); err == nil {
			t.Errorf("WorkerEnv(%q) accepted an unsafe variable", kv)
		}
	}
}

func TestWorkerEnvAllowsSafeExtension(t *testing.T) {
	base, err := WorkerEnv()
	if err != nil {
		t.Fatal(err)
	}
	got, err := WorkerEnv("TERM=xterm")
	if err != nil {
		t.Fatalf("WorkerEnv(TERM): %v", err)
	}
	if len(got) != len(base)+1 || got[len(got)-1] != "TERM=xterm" {
		t.Fatalf("safe extension not appended: %v", got)
	}
}
