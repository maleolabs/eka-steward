package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// stepJSON parses the single JSON log line a Logger emitted.
func stepJSON(t *testing.T, buf *bytes.Buffer) map[string]any {
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

// TestStepEmitsStructuredLog is the AC-1 unit: one step emits one JSON record
// carrying driver, role, outcome, candidateId, stepKind, attempt and
// durationMs.
func TestStepEmitsStructuredLog(t *testing.T) {
	var buf bytes.Buffer
	r := NewRecorder(Config{Driver: "claude", Role: "author"}, NewLogger(&buf))

	r.Step(StepEvent{
		CandidateID: "p/sto:i-1",
		Kind:        "dispatch",
		Outcome:     OutcomeOK,
		Attempts:    2,
		Duration:    1500 * time.Millisecond,
	})

	m := stepJSON(t, &buf)
	want := map[string]any{
		"driver":      "claude",
		"role":        "author",
		"outcome":     "ok",
		"candidateId": "p/sto:i-1",
		"stepKind":    "dispatch",
		"attempt":     float64(2),
		"durationMs":  float64(1500),
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("record[%q] = %v, want %v (record: %v)", k, m[k], v, m)
		}
	}
}

// TestSnapshotCounters pins the metric names and that steps, retries,
// cancellations and saturation counters read back.
func TestSnapshotCounters(t *testing.T) {
	r := NewRecorder(Config{}, nil)
	r.Step(StepEvent{CandidateID: "a", Kind: "dispatch", Outcome: OutcomeOK})
	r.Step(StepEvent{CandidateID: "b", Kind: "dispatch", Outcome: OutcomeFailed})
	r.QueueDepth(3)
	r.InFlight(2)
	r.PeakInFlight(4)
	r.Dispatched(5)
	r.Rejected(1)
	r.Retries(2)
	r.Cancellations(1)

	snap := r.Snapshot()
	if snap.Steps != 2 || snap.Retries != 2 || snap.Cancellations != 1 {
		t.Fatalf("steps/retries/cancels = %d/%d/%d, want 2/2/1", snap.Steps, snap.Retries, snap.Cancellations)
	}
	if snap.PeakInFlight != 4 || snap.QueueDepth != 3 || snap.Dispatched != 5 || snap.Rejected != 1 {
		t.Fatalf("saturation snapshot = %+v", snap)
	}
}

// TestReportListsStepsAndWrites asserts the exact deterministic report bytes
// for a known step + write set.
func TestReportListsStepsAndWrites(t *testing.T) {
	r := NewRecorder(Config{Driver: "d", Role: "r"}, nil)
	r.Step(StepEvent{CandidateID: "a", Kind: "dispatch", Outcome: OutcomeOK, Duration: 7 * time.Millisecond})
	r.Write(WriteEvent{Line: "p/sto:i-1", Kind: "transition", Result: "applied"})

	got, err := r.Report().MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `{"steps":[{"candidateId":"a","kind":"dispatch","outcome":"ok","role":"r","driver":"d","durationMs":7}],"writes":[{"line":"p/sto:i-1","type":"sto","id":"i-1","instanceVersion":0,"kind":"transition","result":"applied"}]}`
	if string(got) != want {
		t.Fatalf("report drifted:\n got: %s\n want: %s", got, want)
	}

	var buf bytes.Buffer
	if err := r.Report().Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.String() != want+"\n" {
		t.Fatalf("Write output = %q, want MarshalJSON + newline", buf.String())
	}
}

// TestReportDeterministicAcrossRuns feeds the same events in different
// insertion orders 20 times and requires byte-identical JSON every time (the
// sort removes map/insertion nondeterminism).
func TestReportDeterministicAcrossRuns(t *testing.T) {
	type ev struct {
		id  string
		dur time.Duration
	}
	steps := []ev{{"c", 3}, {"a", 1}, {"b", 2}}
	writes := []WriteEvent{
		{Line: "p/sto:i-2", Kind: "transition", Result: "applied"},
		{Line: "p/sto:i-1", Kind: "note", Result: "approve"},
	}
	const runs = 20
	var first []byte
	for run := 0; run < runs; run++ {
		r := NewRecorder(Config{Driver: "d", Role: "r"}, nil)
		// Rotate the insertion order by run so accumulation order varies.
		for k := 0; k < len(steps); k++ {
			s := steps[(k+run)%len(steps)]
			r.Step(StepEvent{CandidateID: s.id, Kind: "dispatch", Outcome: OutcomeOK, Duration: s.dur * time.Millisecond})
		}
		for k := 0; k < len(writes); k++ {
			r.Write(writes[(k+run)%len(writes)])
		}
		got, err := r.Report().MarshalJSON()
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

// TestRedactionNeverLeaksSecrets is the AC-4 unit: a secret-shaped token or a
// deny-list literal placed in any emitted field is masked in both the log line
// and the report.
func TestRedactionNeverLeaksSecrets(t *testing.T) {
	var buf bytes.Buffer
	r := NewRecorder(Config{
		Driver:  "sk-ABCDEFGH12345678",
		Role:    "author",
		Secrets: []string{"env-secret-value"},
	}, NewLogger(&buf))

	r.Step(StepEvent{
		CandidateID: "p/sto:ghp_ABCDEFGH1234",
		Kind:        "Bearer abc.def.ghi",
		Outcome:     "env-secret-value",
		Attempts:    1,
	})
	r.Write(WriteEvent{Line: "p/sto:AIzaABCDEFGH12345", Kind: "transition", Result: "applied"})

	report, err := r.Report().MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	logOut := buf.String()

	secrets := []string{
		"sk-ABCDEFGH12345678",
		"ghp_ABCDEFGH1234",
		"AIzaABCDEFGH12345",
		"abc.def.ghi",
		"env-secret-value",
	}
	for _, s := range secrets {
		if strings.Contains(logOut, s) {
			t.Fatalf("secret %q leaked into the log:\n%s", s, logOut)
		}
		if strings.Contains(string(report), s) {
			t.Fatalf("secret %q leaked into the report:\n%s", s, report)
		}
	}
	if !strings.Contains(logOut, Mask) {
		t.Fatalf("log has no mask marker; redaction did not run:\n%s", logOut)
	}
	if !strings.Contains(string(report), Mask) {
		t.Fatalf("report has no mask marker; redaction did not run:\n%s", report)
	}
}

// TestSplitLine parses the qualified line form.
func TestSplitLine(t *testing.T) {
	cases := []struct{ line, typ, id string }{
		{"p/sto:i-1", "sto", "i-1"},
		{"i-1", "", ""},
		{"p/sto", "", ""},
		{"ns/a/b:i-1", "b", "i-1"},
	}
	for _, c := range cases {
		typ, id := splitLine(c.line)
		if typ != c.typ || id != c.id {
			t.Fatalf("splitLine(%q) = (%q,%q), want (%q,%q)", c.line, typ, id, c.typ, c.id)
		}
	}
}
