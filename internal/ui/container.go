package ui

import (
	"fmt"
	"strings"
)

// This file implements the output container: the uniform padding that
// keeps human output off the terminal's corner. It is mirrored from
// eka-cli's cmd/ui/container.go. Human-facing blocks that build their
// own buffer (the help text) render through Container — a leading and a
// trailing blank line (vertical padding) and a uniform left margin on
// every line (horizontal padding).

// Container renders a text block as if inside a padded frame: a leading
// and a trailing blank line plus the uniform left Margin on every line.
// Empty content renders as a single blank line. Deterministic on TTY and
// non-TTY — plain text, no control sequences. It writes through Raw: the
// block IS the container, so the style's wrapped writer must not add the
// margin a second time.
func Container(s *Style, content string) {
	fmt.Fprintln(s.Raw())
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		fmt.Fprintln(s.Raw())
		return
	}
	for _, line := range strings.Split(trimmed, "\n") {
		fmt.Fprintf(s.Raw(), "%s%s\n", Margin, line)
	}
	fmt.Fprintln(s.Raw())
}
