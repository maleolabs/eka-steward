package ui

import "fmt"

// Header is the context header printed before a workflow block, mirrored
// from eka-cli's cmd/ui (header.go): it identifies the object being
// processed (object kind plus identity rows) and the pipeline about to
// run. Minimal by design — no boxes, no decoration — and deterministic
// on both TTY and non-TTY. It renders inside the output container's
// vertical padding: a leading blank line keeps the block from sticking
// to the terminal corner:
//
//	(blank line)
//	Run
//	Project     myproj
//	Role        worker
//	↓ Run
//
// The object kind line is rendered in the accent color on a TTY; the
// rows and the pipeline line are plain text so the values stay readable
// in any environment.
type Header struct {
	s        *Style
	object   string
	rows     [][2]string
	pipeline string
}

// NewHeader starts a context header for the given object kind (e.g.
// "Run").
func NewHeader(s *Style, objectKind string) *Header {
	return &Header{s: s, object: objectKind}
}

// Add appends one identity row (label, value). The label column is
// aligned across all rows at Render time.
func (h *Header) Add(label, value string) *Header {
	h.rows = append(h.rows, [2]string{label, value})
	return h
}

// Pipeline sets the pipeline name rendered on the separator line
// ("↓ <name>"). An empty name suppresses the line.
func (h *Header) Pipeline(name string) *Header {
	h.pipeline = name
	return h
}

// Render prints the header: a leading blank line (the container's
// vertical top padding), object kind, aligned rows, pipeline separator.
func (h *Header) Render() {
	s := h.s
	fmt.Fprintln(s.W)
	fmt.Fprintln(s.W, s.Accent(h.object))
	width := 0
	for _, r := range h.rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range h.rows {
		fmt.Fprintf(s.W, "%-*s   %s\n", width, r[0], r[1])
	}
	if h.pipeline != "" {
		fmt.Fprintf(s.W, "%s %s\n", IconDown, h.pipeline)
	}
}
