package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/app"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
)

// fakeEngine is a store-free Engine for protocol tests. Every method is
// deterministic; Run optionally blocks until released so a cancel test can
// observe the mid-run cancel.
type fakeEngine struct {
	mu      sync.Mutex
	plan    []loop.Step
	step    loop.Step
	stepOK  bool
	ran     chan struct{} // closed (once) when Run/RunSteps completes normally
	block   chan struct{} // when non-nil, Run waits on it or ctx.Done()
	runs    int
	approve []string
	evCh    chan obs.Event // last subscription channel opened by Subscribe
}

func (e *fakeEngine) Plan(context.Context) ([]loop.Step, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]loop.Step(nil), e.plan...), nil
}

func (e *fakeEngine) Step(context.Context) (loop.Step, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.step, e.stepOK, nil
}

func (e *fakeEngine) Run(ctx context.Context) error {
	e.mu.Lock()
	e.runs++
	block := e.block
	e.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.signalRan()
	return nil
}

func (e *fakeEngine) RunSteps(ctx context.Context, _ int) error { return e.Run(ctx) }

func (e *fakeEngine) Cancel() {}

func (e *fakeEngine) Status() app.Status { return app.Status{} }

func (e *fakeEngine) Approve(requestID, decision string) error {
	e.mu.Lock()
	e.approve = append(e.approve, requestID+"="+decision)
	e.mu.Unlock()
	return nil
}

func (e *fakeEngine) Close() error { return nil }

// Subscribe opens a fresh buffered channel per call; the test pushes events
// with push and the returned cancel closes the channel (so the server's
// forwarding goroutine exits, exactly like the real stream's cancel).
func (e *fakeEngine) Subscribe(_ []string, _ uint64) (<-chan obs.Event, func(), error) {
	ch := make(chan obs.Event, 8)
	var once sync.Once
	e.mu.Lock()
	e.evCh = ch
	e.mu.Unlock()
	return ch, func() { once.Do(func() { close(ch) }) }, nil
}

// push sends one event to the most recent subscription channel.
func (e *fakeEngine) push(ev obs.Event) {
	e.mu.Lock()
	ch := e.evCh
	e.mu.Unlock()
	ch <- ev
}

// hasSub reports whether a subscription channel has been opened.
func (e *fakeEngine) hasSub() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.evCh != nil
}

func (e *fakeEngine) signalRan() {
	e.mu.Lock()
	ch := e.ran
	e.ran = nil
	e.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (e *fakeEngine) runCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

func (e *fakeEngine) approvals() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.approve...)
}

// goldenEngine is the fixed engine the golden transcripts are pinned against.
func goldenEngine() *fakeEngine {
	return &fakeEngine{
		plan: []loop.Step{
			{CandidateID: "p/sto:a", Kind: loop.KindDispatch, Priority: 0, State: "planned", CreatedSeq: 1},
			{CandidateID: "p/sto:b", Kind: loop.KindDispatch, Priority: 1, State: "planned", CreatedSeq: 2},
		},
		step:   loop.Step{CandidateID: "p/sto:a", Kind: loop.KindDispatch, Priority: 0, State: "planned", CreatedSeq: 1},
		stepOK: true,
	}
}

// initializeLine is the handshake a golden transcript labelled init:true sends
// before its request; its response is not compared.
const initializeLine = `{"jsonrpc":"2.0","id":"init","method":"steward/initialize","params":{}}`

// TestGoldenTranscripts validates every pinned request/response transcript
// under testdata against a fresh server. Set UPDATE_GOLDEN=1 to regenerate.
func TestGoldenTranscripts(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil {
		t.Fatalf("glob goldens: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no golden transcripts found under testdata/")
	}
	update := os.Getenv("UPDATE_GOLDEN") == "1"

	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			init, request, want := parseGolden(t, data)

			srv := NewServer(goldenEngine())
			if init {
				srv.HandleMessage([]byte(initializeLine))
			}
			got := srv.HandleMessage([]byte(request))

			if update {
				writeGolden(t, path, init, request, string(got))
				return
			}
			if want == "" {
				t.Fatalf("%s has no expected response (run with UPDATE_GOLDEN=1)", path)
			}
			if string(got) != want {
				t.Fatalf("golden mismatch for %s\n got: %s\nwant: %s", path, got, want)
			}
		})
	}
}

