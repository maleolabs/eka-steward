package cmd

import (
	"fmt"
	"strconv"

	"github.com/maleolabs/eka-steward/v2/internal/obs"
	"github.com/maleolabs/eka-steward/v2/internal/ui"
	"github.com/spf13/cobra"
)

// styleFor builds the presentation Style for one command execution from
// the output writer. It mirrors eka-cli's styleFor (there is no
// --verbose flag in the steward surface today, so Verbose is always
// false). Every renderer takes the Style explicitly; there is no
// ambient state.
func styleFor(c *cobra.Command) *ui.Style {
	return ui.NewStyle(c.OutOrStdout(), false)
}

// renderRunReport renders the human `run` report through the design
// system: a context header, the step and EKA-write tables, and a
// summary. It is a pure function of (identity, report, style) — the
// deterministic layout is byte-stable on a non-TTY. Empty tables render
// a dim placeholder so the block still reads.
func renderRunReport(s *ui.Style, project, role, driver string, rep obs.Report) {
	h := ui.NewHeader(s, "Run")
	h.Add("Project", project)
	h.Add("Role", role)
	if driver != "" {
		h.Add("Controller", driver)
	}
	h.Pipeline("Run")
	h.Render()

	fmt.Fprintln(s.W)
	steps := ui.NewTable(s, "Candidate", "Kind", "Outcome", "Role", "Driver", "Time(ms)")
	for _, st := range rep.Steps {
		steps.AddRow(
			[]string{st.CandidateID, st.Kind, st.Outcome, st.Role, st.Driver, strconv.FormatInt(st.DurationMs, 10)},
			[]func(string) string{nil, nil, outcomeColor(s, st.Outcome), nil, nil, nil},
		)
	}
	if len(rep.Steps) == 0 {
		fmt.Fprintln(s.W, s.Dim("(no steps)"))
	} else {
		steps.Render()
	}

	fmt.Fprintln(s.W)
	writes := ui.NewTable(s, "Line", "Kind", "Result", "Version")
	for _, wr := range rep.Writes {
		writes.AddRow([]string{wr.Line, wr.Kind, wr.Result, strconv.Itoa(wr.InstanceVersion)}, nil)
	}
	if len(rep.Writes) == 0 {
		fmt.Fprintln(s.W, s.Dim("(no writes)"))
	} else {
		writes.Render()
	}

	ui.NewSummary(s).
		Add("Steps", stepSummary(rep.Steps)).
		Add("Writes", strconv.Itoa(len(rep.Writes))).
		Add("Result", "quiescent").
		Render()
}

// stepSummary renders the step tallies line: the step count plus the
// per-outcome breakdown.
func stepSummary(steps []obs.StepRecord) string {
	var ok, failed, cancelled int
	for _, st := range steps {
		switch st.Outcome {
		case obs.OutcomeOK:
			ok++
		case obs.OutcomeFailed:
			failed++
		case obs.OutcomeCancelled:
			cancelled++
		}
	}
	return fmt.Sprintf("%d (ok: %d, failed: %d, cancelled: %d)", len(steps), ok, failed, cancelled)
}

// outcomeColor maps a step outcome to its palette method: ok=Success,
// failed=Error, cancelled=Warning. An unknown outcome renders plain
// (nil color).
func outcomeColor(s *ui.Style, outcome string) func(string) string {
	switch outcome {
	case obs.OutcomeOK:
		return s.Success
	case obs.OutcomeFailed:
		return s.Error
	case obs.OutcomeCancelled:
		return s.Warning
	default:
		return nil
	}
}
