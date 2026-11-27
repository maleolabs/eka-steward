package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/obs"
	"github.com/maleolabs/eka-steward/v2/internal/ui"
)

// runFixture is the crafted report the render goldens pin. It exercises
// both outcomes and the two table shapes.
func runFixture() obs.Report {
	return obs.Report{
		Steps: []obs.StepRecord{
			{CandidateID: "p/sto:i-1", Kind: "dispatch", Outcome: obs.OutcomeOK, Role: "worker", Driver: "claude", DurationMs: 12},
			{CandidateID: "p/sto:i-2", Kind: "dispatch", Outcome: obs.OutcomeFailed, Role: "worker", Driver: "claude", DurationMs: 7},
		},
		Writes: []obs.WriteRecord{
			{Line: "p/sto:i-1", Kind: "transition", Result: "applied", InstanceVersion: 0},
		},
	}
}

// TestRenderRunReportGolden pins the human `run` report bytes on a
// non-TTY writer: the margin + UTF-8 icons, NO ANSI. This is the
// byte-stable layout contract of the design-system report.
func TestRenderRunReportGolden(t *testing.T) {
	var buf bytes.Buffer
	renderRunReport(ui.NewStyle(&buf, false), "proj", "worker", "claude", runFixture())
	want := "\n  Run\n  Project      proj\n  Role         worker\n  Controller   claude\n  ↓ Run\n\n" +
		"  Candidate  Kind      Outcome  Role    Driver  Time(ms)\n" +
		"  ──────────────────────────────────────────────────────\n" +
		"  p/sto:i-1  dispatch  ok       worker  claude  12      \n" +
		"  p/sto:i-2  dispatch  failed   worker  claude  7       \n\n" +
		"  Line       Kind        Result   Version\n" +
		"  ───────────────────────────────────────\n" +
		"  p/sto:i-1  transition  applied  0      \n\n" +
		"  Summary:\n" +
		"  └── Steps: 2 (ok: 1, failed: 1, cancelled: 0)\n" +
		"  └── Writes: 1\n" +
		"  └── Result: quiescent\n"
	if got := buf.String(); got != want {
		t.Fatalf("run report mismatch:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(buf.String(), "\x1b") || strings.Contains(buf.String(), "\r") {
		t.Fatalf("non-TTY run report contains ANSI/CR: %q", buf.String())
	}
}

// TestRenderRunReportEmptyGolden pins the empty-report layout: the
// placeholder lines keep the block readable when the tables are empty.
func TestRenderRunReportEmptyGolden(t *testing.T) {
	var buf bytes.Buffer
	renderRunReport(ui.NewStyle(&buf, false), "proj", "worker", "", obs.Report{})
	want := "\n  Run\n  Project   proj\n  Role      worker\n  ↓ Run\n\n" +
		"  (no steps)\n\n  (no writes)\n\n" +
		"  Summary:\n" +
		"  └── Steps: 0 (ok: 0, failed: 0, cancelled: 0)\n" +
		"  └── Writes: 0\n" +
		"  └── Result: quiescent\n"
	if got := buf.String(); got != want {
		t.Fatalf("empty run report mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestRunReportByteStable proves the same report renders byte-identically
// across runs.
func TestRunReportByteStable(t *testing.T) {
	render := func() string {
		var buf bytes.Buffer
		renderRunReport(ui.NewStyle(&buf, false), "proj", "worker", "claude", runFixture())
		return buf.String()
	}
	if a, b := render(), render(); a != b {
		t.Fatalf("run report not byte-stable:\n%q\n%q", a, b)
	}
}

// TestRunJSONParity proves the `--json` path is byte-identical to
// obs.Report.Write: the machine report must not change with the theme.
// Style.Raw bypasses the margin, so the JSON is the unwrapped report.
func TestRunJSONParity(t *testing.T) {
	rep := runFixture()

	var want bytes.Buffer
	if err := rep.Write(&want); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	s := ui.NewStyle(&got, false)
	if err := rep.Write(s.Raw()); err != nil {
		t.Fatal(err)
	}

	if got.String() != want.String() {
		t.Fatalf("--json report = %q, want %q", got.String(), want.String())
	}
	if strings.Contains(got.String(), "\n  ") {
		t.Fatalf("--json report was margin-indented: %q", got.String())
	}
}
