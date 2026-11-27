// Package protocol is the steward's versioned, language-agnostic machine
// boundary: JSON-RPC 2.0 over a newline-delimited byte stream (stdio by default,
// an optional local unix socket). It is the product boundary — the engine and
// the control loop stay in internal/*. Every method REUSES the loop owned by
// eka/sto:steward-run-command through the Engine seam (implemented by
// *app.App); this package never wires a second loop.
//
// Transport, framing and the line cap mirror eka-mcp (newline-delimited,
// flush-per-reply, resync on overflow), stdlib-only.
package protocol

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/maleolabs/eka-steward/v2/internal/app"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
)

// ProtocolVersion is the machine-protocol revision this server speaks. The
// version is negotiated at initialize and evolves additively only: a future v2
// is selected per connection behind the same dispatcher.
const ProtocolVersion = "1"

// The v1 method names. They are the product surface and the initialize
// capabilities.methods list.
const (
	MethodInitialize = "steward/initialize"
	MethodPlan       = "steward/plan"
	MethodStep       = "steward/step"
	MethodRun        = "steward/run"
	MethodResume     = "steward/resume"
	MethodCancel     = "steward/cancel"
	MethodApprove    = "steward/approve"
	MethodStatus     = "steward/status"
	MethodShutdown   = "steward/shutdown"
)

// methodList is the deterministic capability list initialize advertises.
var methodList = []string{
	MethodInitialize, MethodPlan, MethodStep, MethodRun, MethodResume,
	MethodCancel, MethodApprove, MethodStatus, MethodShutdown,
}

// JSON-RPC 2.0 error codes. The standard codes are the spec's; the -320xx
// range is the steward protocol's deterministic refusal classes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601

	// CodeNotInitialized is returned for any request before initialize.
	CodeNotInitialized = -32001
	// CodeVersionUnsupported is returned when initialize offers no version the
	// server speaks.
	CodeVersionUnsupported = -32002
	// CodeRunNotFound is returned for an unknown runId.
	CodeRunNotFound = -32003
	// CodeBusy is returned when a run is already executing on the connection.
	CodeBusy = -32004
	// CodeApprovalTimeout is reserved for the approval bounded-wait timeout.
	CodeApprovalTimeout = -32005
	// CodeCanceled is returned when an operation was canceled.
	CodeCanceled = -32006
	// CodeIdempotencyConflict is returned when a key is reused with different
	// params.
	CodeIdempotencyConflict = -32007
	// CodeInvalidParams is returned for malformed or missing method params.
	CodeInvalidParams = -32008
	// CodeShutdownInProgress is returned for requests after shutdown began.
	CodeShutdownInProgress = -32009
	// CodeInternal is returned for an engine failure.
	CodeInternal = -32010
)

// Engine is the steward engine the protocol drives. *app.App implements it;
// tests inject a fake. Every method delegates to the owned control loop — the
// protocol reuses it and never re-wires a second one.
type Engine interface {
	// Plan returns the pure deterministic step sequence (no dispatch).
	Plan(ctx context.Context) ([]loop.Step, error)
	// Step plans and runs ONE checkpointed candidate; ok is false when nothing
	// is dispatchable.
	Step(ctx context.Context) (loop.Step, bool, error)
	// Run drives the loop to quiescence under the single-instance lock.
	Run(ctx context.Context) error
	// RunSteps drives at most maxSteps steps (maxSteps <= 0 == Run).
	RunSteps(ctx context.Context, maxSteps int) error
	// Cancel cancels the run in progress (idempotent, concurrent-safe).
	Cancel()
	// Status returns the current counters and run report.
	Status() app.Status
	// Approve materializes one human decision for the given request id.
	Approve(requestID, decision string) error
	// Close releases the engine's resources.
	Close() error
}

// Run states the protocol reports in status.
const (
	runRunning  = "running"
	runDone     = "done"
	runFailed   = "failed"
	runCanceled = "canceled"
	runIdle     = "idle"
)

// runHandle is the per-connection record of one run.
type runHandle struct {
	id       string
	ctx      context.Context
	cancel   context.CancelFunc
	state    string
	lastStep *loop.Step
}

// idemEntry is one idempotency-key record: the canonical params the key was
// first used with and the result bytes to replay for a retry.
type idemEntry struct {
	params string
	result json.RawMessage
}

