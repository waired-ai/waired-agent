#!/usr/bin/env python3
"""measure-agent-request.py — measure the SHAPE of a real coding agent's
request, so the agent-grade fixture (#322) can be held to it.

The fixture in internal/agentgrade has to be the weight a coding agent
actually sends: a model that emits perfect tool calls on a two-tool toy
request can still fail on the real thing, and grading on the toy is how a
model passes here and then fails a user. So the floors the fixture is checked
against are measured, not guessed — this is what measures them.

It records ONLY structural numbers:

    tools, total tool-schema bytes, max schema depth, system-prompt bytes,
    whole-request bytes

and prints them. It deliberately does NOT keep the request body. Two reasons,
both hard limits rather than preferences:

  * a real client's request carries its own identifiers — Claude Code sends
    metadata.user_id containing a device id and a session id — and this repo
    is public: "never commit tokens, keys, real device identifiers ... including
    in test fixtures";
  * the system prompt of a third-party agent is that vendor's text, and
    vendoring it into a public repository as a test input is not ours to do.

Pass --dump-shape-only=false to keep a REDACTED body for local inspection; it
strips metadata and replaces prompt text with its length. Even that stays out
of version control — write it under a scratch path, not into the repo.

Usage:

    python3 scripts/dev/measure-agent-request.py -- claude -p hello

Anything after `--` is the agent command to run. The script stands up a
throwaway Anthropic-shaped endpoint, points ANTHROPIC_BASE_URL at it, runs the
command with a dummy key and an isolated config dir, and reports on the first
POST /v1/messages it receives.
"""

import argparse
import json
import os
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = {
    "data": [{"type": "model", "id": "claude-sonnet-5",
              "display_name": "Claude Sonnet 5", "max_input_tokens": 200000}],
    "has_more": False,
}

# A minimal but valid streaming turn, so the agent completes instead of
# hanging or retrying (a retry would just capture the same shape twice).
SSE_TURN = "".join(
    "event: {}\ndata: {}\n\n".format(ev, json.dumps(p))
    for ev, p in [
        ("message_start", {"type": "message_start", "message": {
            "id": "msg_measure", "type": "message", "role": "assistant",
            "model": "claude-sonnet-5", "content": [], "stop_reason": None,
            "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 1}}}),
        ("content_block_start", {"type": "content_block_start", "index": 0,
                                 "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                 "delta": {"type": "text_delta", "text": "ok"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta",
                           "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                           "usage": {"output_tokens": 1}}),
        ("message_stop", {"type": "message_stop"}),
    ])

captured = []
captured_event = threading.Event()


def depth(value):
    if isinstance(value, dict):
        return max([depth(v) for v in value.values()] + [0]) + 1
    if isinstance(value, list):
        return max([depth(v) for v in value] + [0]) + 1
    return 0


def measure(req):
    tools = req.get("tools") or []
    tool_bytes = sum(len(json.dumps(t)) for t in tools)
    max_depth = max([depth(t.get("input_schema", {})) for t in tools] + [0])

    system = req.get("system")
    if isinstance(system, list):
        system_bytes = sum(len(b.get("text", "")) for b in system)
    elif isinstance(system, str):
        system_bytes = len(system)
    else:
        system_bytes = 0

    return {
        "tools": len(tools),
        "tool_schema_bytes": tool_bytes,
        "max_schema_depth": max_depth,
        "system_bytes": system_bytes,
        "whole_request_bytes": len(json.dumps(req)),
        "model": req.get("model"),
        "max_tokens": req.get("max_tokens"),
        "stream": bool(req.get("stream")),
        "tool_names": sorted(t.get("name", "") for t in tools),
    }


def redact(req):
    """Strip identity and replace prose with its length."""
    out = {}
    for k, v in req.items():
        if k == "metadata":
            out[k] = "<removed: carries client device/session identifiers>"
        elif k == "system":
            if isinstance(v, list):
                out[k] = [{"type": b.get("type"), "text_bytes": len(b.get("text", ""))} for b in v]
            else:
                out[k] = {"text_bytes": len(v or "")}
        elif k == "messages":
            out[k] = [{"role": m.get("role"), "content_bytes": len(json.dumps(m.get("content")))}
                      for m in v]
        elif k == "tools":
            out[k] = [{"name": t.get("name"), "bytes": len(json.dumps(t)),
                       "schema_depth": depth(t.get("input_schema", {}))} for t in v]
        else:
            out[k] = v
    return out


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def _send(self, code, body, ctype="application/json"):
        data = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._send(200, json.dumps(MODELS))
        else:
            self._send(404, "{}")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length)
        if self.path.startswith("/v1/messages") and not captured:
            try:
                captured.append(json.loads(raw))
                captured_event.set()
            except json.JSONDecodeError as exc:
                print(f"could not decode request: {exc}", file=sys.stderr)
        self._send(200, SSE_TURN, "text/event-stream")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--redacted-out", metavar="PATH",
                    help="also write a REDACTED body here (identity stripped, prose "
                         "replaced by byte counts). Use a scratch path; never commit it.")
    ap.add_argument("--timeout", type=int, default=120, help="seconds to wait for the agent")
    ap.add_argument("command", nargs=argparse.REMAINDER,
                    help="the agent command, after `--`")
    args = ap.parse_args()

    cmd = [a for a in args.command if a != "--"]
    if not cmd:
        ap.error("give the agent command after `--`, e.g. -- claude -p hello")

    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    port = srv.server_address[1]
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    with tempfile.TemporaryDirectory() as work:
        env = dict(os.environ)
        env["ANTHROPIC_BASE_URL"] = f"http://127.0.0.1:{port}"
        env["ANTHROPIC_API_KEY"] = "measure-dummy-not-a-real-key"
        env["CLAUDE_CONFIG_DIR"] = os.path.join(work, "config")
        os.makedirs(env["CLAUDE_CONFIG_DIR"], exist_ok=True)
        try:
            subprocess.run(cmd, env=env, cwd=work, timeout=args.timeout,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        except subprocess.TimeoutExpired:
            print("agent command timed out", file=sys.stderr)
        except FileNotFoundError:
            print(f"command not found: {cmd[0]}", file=sys.stderr)
            return 2

    srv.shutdown()

    if not captured:
        print("no POST /v1/messages was captured — the agent never sent a turn "
              "(wrong base-url env var, or it failed before the first request?)",
              file=sys.stderr)
        return 1

    req = captured[0]
    shape = measure(req)
    print(json.dumps(shape, indent=2))
    print("\nCompare against the floors in internal/agentgrade/fixture.go "
          "(fixtureMin*). Raise them, and the fixture, if the real request has "
          "grown materially heavier.", file=sys.stderr)

    if args.redacted_out:
        with open(args.redacted_out, "w", encoding="utf-8") as f:
            json.dump(redact(req), f, indent=2)
        print(f"redacted body written to {args.redacted_out} "
              f"(do NOT commit it)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
