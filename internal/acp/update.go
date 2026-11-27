package acp

import (
	"encoding/json"
	"strings"
)

// SessionUpdate is one session/update notification increment streamed by the
// agent during a prompt turn. It carries the common fields of every
// sessionUpdate variant plus the raw content block, so a caller can inspect
// anything version-specific without the client modelling every shape.
type SessionUpdate struct {
	// SessionID is the session the update belongs to (from params).
	SessionID string `json:"-"`
	// Kind is the sessionUpdate discriminator (agent_message_chunk,
	// tool_call_update, state_update, ...).
	Kind string `json:"sessionUpdate"`
	// MessageID groups message chunks into one logical message.
	MessageID string `json:"messageId,omitempty"`
	// State is set on a v2 state_update (running/requires_action/idle).
	State string `json:"state,omitempty"`
	// StopReason is set on the terminal v2 idle state_update.
	StopReason string `json:"stopReason,omitempty"`
	// Status is set on a tool_call_update.
	Status string `json:"status,omitempty"`
	// ToolCallID identifies the tool call a tool_call_update patches.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Content is the raw content block (shape varies by variant/version).
	Content json.RawMessage `json:"content,omitempty"`
}

// isIdle reports whether u is the v2 terminal state_update that ends a turn.
func (u SessionUpdate) isIdle() bool {
	return u.Kind == "state_update" && u.State == "idle"
}

// text returns the agent-message text this update carries, or "" for a
// non-message variant. Thought and tool updates are never agent output.
func (u SessionUpdate) text() string {
	if !strings.HasPrefix(u.Kind, "agent_message") {
		return ""
	}
	return textOf(u.Content)
}

// textOf extracts concatenated text from a content block in either the v1
// object form ({"type":"text","text":"..."}) or the v2 array form
// ([{"type":"text","text":"..."}]).
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Text != "" {
		return obj.Text
	}
	var arr []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		var b strings.Builder
		for _, e := range arr {
			b.WriteString(e.Text)
		}
		return b.String()
	}
	return ""
}