// Server dispatches JSON-RPC 2.0 requests to the Engine. It is safe for one
// session at a time (stdio) and serializes all output writes (replies and any
// notification) under one writer mutex.
type Server struct {
	eng         Engine
	maxLineSize int

	mu         sync.Mutex
	negotiated string
	runs       map[string]*runHandle
	lastRunID  string
	runSeq     int
	idem       map[string]idemEntry
	stopping   bool

	outMu sync.Mutex
	out   *bufio.Writer
}

// NewServer wires a Server around one engine. maxLineSize defaults to
// defaultMaxLineSize; tests may shrink it via SetMaxLineSize.
func NewServer(eng Engine) *Server {
	return &Server{
		eng:         eng,
		maxLineSize: defaultMaxLineSize,
		runs:        map[string]*runHandle{},
		idem:        map[string]idemEntry{},
	}
}

// SetMaxLineSize overrides the per-message line cap. Tests shrink it to
// exercise the overflow refusal cheaply.
func (s *Server) SetMaxLineSize(n int) { s.maxLineSize = n }

// lineCap returns the effective line cap.
func (s *Server) lineCap() int {
	if s.maxLineSize > 0 {
		return s.maxLineSize
	}
	return defaultMaxLineSize
}

// Serve runs the protocol over a stdio-style byte stream: newline-delimited
// JSON-RPC requests from r, responses (and any notifications) to w. It returns
// when the input ends (io.EOF), on a transport error, or after a shutdown
// request has been answered.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	s.outMu.Lock()
	s.out = bufio.NewWriterSize(w, writerBufferSize)
	s.outMu.Unlock()
	defer func() {
		s.outMu.Lock()
		s.out = nil
		s.outMu.Unlock()
	}()

	for {
		line, err := readLine(br, s.lineCap())
		if err == io.EOF {
			return nil
		}
		if err == errLineTooLong {
			if werr := s.emit(lineTooLongResponse()); werr != nil {
				return werr
			}
			continue
		}
		if err != nil {
			return err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		resp := s.HandleMessage(line)
		if len(resp) > 0 {
			if err := s.emit(resp); err != nil {
				return err
			}
		}
		if s.isStopping() {
			return nil
		}
	}
}

// emit writes one line and flushes it under the writer mutex, so a reply and a
// concurrent notification can never interleave bytes.
func (s *Server) emit(line []byte) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if s.out == nil {
		return nil
	}
	if _, err := s.out.Write(line); err != nil {
		return err
	}
	if err := s.out.WriteByte('\n'); err != nil {
		return err
	}
	return s.out.Flush()
}

// Notify writes one server-to-client notification line (no id) through the
// same writer. v1 has no event source (the event stream is a later work item);
// this leaves the transport able to write notification lines for it.
func (s *Server) Notify(method string, params any) error {
	line, err := json.Marshal(notification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return s.emit(line)
}

// --- JSON-RPC wire types ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// HandleMessage processes one JSON-RPC message and returns the response bytes,
// or nil when the message needs no response (a notification). It is the pure
// dispatch unit: parse, route, respond; it never touches the streams directly.
func (s *Server) HandleMessage(line []byte) []byte {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var raw json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return parseErrorResponse()
	}
	if len(raw) == 0 {
		return parseErrorResponse()
	}
	switch raw[0] {
	case '[':
		return s.errorResponse(json.RawMessage("null"), codeInvalidRequest, "batch requests are not supported")
	case '{':
	default:
		return s.errorResponse(json.RawMessage("null"), codeInvalidRequest, "invalid request: expected a JSON-RPC request object")
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return s.errorResponse(json.RawMessage("null"), codeInvalidRequest, "invalid request: malformed request object")
	}
	if !validID(req.ID) {
		return s.errorResponse(json.RawMessage("null"), codeInvalidRequest, "invalid request: malformed request id")
	}
	if req.JSONRPC != "2.0" {
		return s.errorResponse(req.ID, codeInvalidRequest, `"jsonrpc" must be "2.0"`)
	}
	if req.Method == "" {
		return s.errorResponse(req.ID, codeInvalidRequest, "missing method")
	}
	if len(req.ID) == 0 {
		// A notification: fire-and-forget, no response.
		return nil
	}
	return s.dispatch(req)
}

// validID reports whether id is a JSON-RPC 2.0 id (string, number or null).
func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	switch id[0] {
	case '"', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '-', 'n':
		return true
	}
	return false
}

