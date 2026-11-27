// Package obs is the steward's observability surface: one structured-log
// record per loop step, the loop/pool counters, the run report, and the
// redaction rule. It is deliberately stdlib-only (log/slog, encoding/json,
// sync) and adapts the pool's saturation seam (internal/pool.Metrics) — the
// pool and the loop take no logging dependency.
//
// The loop never names a logging stack. It reports a StepEvent / WriteEvent to
// its Config.Observer (this package's Observer); a Recorder here turns those
// events into a slog record, a counter bump and a report row. See
// docs/observability.md for the record shapes, metric names and the redaction
// rule.
package obs

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// Observer is the loop's observability seam (loop.Config.Observer). The loop
// calls Step once per completed or failed step and Write once per applied EKA
// write. An implementation must be safe for concurrent use: the pooled path
// calls it from worker goroutines. It never feeds a control decision.
type Observer interface {
	Step(StepEvent)
	Write(WriteEvent)
}

// StepEvent is what the loop knows about one step: identity, kind, outcome,
// the number of dispatch attempts made, and how long the step took. The
// driver and role labels are the observer's configuration, not the loop's —
// the loop does not own a driver registry.
type StepEvent struct {
	CandidateID string
	Kind        string
	Outcome     string
	Attempts    int
	Duration    time.Duration
}

// WriteEvent is what the loop knows about one applied EKA write: the affected
// line, the write class and the result. InstanceVersion is 0 for a transition
// (a transition re-points the line without bumping the instance).
type WriteEvent struct {
	Line            string
	Kind            string
	Result          string
	InstanceVersion int
}

// Step outcomes the loop reports.
const (
	// OutcomeOK is a step that completed (applied or done).
	OutcomeOK = "ok"
	// OutcomeFailed is a step that returned an error.
	OutcomeFailed = "failed"
	// OutcomeCancelled is a step whose error was a context cancellation or
	// deadline (the deterministic cancel of docs/recovery.md).
	OutcomeCancelled = "cancelled"
)

// Record is one structured loop-step log record (the slog record the Logger
// emits). It is the shipped shape; the field names are the JSON keys.
type Record struct {
	Driver      string `json:"driver"`
	Role        string `json:"role"`
	Outcome     string `json:"outcome"`
	CandidateID string `json:"candidateId"`
	StepKind    string `json:"stepKind"`
	Attempt     int    `json:"attempt"`
	DurationMs  int64  `json:"durationMs"`
}

// StepRecord is one row of the run report's step list (the Record plus the
// step's kind under the report's field name).
type StepRecord struct {
	CandidateID string `json:"candidateId"`
	Kind        string `json:"kind"`
	Outcome     string `json:"outcome"`
	Role        string `json:"role"`
	Driver      string `json:"driver"`
	DurationMs  int64  `json:"durationMs"`
}

// WriteRecord is one row of the run report's EKA-write list.
type WriteRecord struct {
	Line            string `json:"line"`
	Type            string `json:"type"`
	ID              string `json:"id"`
	InstanceVersion int    `json:"instanceVersion"`
	Kind            string `json:"kind"`
	Result          string `json:"result"`
}

// Report is the deterministic run report: the steps a run took and the EKA
// writes it applied. Both lists are sorted by a total order (no map
// iteration), so the same input yields byte-identical JSON.
type Report struct {
	Steps  []StepRecord  `json:"steps"`
	Writes []WriteRecord `json:"writes"`
}

// MarshalJSON serializes the report deterministically, sorting copies of both
// lists so any Report value (however constructed) yields stable bytes. A nil
// list is emitted as an empty array, never null.
func (r Report) MarshalJSON() ([]byte, error) {
	steps := append([]StepRecord(nil), r.Steps...)
	writes := append([]WriteRecord(nil), r.Writes...)
	if steps == nil {
		steps = []StepRecord{}
	}
	if writes == nil {
		writes = []WriteRecord{}
	}
	sortSteps(steps)
	sortWrites(writes)
	return json.Marshal(struct {
		Steps  []StepRecord  `json:"steps"`
		Writes []WriteRecord `json:"writes"`
	}{Steps: steps, Writes: writes})
}

// Write serializes the report to w with a trailing newline. Identical to
// MarshalJSON plus the newline, so both serializers agree byte for byte.
func (r Report) Write(w io.Writer) error {
	b, err := r.MarshalJSON()
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// sortSteps orders step rows by a total order: candidateId, kind, outcome,
// driver, role, then duration. Stable, so ties keep insertion order.
func sortSteps(s []StepRecord) {
	sort.SliceStable(s, func(i, j int) bool {
		a, b := s[i], s[j]
		if a.CandidateID != b.CandidateID {
			return a.CandidateID < b.CandidateID
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Outcome != b.Outcome {
			return a.Outcome < b.Outcome
		}
		if a.Driver != b.Driver {
			return a.Driver < b.Driver
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.DurationMs < b.DurationMs
	})
}

// sortWrites orders write rows by a total order: line, kind, result, instance
// version, type, id.
func sortWrites(w []WriteRecord) {
	sort.SliceStable(w, func(i, j int) bool {
		a, b := w[i], w[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Result != b.Result {
			return a.Result < b.Result
		}
		if a.InstanceVersion != b.InstanceVersion {
			return a.InstanceVersion < b.InstanceVersion
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})
}

// splitLine parses the qualified line form "<namespace>/<type>:<id>" into its
// type and id segments (namespace is not reported). A malformed line yields
// empty type/id rather than an error: the report is best-effort display.
func splitLine(line string) (typ, id string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", ""
	}
	head, id := line[:i], line[i+1:]
	if j := strings.LastIndexByte(head, '/'); j >= 0 {
		return head[j+1:], id
	}
	return head, id
}
