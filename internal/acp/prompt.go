package acp

import (
	"context"
	"encoding/json"
)

// TurnResult is the outcome of one session/prompt turn.
type TurnResult struct {
	// SessionID is the session the turn ran in.
	SessionID string
	// StopReason is the agent's terminal reason (end_turn, cancelled,
	// refusal, error) — from the v1 prompt response or the v2 idle update.
	StopReason string
	// Text is the concatenated agent-message output streamed during the turn.
	Text string
	// MessageID is the v2 insertion acknowledgement (empty for v1).
	MessageID string
}

// Prompt drives one session/prompt turn: it sends the prompt, streams every
// session/update increment to Config.OnUpdate (in order) while accumulating
// agent-message text, and returns when the turn ends. v1 ends on the prompt
// response; v2 ends on an idle state_update after the response acknowledges
// insertion. A cancelled client returns ErrCancelled; a cancelled ctx returns
// ctx.Err(); both leave TurnResult.Text partial so the caller discards it.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (TurnResult, error) {
	res := TurnResult{SessionID: sessionID}
	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}
	raw, err := marshalParams(params)
	if err != nil {
		return res, err
	}

	id := c.newID()
	ch := make(chan callResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	req := Message{ID: idRaw(id), Method: "session/prompt", Params: raw}
	if err := c.writer.WriteMessage(req); err != nil {
		return res, err
	}

	for {
		select {
		case u := <-c.updates:
			c.consume(&res, u)
			if u.isIdle() {
				res.StopReason = u.StopReason
				return res, nil
			}
		case r := <-ch:
			if r.err != nil {
				return res, r.err
			}
			if r.msg.Error != nil {
				return res, r.msg.Error
			}
			if c.handler.responseEndsTurn() {
				// All updates precede the v1 response; drain any that the
				// read loop buffered before delivering it, preserving order.
				c.drainUpdates(&res)
				res.StopReason = stopReasonOf(r.msg.Result)
				return res, nil
			}
			// v2: the response only acknowledges insertion. Keep reading
			// until the idle state_update ends the turn.
			res.MessageID = messageIDOf(r.msg.Result)
		case <-c.ctx.Done():
			return res, ErrCancelled
		case <-c.closed:
			if res.StopReason == "" {
				return res, c.err()
			}
			return res, nil
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

// consume reports one update to the callback and folds its text into res.
func (c *Client) consume(res *TurnResult, u SessionUpdate) {
	if c.cfg.OnUpdate != nil {
		c.cfg.OnUpdate(u)
	}
	res.Text += u.text()
}

// drainUpdates consumes every update currently buffered, in order.
func (c *Client) drainUpdates(res *TurnResult) {
	for {
		select {
		case u := <-c.updates:
			c.consume(res, u)
		default:
			return
		}
	}
}

// Cancel terminates the session deterministically: it cancels every in-flight
// call (which return ErrCancelled) and sends a best-effort session/cancel
// notification. It leaves no partial EKA write — the client never writes EKA.
func (c *Client) Cancel(sessionID string) error {
	c.cancel()
	if sessionID == "" {
		return nil
	}
	params, err := marshalParams(map[string]string{"sessionId": sessionID})
	if err != nil {
		return err
	}
	return c.writer.WriteMessage(Message{Method: "session/cancel", Params: params})
}

// stopReasonOf reads stopReason from a prompt result object.
func stopReasonOf(result json.RawMessage) string {
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.StopReason
}

// messageIDOf reads messageId from a v2 prompt result object.
func messageIDOf(result json.RawMessage) string {
	var r struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return ""
	}
	return r.MessageID
}
