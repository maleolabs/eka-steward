#!/usr/bin/env python3
"""Probe 3: exercise the full opencode ACP method surface and record FULL frames.

Run: python3 internal/acp/probe/methods_probe.py
Writes a frame log to $ACP_TMP (default: system temp).
"""
import sys, json, time, os, subprocess, threading, select, tempfile
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from probe import redact

CWD = os.path.join(tempfile.gettempdir(), "acp-methods-work")
os.makedirs(CWD, exist_ok=True)
LOG = os.path.join(os.environ.get("ACP_TMP", tempfile.gettempdir()), "acp-methods.log")


class Rec:
    def __init__(self, argv, logpath):
        self.p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, text=True, bufsize=1)
        self.fh = open(logpath, "w")
        self._id = 0
        threading.Thread(target=self._err, daemon=True).start()

    def _err(self):
        for l in self.p.stderr:
            self._w("STDERR", l.rstrip())

    def _w(self, tag, text):
        self.fh.write("%s %s\n" % (tag, redact(text)))
        self.fh.flush()

    def req(self, method, params):
        self._id += 1
        rid = self._id
        m = {"jsonrpc": "2.0", "id": rid, "method": method, "params": params}
        self._w(">>>", json.dumps(m))
        self.p.stdin.write(json.dumps(m) + "\n")
        self.p.stdin.flush()
        return rid

    def notif(self, method, params):
        m = {"jsonrpc": "2.0", "method": method, "params": params}
        self._w(">>>", json.dumps(m))
        self.p.stdin.write(json.dumps(m) + "\n")
        self.p.stdin.flush()

    def resp(self, rid, result):
        m = {"jsonrpc": "2.0", "id": rid, "result": result}
        self._w(">>>", json.dumps(m))
        self.p.stdin.write(json.dumps(m) + "\n")
        self.p.stdin.flush()

    def read(self, timeout=30):
        r, _, _ = select.select([self.p.stdout], [], [], timeout)
        if not r:
            return None
        line = self.p.stdout.readline()
        if not line:
            return {"__eof__": True}
        self._w("<<<", line.rstrip())
        try:
            return json.loads(line)
        except Exception:
            return {"__raw__": line.rstrip()}

    def wait(self, rid, timeout=30, autohandle=True):
        notes = []
        end = time.time() + timeout
        while time.time() < end:
            f = self.read(timeout=max(0.2, end - time.time()))
            if f is None:
                return None, notes
            if f.get("__eof__"):
                return f, notes
            if "method" in f and "id" in f:
                if autohandle:
                    self.handle(f)
                    notes.append(f)
                continue
            if f.get("id") == rid and ("result" in f or "error" in f):
                return f, notes
            notes.append(f)
        return None, notes

    def handle(self, f):
        p = f.get("params", {})
        if f["method"] == "session/request_permission":
            opts = p.get("options", [])
            self.resp(f["id"], {"outcome": {"outcome": "selected", "optionId": opts[0]["optionId"] if opts else "once"}})
        elif f["method"] == "fs/read_text_file":
            self.resp(f["id"], {"content": ""})
        elif f["method"] == "fs/write_text_file":
            self.resp(f["id"], {})
        else:
            self.resp(f["id"], {})

    def close(self):
        try:
            self.p.kill()
        except Exception:
            pass
        self.fh.close()


def main():
    r = Rec(["opencode", "acp"], LOG)
    rid = r.req("initialize", {"protocolVersion": 1,
        "clientCapabilities": {"fs": {"readTextFile": True, "writeTextFile": True}, "terminal": True},
        "clientInfo": {"name": "eka-steward-probe", "version": "0.0.1"}})
    init, _ = r.wait(rid)
    print("init ok:", "result" in (init or {}))

    rid = r.req("session/new", {"cwd": CWD, "mcpServers": []})
    sn, _ = r.wait(rid)
    sid = (sn.get("result") or {}).get("sessionId")
    print("sessionId:", sid)

    cfg = (sn.get("result") or {}).get("configOptions") or []
    if cfg:
        first = cfg[0]
        rid = r.req("session/set_config_option", {"sessionId": sid, "configId": first["id"], "value": first.get("currentValue")})
        sc, _ = r.wait(rid)
        print("set_config_option:", json.dumps(sc)[:200])

    rid = r.req("session/list", {})
    sl, _ = r.wait(rid)
    print("session/list:", json.dumps(sl)[:300])

    rid = r.req("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": "Say DONE."}]})
    pr, notes = r.wait(rid, timeout=120)
    print("prompt stopReason:", (pr or {}).get("result"))
    print("update kinds:", sorted({(n.get("params", {}).get("update", {}) or {}).get("sessionUpdate") for n in notes if n.get("method") == "session/update"}))

    rid = r.req("session/load", {"sessionId": sid, "cwd": CWD, "mcpServers": []})
    ld, notes = r.wait(rid, timeout=30)
    print("session/load:", "result" in (ld or {}), "replay-notifs:", len(notes))

    rid = r.req("session/resume", {"sessionId": sid, "cwd": CWD, "mcpServers": []})
    rs, notes = r.wait(rid, timeout=30)
    print("session/resume:", json.dumps(rs)[:120], "notifs:", len(notes))

    rid = r.req("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": "Count slowly from 1 to 1000, one number per line."}]})
    time.sleep(3)
    r.notif("session/cancel", {"sessionId": sid})
    cx, _ = r.wait(rid, timeout=60)
    print("cancel stopReason:", (cx or {}).get("result"))

    rid = r.req("session/close", {"sessionId": sid})
    cl, _ = r.wait(rid, timeout=15)
    print("session/close:", json.dumps(cl)[:120])

    rid = r.req("session/delete", {"sessionId": sid})
    dl, _ = r.wait(rid, timeout=15)
    print("session/delete:", json.dumps(dl)[:120])

    r.close()
    print("LOG:", LOG)


if __name__ == "__main__":
    main()