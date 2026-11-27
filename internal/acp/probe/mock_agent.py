#!/usr/bin/env python3
"""Mock ACP agent (stdio JSON-RPC). Not an LLM: runs a fixed script that emits
session/update notifications and a session/request_permission request, then
branches on the client's answer. Speaks the negotiated major version's shapes.

Run:  python3 mock_agent.py            (v2 or v1 chosen by initialize)
"""
import json, sys, threading, time, queue

PROTO = {"v": 1}
OUT = sys.stdout
LOCK = threading.Lock()
REQS = {}          # our request id -> queue for the client's response
REQ_SEQ = [1000]

def send(obj):
    with LOCK:
        OUT.write(json.dumps(obj) + "\n"); OUT.flush()

def notify(method, params):
    send({"jsonrpc": "2.0", "method": method, "params": params})

def request(method, params, timeout):
    """Send a request to the client; return its response or None on timeout."""
    REQ_SEQ[0] += 1
    rid = "mock-%d" % REQ_SEQ[0]
    q = queue.Queue()
    REQS[rid] = q
    send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    try:
        return q.get(timeout=timeout)
    except queue.Empty:
        return None
    finally:
        REQS.pop(rid, None)

def upd(session_id, update):
    notify("session/update", {"sessionId": session_id, "update": update})