// parseGolden extracts the init flag and the request/expected-response lines.
// Lines starting with '#' are comments; the first directive containing
// "init:" sets the init flag; the first two non-comment lines are the request
// and its expected response.
func parseGolden(t *testing.T, data []byte) (init bool, request, want string) {
	t.Helper()
	var payload []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.Contains(line, "init:") {
				init = strings.Contains(line, "init: true")
			}
			continue
		}
		payload = append(payload, line)
	}
	if len(payload) == 0 {
		t.Fatal("golden has no request line")
	}
	request = payload[0]
	if len(payload) > 1 {
		want = payload[1]
	}
	return init, request, want
}

// writeGolden rewrites one golden file in the canonical format.
func writeGolden(t *testing.T, path string, init bool, request, response string) {
	t.Helper()
	flag := "false"
	if init {
		flag = "true"
	}
	body := "# golden transcript: request line, then expected response line\n" +
		"# init: " + flag + "\n" + request + "\n" + response + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write golden %s: %v", path, err)
	}
}

// --- full cycle over an in-memory pipe ---

// pipeClient drives a Server over a pair of in-memory pipes (the stdio
// transport) for an interactive transcript.
type pipeClient struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
}

func newPipeClient(t *testing.T, eng Engine) *pipeClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(eng)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(inR, outW) }()
	return &pipeClient{t: t, in: inW, out: bufio.NewReader(outR), done: done}
}

func (c *pipeClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *pipeClient) recv() []byte {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("recv: %v", err)
	}
	return bytes.TrimSpace(line)
}

func (c *pipeClient) close() { _ = c.in.Close() }

// TestFullCycle drives initialize -> plan -> run -> status -> shutdown over the
// stdio transport and asserts the complete cycle.
func TestFullCycle(t *testing.T) {
	ran := make(chan struct{})
	eng := &fakeEngine{plan: goldenEngine().plan, step: goldenEngine().step, stepOK: true, ran: ran}
	c := newPipeClient(t, eng)
	defer c.close()

	c.send(`{"jsonrpc":"2.0","id":1,"method":"steward/initialize","params":{"protocolVersions":["1"]}}`)
	if got := resultField(t, c.recv(), "protocolVersion"); got != "1" {
		t.Fatalf("initialize protocolVersion = %v, want 1", got)
	}

	c.send(`{"jsonrpc":"2.0","id":2,"method":"steward/plan","params":{"project":"p"}}`)
	res := resultMap(t, c.recv())
	if sel, _ := res["selected"].(float64); int(sel) != 2 {
		t.Fatalf("plan selected = %v, want 2", res["selected"])
	}

	c.send(`{"jsonrpc":"2.0","id":3,"method":"steward/run","params":{"project":"p"}}`)
	runID, _ := resultMap(t, c.recv())["runId"].(string)
	if runID != "run-1" {
		t.Fatalf("run runId = %q, want run-1", runID)
	}
	<-ran // wait for the run goroutine before asking for status

	c.send(`{"jsonrpc":"2.0","id":4,"method":"steward/status","params":{"runId":"run-1"}}`)
	if state, _ := resultMap(t, c.recv())["state"].(string); state != runDone {
		t.Fatalf("status state = %q, want done", state)
	}

	c.send(`{"jsonrpc":"2.0","id":5,"method":"steward/shutdown","params":{}}`)
	if stop, _ := resultMap(t, c.recv())["stopping"].(bool); !stop {
		t.Fatal("shutdown stopping = false, want true")
	}

	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

// TestIdempotencyKey ensures same key+params replays the identical result with
// no second effect, and a changed params under the same key conflicts.
func TestIdempotencyKey(t *testing.T) {
	eng := &fakeEngine{}
	srv := NewServer(eng)
	srv.HandleMessage([]byte(initializeLine))

	req := `{"jsonrpc":"2.0","id":1,"method":"steward/run","params":{"project":"p","idempotencyKey":"k1"}}`
	first := srv.HandleMessage([]byte(req))
	second := srv.HandleMessage([]byte(req))
	if string(first) != string(second) {
		t.Fatalf("same key+params must replay identically:\n first: %s\nsecond: %s", first, second)
	}
	// Run is launched in a goroutine; wait for it to observe the single effect.
	waitFor(t, func() bool { return eng.runCount() == 1 })
	if n := eng.runCount(); n != 1 {
		t.Fatalf("engine Run called %d times, want 1 (no duplicate effect)", n)
	}

	// A different JSON-RPC id still replays the stored RESULT under the new id.
	firstRunID, _ := resultMap(t, first)["runId"].(string)
	if firstRunID == "" {
		t.Fatalf("first run has no runId: %s", first)
	}
	changedID := `{"jsonrpc":"2.0","id":99,"method":"steward/run","params":{"project":"p","idempotencyKey":"k1"}}`
	if got := resultField(t, srv.HandleMessage([]byte(changedID)), "runId"); got != firstRunID {
		t.Fatalf("replay under new id runId = %v, want %v", got, firstRunID)
	}

	// Same key, different params -> idempotency_conflict.
	conflict := `{"jsonrpc":"2.0","id":2,"method":"steward/run","params":{"project":"other","idempotencyKey":"k1"}}`
	if code := errorCode(t, srv.HandleMessage([]byte(conflict))); code != CodeIdempotencyConflict {
		t.Fatalf("conflict code = %d, want %d", code, CodeIdempotencyConflict)
	}
	if n := eng.runCount(); n != 1 {
		t.Fatalf("conflict must not start a second run; Run called %d times", n)
	}
}

// TestVersionNegotiation covers additive selection and the un-negotiated gate.
func TestVersionNegotiation(t *testing.T) {
	t.Run("highest-supported", func(t *testing.T) {
		srv := NewServer(&fakeEngine{})
		got := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"steward/initialize","params":{"protocolVersions":["2","1"]}}`))
		if v := resultField(t, got, "protocolVersion"); v != "1" {
			t.Fatalf("negotiated %v, want 1", v)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		srv := NewServer(&fakeEngine{})
		got := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"steward/initialize","params":{"protocolVersions":["9"]}}`))
		if code := errorCode(t, got); code != CodeVersionUnsupported {
			t.Fatalf("code = %d, want %d", code, CodeVersionUnsupported)
		}
	})
	t.Run("not-initialized", func(t *testing.T) {
		srv := NewServer(&fakeEngine{})
		got := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"steward/plan","params":{"project":"p"}}`))
		if code := errorCode(t, got); code != CodeNotInitialized {
			t.Fatalf("code = %d, want %d", code, CodeNotInitialized)
		}
	})
}

// TestCancelMidRun cancels a blocked run and asserts the deterministic cancel.
func TestCancelMidRun(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	eng := &fakeEngine{block: block}
	srv := NewServer(eng)
	srv.HandleMessage([]byte(initializeLine))

	run := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"steward/run","params":{"project":"p"}}`))
	runID, _ := resultMap(t, run)["runId"].(string)

	// Wait for Run to be in flight before cancelling.
	waitFor(t, func() bool { return eng.runCount() == 1 })

	cancel := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":2,"method":"steward/cancel","params":{"runId":"` + runID + `"}}`))
	if c, _ := resultMap(t, cancel)["canceled"].(bool); !c {
		t.Fatal("cancel canceled = false, want true")
	}

	// Cancel is idempotent.
	cancel2 := srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":3,"method":"steward/cancel","params":{"runId":"` + runID + `"}}`))
	if c, _ := resultMap(t, cancel2)["canceled"].(bool); !c {
		t.Fatal("second cancel canceled = false, want true")
	}

	waitFor(t, func() bool {
		st := resultMap(t, srv.HandleMessage([]byte(`{"jsonrpc":"2.0","id":4,"method":"steward/status","params":{"runId":"`+runID+`"}}`)))
		return st["state"] == runCanceled
	})
}

