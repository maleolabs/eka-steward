package loop

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/pool"
)

// This file covers the bounded retry + cancel/recovery work item
// (sto:cancel-retry-recovery). See docs/recovery.md for the model, the exact
// retry/cancel semantics and the no-partial-write argument.

// oneTodo is a single dispatchable candidate ("a", todo).
func oneTodo() []decision.Candidate {
	return []decision.Candidate{{ID: "a", State: decision.StateTodo}}
}

// retryLoop wires a write-path loop over cs with the given retry policy and an
// injected metrics recorder.
func retryLoop(t *testing.T, cs []decision.Candidate, d Dispatcher, fw Writer, retry RetryPolicy, rec pool.Metrics) (*Loop, *Store) {
	t.Helper()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	return New(fixtureSource{cs}, d, fw, store, Config{
		MaxWorkers:    8,
		DispatchState: "todo",
		Retry:         retry,
		Metrics:       rec,
	}), store
}

// flakyDispatcher fails the first failFirst dispatch attempts of every
// candidate, then succeeds. It is deterministic (no clock, no randomness).
type flakyDispatcher struct {
	mu        sync.Mutex
	failFirst int
	per       map[string]int
}

func newFlakyDispatcher(failFirst int) *flakyDispatcher {
	return &flakyDispatcher{failFirst: failFirst, per: map[string]int{}}
}

func (d *flakyDispatcher) Dispatch(_ context.Context, c decision.Candidate) (DispatchResult, error) {
	d.mu.Lock()
	d.per[c.ID]++
	n := d.per[c.ID]
	d.mu.Unlock()
	if n <= d.failFirst {
		return DispatchResult{}, errors.New("flaky: transient dispatch error")
	}
	return DispatchResult{CandidateID: c.ID}, nil
}

func (d *flakyDispatcher) calls(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.per[id]
}

// sleepRecorder records the backoff delays the loop requests without waiting.
type sleepRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *sleepRecorder) sleep(d time.Duration) {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
}

func (r *sleepRecorder) get() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.delays...)
}

// TestRetrySucceedsOnFirstAttempt pins the no-retry case: one attempt, one
// apply, zero retries.
func TestRetrySucceedsOnFirstAttempt(t *testing.T) {
	rec := &pool.Recorder{}
	d := newFlakyDispatcher(0) // never fails
	fw := newFakeWriter()
	l, store := retryLoop(t, oneTodo(), d, fw, RetryPolicy{MaxAttempts: 3}, rec)
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.calls("a"); got != 1 {
		t.Fatalf("dispatch attempts = %d, want 1", got)
	}
	if fw.applies != 1 {
		t.Fatalf("writer applies = %d, want exactly 1", fw.applies)
	}
	if got := rec.Snapshot().Retries; got != 0 {
		t.Fatalf("Retries = %d, want 0", got)
	}
	assertDone(t, store, "a")
}

// TestRetrySucceedsAfterFailure is the success-after-retry case: one retry, one
// apply (the retried dispatch never wrote on the failed attempt).
func TestRetrySucceedsAfterFailure(t *testing.T) {
	rec := &pool.Recorder{}
	d := newFlakyDispatcher(1) // fail once, then succeed
	fw := newFakeWriter()
	l, store := retryLoop(t, oneTodo(), d, fw, RetryPolicy{MaxAttempts: 3}, rec)
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.calls("a"); got != 2 {
		t.Fatalf("dispatch attempts = %d, want 2 (fail then succeed)", got)
	}
	if fw.applies != 1 {
		t.Fatalf("writer applies = %d, want exactly 1 (no partial write on the failed attempt)", fw.applies)
	}
	if got := rec.Snapshot().Retries; got != 1 {
		t.Fatalf("Retries = %d, want 1", got)
	}
	assertDone(t, store, "a")
}

