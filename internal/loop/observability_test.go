package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
)

// This file covers the observability work item (sto:steward-observability):
// every loop step emits a structured record (driver/role/outcome), the counters
// reflect the run, and the run report lists the steps and the EKA writes. See
// docs/observability.md.

// testClock is a deterministic, monotonically increasing clock: each call
// advances one millisecond, so a step's measured duration is exactly 1ms.
func testClock() func() time.Time {
	n := 0
	return func() time.Time {
		n++
		return time.Unix(0, int64(n)*int64(time.Millisecond))
	}
}

// singleLine parses the one JSON record a Logger emitted for a step.
func singleLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line, got %d:\n%s", len(lines), buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, lines[0])
	}
	return m
}

// TestLoopStepEmitsStructuredRecord drives one write-path step and asserts the
// emitted record carries driver/role/outcome plus the step detail, the step
// counter advanced, and the run report lists the step and its EKA write.
func TestLoopStepEmitsStructuredRecord(t *testing.T) {
	var buf bytes.Buffer
	rec := obs.NewRecorder(obs.Config{Driver: "fake", Role: "author"}, obs.NewLogger(&buf))

	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	cand := decision.Candidate{ID: "p/sto:a", State: decision.StateTodo}
	l := New(fixtureSource{[]decision.Candidate{cand}}, newCountingDispatcher(), newFakeWriter(), store, Config{
		MaxWorkers:    8,
		DispatchState: "todo",
		Metrics:       rec,
		Observer:      rec,
		Now:           testClock(),
	})

	step, ok, err := l.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if !ok || step.CandidateID != "p/sto:a" {
		t.Fatalf("Step = (%+v, %v), want p/sto:a", step, ok)
	}

	m := singleLine(t, &buf)
	want := map[string]any{
		"driver":      "fake",
		"role":        "author",
		"outcome":     "ok",
		"candidateId": "p/sto:a",
		"stepKind":    "dispatch",
		"attempt":     float64(1),
		"durationMs":  float64(1),
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("record[%q] = %v, want %v (record: %v)", k, m[k], v, m)
		}
	}

	if got := rec.Snapshot().Steps; got != 1 {
		t.Fatalf("Steps = %d, want 1", got)
	}

	rep := rec.Report()
	if len(rep.Steps) != 1 {
		t.Fatalf("report steps = %+v, want 1", rep.Steps)
	}
	if s := rep.Steps[0]; s.CandidateID != "p/sto:a" || s.Kind != "dispatch" || s.Outcome != "ok" || s.Driver != "fake" || s.Role != "author" {
		t.Fatalf("report step = %+v", s)
	}
	if len(rep.Writes) != 1 {
		t.Fatalf("report writes = %+v, want 1", rep.Writes)
	}
	if w := rep.Writes[0]; w.Line != "p/sto:a" || w.Type != "sto" || w.ID != "a" || w.Kind != "transition" || w.Result != "applied" {
		t.Fatalf("report write = %+v", w)
	}
}

// TestObservabilityCountersReflectRun covers steps/retries/cancellations and
// pool saturation in small fake runs.
func TestObservabilityCountersReflectRun(t *testing.T) {
	t.Run("retries", func(t *testing.T) {
		var buf bytes.Buffer
		rec := obs.NewRecorder(obs.Config{}, obs.NewLogger(&buf))
		store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
		l := New(fixtureSource{oneTodo()}, newFlakyDispatcher(1), newFakeWriter(), store, Config{
			MaxWorkers:    8,
			DispatchState: "todo",
			Retry:         RetryPolicy{MaxAttempts: 3},
			Metrics:       rec,
			Observer:      rec,
		})
		if err := l.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		snap := rec.Snapshot()
		if snap.Steps != 1 || snap.Retries != 1 {
			t.Fatalf("snapshot = %+v, want Steps 1 Retries 1", snap)
		}
		// The step record carries the attempt count (2: one failure + one success).
		if m := singleLine(t, &buf); m["attempt"] != float64(2) {
			t.Fatalf("attempt = %v, want 2", m["attempt"])
		}
	})

	t.Run("saturation", func(t *testing.T) {
		const (
			sizing     = 4
			queueLimit = 8
		)
		rec := obs.NewRecorder(obs.Config{}, nil)
		store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
		d := newBlockingDisp()
		l := New(fixtureSource{bpCandidates(10)}, d, nil, store, Config{
			MaxWorkers:   4,
			CgroupPrefix: writeCgroup(t, "17179869184\n", "1600000 100000\n"),
			QueueLimit:   queueLimit,
			Metrics:      rec,
			Observer:     rec,
		})

		errc := make(chan error, 1)
		go func() { errc <- l.Run(context.Background()) }()
		waitForPeak(t, d.peak, sizing)
		close(d.release)
		if err := <-errc; err != nil {
			t.Fatalf("Run: %v", err)
		}

		snap := rec.Snapshot()
		if snap.PeakInFlight != sizing {
			t.Fatalf("PeakInFlight = %d, want %d", snap.PeakInFlight, sizing)
		}
		if snap.Dispatched != queueLimit || snap.Steps != queueLimit {
			t.Fatalf("Dispatched/Steps = %d/%d, want %d each (deterministic overflow)", snap.Dispatched, snap.Steps, queueLimit)
		}
		if snap.Rejected < 1 {
			t.Fatalf("Rejected = %d, want >= 1 (overflow observable)", snap.Rejected)
		}
		if snap.PeakQueueDepth > queueLimit {
			t.Fatalf("PeakQueueDepth = %d, exceeds QueueLimit %d", snap.PeakQueueDepth, queueLimit)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		rec := obs.NewRecorder(obs.Config{}, nil)
		store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
		d := newBlockingCancelableDisp()
		l := New(fixtureSource{oneTodo()}, d, newFakeWriter(), store, Config{
			MaxWorkers:    4,
			DispatchState: "todo",
			Metrics:       rec,
			Observer:      rec,
		})

		errc := make(chan error, 1)
		go func() { errc <- l.Run(context.Background()) }()
		if !d.waitStarted(2 * time.Second) {
			t.Fatal("worker never started")
		}
		l.Cancel()
		if err := <-errc; err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}

		snap := rec.Snapshot()
		if snap.Cancellations != 1 || snap.Steps != 1 {
			t.Fatalf("snapshot = %+v, want Cancellations 1 Steps 1", snap)
		}
		if rep := rec.Report(); len(rep.Steps) != 1 || rep.Steps[0].Outcome != obs.OutcomeCancelled {
			t.Fatalf("cancelled step record = %+v, want one cancelled outcome", rep.Steps)
		}
	})
}

