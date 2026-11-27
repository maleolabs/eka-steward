package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// toolCall is the subset of an ACP tool-call descriptor the permission path
// reads: the tool class and, if the agent supplied one, the target path.
type toolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Path       string `json:"path,omitempty"`
}

// permissionOption is one option an agent offers in a permission request. The
// Kind is the guard.OptionID (allow_once/allow_always/reject_once/reject_always);
// the OptionID is the agent's own id, echoed back verbatim when selected.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// handleServerRequest answers one server-initiated request. Each handler runs
// on its own goroutine so a bounded permission wait never blocks the read
// loop.
func (c *Client) handleServerRequest(m Message) {
	switch m.Method {
	case "session/request_permission":
		c.handlePermission(m)
	case "fs/read_text_file":
		c.handleReadTextFile(m)
	case "fs/write_text_file":
		c.handleWriteTextFile(m)
	default:
		if isTerminalMethod(m.Method) {
			c.handleTerminal(m)
			return
		}
		c.respondError(m, codeMethodNotFound, "method not found")
	}
}

// handlePermission resolves one session/request_permission through the guard
// policy: Decide for the pure decision, AwaitAnswer (bounded) for an
// undecidable request escalated to the human approval channel, and a default
// deny for everything the policy does not clearly allow. An unoffered option
// is never invented; an ungrantable request is answered "cancelled".
func (c *Client) handlePermission(m Message) {
	var p struct {
		SessionID string             `json:"sessionId"`
		Options   []permissionOption `json:"options"`
	}
	_ = json.Unmarshal(m.Params, &p)
	tc, _ := c.handler.permissionToolCall(m.Params)

	allowID := optionOfKind(p.Options, string(guard.OptionAllowOnce))
	rejectID := optionOfKind(p.Options, string(guard.OptionRejectOnce))
	if rejectID == "" {
		rejectID = optionOfKind(p.Options, string(guard.OptionRejectAlways))
	}

	// No grant-eligible option offered: refuse (never invent an allow id).
	if allowID == "" {
		c.respondSelected(m, rejectID)
		return
	}

	req := guard.PermissionRequest{
		ToolKind:      tc.Kind,
		Path:          tc.Path,
		WorkspaceRoot: c.cfg.WorkspaceRoot,
		OptionID:      guard.OptionAllowOnce,
	}
	if c.decidePermission(c.ctx, req) == guard.Allow {
		c.respondSelected(m, allowID)
		return
	}
	c.respondSelected(m, rejectID)
}

// decidePermission resolves req to Allow or Deny. Allow/Deny come straight
// from Policy.Decide; an Escalate is handed to the human channel through
// Policy.AwaitAnswer (the outer bound) wrapping approval.AskBounded (the
// channel bound). With no approver an escalation is a Deny. The bounded waits
// guarantee the client never hangs.
func (c *Client) decidePermission(ctx context.Context, req guard.PermissionRequest) guard.Outcome {
	switch c.cfg.Policy.Decide(req) {
	case guard.Allow:
		return guard.Allow
	case guard.Deny:
		return guard.Deny
	}
	if c.cfg.Approver == nil {
		return guard.Deny
	}
	outcome, _ := c.cfg.Policy.AwaitAnswer(ctx, func(ctx context.Context) (guard.OptionID, error) {
		d, err := approval.AskBounded(ctx, c.cfg.Policy, c.cfg.Approver, approval.RequestFor(req))
		if err != nil {
			return guard.OptionRejectOnce, err
		}
		if d.Approved() {
			return guard.OptionAllowOnce, nil
		}
		return guard.OptionRejectOnce, nil
	})
	return outcome
}

