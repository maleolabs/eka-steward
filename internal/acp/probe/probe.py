#!/usr/bin/env python3
"""Minimal ACP stdio client used by the spike probes.

Not production code. Not imported by any Go package. Spawns an ACP agent and
drives JSON-RPC over stdin/stdout, recording every frame (secrets redacted).

stdout is drained by a reader thread into a queue, so read_frame never mixes
select() with a buffered text wrapper (which can hide already-buffered lines).
"""
import json, os, subprocess, threading, time, queue, re

SECRET = re.compile(r'(sk-[A-Za-z0-9_\-]{8,}|AIza[A-Za-z0-9_\-]{10,}|ghp_[A-Za-z0-9]{10,}|Bearer\s+[A-Za-z0-9._\-]+)', re.I)


def redact(obj):
    s = obj if isinstance(obj, str) else json.dumps(obj, ensure_ascii=False)
    return SECRET.sub('[REDACTED]', s)


class Agent:
    def __init__(self, argv, env=None):
        e = dict(os.environ)
        if env:
            e.update(env)
        self.p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.PIPE, env=e, text=True, bufsize=1)
        self.log = []
        self._id = 0
        self._q = queue.Queue()
        threading.Thread(target=self._drain_err, daemon=True).start()
        threading.Thread(target=self._drain_out, daemon=True).start()

    def _drain_out(self):
        for line in self.p.stdout:
            self._q.put(line)

    def _drain_err(self):
        for line in self.p.stderr:
            self.log.append(("stderr", redact(line.rstrip())))

    def send(self, method, params, rid=None, notify=False):
        m = {"jsonrpc": "2.0", "method": method, "params": params}
        if not notify:
            if rid is None:
                self._id += 1
                rid = self._id
            m["id"] = rid
        line = json.dumps(m)
        self.log.append((">>>", redact(line)))
        try:
            self.p.stdin.write(line + "\n")
            self.p.stdin.flush()
        except BrokenPipeError:
            self.log.append(("!!", "broken pipe writing %s" % method))
        return rid

    def read_frame(self, timeout=5.0):
        try:
            line = self._q.get(timeout=timeout)
        except queue.Empty:
            return None
        if not line:
            return {"__eof__": True}
        self.log.append(("<<<", redact(line.rstrip())))
        try:
            return json.loads(line)
        except Exception:
            return {"__raw__": line.rstrip()}

    def wait_response(self, rid, timeout=10.0):
        notes = []
        end = time.time() + timeout
        while time.time() < end:
            f = self.read_frame(timeout=max(0.1, end - time.time()))
            if f is None:
                return None, notes
            if f.get("__eof__"):
                return {"__eof__": True}, notes
            if f.get("id") == rid and ("result" in f or "error" in f):
                return f, notes
            notes.append(f)
        return None, notes

    def kill(self):
        try:
            self.p.kill()
        except Exception:
            pass


def dump(agent, title):
    print("\n===== %s =====" % title)
    for tag, line in agent.log:
        print("%-4s %s" % (tag, line))