// dispatch routes one request (with id) to its method handler.
func (s *Server) dispatch(req request) []byte {
	if req.Method != MethodInitialize && !s.initialized() {
		return s.errorResponse(req.ID, CodeNotInitialized, "not initialized: call steward/initialize first")
	}
	if s.isStopping() && req.Method != MethodShutdown {
		return s.errorResponse(req.ID, CodeShutdownInProgress, "shutdown in progress")
	}
	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(req)
	case MethodPlan:
		return s.handlePlan(req)
	case MethodStep:
		return s.handleStep(req)
	case MethodRun:
		return s.handleRun(req)
	case MethodResume:
		return s.handleResume(req)
	case MethodCancel:
		return s.handleCancel(req)
	case MethodApprove:
		return s.handleApprove(req)
	case MethodStatus:
		return s.handleStatus(req)
	case MethodShutdown:
		return s.handleShutdown(req)
	default:
		return s.errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method)
	}
}

// initialized reports whether a version was negotiated.
func (s *Server) initialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.negotiated != ""
}

// stopping reports whether shutdown has begun.
func (s *Server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// Stopped reports whether a shutdown request has been processed. cmd/serve
// uses it to stop accepting socket connections after a graceful shutdown.
func (s *Server) Stopped() bool { return s.isStopping() }

// --- initialize ---

type initializeParams struct {
	ProtocolVersions []string `json:"protocolVersions"`
}

type capabilities struct {
	Methods []string `json:"methods"`
	Events  []string `json:"events"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
}

// handleInitialize negotiates the protocol version. When the client offers no
// versions the server assumes its highest supported; when it offers versions
// but none is supported the request is refused with version_unsupported.
func (s *Server) handleInitialize(req request) []byte {
	var p initializeParams
	if !decodeInto(req.Params, &p) {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/initialize params must be an object")
	}
	v, ok := negotiate(p.ProtocolVersions)
	if !ok {
		return s.errorResponse(req.ID, CodeVersionUnsupported, "no supported protocol version offered (supported: "+ProtocolVersion+")")
	}
	s.mu.Lock()
	s.negotiated = v
	s.mu.Unlock()

	events := []string{}
	methods := append([]string(nil), methodList...)
	return s.resultJSON(req.ID, initializeResult{
		ProtocolVersion: v,
		Capabilities:    capabilities{Methods: methods, Events: events},
	})
}

// negotiate picks the highest supported version from the client's offer. An
// empty offer means the server's highest supported version.
func negotiate(offered []string) (string, bool) {
	if len(offered) == 0 {
		return ProtocolVersion, true
	}
	supported := map[string]bool{ProtocolVersion: true}
	best := ""
	for _, v := range offered {
		if supported[v] && (best == "" || v > best) {
			best = v
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// --- plan ---

type planParams struct {
	Project        string `json:"project"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type candidateInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Priority   int    `json:"priority"`
	Kind       string `json:"kind"`
	CreatedSeq uint64 `json:"createdSeq"`
}

type planResult struct {
	Candidates []candidateInfo `json:"candidates"`
	Selected   int             `json:"selected"`
	Blocked    int             `json:"blocked"`
}

// handlePlan maps to the pure loop Plan pass. It never dispatches. Blocked is
// always 0: the planner yields only dispatchable candidates, so blocked work is
// not surfaced by v1 plan.
func (s *Server) handlePlan(req request) []byte {
	var p planParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/plan")
	}
	if p.Project == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/plan requires a non-empty project")
	}
	if resp, done := s.replay(MethodPlan, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	steps, err := s.eng.Plan(context.Background())
	if err != nil {
		return s.internal(req.ID, "plan", err)
	}
	cands := make([]candidateInfo, 0, len(steps))
	for _, st := range steps {
		cands = append(cands, candidateInfo{
			ID: st.CandidateID, State: st.State, Priority: st.Priority,
			Kind: st.Kind, CreatedSeq: st.CreatedSeq,
		})
	}
	return s.reply(req.ID, MethodPlan, p.IdempotencyKey, req.Params, planResult{
		Candidates: cands, Selected: len(cands), Blocked: 0,
	})
}

// --- step ---

