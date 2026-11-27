// Package ui provides the presentation primitives of the eka-steward
// CLI: the same left margin, ANSI 256-color palette, Unicode icon set
// and aligned-block renderers (header, table, summary) the eka-cli
// `cmd/ui` package renders.
//
// This is a DELIBERATE MIRROR of eka-cli's `cmd/ui`, not a shared
// package: no cross-module package exists today, and eka-steward is a
// standalone module that must NOT import eka-cli (it is a consumer, and
// the import direction is one way). The tokens below are copied
// verbatim so the theme is byte-identical to `eka`; if eka-cli's theme
// changes, this mirror must change with it. The duplication is
// documented here rather than factored into a shared dependency
// (`ponytail:` a shared `ui` module would remove the copy — add one
// only when a third consumer appears).
//
// The package imports the standard library ONLY. In particular, unlike
// eka-cli, it does NOT import golang.org/x/term: TTY detection is the
// stdlib char-device check (see IsTTY), which needs no dependency and
// keeps the steward's go.mod free of a terminal package.
//
// Determinism contract (see docs/determinism.md):
//
//   - Non-TTY output (pipes, CI, tests) is plain text plus UTF-8 icons:
//     no ANSI escapes, no "\r", byte-identical across runs.
//   - TTY output carries the same structure, plus colors. The decision
//     point is Style, created once per command execution from the
//     writer; there is no global state and no ambient detection.
package ui

import (
	"io"
	"os"
)

// Style carries the presentation context for one command execution:
// whether the writer is a terminal, whether colors are enabled, whether
// --verbose was given, and the destination writers.
type Style struct {
	// Color reports whether ANSI SGR colors may be emitted.
	Color bool
	// TTY reports whether the writer is a real terminal.
	TTY bool
	// Verbose mirrors the presentation-only --verbose flag.
	Verbose bool
	// W is the destination writer for all HUMAN command output — a
	// line-indenting writer applying the container's uniform left
	// margin. Machine output (run --json) uses Raw() instead.
	W io.Writer

	// raw is the unwrapped writer W wraps (see Raw).
	raw io.Writer
}

// IsTTY reports whether w is a terminal. Stdlib-only implementation:
// w must be an *os.File whose Stat mode has os.ModeCharDevice set (a
// character device such as a TTY); pipes and regular files are not
// terminals. This replaces eka-cli's golang.org/x/term check without
// the dependency.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// colorEnabled reports whether colors should be emitted on w: the
// writer must be a terminal, NO_COLOR must be unset, and TERM must not
// be "dumb" (https://no-color.org).
func colorEnabled(w io.Writer) bool {
	if !IsTTY(w) {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

// NewStyle builds the Style for one command execution against w.
// Verbose is the presentation-only --verbose flag value.
func NewStyle(w io.Writer, verbose bool) *Style {
	return &Style{
		Color:   colorEnabled(w),
		TTY:     IsTTY(w),
		Verbose: verbose,
		W:       newMarginWriter(w),
		raw:     w,
	}
}

// Raw returns the UNWRAPPED writer: machine output (run --json) bypasses
// the global left margin through it. Human output must go through W.
// Styles constructed directly (tests) carry no wrapper — Raw falls back
// to W.
func (s *Style) Raw() io.Writer {
	if s.raw != nil {
		return s.raw
	}
	return s.W
}
