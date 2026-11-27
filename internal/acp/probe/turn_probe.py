#!/usr/bin/env python3
"""Probe 2: one real prompt turn — session/new, session/prompt, and answers to
any inbound client-method (session/request_permission, fs/*) per POLICY.

Usage: AGENT=opencode|gemini python3 turn_probe.py <workdir> <allow|deny|timeout> "<prompt>"
"""
import sys, json, time, os
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from probe import Agent

CWD = sys.argv[1] if len(sys.argv) > 1 else "/tmp/acp-turn-work"
POLICY = sys.argv[2] if len(sys.argv) > 2 else "allow"
PROMPT = sys.argv[3] if len(sys.argv) > 3 else "Reply with exactly the word PONG and nothing else."
os.makedirs(CWD, exist_ok=True)

AGENTS = {"opencode": ["opencode", "acp"], "gemini": ["gemini", "--acp"]}
WHICH = os.environ.get("AGENT", "opencode")


def handle(req, policy):
    m = req["method"]
    p = req.get("params", {})
    if m == "session/request_permission":
        opts = p.get("options", [])
        allow = next((o["optionId"] for o in opts if o.get("kind") in ("allow_once", "allow_always") or "allow" in o.get("optionId", "")), None)
        deny = next((o["optionId"] for o in opts if o.get("kind") in ("reject_once", "reject_always") or "reject" in o.get("optionId", "")), None)
        print("   PERMISSION options=%s" % json.dumps(opts))
        if policy == "deny":
            return {"outcome": {"outcome": "selected", "optionId": deny or "reject"}}
        return {"outcome": {"outcome": "selected", "optionId": allow or (opts[0]["optionId"] if opts else "allow")}}
    if m == "fs/read_text_file":
        return {"content": ""}
    if m == "fs/write_text_file":
        return {}
    if m.startswith("terminal/"):
        return {"output": "", "exitStatus": {"exitCode": 1}}
    return {}


def main():
    a = Agent(AGENTS[WHICH])
    rid = a.send("initialize", {"protocolVersion": 1,
        "clientCapabilities": {"fs": {"readTextFile": True, "writeTextFile": True}, "terminal": False},
        "clientInfo": {"name": "eka-steward-probe", "version": "0.0.1"}})
    resp, _ = a.wait_response(rid, timeout=12)
    print("INIT:", json.dumps(resp, ensure_ascii=False)[:300])
    if not resp or "result" not in resp:
        print("init failed")
        a.kill()
        return
    rid = a.send("session/new", {"cwd": CWD, "mcpServers": []})
    resp, notes = a.wait_response(rid, timeout=15)
    print("SESSION/NEW:", json.dumps(resp, ensure_ascii=False)[:500])
    for n in notes:
        print("  NEWS-NOTIF:", json.dumps(n)[:200])
    if not resp or "result" not in resp:
        print("session/new failed")
        a.kill()
        return
    sid = resp["result"].get("sessionId")
    rid = a.send("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": PROMPT}]})
    print("PROMPT id=%s sid=%s" % (rid, sid))
    end = time.time() + float(os.environ.get("TURN_TIMEOUT", "45"))
    done = False
    while time.time() < end and not done:
        f = a.read_frame(timeout=max(0.2, end - time.time()))
        if f is None:
            print("...no frame (idle)")
            break
        if f.get("__eof__"):
            print("EOF from agent")
            break
        if "method" in f and "id" in f:
            print("<<REQ %s id=%s params=%s" % (f["method"], f["id"], json.dumps(f.get("params", {}))[:300]))
            if POLICY == "timeout":
                print("   (deliberately NOT answering -> tests client bound)")
            else:
                result = handle(f, POLICY)
                m = {"jsonrpc": "2.0", "id": f["id"], "result": result}
                a.p.stdin.write(json.dumps(m) + "\n")
                a.p.stdin.flush()
                a.log.append((">>>", json.dumps(m)))
        elif "method" in f:
            print("<-NOTIF %s %s" % (f["method"], json.dumps(f.get("params", {}))[:240]))
        elif f.get("id") == rid:
            print("PROMPT RESPONSE:", json.dumps(f, ensure_ascii=False)[:600])
            done = True
        else:
            print("<-OTHER", json.dumps(f)[:240])
    if not done and POLICY != "timeout":
        print("TURN DID NOT COMPLETE within timeout")
    a.kill()


if __name__ == "__main__":
    main()