// TestFrameRefusals covers malformed JSON and an oversized line.
func TestFrameRefusals(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		srv := NewServer(&fakeEngine{})
		if code := errorCode(t, srv.HandleMessage([]byte(`{not json`))); code != codeParseError {
			t.Fatalf("code = %d, want parse error %d", code, codeParseError)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		srv := NewServer(&fakeEngine{})
		// The cap must be larger than the valid initialize line but smaller
		// than the offending one, so only the first line is refused.
		srv.SetMaxLineSize(128)
		var out bytes.Buffer
		// A request line well past the cap; Serve must refuse and resync,
		// then process the following valid line.
		input := strings.Repeat("x", 300) + "\n" + `{"jsonrpc":"2.0","id":1,"method":"steward/initialize","params":{}}` + "\n"
		if err := srv.Serve(strings.NewReader(input), &out); err != nil {
			t.Fatalf("Serve: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("got %d response lines, want 2 (refusal + reply): %q", len(lines), out.String())
		}
		if code := errorCode(t, []byte(lines[0])); code != codeInvalidRequest {
			t.Fatalf("oversized refusal code = %d, want %d", code, codeInvalidRequest)
		}
		if v := resultField(t, []byte(lines[1]), "protocolVersion"); v != "1" {
			t.Fatalf("post-resync initialize protocolVersion = %v, want 1", v)
		}
	})
}

