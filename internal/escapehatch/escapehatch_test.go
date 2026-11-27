package escapehatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHelperProcess is not a real test: it is the child process the Run
// tests spawn. It is a no-op unless the special "-eka-helper" argument
// is present (placed after "--" so the test framework ignores it).
//
// When triggered it writes a fixed payload — "out" plus the value of
// PARENT_ONLY (empty unless leaked) to stdout, "err" to stderr — and
// exits with HELPER_EXIT (default 0).
func TestHelperProcess(t *testing.T) {
	trigger := false
	for _, a := range os.Args {
		if a == "-eka-helper" {
			trigger = true
		}
	}
	if !trigger {
		return
	}
	_, _ = os.Stdout.WriteString("out" + os.Getenv("PARENT_ONLY"))
	_, _ = os.Stderr.WriteString("err")
	code := 0
	if v := os.Getenv("HELPER_EXIT"); v != "" {
		code, _ = strconv.Atoi(v)
	}
	os.Exit(code)
}

// selfBinary returns the path to this test binary and its base name
// (what the AllowList matches on).
func selfBinary() (path, base string) {
	path = os.Args[0]
	return path, filepath.Base(path)
}

// specSelf builds a Spec that spawns this test binary's helper process
// with the given environment.
func specSelf(env []string) Spec {
	path, _ := selfBinary()
	return Spec{
		Name: path,
		Args: []string{"-test.run=TestHelperProcess", "--", "-eka-helper"},
		Env:  env,
	}
}

func TestAllowedCommandRunsAndCapturesStreams(t *testing.T) {
	_, base := selfBinary()
	al := NewAllowList(base)

	res, err := al.Run(context.Background(), specSelf([]string{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := string(res.Stdout); got != "out" {
		t.Fatalf("stdout = %q, want %q", got, "out")
	}
	if got := string(res.Stderr); got != "err" {
		t.Fatalf("stderr = %q, want %q", got, "err")
	}
}

func TestNonZeroExitIsCapturedNotAnError(t *testing.T) {
	_, base := selfBinary()
	al := NewAllowList(base)

	res, err := al.Run(context.Background(), specSelf([]string{"HELPER_EXIT=3"}))
	if err != nil {
		t.Fatalf("Run returned an error for a clean non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

func TestDisallowedCommandRefusedDeterministically(t *testing.T) {
	al := NewAllowList("allowed-only")

	_, err := al.Run(context.Background(), Spec{Name: "curl"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v, want ErrNotAllowed", err)
	}
	if got := err.Error(); !strings.Contains(got, "curl") {
		t.Fatalf("refusal must name the command: %v", err)
	}

	// Same input -> byte-identical refusal.
	_, err2 := al.Run(context.Background(), Spec{Name: "curl"})
	if err2 == nil || err2.Error() != err.Error() {
		t.Fatalf("refusal not deterministic: %v vs %v", err, err2)
	}
}

func TestUnknownCommandNameErrors(t *testing.T) {
	al := NewAllowList("git", "go")

	_, err := al.Run(context.Background(), Spec{Name: "/usr/bin/definitely-not-allowed"})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v, want ErrNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "definitely-not-allowed") {
		t.Fatalf("unknown command error must name it: %v", err)
	}
}

func TestCancellationReturnsContextError(t *testing.T) {
	_, base := selfBinary()
	al := NewAllowList(base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := al.Run(ctx, specSelf([]string{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNoEnvironmentLeakage(t *testing.T) {
	t.Setenv("PARENT_ONLY", "secret")

	_, base := selfBinary()
	al := NewAllowList(base)

	// Empty Env must mean an empty child environment, not the parent's.
	res, err := al.Run(context.Background(), specSelf([]string{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != "out" {
		t.Fatalf("child saw leaked environment: stdout = %q, want %q", got, "out")
	}
}