// TestRetryExhaustedLeavesCandidateUndone pins the exhaustion path: exactly
// MaxAttempts attempts, zero applies, the candidate is deferred (un-Done, no
// in-flight intent) and Run returns the dispatch error.
func TestRetryExhaustedLeavesCandidateUndone(t *testing.T) {
	rec := &pool.Recorder{}
	d := newFlakyDispatcher(5) // always fails within MaxAttempts 3
	fw := newFakeWriter()
	l, store := retryLoop(t, oneTodo(), d, fw, RetryPolicy{MaxAttempts: 3}, rec)
	if err := l.Run(context.Background()); err == nil {
		t.Fatal("Run = nil, want the exhausted dispatch error")
	}
	if got := d.calls("a"); got != 3 {
		t.Fatalf("dispatch attempts = %d, want the MaxAttempts cap 3", got)
	}
	if fw.applies != 0 {
		t.Fatalf("writer applies = %d, want 0 (a failed worker must leave no partial EKA write)", fw.applies)
	}
	if got := rec.Snapshot().Retries; got != 2 {
		t.Fatalf("Retries = %d, want 2 (MaxAttempts-1)", got)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Has("a") {
		t.Fatal("candidate recorded done after exhausting retries, want deferred (un-Done)")
	}
	if len(st.InFlight) != 0 {
		t.Fatalf("InFlight = %+v, want empty (the failed intent must be rolled back)", st.InFlight)
	}
}

// TestRetryZeroValueIsNoRetry pins MaxAttempts semantics explicitly: the zero
// policy is one attempt, no retry (pre-retry behaviour).
func TestRetryZeroValueIsNoRetry(t *testing.T) {
	d := newFlakyDispatcher(1)
	fw := newFakeWriter()
	l, _ := retryLoop(t, oneTodo(), d, fw, RetryPolicy{}, nil)
	if err := l.Run(context.Background()); err == nil {
		t.Fatal("Run = nil, want the dispatch error (no retry)")
	}
	if got := d.calls("a"); got != 1 {
		t.Fatalf("dispatch attempts = %d, want 1 (zero policy = no retry)", got)
	}
	if fw.applies != 0 {
		t.Fatalf("writer applies = %d, want 0", fw.applies)
	}
}

// TestRetryIsDeterministicAcrossRuns asserts the attempt count is identical
// across N runs over the same inputs — retries are bounded and reproducible.
func TestRetryIsDeterministicAcrossRuns(t *testing.T) {
	const runs = 20
	for r := 0; r < runs; r++ {
		d := newFlakyDispatcher(1)
		fw := newFakeWriter()
		l, _ := retryLoop(t, oneTodo(), d, fw, RetryPolicy{MaxAttempts: 3}, nil)
		if err := l.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", r, err)
		}
		if got := d.calls("a"); got != 2 {
			t.Fatalf("run %d attempts = %d, want 2", r, got)
		}
		if fw.applies != 1 {
			t.Fatalf("run %d applies = %d, want 1", r, fw.applies)
		}
	}
}

// TestRetryBackoffSequenceDeterministic asserts the backoff is deterministic
// (the injected seam records the exact delay sequence) and that a retry run
// never waits on the wall clock.
func TestRetryBackoffSequenceDeterministic(t *testing.T) {
	const backoff = 5 * time.Millisecond
	rec := &sleepRecorder{}
	d := newFlakyDispatcher(9) // all attempts fail
	fw := newFakeWriter()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	l := New(fixtureSource{oneTodo()}, d, fw, store, Config{
		MaxWorkers:    8,
		DispatchState: "todo",
		Retry:         RetryPolicy{MaxAttempts: 4, Backoff: backoff},
		Sleep:         rec.sleep,
	})

	start := time.Now()
	if err := l.Run(context.Background()); err == nil {
		t.Fatal("Run = nil, want the exhausted dispatch error")
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("retry run waited on the wall clock (%v); the Sleep seam must absorb the backoff", elapsed)
	}
	if got := d.calls("a"); got != 4 {
		t.Fatalf("dispatch attempts = %d, want 4", got)
	}
	got := rec.get()
	if len(got) != 3 {
		t.Fatalf("backoff sleeps = %v, want 3 (one between each of 4 attempts)", got)
	}
	for i, d := range got {
		if d != backoff {
			t.Fatalf("sleep %d = %v, want the deterministic backoff %v", i, d, backoff)
		}
	}
}

