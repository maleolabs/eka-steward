// Package render turns a bounded selection of knowledge into the
// deterministic markdown context payload handed to a steward worker.
//
// It is pure. The same Input and Options produce the same bytes, always:
// no map iteration reaches the output, no timestamp, no randomness, no
// clock. The package imports only the standard library and can call
// nothing that observes the outside world — no I/O, no clock, no
// subprocess, no LLM / agent client, no EKA. It NEVER writes EKA: rendering
// is a one-way, in-memory transformation of caller-supplied bytes. The
// caller maps EKA knowledge into Input; this package only lays it out.
// See docs/context.md.
//
// The payload is byte-bounded. A hard budget (Options.MaxBytes, defaulting
// to DefaultMaxBytes) caps its size; when the full content does not fit,
// content is reduced in a fixed, documented truncation order and the result
// is still a valid payload. Render never returns an over-budget payload for
// any budget at or above MinMaxBytes.
package render

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

// DefaultMaxBytes is the budget used when Options.MaxBytes is <= 0.
const DefaultMaxBytes = 8192

// MinMaxBytes is the hard floor of the budget. A positive MaxBytes below it
// cannot carry even the documented minimum payload, so Render rejects it
// with ErrBudgetTooSmall instead of guessing.
const MinMaxBytes = 64

// TruncatedMarker is appended to a section body (or the header) when it is
// shortened to fit the budget. Its exact bytes are part of the golden
// output contract.
const TruncatedMarker = "...[truncated]"

// Recognized section kinds. A Kind outside this set gets keepRankUnknown
// (the lowest keep-rank) and is therefore truncated first.
const (
	KindInstruction = "instruction"
	KindAcceptance  = "acceptance"
	KindConstraint  = "constraint"
	KindKnowledge   = "knowledge"
	KindNote        = "note"
)

// keepRankUnknown is the keep-rank of any Kind not in keepRanks. It is
// lower than every recognized rank, so unknown sections truncate first.
const keepRankUnknown = 0

// keepRanks is the pinned kind precedence. HIGHER rank is kept LONGER:
// truncation removes sections in ascending rank order, so note goes before
// knowledge, knowledge before constraint, constraint before acceptance, and
// acceptance before instruction. Changing this table is a determinism- and
// output-breaking change and must update docs/context.md and the golden file.
var keepRanks = map[string]int{
	KindInstruction: 100,
	KindAcceptance:  80,
	KindConstraint:  60,
	KindKnowledge:   40,
	KindNote:        20,
}

// keepRank returns the keep-rank of kind (keepRankUnknown when unrecognized).
func keepRank(kind string) int {
	if r, ok := keepRanks[kind]; ok {
		return r
	}
	return keepRankUnknown
}

// ErrBudgetTooSmall is returned when a positive MaxBytes is below MinMaxBytes.
var ErrBudgetTooSmall = errors.New("render: MaxBytes below the minimum payload floor")

// Section is one rendered part of the context payload. The caller sets Kind
// to one of the recognized kind constants (or any string: unknown kinds get
// the lowest keep-rank) and orders Sections itself; Render preserves that
// order in the output and only removes or shortens to fit the budget.
type Section struct {
	Kind  string // e.g. "instruction", "constraint", "acceptance", "knowledge", "note"
	Title string
	Body  string
}

// Input is everything the renderer needs; the caller maps EKA knowledge
// into it. Header is the payload identity (e.g. task id + title).
type Input struct {
	Header   string
	Sections []Section
}

// Options controls the budget. MaxBytes <= 0 means DefaultMaxBytes.
type Options struct {
	MaxBytes int
}

// chunk is one rendered block: the header, or one section. head is the
// markdown heading line; body is the section body (empty for the header).
type chunk struct {
	head    string // "# Header" or "## Title"
	body    string
	raw     string // header text without the "# " prefix (header chunk only)
	dropped bool
}

func (c *chunk) render() string {
	if c.body == "" {
		return c.head
	}
	return c.head + "\n\n" + c.body
}