// TestNotifyWritesNotificationLine proves the transport can write a
// notification line (the hook the later event stream uses); v1 wires no source.
func TestNotifyWritesNotificationLine(t *testing.T) {
	srv := NewServer(&fakeEngine{})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(inR, outW) }()

	// Wait until Serve has published its writer, then emit one notification.
	waitFor(t, func() bool {
		srv.outMu.Lock()
		defer srv.outMu.Unlock()
		return srv.out != nil
	})
	// Notify writes to the output pipe, which blocks until the test reads it,
	// so run it concurrently with the read.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Notify("steward/event", map[string]any{"seq": 1}) }()
	br := bufio.NewReader(outR)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode notification %q: %v", line, err)
	}
	if m["method"] != "steward/event" {
		t.Fatalf("notification method = %v, want steward/event", m["method"])
	}
	if _, hasID := m["id"]; hasID {
		t.Fatal("notification must carry no id")
	}
	_ = inW.Close()
	<-done
}

// TestEventsSubscribeOverPipe drives events.subscribe -> steward/event
// notifications -> events.unsubscribe over the stdio transport and proves the
// notification lines interleave correctly with replies (one JSON object per
// line, notifications carry no id).
func TestEventsSubscribeOverPipe(t *testing.T) {
	eng := &fakeEngine{}
	c := newPipeClient(t, eng)
	defer c.close()

	c.send(initializeLine)
	_ = c.recv()

	c.send(`{"jsonrpc":"2.0","id":2,"method":"steward/events.subscribe","params":{"types":[],"fromSeq":0}}`)
	sub := resultMap(t, c.recv())
	if ok, _ := sub["subscribed"].(bool); !ok {
		t.Fatalf("subscribe result = %v, want subscribed:true", sub)
	}
	subID, _ := sub["subscriptionId"].(string)
	if subID == "" {
		t.Fatalf("subscribe result has no subscriptionId: %v", sub)
	}
	waitFor(t, eng.hasSub)

	eng.push(obs.Event{Seq: 1, Type: obs.EventStep, Data: obs.StepData{CandidateID: "p/sto:i-1"}})
	eng.push(obs.Event{Seq: 2, Type: obs.EventWrite, Data: obs.WriteData{Line: "p/sto:i-1"}})

	// Interleave a normal request with the pending notifications: read lines
	// until both the plan reply and the two notifications have arrived, in any
	// order. Every line must be valid JSON (no byte interleaving).
	c.send(`{"jsonrpc":"2.0","id":3,"method":"steward/plan","params":{"project":"p"}}`)
	var notifSeqs []uint64
	gotPlan := false
	deadline := time.Now().Add(2 * time.Second)
	for len(notifSeqs) < 2 || !gotPlan {
		if time.Now().After(deadline) {
			t.Fatalf("timed out; notifications=%v planReply=%v", notifSeqs, gotPlan)
		}
		line := c.recv()
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("line is not JSON: %v\n%s", err, line)
		}
		if method, _ := m["method"].(string); method == MethodEvent {
			if _, hasID := m["id"]; hasID {
				t.Fatal("event notification must carry no id")
			}
			params, _ := m["params"].(map[string]any)
			seq, _ := params["seq"].(float64)
			notifSeqs = append(notifSeqs, uint64(seq))
			continue
		}
		if id, _ := m["id"].(float64); id == 3 {
			gotPlan = true
		}
	}
	if len(notifSeqs) != 2 || notifSeqs[0] != 1 || notifSeqs[1] != 2 {
		t.Fatalf("notification seq = %v, want [1 2]", notifSeqs)
	}

	c.send(`{"jsonrpc":"2.0","id":4,"method":"steward/events.unsubscribe","params":{"subscriptionId":"` + subID + `"}}`)
	if ok, _ := resultMap(t, c.recv())["unsubscribed"].(bool); !ok {
		t.Fatal("unsubscribe result = false, want true")
	}
}

// --- helpers ---

func resultMap(t *testing.T, resp []byte) map[string]any {
	t.Helper()
	var m struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(resp, &m); err != nil {
		t.Fatalf("decode response %q: %v", resp, err)
	}
	if m.Result == nil {
		t.Fatalf("response has no result: %s", resp)
	}
	return m.Result
}

func resultField(t *testing.T, resp []byte, field string) any {
	t.Helper()
	return resultMap(t, resp)[field]
}

func errorCode(t *testing.T, resp []byte) int {
	t.Helper()
	var m struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &m); err != nil {
		t.Fatalf("decode error response %q: %v", resp, err)
	}
	if m.Error == nil {
		t.Fatalf("response has no error: %s", resp)
	}
	return m.Error.Code
}

// waitFor polls cond until true or a bounded deadline elapses.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within the poll budget")
}
