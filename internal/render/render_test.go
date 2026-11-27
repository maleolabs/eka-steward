package render

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// goldenInput is the representative payload pinned by testdata/context.golden.
func goldenInput() Input {
	return Input{
		Header: "eka/sto:context-renderer — bounded context payload",
		Sections: []Section{
			{Kind: KindInstruction, Title: "Instructions", Body: "- Render deterministically.\n- Never write EKA."},
			{Kind: KindConstraint, Title: "Constraints", Body: "- stdlib only.\n- No new dependency."},
			{Kind: KindAcceptance, Title: "Acceptance", Body: "- Byte-stable output.\n- Budget enforced."},
			{Kind: KindKnowledge, Title: "Knowledge", Body: "docs/context.md describes the payload shape."},
			{Kind: KindNote, Title: "Note", Body: "The caller maps EKA knowledge into Input."},
		},
	}
}

func render(t *testing.T, in Input, opts Options) string {
	t.Helper()
	out, err := Render(in, opts)
	if err != nil {
		t.Fatalf("Render unexpected error: %v", err)
	}
	return string(out)
}

// TestGolden pins the exact rendered bytes. Regenerate deliberately with
// UPDATE_GOLDEN=1 go test ./internal/render -run TestGolden
func TestGolden(t *testing.T) {
	path := filepath.Join("testdata", "context.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(render(t, goldenInput(), Options{})), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := render(t, goldenInput(), Options{})
	if got != string(want) {
		t.Fatalf("rendered payload drifted from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestByteIdentityAcrossRuns asserts identical inputs produce identical bytes
// on every call — the observable determinism guarantee.
func TestByteIdentityAcrossRuns(t *testing.T) {
	in := goldenInput()
	first := render(t, in, Options{})
	for i := 0; i < 100; i++ {
		if got := render(t, in, Options{}); got != first {
			t.Fatalf("call %d produced different bytes", i)
		}
	}
}

// TestBudgetByFixedBytes asserts different MaxBytes values render as expected:
// under, exact, over, and the hard floor.
func TestBudgetByFixedBytes(t *testing.T) {
	in := goldenInput()
	full := render(t, in, Options{})

	tests := []struct {
		name     string
		maxBytes int
		wantErr  bool
	}{
		{"zero uses default", 0, false},
		{"negative uses default", -5, false},
		{"under budget", len(full) + 100, false},
		{"exact budget", len(full), false},
		{"one byte over", len(full) - 1, false},
		{"hard floor", MinMaxBytes, false},
		{"above floor", MinMaxBytes + 1, false},
		{"below floor", MinMaxBytes - 1, true},
		{"one byte", 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Render(in, Options{MaxBytes: tc.maxBytes})
			if tc.wantErr {
				if !errors.Is(err, ErrBudgetTooSmall) {
					t.Fatalf("Render(MaxBytes=%d) error = %v, want ErrBudgetTooSmall", tc.maxBytes, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Render(MaxBytes=%d) unexpected error: %v", tc.maxBytes, err)
			}
			max := tc.maxBytes
			if max <= 0 {
				max = DefaultMaxBytes
			}
			if len(out) > max {
				t.Fatalf("payload %d bytes exceeds budget %d", len(out), max)
			}
			if max >= len(full) && string(out) != full {
				t.Fatalf("fits-budget payload changed unexpectedly")
			}
		})
	}
}

// TestNeverOverBudget asserts the invariant across a sweep of budgets.
func TestNeverOverBudget(t *testing.T) {
	in := goldenInput()
	full := len(render(t, in, Options{}))
	for max := MinMaxBytes; max <= full+16; max++ {
		out, err := Render(in, Options{MaxBytes: max})
		if err != nil {
			t.Fatalf("Render(MaxBytes=%d) unexpected error: %v", max, err)
		}
		if len(out) > max {
			t.Fatalf("payload %d bytes exceeds budget %d", len(out), max)
		}
	}
}

// TestTruncationOrder asserts a one-byte overflow drops the lowest-priority
// section (note) first while keeping the instruction section.
func TestTruncationOrder(t *testing.T) {
	in := goldenInput()
	full := render(t, in, Options{})
	out := render(t, in, Options{MaxBytes: len(full) - 1})
	if strings.Contains(out, "## Note") {
		t.Fatalf("note should be dropped first on overflow:\n%s", out)
	}
	for _, want := range []string{"## Instructions", "## Constraints", "## Acceptance", "## Knowledge"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q to survive:\n%s", want, out)
		}
	}
}

// TestInstructionSurvivesWhenOthersDropped asserts instruction is kept first:
// dropping every other section should still leave the instruction section.
func TestInstructionSurvivesWhenOthersDropped(t *testing.T) {
	in := goldenInput()
	// Budget just above the instruction-only payload but below full.
	instrOnly := Input{Header: in.Header, Sections: []Section{in.Sections[0]}}
	budget := len(render(t, instrOnly, Options{})) + 1
	out := render(t, in, Options{MaxBytes: budget})
	if !strings.Contains(out, "## Instructions") {
		t.Fatalf("instruction section must survive:\n%s", out)
	}
}

// TestTruncationMarker asserts an over-budget body is shortened with the
// documented marker.
func TestTruncationMarker(t *testing.T) {
	in := Input{
		Header: "h",
		Sections: []Section{{
			Kind:  KindInstruction,
			Title: "Long",
			Body:  strings.Repeat("line of text\n", 200),
		}},
	}
	full := len(render(t, in, Options{}))
	out := render(t, in, Options{MaxBytes: full / 2})
	if !strings.Contains(out, TruncatedMarker) {
		t.Fatalf("expected truncation marker in:\n%s", out)
	}
	if len(out) > full/2 {
		t.Fatalf("payload %d exceeds budget %d", len(out), full/2)
	}
}

// TestUTF8Safety asserts truncation never splits a multibyte rune.
func TestUTF8Safety(t *testing.T) {
	body := strings.Repeat("日本語テキスト\n", 100)
	in := Input{Sections: []Section{{Kind: KindInstruction, Title: "T", Body: body}}}
	full := len(render(t, in, Options{}))
	for max := MinMaxBytes; max < full; max++ {
		out := render(t, in, Options{MaxBytes: max})
		if !utf8.ValidString(out) {
			t.Fatalf("budget %d produced invalid UTF-8:\n%q", max, out)
		}
	}
}

// TestUnknownKindRankedLast asserts an unrecognized Kind is truncated before
// every recognized kind.
func TestUnknownKindRankedLast(t *testing.T) {
	in := Input{Sections: []Section{
		{Kind: KindAcceptance, Title: "A", Body: strings.Repeat("keep acceptance. ", 20)},
		{Kind: "mystery", Title: "M", Body: strings.Repeat("drop this first. ", 20)},
	}}
	full := render(t, in, Options{})
	out := render(t, in, Options{MaxBytes: len(full) - 1})
	if strings.Contains(out, "## M") {
		t.Fatalf("unknown kind should be dropped first:\n%s", out)
	}
	if !strings.Contains(out, "## A") {
		t.Fatalf("acceptance should survive:\n%s", out)
	}
}