type stepParams struct {
	RunID          string `json:"runId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type stepResult struct {
	Step *loop.Step `json:"step"`
}

// handleStep runs ONE checkpointed candidate. A matching run that is still
// executing is refused with busy (one driver per connection); an unknown runId
// is allowed so a consumer may drive a client-tracked run id.
func (s *Server) handleStep(req request) []byte {
	var p stepParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/step")
	}
	if p.RunID == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/step requires a non-empty runId")
	}
	if resp, done := s.replay(MethodStep, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	s.mu.Lock()
	h := s.runs[p.RunID]
	busy := h != nil && h.state == runRunning
	s.mu.Unlock()
	if busy {
		return s.errorResponse(req.ID, CodeBusy, "run "+p.RunID+" is executing; cancel it or wait")
	}
	step, ok, err := s.eng.Step(context.Background())
	if err != nil {
		return s.internal(req.ID, "step", err)
	}
	var res stepResult
	if ok {
		st := step
		res.Step = &st
		if h != nil {
			s.mu.Lock()
			h.lastStep = &st
			s.mu.Unlock()
		}
	}
	return s.reply(req.ID, MethodStep, p.IdempotencyKey, req.Params, res)
}

// --- run ---

type runParams struct {
	Project        string `json:"project"`
	MaxSteps       int    `json:"maxSteps"`
	DryRun         bool   `json:"dryRun"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type runResult struct {
	RunID string `json:"runId"`
}

// handleRun starts an asynchronous run in a goroutine and returns its runId
// immediately. A dryRun performs the pure plan only (no dispatch, no write).
func (s *Server) handleRun(req request) []byte {
	var p runParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/run")
	}
	if p.Project == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/run requires a non-empty project")
	}
	if resp, done := s.replay(MethodRun, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	id := s.newRunID(p.Project, p.IdempotencyKey)
	h := s.addRun(id)
	s.startRun(h, p.MaxSteps, p.DryRun)
	return s.reply(req.ID, MethodRun, p.IdempotencyKey, req.Params, runResult{RunID: id})
}

// --- resume ---

type resumeParams struct {
	RunID          string `json:"runId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type resumeResult struct {
	RunID       string `json:"runId"`
	ResumedFrom string `json:"resumedFrom"`
}

// handleResume restarts a known run: the engine reconciles any in-flight intent
// (Resume) before it dispatches, so this is the crash-recovery entry.
func (s *Server) handleResume(req request) []byte {
	var p resumeParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/resume")
	}
	if p.RunID == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/resume requires a non-empty runId")
	}
	if resp, done := s.replay(MethodResume, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	s.mu.Lock()
	h := s.runs[p.RunID]
	busy := h != nil && h.state == runRunning
	s.mu.Unlock()
	if h == nil {
		return s.errorResponse(req.ID, CodeRunNotFound, "unknown run: "+p.RunID)
	}
	if busy {
		return s.errorResponse(req.ID, CodeBusy, "run "+p.RunID+" is already executing")
	}
	s.startRun(h, 0, false)
	return s.reply(req.ID, MethodResume, p.IdempotencyKey, req.Params, resumeResult{RunID: p.RunID, ResumedFrom: p.RunID})
}

// --- cancel ---

type cancelParams struct {
	RunID          string `json:"runId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type cancelResult struct {
	RunID    string `json:"runId"`
	Canceled bool   `json:"canceled"`
}

// handleCancel cancels a run. It is idempotent: a repeat (even on a finished
// run) returns the same canceled:true result.
func (s *Server) handleCancel(req request) []byte {
	var p cancelParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/cancel")
	}
	if p.RunID == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/cancel requires a non-empty runId")
	}
	if resp, done := s.replay(MethodCancel, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	s.mu.Lock()
	h := s.runs[p.RunID]
	s.mu.Unlock()
	if h == nil {
		return s.errorResponse(req.ID, CodeRunNotFound, "unknown run: "+p.RunID)
	}
	s.mu.Lock()
	if h.cancel != nil {
		h.cancel()
	}
	h.state = runCanceled
	s.mu.Unlock()
	s.eng.Cancel()
	return s.reply(req.ID, MethodCancel, p.IdempotencyKey, req.Params, cancelResult{RunID: p.RunID, Canceled: true})
}

// --- approve ---

