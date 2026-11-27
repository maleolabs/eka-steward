package obs

// Structured logging and redaction. The Logger is a thin wrapper over
// log/slog's JSON handler behind a one-method Sink, so a test captures the
// exact bytes. Every string field a Recorder logs or reports is passed through
// a Redactor first (docs/observability.md).

import (
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
)

// Mask replaces a redacted token. It is a fixed marker, so the mask leaks no
// secret length and the output is deterministic.
const Mask = "■■■"

// Sink is the tiny logging seam: one Log call per structured record. A test
// injects a capturing sink; production uses NewLogger (slog JSON).
type Sink interface {
	Log(Record)
}

// Logger is the slog-backed Sink. It emits one JSON record per loop step.
type Logger struct {
	l *slog.Logger
}

// NewLogger returns a Sink that writes newline-delimited JSON records to w.
func NewLogger(w io.Writer) *Logger {
	return &Logger{l: slog.New(slog.NewJSONHandler(w, nil))}
}

// Log emits one structured step record. The Recorder passes an already
// redacted Record, so the Logger never sees a raw secret.
func (l *Logger) Log(r Record) {
	l.l.Info("loop step",
		slog.String("driver", r.Driver),
		slog.String("role", r.Role),
		slog.String("outcome", r.Outcome),
		slog.String("candidateId", r.CandidateID),
		slog.String("stepKind", r.StepKind),
		slog.Int("attempt", r.Attempt),
		slog.Int64("durationMs", r.DurationMs),
	)
}

// secretPatterns are the token shapes the steward masks. They mirror the
// redaction set of docs/acp-spike.md. This is pattern-based: a secret that
// does not match one of these shapes (and is not in the deny-list) is not
// caught — see the residual risk in docs/observability.md.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{8,}`),
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-]+`),
}

// Redactor masks secret-shaped tokens and an operator-supplied deny-list of
// literal values (e.g. env-var values that must never be logged). A nil
// Redactor returns its input unchanged, so a zero configuration is a no-op.
type Redactor struct {
	literals []string
}

// NewRedactor builds a Redactor that additionally masks every non-empty
// literal in secrets. Literals are applied longest-first so a longer value is
// masked before a shorter substring of it.
func NewRedactor(secrets ...string) *Redactor {
	lits := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			lits = append(lits, s)
		}
	}
	sort.Slice(lits, func(i, j int) bool { return len(lits[i]) > len(lits[j]) })
	return &Redactor{literals: lits}
}

// Redact returns s with every secret-shaped token and deny-list literal
// replaced by Mask. Deterministic: the same input always yields the same
// output.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, lit := range r.literals {
		s = strings.ReplaceAll(s, lit, Mask)
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Mask)
	}
	return s
}
