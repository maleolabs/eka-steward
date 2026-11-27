#!/usr/bin/env python3
"""Drive mock_agent.py: initialize -> session/new -> session/prompt, answering
session/request_permission per POLICY (allow|deny|timeout). Proves the client
never hangs: the timeout branch waits a bounded T then cancels the request.

Usage: python3 mock_drive.py <allow|deny|timeout> <1|2>
       PERM_BOUND=<seconds> DUMP=1 python3 mock_drive.py ...
"""
import sys, json, time, os, tempfile
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import probe

POLICY = sys.argv[1] if len(sys.argv) > 1 else "allow"
VERSION = int(sys.argv[2]) if len(sys.argv) > 2 else 2
BOUND = float(os.environ.get("PERM_BOUND", "4"))
WORK = os.path.join(tempfile.gettempdir(), "acp-mock-work")
os.makedirs(WORK, exist_ok=True)

a = probe.Agent([sys.executable, os.path.join(HERE, "mock_agent.py")])

info = {"name": "eka-steward-probe", "title": "EKA Steward Probe", "version": "0.0.1"}
if VERSION == 2:
    rid = a.send("initialize", {"protocolVersion": 2, "capabilities": {}, "info": info})
else:
    rid = a.send("initialize", {"protocolVersion": 1, "clientCapabilities": {}, "clientInfo": info})
init, _ = a.wait_response(rid)
print("INIT proto=%s" % (init.get("result", {}).get("protocolVersion")))

rid = a.send("session/new", {"cwd": WORK, "mcpServers": []})
sn, _ = a.wait_response(rid)
sid = sn["result"]["sessionId"]

rid = a.send("session/prompt", {"sessionId": sid,
             "prompt": [{"type": "text", "text": "run the setup script"}]})
print("PROMPT sent (v%d)" % VERSION)

prompt_resp = None
seen_idle = False
perm_req = None
perm_t0 = None
cancel_sent = False
deadline = time.time() + 20
while time.time() < deadline:
    if perm_req is not None and POLICY == "timeout" and not cancel_sent and time.time() >= perm_t0 + BOUND:
        # bounded give-up: tell the agent to drop its pending permission request
        a.send("$/cancel_request", {"requestId": perm_req["id"]}, notify=True)
        elapsed = time.time() - perm_t0
        print("PERMISSION TIMEOUT: waited %.2fs (bound %.1fs) then sent $/cancel_request requestId=%s"
              % (elapsed, BOUND, perm_req["id"]))
        cancel_sent = True
        perm_req = None

    f = a.read_frame(timeout=0.5)
    if f is None or f.get("__eof__"):
        if f and f.get("__eof__"): print("EOF"); break
        continue
    if "method" in f and "id" in f:
        m = f["method"]
        print("<<REQUEST %s id=%s params=%s" % (m, f["id"], json.dumps(f.get("params"))[:300]))
        if m == "session/request_permission":
            if POLICY == "allow":
                a.p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": f["id"], "result":
                    {"outcome": {"outcome": "selected", "optionId": "allow-once"}}}) + "\n")
                a.p.stdin.flush(); print(">>ANSWER allow-once")
            elif POLICY == "deny":
                a.p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": f["id"], "result":
                    {"outcome": {"outcome": "selected", "optionId": "reject-once"}}}) + "\n")
                a.p.stdin.flush(); print(">>ANSWER reject-once")
            else:  # timeout
                perm_req = f; perm_t0 = time.time()
                print("   (policy=timeout: holding, will cancel after %.1fs)" % BOUND)
        continue
    if "method" in f:
        p = f.get("params", {})
        u = p.get("update", {}) if isinstance(p, dict) else {}
        if u.get("sessionUpdate") == "state_update" and u.get("state") == "idle":
            seen_idle = True
        print("<-NOTIF %s %s" % (f["method"], json.dumps(p)[:200]))
        continue
    if f.get("id") == rid and ("result" in f or "error" in f):
        prompt_resp = f
        print("PROMPT RESPONSE: %s" % json.dumps(f)[:300])
        if VERSION == 1:
            break
        # v2: keep reading until idle state_update
        if seen_idle:
            break

print("RESULT policy=%s version=%d stop=%s idle=%s" % (
    POLICY, VERSION, json.dumps((prompt_resp or {}).get("result")), seen_idle))
a.kill()
for tag, line in a.log:
    if tag == "stderr" and line.strip():
        print("MOCK-STDERR:", line)
if os.environ.get("DUMP"):
    for tag, line in a.log:
        print("%-4s %s" % (tag, line))
