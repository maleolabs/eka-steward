package ui

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMarginConstant pins the two-space container margin to the eka-cli
// value (byte-identical theme).
func TestMarginConstant(t *testing.T) {
	if Margin != "  " {
		t.Fatalf("Margin = %q, want two spaces", Margin)
	}
}

// TestMarginWriterSemantics pins the line-indenting contract: content
// lines get the margin, a blank line stays truly blank, and a write that
// starts with a carriage return (a redraw) is not indented.
func TestMarginWriterSemantics(t *testing.T) {
	var buf bytes.Buffer
	w := newMarginWriter(&buf)

	if _, err := w.Write([]byte("a\nb\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\n")); err != nil { // blank line
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\rx")); err != nil { // redraw: no margin
		t.Fatal(err)
	}
	got := buf.String()
	want := "  a\n  b\n\n\rx"
	if got != want {
		t.Fatalf("margin output = %q, want %q", got, want)
	}
}

// TestMarginWriterMidLine proves the margin is inserted once at the start
// of a line, not on every write: a continuation write mid-line adds none.
func TestMarginWriterMidLine(t *testing.T) {
	var buf bytes.Buffer
	w := newMarginWriter(&buf)
	_, _ = w.Write([]byte("a"))
	_, _ = w.Write([]byte("b\n"))
	if got, want := buf.String(), "  ab\n"; got != want {
		t.Fatalf("mid-line output = %q, want %q", got, want)
	}
}

// TestNewStyleNonTTY proves a non-TTY writer (bytes.Buffer) enables
// neither TTY nor Color, and that the human writer carries the margin
// while Raw bypasses it.
func TestNewStyleNonTTY(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	if s.TTY || s.Color {
		t.Fatalf("non-TTY Style = {TTY:%v Color:%v}, want both false", s.TTY, s.Color)
	}
	fmt.Fprintln(s.W, "human")
	fmt.Fprintln(s.Raw(), "raw")
	if got, want := buf.String(), "  human\nraw\n"; got != want {
		t.Fatalf("Style output = %q, want %q", got, want)
	}
}

// TestIsTTYFalseForNonTerminals proves the stdlib char-device check
// rejects a buffer, a pipe and a regular file.
func TestIsTTYFalseForNonTerminals(t *testing.T) {
	if IsTTY(&bytes.Buffer{}) {
		t.Fatal("bytes.Buffer reported as TTY")
	}
	r, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer wp.Close()
	if IsTTY(r) {
		t.Fatal("pipe reported as TTY")
	}
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTTY(f) {
		t.Fatal("regular file reported as TTY")
	}
}

// TestPaintColorsOnlyWhenEnabled proves the escape sequences appear only
// when Color is set, and pins the exact SGR codes for every palette
// method.
func TestPaintColorsOnlyWhenEnabled(t *testing.T) {
	on := &Style{Color: true}
	off := &Style{Color: false}

	if got, want := on.Info("x"), "\x1b[38;5;75mx\x1b[0m"; got != want {
		t.Fatalf("Info color = %q, want %q", got, want)
	}
	if got, want := on.Success("x"), "\x1b[38;5;114mx\x1b[0m"; got != want {
		t.Fatalf("Success color = %q, want %q", got, want)
	}
	if got, want := on.Warning("x"), "\x1b[38;5;214mx\x1b[0m"; got != want {
		t.Fatalf("Warning color = %q, want %q", got, want)
	}
	if got, want := on.Error("x"), "\x1b[38;5;167mx\x1b[0m"; got != want {
		t.Fatalf("Error color = %q, want %q", got, want)
	}
	if got, want := on.Progress("x"), "\x1b[38;5;80mx\x1b[0m"; got != want {
		t.Fatalf("Progress color = %q, want %q", got, want)
	}
	if got, want := on.Dim("x"), "\x1b[38;5;245mx\x1b[0m"; got != want {
		t.Fatalf("Dim color = %q, want %q", got, want)
	}
	if got, want := on.Accent("x"), "\x1b[38;5;75mx\x1b[0m"; got != want {
		t.Fatalf("Accent color = %q, want %q", got, want)
	}
	if got := off.Info("x"); got != "x" {
		t.Fatalf("Color=false Info = %q, want plain", got)
	}
}

// TestSummaryGolden pins the summary block bytes on a non-TTY writer.
func TestSummaryGolden(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	NewSummary(s).Add("Steps", "2").Add("", "bare").Render()
	want := "\n  Summary:\n  └── Steps: 2\n  └── bare\n"
	if got := buf.String(); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

// TestTableGolden pins the aligned-table bytes on a non-TTY writer: the
// accent header, the dim unicode underline and padded rows.
func TestTableGolden(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	NewTable(s, "A", "Bee").AddRow([]string{"1", "two"}, nil).Render()
	want := "  A  Bee\n  ──────\n  1  two\n"
	if got := buf.String(); got != want {
		t.Fatalf("table = %q, want %q", got, want)
	}
}

// TestTableEmptyRendersNothing proves an empty table emits no bytes (the
// caller prints the placeholder).
func TestTableEmptyRendersNothing(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	NewTable(s, "A").Render()
	if got := buf.String(); got != "" {
		t.Fatalf("empty table = %q, want empty", got)
	}
}

// TestHeaderGolden pins the context header bytes on a non-TTY writer.
func TestHeaderGolden(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	NewHeader(s, "Run").Add("Project", "proj").Add("Role", "worker").Pipeline("Run").Render()
	want := "\n  Run\n  Project   proj\n  Role      worker\n  ↓ Run\n"
	if got := buf.String(); got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
}

// TestNonTTYHasNoANSI proves every renderer stays escape-free on a
// non-TTY writer.
func TestNonTTYHasNoANSI(t *testing.T) {
	var buf bytes.Buffer
	s := NewStyle(&buf, false)
	NewHeader(s, "Run").Add("Project", "proj").Pipeline("Run").Render()
	NewTable(s, "A", "Bee").AddRow([]string{"1", "two"}, []func(string) string{nil, s.Success}).Render()
	NewSummary(s).Add("Steps", "1").Render()
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatalf("non-TTY output contains ANSI: %q", buf.String())
	}
	if strings.Contains(buf.String(), "\r") {
		t.Fatalf("non-TTY output contains CR: %q", buf.String())
	}
}

// TestRenderByteStable proves the same input renders byte-identical
// output across runs (the determinism contract).
func TestRenderByteStable(t *testing.T) {
	render := func() string {
		var buf bytes.Buffer
		s := NewStyle(&buf, false)
		NewHeader(s, "Run").Add("Project", "proj").Add("Role", "worker").Pipeline("Run").Render()
		NewTable(s, "Candidate", "Outcome").AddRow([]string{"c1", "ok"}, []func(string) string{nil, s.Success}).Render()
		NewSummary(s).Add("Steps", "1").Render()
		return buf.String()
	}
	if a, b := render(), render(); a != b {
		t.Fatalf("output not byte-stable:\n%q\n%q", a, b)
	}
}
