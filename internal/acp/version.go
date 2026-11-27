package acp

import (
	"encoding/json"

	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// ACP protocol versions the steward speaks. v1 is the shipping baseline; v2
// is additive and selected PER CONNECTION from the version the agent returns
// in its initialize response (docs/acp-spike.md §11). Nothing outside the
// selected handler is version-aware.
const (
	// Version1 is the baseline ACP revision.
	Version1 = 1
	// Version2 is the additive revision (restructured initialize, ack-based
	// prompt turns, restructured permission requests).
	Version2 = 2
)

// ClientInfo identifies the steward to the agent in the initialize handshake.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// versionHandler adapts the wire shapes that differ between ACP revisions. A
// handler is chosen once per connection from the negotiated version.
type versionHandler interface {
	// version is the protocol version this handler speaks.
	version() int
	// initializeParams builds the initialize request params.
	initializeParams(info ClientInfo) map[string]any
	// responseEndsTurn reports whether the session/prompt RESPONSE ends the
	// turn (v1) rather than being an insertion acknowledgement (v2).
	responseEndsTurn() bool
	// permissionToolCall extracts the requested tool call from a
	// session/request_permission params object.
	permissionToolCall(params json.RawMessage) (toolCall, bool)
}

// handlerFor returns the handler for a protocol version, or false when the
// version is not supported.
func handlerFor(v int) (versionHandler, bool) {
	switch v {
	case Version1:
		return v1Handler{}, true
	case Version2:
		return v2Handler{}, true
	default:
		return nil, false
	}
}

// requestedVersion clamps the configured maximum to a supported version. The
// zero value requests v1 (the shipping baseline).
func requestedVersion(max int) int {
	if max < Version1 {
		return Version1
	}
	if max > Version2 {
		return Version2
	}
	return max
}

// v1Handler speaks ACP v1: boolean/object capability markers under
// clientCapabilities, the prompt response ends the turn, and a permission
// request carries a bare toolCall.
type v1Handler struct{}

func (v1Handler) version() int { return Version1 }

func (v1Handler) initializeParams(info ClientInfo) map[string]any {
	return map[string]any{
		"protocolVersion": Version1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": true,
		},
		"clientInfo": info,
	}
}

func (v1Handler) responseEndsTurn() bool { return true }

func (v1Handler) permissionToolCall(params json.RawMessage) (toolCall, bool) {
	var p struct {
		ToolCall toolCall `json:"toolCall"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolCall{}, false
	}
	if p.ToolCall.ToolCallID == "" && p.ToolCall.Kind == "" {
		return toolCall{}, false
	}
	return p.ToolCall, true
}

// v2Handler speaks ACP v2: object-presence capabilities under capabilities, an
// ack-based prompt turn (the turn ends on an idle state_update), and a
// restructured permission request whose subject is a tagged union.
type v2Handler struct{}

func (v2Handler) version() int { return Version2 }

func (v2Handler) initializeParams(info ClientInfo) map[string]any {
	return map[string]any{
		"protocolVersion": Version2,
		"capabilities":    map[string]any{},
		"info":            info,
	}
}

func (v2Handler) responseEndsTurn() bool { return false }

func (v2Handler) permissionToolCall(params json.RawMessage) (toolCall, bool) {
	var p struct {
		Subject struct {
			Type     string   `json:"type"`
			ToolCall toolCall `json:"toolCall"`
			Command  string   `json:"command"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolCall{}, false
	}
	if p.Subject.Type == "command" && p.Subject.Command != "" {
		return toolCall{Kind: guard.ToolExecute, Title: p.Subject.Command}, true
	}
	tc := p.Subject.ToolCall
	return tc, tc.ToolCallID != "" || tc.Kind != ""
}
