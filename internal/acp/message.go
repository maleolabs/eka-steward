// Package acp is the steward's live Agent Client Protocol client: it speaks
// newline-delimited JSON-RPC 2.0 over an agent's stdio, negotiates the
// protocol version per connection, drives one session/prompt turn while
// streaming session/update increments, and answers server-initiated requests
// (session/request_permission, fs/*, terminal/*) through the guard policy and
// the human-approval channel.
//
// It is pure Go (standard library only) and NEVER spawns a process itself:
// the production Dispatcher launches the agent through internal/escapehatch,
// the one audited subprocess adapter (docs/boundary.md). Tests drive the
// client against an in-process fake agent over a pipe, so the default test
// path needs no agent and no network.
//
// It performs no EKA writes. Cancellation leaves no partial write behind —
// the caller's EKA write path (the loop Writer) never runs on a cancelled
// turn.
package acp

import (
	"encoding/json"
	"fmt"
)

// jsonrpcVersion is the only protocol version string ACP uses.
const jsonrpcVersion = "2.0"

// Standard JSON-RPC 2.0 error codes the client emits.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// Message is one newline-delimited JSON-RPC 2.0 frame, in either direction.
// A non-empty Method marks a request (with ID) or a notification (no ID); an
// empty Method marks a response. ID is kept as a raw JSON value so an id the
// agent assigned is echoed back byte-identically.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error renders the rpc error deterministically.
func (e *Error) Error() string {
	if e == nil {
		return "acp: <nil rpc error>"
	}
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// isResponse reports whether m is a response to a request (no method, has id).
func (m Message) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// isRequest reports whether m is a request the peer expects an answer to.
func (m Message) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// isNotification reports whether m is a one-way notification (method, no id).
func (m Message) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// marshalParams renders params as raw JSON (nil renders the JSON null).
func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("acp: encode params: %w", err)
	}
	return b, nil
}