// Render returns a deterministic markdown payload truncated to fit
// opts.MaxBytes under the documented truncation order. Identical Input and
// Options always yield identical bytes.
//
// Error is ErrBudgetTooSmall when 0 < opts.MaxBytes < MinMaxBytes. Every
// other budget yields a payload of at most opts.MaxBytes bytes.
func Render(in Input, opts Options) ([]byte, error) {
	max := opts.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	if max < MinMaxBytes {
		return nil, ErrBudgetTooSmall
	}

	// Chunks in output order: header first (if any), then sections in the
	// caller's order. Section chunk i sits at position i+offset.
	var chunks []*chunk
	var header *chunk
	offset := 0
	if in.Header != "" {
		header = &chunk{head: "# " + in.Header, raw: in.Header}
		chunks = append(chunks, header)
		offset = 1
	}
	for _, s := range in.Sections {
		chunks = append(chunks, &chunk{head: "## " + s.Title, body: s.Body})
	}

	if len(assemble(chunks)) <= max {
		return []byte(assemble(chunks)), nil
	}

	// Phase A: drop whole non-instruction sections, lowest keep-rank first.
	// Instruction sections are kept first (never dropped here); their bodies
	// are shortened in Phase B instead.
	for _, i := range orderIndices(in.Sections, false) {
		if len(assemble(chunks)) <= max {
			break
		}
		chunks[offset+i].dropped = true
	}

	// Phase B: shorten instruction bodies to fit, in ascending keep-rank
	// order (ties: later section first). A body that cannot shrink enough is
	// dropped as a last resort.
	for _, i := range orderIndices(in.Sections, true) {
		c := chunks[offset+i]
		if len(assemble(chunks)) <= max {
			break
		}
		excess := len(assemble(chunks)) - max
		c.body = truncate(c.body, len(c.body)-excess)
		if len(assemble(chunks)) > max {
			c.dropped = true
		}
	}

	// Phase C: shorten the header, then drop it, as the absolute last resort.
	if header != nil && !header.dropped {
		for len(assemble(chunks)) > max {
			excess := len(assemble(chunks)) - max
			next := truncate(header.raw, len(header.raw)-excess)
			if next == header.raw {
				header.dropped = true
				break
			}
			header.raw = next
			header.head = "# " + next
		}
	}

	return []byte(assemble(chunks)), nil
}

// assemble joins the non-dropped chunks in order with a blank line between
// them and a single trailing newline. An empty payload is the empty string.
func assemble(chunks []*chunk) string {
	parts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if c.dropped {
			continue
		}
		parts = append(parts, c.render())
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// orderIndices returns the section indices to reduce, in truncation order:
// ascending keep-rank, ties broken by descending index (later sections
// first). When wantInstruction is true only instruction sections are
// returned; otherwise only non-instruction sections.
func orderIndices(secs []Section, wantInstruction bool) []int {
	idx := make([]int, 0, len(secs))
	for i, s := range secs {
		if (s.Kind == KindInstruction) == wantInstruction {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := keepRank(secs[idx[a]].Kind), keepRank(secs[idx[b]].Kind)
		if ra != rb {
			return ra < rb
		}
		return idx[a] > idx[b]
	})
	return idx
}

// truncate returns s unchanged when it already fits limit bytes; otherwise
// it cuts s at a line boundary (or, failing that, a UTF-8 rune boundary) to
// at most limit-TruncatedMarker bytes and appends TruncatedMarker. When limit
// is too small to carry the marker it returns the empty string.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= len(TruncatedMarker) {
		return ""
	}
	cut := limit - len(TruncatedMarker)
	if cut > len(s) {
		cut = len(s)
	}
	if nl := strings.LastIndex(s[:cut], "\n"); nl >= 0 {
		cut = nl
	}
	return s[:runeFloor(s, cut)] + TruncatedMarker
}

// runeFloor returns the largest index <= n that does not split a UTF-8 rune.
func runeFloor(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}
