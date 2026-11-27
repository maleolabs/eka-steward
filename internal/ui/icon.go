package ui

// The EKA CLI icon set, mirrored verbatim from eka-cli's cmd/ui
// (icon.go): a small, deliberately minimal set of Unicode glyphs (no
// emojis). All bytes are valid UTF-8. Icons are decoration; text always
// carries the meaning.
const (
	// IconDone marks a completed step.
	IconDone = "✓"
	// IconBullet prefixes items in detail lists.
	IconBullet = "•"
	// IconArrow marks a relationship direction; used sparingly.
	IconArrow = "→"
	// IconDown marks the pipeline separator in the context header.
	IconDown = "↓"
	// IconLine is the light horizontal box-drawing line, the table
	// underline of the aligned-table primitive.
	IconLine = "─"

	// Tree connectors for deterministic non-TTY tree lines.
	TreeBranch = "├──"
	TreeLast   = "└──"
	TreeVert   = "│"
	TreeSpace  = "   "
)
