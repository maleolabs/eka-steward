package obs

// Recorder is the one concrete observability sink: it implements the pool's
// saturation seam (pool.Metrics), the loop's Observer seam, and accumulates
// the run report. Wire the SAME Recorder into loop.Config.Metrics and
// loop.Config.Observer so one object keeps the whole picture. It is safe for
// concurrent use (guarded by one mutex).

import (
	"sync"

	"github.com/maleolabs/eka-steward/v2/internal/pool"
)

// Compile-time assertions: the Recorder satisfies both seams.
var (
	_ pool.Metrics = (*Recorder)(nil)
	_ Observer     = (*Recorder)(nil)
)

// Config is the Recorder's operator-owned labelling.
type Config struct {
	// Driver is the driver label every step record carries (e.g. "claude").
	// The loop does not own a driver registry, so this is operator config.
	Driver string
	// Role is the worker-role label every step record carries.
	Role string
	// Secrets is the deny-list of literal values to mask before any log or
	// report write (e.g. env-var values). Empty is allowed.
	Secrets []string
}

// Recorder converts step/write events into a slog record, a counter bump and
// a report row, and adapts the pool saturation counters. Construct it with
// NewRecorder.
type Recorder struct {
	mu   sync.Mutex
	red  *Redactor
	sink Sink

	// redacted labels, precomputed once.
	driver string
	role   string

	// Saturation (pool.Metrics).
	queueDepth     int
	peakQueueDepth int
	inFlight       int
	peakInFlight   int
	dispatched     int
	rejected       int
	retries        int
	cancellations  int

	// Steps is the count of step events the Recorder observed.
	steps int

	// Report accumulation.
	stepRecords  []StepRecord
	writeRecords []WriteRecord
}

// NewRecorder builds a Recorder over cfg. sink may be nil (metrics/report
// only, no logging).
func NewRecorder(cfg Config, sink Sink) *Recorder {
	red := NewRedactor(cfg.Secrets...)
	return &Recorder{
		red:    red,
		sink:   sink,
		driver: red.Redact(cfg.Driver),
		role:   red.Redact(cfg.Role),
	}
}

// --- pool.Metrics (saturation + loop retry/cancel counters) ---

func (r *Recorder) QueueDepth(n int) {
	r.mu.Lock()
	r.queueDepth = n
	if n > r.peakQueueDepth {
		r.peakQueueDepth = n
	}
	r.mu.Unlock()
}

func (r *Recorder) InFlight(n int) {
	r.mu.Lock()
	r.inFlight = n
	r.mu.Unlock()
}

func (r *Recorder) Dispatched(n int) {
	r.mu.Lock()
	r.dispatched = n
	r.mu.Unlock()
}

func (r *Recorder) Rejected(n int) {
	r.mu.Lock()
	r.rejected = n
	r.mu.Unlock()
}

func (r *Recorder) PeakInFlight(n int) {
	r.mu.Lock()
	if n > r.peakInFlight {
		r.peakInFlight = n
	}
	r.mu.Unlock()
}

func (r *Recorder) Retries(n int) {
	r.mu.Lock()
	r.retries = n
	r.mu.Unlock()
}

func (r *Recorder) Cancellations(n int) {
	r.mu.Lock()
	r.cancellations = n
	r.mu.Unlock()
}

// --- Observer (step + write events) ---

// Step logs one step record, bumps the step counter and appends a redacted
// report row. Logging happens outside the lock so a slow sink never blocks a
// concurrent step.
func (r *Recorder) Step(e StepEvent) {
	rec := Record{
		Driver:      r.driver,
		Role:        r.role,
		Outcome:     r.red.Redact(e.Outcome),
		CandidateID: r.red.Redact(e.CandidateID),
		StepKind:    r.red.Redact(e.Kind),
		Attempt:     e.Attempts,
		DurationMs:  e.Duration.Milliseconds(),
	}
	row := StepRecord{
		CandidateID: rec.CandidateID,
		Kind:        rec.StepKind,
		Outcome:     rec.Outcome,
		Role:        rec.Role,
		Driver:      rec.Driver,
		DurationMs:  rec.DurationMs,
	}

	r.mu.Lock()
	r.steps++
	r.stepRecords = append(r.stepRecords, row)
	sink := r.sink
	r.mu.Unlock()

	if sink != nil {
		sink.Log(rec)
	}
}

// Write appends one redacted EKA-write row to the report.
func (r *Recorder) Write(e WriteEvent) {
	typ, id := splitLine(e.Line)
	row := WriteRecord{
		Line:            r.red.Redact(e.Line),
		Type:            r.red.Redact(typ),
		ID:              r.red.Redact(id),
		InstanceVersion: e.InstanceVersion,
		Kind:            r.red.Redact(e.Kind),
		Result:          r.red.Redact(e.Result),
	}
	r.mu.Lock()
	r.writeRecords = append(r.writeRecords, row)
	r.mu.Unlock()
}

// --- read-back ---

// Snapshot is an instantaneous reading of the Recorder's counters.
type Snapshot struct {
	Steps          int `json:"steps"`
	Retries        int `json:"retries"`
	Cancellations  int `json:"cancellations"`
	PeakInFlight   int `json:"peakInFlight"`
	PeakQueueDepth int `json:"peakQueueDepth"`
	InFlight       int `json:"inFlight"`
	QueueDepth     int `json:"queueDepth"`
	Dispatched     int `json:"dispatched"`
	Rejected       int `json:"rejected"`
}

// Snapshot returns the current counter values.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{
		Steps:          r.steps,
		Retries:        r.retries,
		Cancellations:  r.cancellations,
		PeakInFlight:   r.peakInFlight,
		PeakQueueDepth: r.peakQueueDepth,
		InFlight:       r.inFlight,
		QueueDepth:     r.queueDepth,
		Dispatched:     r.dispatched,
		Rejected:       r.rejected,
	}
}

// Report returns the deterministic run report (steps + EKA writes), sorted so
// the same input yields byte-identical JSON. The returned slices are copies.
func (r *Recorder) Report() Report {
	r.mu.Lock()
	steps := append([]StepRecord(nil), r.stepRecords...)
	writes := append([]WriteRecord(nil), r.writeRecords...)
	r.mu.Unlock()

	sortSteps(steps)
	sortWrites(writes)
	if steps == nil {
		steps = []StepRecord{}
	}
	if writes == nil {
		writes = []WriteRecord{}
	}
	return Report{Steps: steps, Writes: writes}
}
