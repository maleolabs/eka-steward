#!/usr/bin/env python3
"""Probe 1: initialize handshake against real agents, v1- and v2-shaped params.

Run from anywhere:  python3 internal/acp/probe/init_probe.py
"""
import sys, json, os
HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from probe import Agent

TARGETS = {
    "opencode": ["opencode", "acp"],
    "gemini": ["gemini", "--acp"],
}

V1_CAPS = {"auth": {"terminal": True}, "fs": {"readTextFile": True, "writeTextFile": True}, "terminal": True}
INFO = {"name": "eka-steward-probe", "title": "EKA Steward Probe", "version": "0.0.1"}


def run(name, version, style):
    a = Agent(TARGETS[name])
    try:
        if style == "v1":
            rid = a.send("initialize", {"protocolVersion": version, "clientCapabilities": V1_CAPS, "clientInfo": INFO})
        else:
            rid = a.send("initialize", {"protocolVersion": version, "capabilities": {"auth": {"terminal": {}}}, "info": INFO})
        resp, notes = a.wait_response(rid, timeout=12)
        print("\n########## %s version=%s style=%s ##########" % (name, version, style))
        print("RESPONSE:", json.dumps(resp, ensure_ascii=False))
        for n in notes:
            print("NOTIF:", json.dumps(n, ensure_ascii=False)[:400])
    finally:
        a.kill()
        for tag, line in a.log:
            if tag == "stderr" and line.strip():
                print("STDERR:", line[:300])


if __name__ == "__main__":
    for name in TARGETS:
        run(name, 1, "v1")
        run(name, 2, "v2")