class Session:
    def __init__(self, sid, version):
        self.sid = sid
        self.v = version

    def run_turn(self, prompt):
        v2 = self.v == 2
        # --- acknowledgement ---
        if v2:
            # prompt response is sent by the caller (accepted/inserted)
            upd(self.sid, {"sessionUpdate": "user_message", "messageId": "msg_user_1",
                           "content": [{"type": "text", "text": prompt}]})
            upd(self.sid, {"sessionUpdate": "state_update", "state": "running"})
        else:
            upd(self.sid, {"sessionUpdate": "agent_message_chunk", "messageId": "msg_a1",
                           "content": {"type": "text", "text": "Working on it..."}})
        # --- tool call announced ---
        tc = {"sessionUpdate": "tool_call_update", "toolCallId": "call_perm_1",
              "title": "Run setup command", "kind": "execute", "status": "pending"}
        upd(self.sid, tc)
        # --- permission request (v1 vs v2 shapes) ---
        opts = [
            {"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
            {"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
        ]
        if v2:
            params = {"sessionId": self.sid, "title": "Run this command?",
                      "description": "The agent wants to execute `echo hello-world`.",
                      "subject": {"type": "tool_call",
                                  "toolCall": {"toolCallId": "call_perm_1", "title": "Run setup command",
                                               "kind": "execute", "status": "pending"}},
                      "options": opts}
            upd(self.sid, {"sessionUpdate": "state_update", "state": "requires_action"})
        else:
            params = {"sessionId": self.sid, "toolCall": {"toolCallId": "call_perm_1",
                                                          "title": "Run setup command",
                                                          "kind": "execute", "status": "pending"},
                      "options": opts}
        resp = request("session/request_permission", params, timeout=8.0)
        outcome = "cancelled"
        if resp is not None and isinstance(resp.get("result"), dict):
            outcome = resp["result"].get("outcome", {}).get("outcome", "cancelled")
            if outcome == "selected":
                outcome = resp["result"]["outcome"].get("optionId", "cancelled")
        sys.stderr.write("MOCK: permission outcome=%r\n" % outcome); sys.stderr.flush()

        allowed = outcome in ("allow-once", "allow", "allow-always", "always")
        if allowed:
            upd(self.sid, {"sessionUpdate": "tool_call_update", "toolCallId": "call_perm_1",
                           "status": "in_progress"})
            upd(self.sid, {"sessionUpdate": "tool_call_update", "toolCallId": "call_perm_1",
                           "status": "completed",
                           "content": [{"type": "content",
                                        "content": {"type": "text", "text": "hello-world"}}]})
            if v2:
                upd(self.sid, {"sessionUpdate": "agent_message", "messageId": "msg_a1",
                               "content": [{"type": "text", "text": "Command ran."}]})
                upd(self.sid, {"sessionUpdate": "state_update", "state": "idle",
                               "stopReason": "end_turn"})
                return None
            upd(self.sid, {"sessionUpdate": "agent_message_chunk", "messageId": "msg_a1",
                           "content": {"type": "text", "text": "Command ran."}})
            return {"stopReason": "end_turn"}
        # denied / cancelled
        upd(self.sid, {"sessionUpdate": "tool_call_update", "toolCallId": "call_perm_1",
                       "status": "failed",
                       "content": [{"type": "content",
                                    "content": {"type": "text", "text": "Permission denied"}}]})
        if v2:
            stop = "cancelled" if outcome == "cancelled" else "end_turn"
            upd(self.sid, {"sessionUpdate": "state_update", "state": "idle", "stopReason": stop})
            return None
        return {"stopReason": "cancelled" if outcome == "cancelled" else "refusal"}

def handle(msg):
    method = msg.get("method")
    if method is None:
        # a response to one of our requests
        q = REQS.get(msg.get("id"))
        if q: q.put(msg)
        return
    if "id" not in msg:  # notification
        if method == "$/cancel_request":
            rid = (msg.get("params") or {}).get("requestId")
            sys.stderr.write("MOCK: got $/cancel_request requestId=%s\n" % rid); sys.stderr.flush()
            q = REQS.get(rid)
            if q is not None:
                q.put({"jsonrpc": "2.0", "id": rid, "result": {"outcome": {"outcome": "cancelled"}}})
        elif method == "session/cancel":
            sys.stderr.write("MOCK: got session/cancel\n"); sys.stderr.flush()
        return
    rid = msg["id"]
    p = msg.get("params", {}) or {}
    if method == "initialize":
        PROTO["v"] = 2 if p.get("protocolVersion", 1) >= 2 else 1
        if PROTO["v"] == 2:
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "protocolVersion": 2,
                "capabilities": {"session": {"prompt": {"embeddedContext": {}},
                                             "mcp": {"stdio": {}, "http": {}}}},
                "info": {"name": "mock-acp-agent", "title": "Mock ACP Agent", "version": "0.0.1"},
                "authMethods": []}})
        else:
            send({"jsonrpc": "2.0", "id": rid, "result": {
                "protocolVersion": 1,
                "agentCapabilities": {"loadSession": True,
                                      "promptCapabilities": {"embeddedContext": True},
                                      "mcpCapabilities": {"http": True, "sse": False}},
                "agentInfo": {"name": "mock-acp-agent", "version": "0.0.1"},
                "authMethods": []}})
        return
    if method == "session/new":
        sid = "sess_mock_1"
        send({"jsonrpc": "2.0", "id": rid, "result": {"sessionId": sid}})
        return
    if method == "session/prompt":
        sid = p.get("sessionId")
        prompt = "".join(b.get("text", "") for b in p.get("prompt", []) if b.get("type") == "text")
        if PROTO["v"] == 2:
            # respond first: user message inserted
            send({"jsonrpc": "2.0", "id": rid, "result": {"messageId": "msg_user_1"}})
            threading.Thread(target=Session(sid, 2).run_turn, args=(prompt,), daemon=True).start()
        else:
            threading.Thread(target=lambda: send({"jsonrpc": "2.0", "id": rid,
                                                  "result": Session(sid, 1).run_turn(prompt)}),
                             daemon=True).start()
        return
    if method == "session/close":
        send({"jsonrpc": "2.0", "id": rid, "result": {}})
        return
    # unknown
    send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "Method not found"}})

def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        line = line.strip()
        if not line: continue
        try:
            handle(json.loads(line))
        except Exception as e:
            sys.stderr.write("MOCK ERROR: %r\n" % (e,)); sys.stderr.flush()

if __name__ == "__main__":
    main()
