package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

var (
	// ErrClosed is returned to a pending call when the agent stream ends.
	ErrClosed = errors.New("acp: connection closed")
	// ErrCancelled is returned to a call the client's Cancel aborted.
	ErrCancelled = errors.New("acp: session cancelled")
)

// Conn is the byte-stream pair to an agent process: Reader is the agent's
// stdout, Writer its stdin. stderr is separate and never part of the protocol.
type Conn struct {
	Reader io.Reader
	Writer io.Writer
}

// Config is the client's per-connection configuration. The zero value is
// default-deny: a read is never allowed and no approval channel is set.
type Config struct {
	// Info identifies the steward in the initialize handshake.
	Info ClientInfo
	// MaxVersion is the highest ACP version to request (0 requests v1).
	MaxVersion int
	// MaxFrameBytes caps a single frame line (0 applies the 64 MiB default).
	MaxFrameBytes int
	// Policy decides permission requests. The zero value is default-deny.
	Policy guard.Policy
	// Approver is the human channel for an undecidable (Escalate) request.
	// nil means an escalation resolves to Deny.
	Approver approval.Approver
	// WorkspaceRoot is the session workspace root; a read is allowed there
	// only when Policy.AllowReadWithinRoot is set.
	WorkspaceRoot string
	// OnUpdate, when set, observes every streamed session/update in order.
	OnUpdate func(SessionUpdate)
}

// callResult is the outcome delivered to a pending request's waiter.
type callResult struct {
	msg Message
	err error
}

// Client is one live ACP connection. Construct it with NewClient, start the
// read loop with Start, negotiate with Initialize, then open a session and
// drive prompts. A Client is safe for one session and one prompt at a time.
type Client struct {
	reader *FrameReader
	writer *FrameWriter
	cfg    Config

	handler    versionHandler
	negotiated int

	updates chan SessionUpdate

	idMu   sync.Mutex
	nextID int

	pendingMu sync.Mutex
	pending   map[int]chan callResult

	ctx    context.Context
	cancel context.CancelFunc
	closed chan struct{}
	once   sync.Once

	readErrMu sync.Mutex
	readErr   error
}

// NewClient builds a client over conn. It does no I/O until Start.
func NewClient(conn Conn, cfg Config) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	h, _ := handlerFor(requestedVersion(cfg.MaxVersion))
	return &Client{
		reader:  NewFrameReader(conn.Reader, cfg.MaxFrameBytes),
		writer:  NewFrameWriter(conn.Writer),
		cfg:     cfg,
		handler: h,
		updates: make(chan SessionUpdate, 256),
		pending: map[int]chan callResult{},
		ctx:     ctx,
		cancel:  cancel,
		closed:  make(chan struct{}),
	}
}

// Start launches the read loop. It returns immediately; the loop runs until
// the agent stream ends, Close/Cancel is called, or a read error occurs.
func (c *Client) Start() { go c.readLoop() }

// Close stops the client. It cancels any in-flight call (as ErrCancelled) and
// lets the read loop end when the stream closes.
func (c *Client) Close() error {
	c.cancel()
	return nil
}

// NegotiatedVersion returns the protocol version selected by the initialize
// response, or 0 before Initialize.
func (c *Client) NegotiatedVersion() int { return c.negotiated }

// readLoop reads frames until the stream ends, then fails every pending call.
func (c *Client) readLoop() {
	for {
		m, err := c.reader.ReadMessage()
		if err != nil {
			c.failAll(err)
			return
		}
		switch {
		case m.isResponse():
			c.deliver(m)
		case m.isRequest():
			go c.handleServerRequest(m)
		case m.isNotification():
			c.handleNotification(m)
		}
	}
}

// deliver hands a response to the waiter registered for its id.
func (c *Client) deliver(m Message) {
	id, ok := parseID(m.ID)
	if !ok {
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- callResult{msg: m}
	}
}