// TestAttemptTimeoutIsRetryableAndBounded pins the timeout path: a per-attempt
// deadline makes a hung worker a retryable failure, bounded by MaxAttempts,
// with no partial write.
func TestAttemptTimeoutIsRetryableAndBounded(t *testing.T) {
	rec := &pool.Recorder{}
	d := newBlockingCancelableDisp()
	fw := newFakeWriter()
	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	l := New(fixtureSource{oneTodo()}, d, fw, store, Config{
		MaxWorkers:     8,
		DispatchState:  "todo",
		Retry:          RetryPolicy{MaxAttempts: 3},
		AttemptTimeout: 10 * time.Millisecond,
		Metrics:        rec,
	})
	if err := l.Run(context.Background()); err == nil {
		t.Fatal("Run = nil, want the exhausted (timed-out) dispatch error")
	}
	if got := d.calls(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (each attempt times out, bounded by MaxAttempts)", got)
	}
	if fw.applies != 0 {
		t.Fatalf("writer applies = %d, want 0 (a timed-out worker must leave no partial write)", fw.applies)
	}
	if got := rec.Snapshot().Retries; got != 2 {
		t.Fatalf("Retries = %d, want 2", got)
	}
	st, _ := store.Load()
	if st.Has("a") || len(st.InFlight) != 0 {
		t.Fatalf("state = %+v, want candidate deferred with no in-flight intent", st)
	}
}

// TestCancelCancelsInFlightWithoutWrite is the cancel AC: a blocking worker is
// canceled, its ctx is canceled, no EKA apply happens for it, Run returns nil
// deterministically, and the candidate is left un-Done (re-selectable).
func TestCancelCancelsInFlightWithoutWrite(t *testing.T) {
	cancellations, applies, undone := runCancelScenario(t, false)
	if cancellations != 1 {
		t.Fatalf("Cancellations = %d, want 1", cancellations)
	}
	if applies != 0 {
		t.Fatalf("writer applies = %d, want 0 (a canceled worker must leave no EKA write)", applies)
	}
	if !undone {
		t.Fatal("canceled candidate was recorded done, want deferred (un-Done)")
	}
}

// TestCancelIsDeterministicAcrossRuns runs the cancel scenario N times and
// requires an identical outcome every time.
func TestCancelIsDeterministicAcrossRuns(t *testing.T) {
	const runs = 20
	for r := 0; r < runs; r++ {
		cancellations, applies, undone := runCancelScenario(t, false)
		if cancellations != 1 || applies != 0 || !undone {
			t.Fatalf("run %d outcome = (cancellations=%d applies=%d undone=%v), want (1 0 true)",
				r, cancellations, applies, undone)
		}
	}
}

// runCancelScenario drives one cancel run and returns the observable outcome:
// the Cancellations metric, the writer apply count and whether the candidate is
// still un-Done. When pooled it exercises the worker pool path.
func runCancelScenario(t *testing.T, pooled bool) (cancellations, applies int, undone bool) {
	t.Helper()
	rec := &pool.Recorder{}
	d := newBlockingCancelableDisp()
	fw := newFakeWriter()

	store := &Store{Path: filepath.Join(t.TempDir(), "state.json")}
	cfg := Config{
		MaxWorkers:    4,
		DispatchState: "todo",
		Metrics:       rec,
	}
	if pooled {
		cfg.CgroupPrefix = writeCgroup(t, "17179869184\n", "1600000 100000\n")
		cfg.QueueLimit = 8
	}
	l := New(fixtureSource{oneTodo()}, d, fw, store, cfg)

	errc := make(chan error, 1)
	go func() { errc <- l.Run(context.Background()) }()

	// Wait until the worker is in flight, then cancel the batch.
	if !d.waitStarted(2 * time.Second) {
		t.Fatal("worker never started")
	}
	l.Cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil (clean cancel)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if !d.wasCanceled() {
		t.Fatal("in-flight worker's context was not canceled")
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return rec.Snapshot().Cancellations, fw.applies, !st.Has("a")
}

// TestCancelledCandidateReselectedOnNextRun proves the cancel left the
// candidate deferred: a fresh loop over the SAME store re-selects it and
// completes it normally.
func TestCancelledCandidateReselectedOnNextRun(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "state.json")}
	blocking := newBlockingCancelableDisp()
	fw := newFakeWriter()

	l := New(fixtureSource{oneTodo()}, blocking, fw, store, Config{MaxWorkers: 8, DispatchState: "todo"})
	errc := make(chan error, 1)
	go func() { errc <- l.Run(context.Background()) }()
	if !blocking.waitStarted(2 * time.Second) {
		t.Fatal("worker never started")
	}
	l.Cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run after cancel = %v, want nil", err)
	}

	// A fresh loop over the same checkpoint, now with a succeeding dispatcher.
	good := newCountingDispatcher()
	l2 := New(fixtureSource{oneTodo()}, good, fw, store, Config{MaxWorkers: 8, DispatchState: "todo"})
	if err := l2.Run(context.Background()); err != nil {
		t.Fatalf("reselect Run: %v", err)
	}
	if good.total() != 1 || fw.applies != 1 {
		t.Fatalf("reselect: dispatches=%d applies=%d, want 1 and 1", good.total(), fw.applies)
	}
	assertDone(t, store, "a")
}