type approveParams struct {
	RunID          string `json:"runId"`
	RequestID      string `json:"requestId"`
	Decision       string `json:"decision"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type approveResult struct {
	RequestID string `json:"requestId"`
	Accepted  bool   `json:"accepted"`
}

// handleApprove routes a human decision to the approval channel the loop polls.
func (s *Server) handleApprove(req request) []byte {
	var p approveParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/approve")
	}
	if p.RequestID == "" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/approve requires a non-empty requestId")
	}
	if p.Decision != "approve" && p.Decision != "deny" {
		return s.errorResponse(req.ID, CodeInvalidParams, "steward/approve decision must be approve|deny")
	}
	if resp, done := s.replay(MethodApprove, p.IdempotencyKey, req.Params, req.ID); done {
		return resp
	}
	if err := s.eng.Approve(p.RequestID, p.Decision); err != nil {
		return s.internal(req.ID, "approve", err)
	}
	return s.reply(req.ID, MethodApprove, p.IdempotencyKey, req.Params, approveResult{RequestID: p.RequestID, Accepted: true})
}

// --- status ---

type statusParams struct {
	RunID string `json:"runId"`
}

type statusResult struct {
	RunID    string       `json:"runId"`
	State    string       `json:"state"`
	Counters obs.Snapshot `json:"counters"`
	LastStep *loop.Step   `json:"lastStep"`
	Report   obs.Report   `json:"report"`
}

// handleStatus reports the current counters, last step and run report. With no
// runId it reports the most recent run (or idle when none).
func (s *Server) handleStatus(req request) []byte {
	var p statusParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/status")
	}
	s.mu.Lock()
	id := p.RunID
	if id == "" {
		id = s.lastRunID
	}
	h := s.runs[id]
	s.mu.Unlock()

	st := s.eng.Status()
	res := statusResult{RunID: id, State: runIdle, Counters: st.Snapshot, Report: st.Report}
	if h != nil {
		s.mu.Lock()
		res.State = h.state
		res.LastStep = h.lastStep
		s.mu.Unlock()
	}
	return s.resultJSON(req.ID, res)
}

// --- shutdown ---

type shutdownParams struct {
	Graceful bool `json:"graceful"`
}

type shutdownResult struct {
	Stopping bool `json:"stopping"`
}

// handleShutdown stops the server: cancels every running run and answers
// stopping:true. Serve returns after writing the reply. The engine is closed by
// the caller (cmd/serve), not here.
func (s *Server) handleShutdown(req request) []byte {
	var p shutdownParams
	if !decodeInto(req.Params, &p) {
		return s.invalidParams(req.ID, "steward/shutdown")
	}
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		for _, h := range s.runs {
			if h.state == runRunning && h.cancel != nil {
				h.cancel()
				h.state = runCanceled
			}
		}
	}
	s.mu.Unlock()
	s.eng.Cancel()
	return s.resultJSON(req.ID, shutdownResult{Stopping: true})
}

// --- run lifecycle ---

// newRunID derives a deterministic run id: a hash of project+key when an
// idempotency key is given (so a retry names the same run), else a monotonic
// per-connection counter ("run-1", "run-2", ...).
func (s *Server) newRunID(project, key string) string {
	if key != "" {
		sum := sha256.Sum256([]byte("run/1\x00" + project + "\x00" + key))
		return "run-" + hex.EncodeToString(sum[:])[:12]
	}
	s.mu.Lock()
	s.runSeq++
	n := s.runSeq
	s.mu.Unlock()
	return "run-" + strconv.Itoa(n)
}

// addRun records a fresh run handle and makes it the most recent run.
func (s *Server) addRun(id string) *runHandle {
	h := &runHandle{id: id, state: runRunning}
	s.mu.Lock()
	s.runs[id] = h
	s.lastRunID = id
	s.mu.Unlock()
	return h
}

// startRun (re)builds the run's context and launches its goroutine.
func (s *Server) startRun(h *runHandle, maxSteps int, dryRun bool) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	h.ctx = ctx
	h.cancel = cancel
	h.state = runRunning
	s.mu.Unlock()

	go func() {
		var err error
		switch {
		case dryRun:
			_, err = s.eng.Plan(ctx)
		case maxSteps > 0:
			err = s.eng.RunSteps(ctx, maxSteps)
		default:
			err = s.eng.Run(ctx)
		}
		s.finishRun(h, err)
	}()
}

// finishRun records a run's terminal state deterministically: a cancelled
// context is canceled, else an error is failed, else done.
func (s *Server) finishRun(h *runHandle, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Decide the terminal state BEFORE releasing the context: calling cancel()
	// sets ctx.Err() to context.Canceled, so reading it afterwards would report
	// every clean completion as canceled.
	canceled := h.state == runCanceled || (h.ctx != nil && errors.Is(h.ctx.Err(), context.Canceled))
	if h.cancel != nil {
		h.cancel()
	}
	switch {
	case canceled:
		h.state = runCanceled
	case err != nil:
		h.state = runFailed
	default:
		h.state = runDone
	}
}

// --- idempotency ---

// replay returns the stored response for an idempotency key when the same
// params were used before, or an idempotency_conflict error when the key was
// first used with different params. done is false when the key is new.
func (s *Server) replay(method, key string, params json.RawMessage, id json.RawMessage) ([]byte, bool) {
	if key == "" {
		return nil, false
	}
	canon := canonicalParams(params)
	s.mu.Lock()
	e, ok := s.idem[method+"\x00"+key]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	if e.params != canon {
		return s.errorResponse(id, CodeIdempotencyConflict, "idempotency conflict: key reused with different params"), true
	}
	return s.resultRaw(id, e.result), true
}

// reply records the result under the idempotency key (when given) and returns
// the success response.
func (s *Server) reply(id json.RawMessage, method, key string, params json.RawMessage, result any) []byte {
	if key != "" {
		if b, err := json.Marshal(result); err == nil {
			s.mu.Lock()
			s.idem[method+"\x00"+key] = idemEntry{params: canonicalParams(params), result: b}
			s.mu.Unlock()
		}
	}
	return s.resultJSON(id, result)
}

// canonicalParams returns a stable canonical form of a params object (sorted
// keys via map round-trip), so whitespace/ordering differences are the same
// params. Absent params are "null".
func canonicalParams(params json.RawMessage) string {
	t := bytes.TrimSpace(params)
	if len(t) == 0 || string(t) == "null" {
		return "null"
	}
	var v any
	if err := json.Unmarshal(t, &v); err != nil {
		return string(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(t)
	}
	return string(b)
}

// --- response builders ---

func (s *Server) resultJSON(id json.RawMessage, result any) []byte {
	b, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: result})
	if err != nil {
		return internalResponse(id)
	}
	return b
}

// resultRaw wraps already-serialized result bytes.
func (s *Server) resultRaw(id json.RawMessage, result json.RawMessage) []byte {
	return marshalResponse(rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: result})
}

func (s *Server) errorResponse(id json.RawMessage, code int, msg string) []byte {
	return marshalResponse(rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error:   &rpcError{Code: code, Message: msg},
	})
}

// invalidParams is the deterministic refusal for a non-object params payload.
func (s *Server) invalidParams(id json.RawMessage, method string) []byte {
	return s.errorResponse(id, CodeInvalidParams, method+" params must be a JSON object")
}

// internal is the deterministic refusal for an engine failure (sanitized).
func (s *Server) internal(id json.RawMessage, op string, err error) []byte {
	return s.errorResponse(id, CodeInternal, op+": "+sanitize(err))
}

// normalizeID makes an absent id null (JSON-RPC §4.3).
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// decodeInto decodes a params payload into dst. Absent/null params leave dst at
// its zero value; a non-object payload is reported false.
func decodeInto(raw json.RawMessage, dst any) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return true
	}
	if t[0] != '{' {
		return false
	}
	return json.Unmarshal(t, dst) == nil
}

// sanitize reduces an error to its deterministic first line (no stack traces).
func sanitize(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "engine failure"
	}
	return msg
}

// parseErrorResponse is the fixed response for JSON that does not parse.
func parseErrorResponse() []byte {
	return marshalResponse(rpcResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("null"),
		Error:   &rpcError{Code: codeParseError, Message: "parse error: invalid JSON"},
	})
}

// internalResponse is the fixed marshaling-failure response.
func internalResponse(id json.RawMessage) []byte {
	return marshalResponse(rpcResponse{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error:   &rpcError{Code: CodeInternal, Message: "internal error"},
	})
}

// marshalResponse serializes a response; the shapes this package builds cannot
// fail to marshal, so a failure falls back to the fixed internal error.
func marshalResponse(r rpcResponse) []byte {
	b, err := json.Marshal(r)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32010,"message":"internal error"}}`)
	}
	return b
}
