package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
)

// fakeConfig configures the scripted fake ACP agent used by the hermetic
// tests. It is a fixed script — not an LLM — mirroring internal/acp/probe/
// mock_agent.py in Go, so the default test path needs no Python and no
// network.
type fakeConfig struct {
	// version is the agent's maximum supported protocol version (default 1).
	version int
	// permission makes the turn emit a session/request_permission request.
	permission bool
	// permKind is the tool-call kind of the permission request (default
	// "execute").
	permKind string
	// permPath is the tool-call path of the permission request, if any.
	permPath string
	// hang makes the turn send one update then block until session/cancel,
	// so a test can exercise deterministic cancellation.
	hang bool
	// fsReadPath, when set, makes the turn issue a v1 fs/read_text_file
	// request for the path and record the client's answer.
	fsReadPath string
	// terminalMethod, when set, makes the turn issue that v1 terminal/*
	// request and record the client's answer.
	terminalMethod string
}

// fakeAgent is a newline-delimited JSON-RPC ACP agent over a byte stream. It
// records the prompts it receives and the permission option the client
// selected.
type fakeAgent struct {
	cfg fakeConfig

	mu  sync.Mutex
	out *bufio.Writer

	reqMu sync.Mutex
	reqs  map[string]chan Message
	seq   int

	negotiated int

	promptMu sync.Mutex
	prompts  []string

	// permOutcomes receives the option id (or "cancelled") the client
	// selected for each permission request, in order.
	permOutcomes chan string
	// fsOutcomes receives the client's answer to each fs request: the file
	// content on success, or "error:<message>" on a JSON-RPC error.
	fsOutcomes chan string
	// terminalOutcomes receives the client's answer to each terminal/*
	// request, formatted like fsOutcomes.
	terminalOutcomes chan string
	// cancelCh is closed when a session/cancel notification arrives.
	cancelCh chan struct{}
	// closed is closed when serve returns.
	closed chan struct{}
}

func newFakeAgent(cfg fakeConfig) *fakeAgent {
	if cfg.version == 0 {
		cfg.version = Version1
	}
	if cfg.permKind == "" {
		cfg.permKind = "execute"
	}
	return &fakeAgent{
		cfg:              cfg,
		reqs:             map[string]chan Message{},
		permOutcomes:     make(chan string, 4),
		fsOutcomes:       make(chan string, 4),
		terminalOutcomes: make(chan string, 4),
		cancelCh:         make(chan struct{}),
		closed:           make(chan struct{}),
	}
}

// PermOutcome returns the next recorded permission outcome (blocks).
func (f *fakeAgent) PermOutcome() string { return <-f.permOutcomes }

// FSOutcome returns the next recorded fs-request answer (blocks).
func (f *fakeAgent) FSOutcome() string { return <-f.fsOutcomes }

// TerminalOutcome returns the next recorded terminal-request answer (blocks).
func (f *fakeAgent) TerminalOutcome() string { return <-f.terminalOutcomes }

// Prompts returns the prompts received so far.
func (f *fakeAgent) Prompts() []string {
	f.promptMu.Lock()
	defer f.promptMu.Unlock()
	out := make([]string, len(f.prompts))
	copy(out, f.prompts)
	return out
}

// serve runs the agent read loop until the stream ends.
func (f *fakeAgent) serve(r io.Reader, w io.Writer) {
	f.out = bufio.NewWriter(w)
	f.negotiated = f.cfg.version
	defer close(f.closed)

	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m Message
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.isResponse():
			f.routeResponse(m)
		case m.isNotification():
			f.routeNotification(m)
		default:
			f.handleRequest(m)
		}
	}
}

// routeResponse delivers a response to the waiter registered for its id.
func (f *fakeAgent) routeResponse(m Message) {
	f.reqMu.Lock()
	ch := f.reqs[idKey(m.ID)]
	f.reqMu.Unlock()
	if ch != nil {
		ch <- m
	}
}

// idKey normalizes a JSON id (number or string) to the plain key the fake
// registers its requests under.
func idKey(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// routeNotification handles one-way notifications.
func (f *fakeAgent) routeNotification(m Message) {
	if m.Method == "session/cancel" {
		select {
		case <-f.cancelCh:
		default:
			close(f.cancelCh)
		}
	}
}

// handleRequest answers one client request.
func (f *fakeAgent) handleRequest(m Message) {
	switch m.Method {
	case "initialize":
		f.handleInitialize(m)
	case "session/new":
		f.sendResult(m.ID, map[string]any{"sessionId": "sess_fake_1"})
	case "session/prompt":
		f.handlePrompt(m)
	case "session/close":
		f.sendResult(m.ID, map[string]any{})
	default:
		f.sendError(m.ID, codeMethodNotFound, "Method not found")
	}
}

func (f *fakeAgent) handleInitialize(m Message) {
	var p struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(m.Params, &p)
	v := p.ProtocolVersion
	if v < Version1 {
		v = Version1
	}
	if v > f.cfg.version {
		v = f.cfg.version
	}
	f.negotiated = v
	if v == Version2 {
		f.sendResult(m.ID, map[string]any{
			"protocolVersion": Version2,
			"capabilities":    map[string]any{"session": map[string]any{}},
			"info":            map[string]any{"name": "fake-acp-agent", "version": "0.0.1"},
			"authMethods":     []any{},
		})
		return
	}
	f.sendResult(m.ID, map[string]any{
		"protocolVersion":   Version1,
		"agentCapabilities": map[string]any{"loadSession": true},
		"agentInfo":         map[string]any{"name": "fake-acp-agent", "version": "0.0.1"},
		"authMethods":       []any{},
	})
}

func (f *fakeAgent) handlePrompt(m Message) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var text strings.Builder
	for _, b := range p.Prompt {
		text.WriteString(b.Text)
	}
	f.promptMu.Lock()
	f.prompts = append(f.prompts, text.String())
	f.promptMu.Unlock()

	if f.cfg.hang {
		go f.turnHang(p.SessionID)
		return
	}
	go f.turn(m.ID, p.SessionID)
}

