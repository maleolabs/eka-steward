#!/usr/bin/env python3
"""Deliver the steward context payload (internal/render golden output) to a REAL
agent as a `resource` content block in session/prompt (embeddedContext), plus a
baseline `text` block. Shows structured-context handoff over ACP v1.

Run: python3 context_probe.py
"""
import sys, json, os, tempfile
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import probe


def module_root():
    d = HERE
    while d != os.path.dirname(d):
        if os.path.exists(os.path.join(d, "go.mod")):
            return d
        d = os.path.dirname(d)
    raise SystemExit("go.mod not found above %s" % HERE)


GOLDEN = os.path.join(module_root(), "internal", "render", "testdata", "context.golden")
PAYLOAD = open(GOLDEN).read()
TASK = "Using ONLY the attached context document, reply with the acceptance criteria as a single line."
WORK = os.path.join(tempfile.gettempdir(), "acp-context-work")
os.makedirs(WORK, exist_ok=True)

a = probe.Agent(["opencode", "acp"])
rid = a.send("initialize", {"protocolVersion": 1,
    "clientCapabilities": {"fs": {"readTextFile": True, "writeTextFile": True}, "terminal": False},
    "clientInfo": {"name": "eka-steward-probe", "version": "0.0.1"}})
init, _ = a.wait_response(rid)
print("INIT ok, embeddedContext=%s" % init["result"]["agentCapabilities"]["promptCapabilities"].get("embeddedContext"))

rid = a.send("session/new", {"cwd": WORK, "mcpServers": []})
sn, _ = a.wait_response(rid)
sid = sn["result"]["sessionId"]

prompt = [
    {"type": "resource",
     "resource": {"uri": "file://" + os.path.join(WORK, "context-payload.md"),
                  "mimeType": "text/markdown", "text": PAYLOAD}},
    {"type": "text", "text": TASK},
]
rid = a.send("session/prompt", {"sessionId": sid, "prompt": prompt})
resp, notes = a.wait_response(rid, timeout=120)
print("PROMPT RESPONSE:", json.dumps(resp))
got = "".join(n.get("params", {}).get("update", {}).get("content", {}).get("text", "")
              for n in notes
              if n.get("params", {}).get("update", {}).get("sessionUpdate") == "agent_message_chunk")
print("AGENT TEXT:", got[:400])
a.kill()
