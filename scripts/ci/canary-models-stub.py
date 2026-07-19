#!/usr/bin/env python3
"""canary-models-stub.py — a throwaway Anthropic-shaped gateway for the Claude
Code canary's discovery E2E (#52). It stands in for waired's local gateway so
the canary can drive the REAL `claude` binary and observe how Claude Code
consumes GET /v1/models — specifically its /model-picker id filter, which is a
Claude Code contract the grep checks cannot see.

Endpoints:
  GET  /v1/models[...]  -> a model list that mixes the two reserved #52 route
                          directive ids, a real claude-* model, and a JUNK id
                          that does not match ^(claude|anthropic). Whether the
                          junk id survives into Claude Code's model cache tells
                          us if the picker filter still exists / still matches
                          our directive ids.
  POST /v1/messages     -> a minimal, valid Anthropic SSE turn so `claude -p`
                          completes instead of hanging (discovery fires at
                          startup regardless, but a clean turn keeps runtime low).
  anything else         -> 404.

argv[1] is a path the stub writes its actual listen port to (it binds :0), so
the caller can point ANTHROPIC_BASE_URL at it without racing on a fixed port.
No auth is checked; loopback only.
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote

# The junk id MUST NOT start with "claude" or "anthropic": it is the negative
# probe for Claude Code's ^(claude|anthropic) /model-picker id filter.
JUNK_ID = "waired-junk-should-be-filtered"

MODELS = {
    "data": [
        {"type": "model", "id": "anthropic-waired-auto",
         "display_name": "Waired auto (local, fallback to Anthropic)", "max_input_tokens": 250000},
        {"type": "model", "id": "anthropic-waired-local",
         "display_name": "Waired local (this device)", "max_input_tokens": 250000},
        {"type": "model", "id": "claude-waired-cloud[1m]",
         "display_name": "Waired cloud (Anthropic API)", "max_input_tokens": 1000000},
        {"type": "model", "id": "claude-sonnet-5",
         "display_name": "Claude Sonnet 5", "max_input_tokens": 200000},
        {"type": "model", "id": JUNK_ID, "display_name": "junk"},
    ],
    "has_more": False,
    "first_id": "anthropic-waired-auto",
    "last_id": JUNK_ID,
}

SSE_TURN = "".join(
    "event: {}\ndata: {}\n\n".format(ev, json.dumps(payload))
    for ev, payload in [
        ("message_start", {"type": "message_start", "message": {
            "id": "msg_canary", "type": "message", "role": "assistant",
            "model": "claude-sonnet-5", "content": [], "stop_reason": None,
            "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 1}}}),
        ("content_block_start", {"type": "content_block_start", "index": 0,
                                 "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                 "delta": {"type": "text_delta", "text": "pong"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta",
                           "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                           "usage": {"output_tokens": 1}}),
        ("message_stop", {"type": "message_stop"}),
    ]
)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # quiet

    def _json(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/v1/models":
            self._json(MODELS)
            return
        if path.startswith("/v1/models/"):
            mid = unquote(path[len("/v1/models/"):])
            for m in MODELS["data"]:
                if m["id"] == mid:
                    self._json(m)
                    return
            self.send_error(404)
            return
        self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        if length:
            self.rfile.read(length)
        if self.path.split("?", 1)[0] == "/v1/messages":
            body = SSE_TURN.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_error(404)


def main():
    port_file = sys.argv[1] if len(sys.argv) > 1 else None
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    port = httpd.server_address[1]
    if port_file:
        with open(port_file, "w") as f:
            f.write(str(port))
    else:
        print(port, flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