// turn runs a scripted v1/v2 turn: three chunks, an optional permission
// request, then the terminal signal for the negotiated version.
func (f *fakeAgent) turn(id json.RawMessage, sid string) {
	v2 := f.negotiated == Version2
	if v2 {
		f.sendResult(id, map[string]any{"messageId": "msg_user_1"})
	}
	f.chunk(sid, "A")
	f.chunk(sid, "B")
	if f.cfg.permission {
		f.requestPermission(sid)
	}
	if f.cfg.fsReadPath != "" {
		ans := f.request("fs/read_text_file", map[string]any{"sessionId": sid, "path": f.cfg.fsReadPath})
		f.fsOutcomes <- answerOf(ans)
	}
	if f.cfg.terminalMethod != "" {
		ans := f.request(f.cfg.terminalMethod, map[string]any{"sessionId": sid})
		f.terminalOutcomes <- answerOf(ans)
	}
	f.chunk(sid, "C")
	if v2 {
		f.notify("session/update", map[string]any{
			"sessionId": sid,
			"update":    map[string]any{"sessionUpdate": "state_update", "state": "idle", "stopReason": "end_turn"},
		})
		return
	}
	f.sendResult(id, map[string]any{"stopReason": "end_turn"})
}

// turnHang sends one chunk then blocks until the session is cancelled or the
// stream ends, never ending the turn.
func (f *fakeAgent) turnHang(sid string) {
	f.chunk(sid, "A")
	select {
	case <-f.cancelCh:
	case <-f.closed:
	}
}

// chunk emits one agent_message_chunk increment.
func (f *fakeAgent) chunk(sid, text string) {
	f.notify("session/update", map[string]any{
		"sessionId": sid,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"messageId":     "msg_1",
			"content":       map[string]any{"type": "text", "text": text},
		},
	})
}

// requestPermission sends a session/request_permission request and records the
// client's answer.
func (f *fakeAgent) requestPermission(sid string) {
	opts := []map[string]any{
		{"optionId": "allow-once", "name": "Allow once", "kind": string(guardOptionAllow)},
		{"optionId": "reject-once", "name": "Reject", "kind": string(guardOptionReject)},
	}
	tc := map[string]any{"toolCallId": "call_1", "title": "thing", "kind": f.cfg.permKind}
	if f.cfg.permPath != "" {
		tc["path"] = f.cfg.permPath
	}
	var params map[string]any
	if f.negotiated == Version2 {
		params = map[string]any{
			"sessionId": sid,
			"title":     "Run this?",
			"subject":   map[string]any{"type": "tool_call", "toolCall": tc},
			"options":   opts,
		}
	} else {
		params = map[string]any{"sessionId": sid, "toolCall": tc, "options": opts}
	}
	ans := f.request("session/request_permission", params)
	f.permOutcomes <- permOutcomeOf(ans)
}

// request sends a request to the client and waits for its response.
func (f *fakeAgent) request(method string, params any) Message {
	f.reqMu.Lock()
	f.seq++
	id := strconv.Itoa(f.seq)
	ch := make(chan Message, 1)
	f.reqs[id] = ch
	f.reqMu.Unlock()

	f.write(map[string]any{"jsonrpc": jsonrpcVersion, "id": id, "method": method, "params": params})

	ans := <-ch
	f.reqMu.Lock()
	delete(f.reqs, id)
	f.reqMu.Unlock()
	return ans
}

// permOutcomeOf extracts the selected option id (or "cancelled").
func permOutcomeOf(m Message) string {
	var r struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		return ""
	}
	if r.Outcome.Outcome == "selected" {
		return r.Outcome.OptionID
	}
	return r.Outcome.Outcome
}

// answerOf renders a client answer to an fs/terminal request: the result's
// "content" on success, or an "error:<message>" marker on a JSON-RPC error.
func answerOf(m Message) string {
	if m.Error != nil {
		return "error:" + m.Error.Message
	}
	var r struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(m.Result, &r)
	return r.Content
}

func (f *fakeAgent) notify(method string, params any) {
	f.write(map[string]any{"jsonrpc": jsonrpcVersion, "method": method, "params": params})
}

func (f *fakeAgent) sendResult(id json.RawMessage, result any) {
	f.write(map[string]any{"jsonrpc": jsonrpcVersion, "id": id, "result": result})
}

func (f *fakeAgent) sendError(id json.RawMessage, code int, msg string) {
	f.write(map[string]any{"jsonrpc": jsonrpcVersion, "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (f *fakeAgent) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.out.Write(b)
	_ = f.out.WriteByte('\n')
	_ = f.out.Flush()
}

// guardOptionAllow / guardOptionReject mirror the guard option kinds the fake
// offers, named locally to avoid importing guard in the fake.
const (
	guardOptionAllow  = "allow_once"
	guardOptionReject = "reject_once"
)

// parseFakeArgs reads the fake agent's behaviour from argv (used by the
// child-process helper the Dispatcher test spawns through the escape hatch).
func parseFakeArgs(args []string) fakeConfig {
	cfg := fakeConfig{version: Version1}
	for _, a := range args {
		switch a {
		case "-fake-v2":
			cfg.version = Version2
		case "-fake-permission":
			cfg.permission = true
		case "-fake-hang":
			cfg.hang = true
		}
	}
	return cfg
}
