package acp

import (
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// TestNewDispatcherThreadsEnv guards the worker-environment wiring: the
// DispatcherConfig.Env must reach the escapehatch Spec the dispatcher launches
// (so secrets.Broker.WorkerEnv's declared credentials reach the agent), and a
// nil Env must stay nil (escapehatch's empty-environment contract, never the
// parent's).
func TestNewDispatcherThreadsEnv(t *testing.T) {
	env := []string{"TZ=UTC", "FOO=bar"}
	d, err := NewDispatcher(DispatcherConfig{
		Driver: driver.Spec{Tool: "opencode", Cmd: "opencode", Kind: driver.KindACP},
		Env:    env,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if len(d.spec.Env) != len(env) || d.spec.Env[0] != "TZ=UTC" || d.spec.Env[1] != "FOO=bar" {
		t.Fatalf("spec.Env = %v, want %v", d.spec.Env, env)
	}

	none, err := NewDispatcher(DispatcherConfig{
		Driver: driver.Spec{Tool: "opencode", Cmd: "opencode", Kind: driver.KindACP},
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	if none.spec.Env != nil {
		t.Fatalf("spec.Env = %v, want nil (empty-environment contract)", none.spec.Env)
	}
}
