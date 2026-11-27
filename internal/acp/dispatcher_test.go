package acp

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// TestACPHelperAgent is not a real test: it is the fake ACP agent the
// Dispatcher test spawns through internal/escapehatch. It runs only when the
// special "-eka-acp-agent" argument is present (placed after "--"), speaks the
// fake protocol on stdin/stdout, then os.Exit so the Go test harness never
// writes its own PASS line onto the protocol stream.
func TestACPHelperAgent(t *testing.T) {
	if !hasArg(os.Args, "-eka-acp-agent") {
		return
	}
	f := newFakeAgent(parseFakeArgs(os.Args))
	f.serve(os.Stdin, os.Stdout)
	os.Exit(0)
}

// hasArg reports whether args contains want.
func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// newTestDispatcher builds a Dispatcher whose driver is this test binary
// re-invoked as the fake agent. extra holds fake-agent behaviour flags.
func newTestDispatcher(t *testing.T, ccfg Config, extra ...string) *Dispatcher {
	t.Helper()
	args := append([]string{"-test.run=TestACPHelperAgent", "--", "-eka-acp-agent"}, extra...)
	d, err := NewDispatcher(DispatcherConfig{
		Driver: driver.Spec{
			Tool: "fake",
			Cmd:  os.Args[0],
			Args: args,
			Kind: driver.KindACP,
		},
		Client: ccfg,
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func TestDispatcherRunsAgainstFakeAgent(t *testing.T) {
	d := newTestDispatcher(t, Config{})
	res, err := d.Dispatch(context.Background(), decision.Candidate{
		ID:    "eka/sto:item-1",
		State: decision.StateTodo,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.CandidateID != "eka/sto:item-1" {
		t.Fatalf("CandidateID = %q", res.CandidateID)
	}
	if res.Output != "ABC" {
		t.Fatalf("Output = %q, want ABC", res.Output)
	}
}

func TestDispatcherCancelReturnsNoOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newTestDispatcher(t, Config{
		OnUpdate: func(SessionUpdate) { cancel() },
	}, "-fake-hang")

	res, err := d.Dispatch(ctx, decision.Candidate{ID: "eka/sto:item-2", State: decision.StateTodo})
	if err == nil {
		t.Fatal("Dispatch returned nil error on cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch err = %v, want context.Canceled", err)
	}
	if res.CandidateID != "" || res.Output != "" {
		t.Fatalf("cancelled dispatch leaked a result: %+v", res)
	}
}

func TestNewDispatcherRejectsNonACP(t *testing.T) {
	_, err := NewDispatcher(DispatcherConfig{
		Driver: driver.Spec{Tool: "claude", Cmd: "claude", Kind: driver.KindCLI},
	})
	if err == nil {
		t.Fatal("want error for a non-ACP driver")
	}
}