// failAll records the read error and wakes every pending call with it.
func (c *Client) failAll(err error) {
	c.readErrMu.Lock()
	c.readErr = err
	c.readErrMu.Unlock()

	c.pendingMu.Lock()
	pend := c.pending
	c.pending = map[int]chan callResult{}
	c.pendingMu.Unlock()
	for _, ch := range pend {
		ch <- callResult{err: err}
	}
	c.once.Do(func() { close(c.closed) })
}

// err returns the recorded read error, or ErrClosed.
func (c *Client) err() error {
	c.readErrMu.Lock()
	defer c.readErrMu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return ErrClosed
}

// newID returns a fresh monotonically increasing request id.
func (c *Client) newID() int {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	c.nextID++
	return c.nextID
}

// exchange sends req and waits for its response, the connection closing, a
// cancel, or ctx. Responses are matched by id.
func (c *Client) exchange(ctx context.Context, id int, req Message) (Message, error) {
	ch := make(chan callResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.writer.WriteMessage(req); err != nil {
		return Message{}, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return Message{}, r.err
		}
		if r.msg.Error != nil {
			return r.msg, r.msg.Error
		}
		return r.msg, nil
	case <-c.ctx.Done():
		return Message{}, ErrCancelled
	case <-c.closed:
		return Message{}, c.err()
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// call sends a request and returns its response.
func (c *Client) call(ctx context.Context, method string, params any) (Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return Message{}, err
	}
	id := c.newID()
	req := Message{ID: idRaw(id), Method: method, Params: raw}
	return c.exchange(ctx, id, req)
}

// Handshake is the initialize result recorded for a connection.
type Handshake struct {
	// ProtocolVersion is the version the agent selected.
	ProtocolVersion int
	// AgentName and AgentVersion identify the agent (from agentInfo or info).
	AgentName    string
	AgentVersion string
	// Raw is the unmodified initialize result.
	Raw json.RawMessage
}

// Initialize performs the ACP handshake and selects the per-connection version
// handler from the version the agent returns. A version the client does not
// speak is a deterministic error.
func (c *Client) Initialize(ctx context.Context) (Handshake, error) {
	params := c.handler.initializeParams(c.cfg.Info)
	resp, err := c.call(ctx, "initialize", params)
	if err != nil {
		return Handshake{}, fmt.Errorf("acp: initialize: %w", err)
	}
	var wire struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		Info struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		return Handshake{}, fmt.Errorf("acp: initialize response: %w", err)
	}
	h, ok := handlerFor(wire.ProtocolVersion)
	if !ok {
		return Handshake{}, fmt.Errorf("acp: agent selected unsupported protocol version %d", wire.ProtocolVersion)
	}
	c.handler = h
	c.negotiated = wire.ProtocolVersion
	return Handshake{
		ProtocolVersion: wire.ProtocolVersion,
		AgentName:       firstNonEmpty(wire.AgentInfo.Name, wire.Info.Name),
		AgentVersion:    firstNonEmpty(wire.AgentInfo.Version, wire.Info.Version),
		Raw:             resp.Result,
	}, nil
}

// SessionNew opens a new session rooted at cwd and returns its id.
func (c *Client) SessionNew(ctx context.Context, cwd string) (string, error) {
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	resp, err := c.call(ctx, "session/new", params)
	if err != nil {
		return "", fmt.Errorf("acp: session/new: %w", err)
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return "", fmt.Errorf("acp: session/new response: %w", err)
	}
	if r.SessionID == "" {
		return "", errors.New("acp: session/new returned no sessionId")
	}
	return r.SessionID, nil
}

// handleNotification routes a one-way notification. Only session/update is
// consumed; anything else is ignored.
func (c *Client) handleNotification(m Message) {
	if m.Method != "session/update" {
		return
	}
	var p struct {
		SessionID string        `json:"sessionId"`
		Update    SessionUpdate `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return
	}
	p.Update.SessionID = p.SessionID
	select {
	case c.updates <- p.Update:
	case <-c.ctx.Done():
	case <-c.closed:
	}
}

// idRaw renders an integer request id as raw JSON.
func idRaw(id int) json.RawMessage { return json.RawMessage(strconv.Itoa(id)) }

// parseID parses a numeric request id the client assigned.
func parseID(raw json.RawMessage) (int, bool) {
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