// handleReadTextFile serves a v1 fs/read_text_file request only when the
// policy allows a read of the path inside the workspace root; otherwise it is
// refused. It returns the whole file. The path is symlink-resolved before the
// containment check, so a symlink inside the root cannot read outside it (the
// agent-supplied path is untrusted). The line/limit read range is not applied
// — add it when an agent needs a windowed read.
func (c *Client) handleReadTextFile(m Message) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		c.respondError(m, codeInternalError, "invalid fs/read_text_file params")
		return
	}
	path, err := filepath.EvalSymlinks(p.Path)
	if err != nil {
		c.respondError(m, codeInternalError, "read refused")
		return
	}
	root := c.cfg.WorkspaceRoot
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = resolved
	}
	req := guard.PermissionRequest{
		ToolKind:      guard.ToolRead,
		Path:          path,
		WorkspaceRoot: root,
		OptionID:      guard.OptionAllowOnce,
	}
	if c.cfg.Policy.Decide(req) != guard.Allow {
		c.respondError(m, codeInternalError, "read refused")
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		c.respondError(m, codeInternalError, "read failed")
		return
	}
	c.respondResult(m, map[string]string{"content": string(content)})
}

// handleWriteTextFile refuses a v1 fs/write_text_file request unless the policy
// allows the write. The built-in policy never allows an edit, so this is
// default-deny; a policy that never allows edit keeps it a refusal.
func (c *Client) handleWriteTextFile(m Message) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		c.respondError(m, codeInternalError, "invalid fs/write_text_file params")
		return
	}
	req := guard.PermissionRequest{
		ToolKind:      guard.ToolEdit,
		Path:          p.Path,
		WorkspaceRoot: c.cfg.WorkspaceRoot,
		OptionID:      guard.OptionAllowOnce,
	}
	if c.cfg.Policy.Decide(req) != guard.Allow {
		c.respondError(m, codeInternalError, "write refused")
		return
	}
	// ponytail: no write path exists today (policy never allows edit); add the
	// write here if a policy ever grants it.
	c.respondError(m, codeInternalError, "write refused")
}

// handleTerminal refuses every v1 terminal/* request unless the policy allows
// execute. The built-in policy never does, so this is default-deny.
func (c *Client) handleTerminal(m Message) {
	req := guard.PermissionRequest{
		ToolKind:      guard.ToolExecute,
		WorkspaceRoot: c.cfg.WorkspaceRoot,
		OptionID:      guard.OptionAllowOnce,
	}
	if c.cfg.Policy.Decide(req) != guard.Allow {
		c.respondError(m, codeMethodNotFound, "terminal not supported")
		return
	}
	c.respondError(m, codeMethodNotFound, "terminal not supported")
}

// isTerminalMethod reports whether method is a v1 terminal/* request.
func isTerminalMethod(method string) bool {
	return strings.HasPrefix(method, "terminal/")
}

// optionOfKind returns the agent's optionId for an offered option kind, or "".
func optionOfKind(opts []permissionOption, kind string) string {
	for _, o := range opts {
		if o.Kind == kind {
			return o.OptionID
		}
	}
	return ""
}

// respondSelected answers a permission request with a selected option, or
// "cancelled" when no option id is available.
func (c *Client) respondSelected(m Message, optionID string) {
	var result json.RawMessage
	if optionID == "" {
		result = json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`)
	} else {
		b, err := json.Marshal(map[string]any{
			"outcome": map[string]string{"outcome": "selected", "optionId": optionID},
		})
		if err != nil {
			c.respondError(m, codeInternalError, "encode permission outcome")
			return
		}
		result = b
	}
	_ = c.writer.WriteMessage(Message{ID: m.ID, Result: result})
}

// respondResult answers a server request with a success result.
func (c *Client) respondResult(m Message, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		c.respondError(m, codeInternalError, "encode result")
		return
	}
	_ = c.writer.WriteMessage(Message{ID: m.ID, Result: b})
}

// respondError answers a server request with a JSON-RPC error.
func (c *Client) respondError(m Message, code int, msg string) {
	_ = c.writer.WriteMessage(Message{ID: m.ID, Error: &Error{Code: code, Message: msg}})
}
