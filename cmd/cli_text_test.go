package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/ui"
)

// TestVersionCommandMargin proves the version lines carry the container
// left margin (the design-system Style.W), one indented line per axis.
func TestVersionCommandMargin(t *testing.T) {
	old := version
	version = "2.0.0"
	t.Cleanup(func() { version = old })

	var out bytes.Buffer
	if code := Execute([]string{"version"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("version output lines = %d, want 2:\n%q", len(lines), out.String())
	}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, ui.Margin) {
			t.Fatalf("version line %q missing margin %q", ln, ui.Margin)
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("non-TTY version output contains ANSI: %q", out.String())
	}
}

// TestRootHelpDropsScaffold proves the stale "scaffold" wording is gone
// from the root help: it now describes the shipped surface.
func TestRootHelpDropsScaffold(t *testing.T) {
	var out bytes.Buffer
	if code := Execute([]string{"--help"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	help := out.String()
	if strings.Contains(strings.ToLower(help), "scaffold") {
		t.Fatalf("root help still says scaffold:\n%s", help)
	}
	for _, want := range []string{"run", "serve", "manifest", "install", "version"} {
		if !strings.Contains(help, want) {
			t.Fatalf("root help missing %q:\n%s", want, help)
		}
	}
}

// TestRootHelpMargin proves the root help renders inside the output
// container: a leading blank line, the uniform left margin on every
// non-empty line, and no ANSI on a non-TTY.
func TestRootHelpMargin(t *testing.T) {
	var out bytes.Buffer
	if code := Execute([]string{"--help"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	s := out.String()
	if !strings.HasPrefix(s, "\n") {
		t.Fatalf("help missing leading blank line: %q", s)
	}
	if strings.Contains(s, "\x1b") {
		t.Fatalf("non-TTY help contains ANSI: %q", s)
	}
	for _, ln := range strings.Split(s, "\n") {
		if ln != "" && !strings.HasPrefix(ln, ui.Margin) {
			t.Fatalf("help line missing margin %q: %q", ui.Margin, ln)
		}
	}
	for _, want := range []string{"Usage:", "Available Commands:", "run", "version"} {
		if !strings.Contains(s, want) {
			t.Fatalf("help missing %q:\n%s", want, s)
		}
	}
}

// TestSubcommandHelpMargin proves subcommand help resolves the root
// HelpFunc: same container margin, no ANSI on a non-TTY.
func TestSubcommandHelpMargin(t *testing.T) {
	var out bytes.Buffer
	if code := Execute([]string{"run", "--help"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	s := out.String()
	if strings.Contains(s, "\x1b") {
		t.Fatalf("non-TTY run help contains ANSI: %q", s)
	}
	for _, ln := range strings.Split(s, "\n") {
		if ln != "" && !strings.HasPrefix(ln, ui.Margin) {
			t.Fatalf("run help line missing margin %q: %q", ui.Margin, ln)
		}
	}
	if !strings.Contains(s, "--json") {
		t.Fatalf("run help missing --json:\n%s", s)
	}
}

// TestVersionFlagMargin proves `--version` prints the bare build version
// through the design-system margin (matching `eka --version`).
func TestVersionFlagMargin(t *testing.T) {
	old := version
	version = "2.2.1"
	t.Cleanup(func() { version = old })

	var out bytes.Buffer
	if code := Execute([]string{"--version"}, nil, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := out.String(), ui.Margin+"2.2.1\n"; got != want {
		t.Fatalf("--version = %q, want %q", got, want)
	}
}
