package ui

import (
	"bytes"
	"strings"
	"testing"
)

// TestContainerGolden pins the container bytes on a non-TTY writer: a
// leading and trailing blank line, the uniform left Margin on every line
// (blank content lines included, matching eka-cli), NO ANSI.
func TestContainerGolden(t *testing.T) {
	var buf bytes.Buffer
	Container(NewStyle(&buf, false), "line one\nline two\n\nline four\n")
	want := "\n  line one\n  line two\n  \n  line four\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("container = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatalf("container emitted ANSI on a non-TTY: %q", buf.String())
	}
}

// TestContainerEmpty proves empty content renders as a single blank line
// (the leading and trailing padding only).
func TestContainerEmpty(t *testing.T) {
	var buf bytes.Buffer
	Container(NewStyle(&buf, false), "")
	if got := buf.String(); got != "\n\n" {
		t.Fatalf("empty container = %q, want %q", got, "\n\n")
	}
}