// TestObservabilityReportDeterministicAcrossRuns runs the sequential loop over
// the same fixture 20 times with a deterministic clock and requires
// byte-identical run reports every time.
func TestObservabilityReportDeterministicAcrossRuns(t *testing.T) {
	cs := []decision.Candidate{
		{ID: "p/sto:c", State: decision.StateTodo},
		{ID: "p/sto:a", State: decision.StateTodo},
		{ID: "p/sto:b", State: decision.StateTodo},
	}
	const runs = 20
	var first []byte
	for run := 0; run < runs; run++ {
		rec := obs.NewRecorder(obs.Config{Driver: "fake", Role: "author"}, nil)
		store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
		l := New(fixtureSource{cs}, newCountingDispatcher(), newFakeWriter(), store, Config{
			MaxWorkers:    8,
			DispatchState: "todo",
			Metrics:       rec,
			Observer:      rec,
			Now:           testClock(),
		})
		if err := l.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got, err := rec.Report().MarshalJSON()
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if run == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d report differs from run 0:\n%s\nvs\n%s", run, got, first)
		}
	}
}

// TestObservabilityStoreBackedWriteListing is the store-backed report test: a
// real EKA workspace + the real write path, and the run report lists the step
// and the transition it applied. Skipped under -short.
func TestObservabilityStoreBackedWriteListing(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed report test (opens a real EKA workspace)")
	}
	ctx := context.Background()
	t.Setenv("EKA_HOME", t.TempDir())
	rt, err := api.Ensure()
	if err != nil {
		t.Fatalf("api.Ensure: %v", err)
	}
	defer rt.Close()

	w := ekawrite.NewSteward(rt)
	repo := testRepo(t, "p", "p-repo", "p")
	if _, err := w.Sync(repo, api.SyncOptions{}); err != nil {
		t.Fatalf("register repo: %v", err)
	}
	if _, err := w.Publish(ekawrite.PublishInput{Project: "p", Namespace: "p", Type: "sto", ID: "i-1", Content: stoContent("d")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	target := "p/sto:i-1"
	cand := decision.Candidate{ID: target, State: decision.StatePlanned}
	store := &Store{Path: filepath.Join(t.TempDir(), "checkpoint.json")}
	rec := obs.NewRecorder(obs.Config{Driver: "fake", Role: "author"}, nil)
	l := New(fixtureSource{[]decision.Candidate{cand}}, newCountingDispatcher(), EkawriteWriter{Writer: w, RepoPath: repo}, store, Config{
		MaxWorkers:    4,
		DispatchState: "todo",
		Metrics:       rec,
		Observer:      rec,
		Now:           testClock(),
	})
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rep := rec.Report()
	if len(rep.Steps) != 1 || rep.Steps[0].CandidateID != target || rep.Steps[0].Outcome != obs.OutcomeOK {
		t.Fatalf("report steps = %+v, want the one ok step", rep.Steps)
	}
	if len(rep.Writes) != 1 {
		t.Fatalf("report writes = %+v, want 1", rep.Writes)
	}
	if got := rep.Writes[0]; got.Line != target || got.Type != "sto" || got.ID != "i-1" || got.Kind != "transition" || got.Result != "applied" {
		t.Fatalf("report write = %+v, want the transition on %s", got, target)
	}

	// The report matches the store: the line moved to todo.
	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 || units[0].StateVector.ExecutionState != "todo" {
		t.Fatalf("store line = %+v, %v; want one unit at todo", units, err)
	}
}