// TestPooledCancelStopsBatch asserts cancel works on the bounded-pool path too:
// the in-flight worker is canceled, no write happens and Run returns nil.
func TestPooledCancelStopsBatch(t *testing.T) {
	cancellations, applies, undone := runCancelScenario(t, true)
	if cancellations != 1 {
		t.Fatalf("Cancellations = %d, want 1", cancellations)
	}
	if applies != 0 {
		t.Fatalf("writer applies = %d, want 0", applies)
	}
	if !undone {
		t.Fatal("canceled candidate was recorded done, want deferred")
	}
}

// TestRetryNoPartialWriteStoreBacked is the store-backed recovery smoke test:
// against a real EKA workspace and the real write path (internal/ekawrite), a
// worker that fails twice then succeeds applies the transition exactly once,
// and a re-run is a no-op (the replay gate still holds). Skipped under -short.
func TestRetryNoPartialWriteStoreBacked(t *testing.T) {
	if testing.Short() {
		t.Skip("store-backed recovery smoke test (opens a real EKA workspace)")
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
	d := newFlakyDispatcher(2) // fail twice, succeed on the third attempt
	l := New(fixtureSource{[]decision.Candidate{cand}}, d, EkawriteWriter{Writer: w, RepoPath: repo}, store, Config{
		MaxWorkers:    4,
		DispatchState: "todo",
		Retry:         RetryPolicy{MaxAttempts: 3},
	})
	if err := l.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.calls(target); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}

	// Exactly one instance, moved to todo — the failed attempts wrote nothing.
	units, err := rt.Resolver().ResolveLine("p", "sto", "i-1")
	if err != nil || len(units) != 1 {
		t.Fatalf("ResolveLine = %d units, %v; want exactly 1", len(units), err)
	}
	if got := units[0].StateVector.ExecutionState; got != "todo" {
		t.Fatalf("i-1 execution-state = %q, want todo", got)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Has(target) || len(st.InFlight) != 0 {
		t.Fatalf("checkpoint = %+v, want done and no in-flight", st)
	}

	// A re-run over the same store is a no-op (replay gate / already done).
	before := d.calls(target)
	if err := l.Run(ctx); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := d.calls(target); got != before {
		t.Fatalf("re-run dispatched again (%d > %d); completed work must not re-dispatch", got, before)
	}
}

// --- test doubles ---

// blockingCancelableDisp blocks every dispatch until its context is canceled,
// recording how many dispatches entered and how many observed cancellation.
type blockingCancelableDisp struct {
	started chan struct{}
	mu      sync.Mutex
	entered int
	done    int
}

func newBlockingCancelableDisp() *blockingCancelableDisp {
	return &blockingCancelableDisp{started: make(chan struct{}, 1)}
}

func (d *blockingCancelableDisp) Dispatch(ctx context.Context, _ decision.Candidate) (DispatchResult, error) {
	d.mu.Lock()
	d.entered++
	d.mu.Unlock()
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	d.mu.Lock()
	d.done++
	d.mu.Unlock()
	return DispatchResult{}, ctx.Err()
}

func (d *blockingCancelableDisp) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entered
}

func (d *blockingCancelableDisp) wasCanceled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.done > 0
}

func (d *blockingCancelableDisp) waitStarted(timeout time.Duration) bool {
	select {
	case <-d.started:
		return true
	case <-time.After(timeout):
		return false
	}
}

// assertDone fails unless id is recorded done.
func assertDone(t *testing.T, store *Store, id string) {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Has(id) {
		t.Fatalf("state missing done id %q: %+v", id, st)
	}
}
