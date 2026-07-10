#!/usr/bin/env python3
"""
Tiny HTTP server used by scripts/dev/full-e2e-claude.sh stage 2.

It listens on 127.0.0.1:<port>, accepts any POST to any path, captures
every request header into <log_path>, and replies with a canonical
Anthropic-Messages-shaped JSON body so claude(1) thinks the call
succeeded.

We use this instead of the real Local Gateway because the real
gateway requires Ollama to actually serve a completion. The
integration we're proving here is "does claude reach a server with
the right Authorization header", which the stub is enough for.

Usage: stub-gateway.py <port> <log_path>
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def make_handler(log_path):
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            content_length = int(self.headers.get("Content-Length", "0"))
            _ = self.rfile.read(content_length) if content_length else b""

            with open(log_path, "a", encoding="utf-8") as f:
                f.write(f"--- {self.command} {self.path}\n")
                for key, value in self.headers.items():
                    f.write(f"{key}: {value}\n")
                f.write("\n")

            body = json.dumps(
                {
                    "id": "msg_stub",
                    "type": "message",
                    "role": "assistant",
                    "model": "waired-coding-auto",
                    "content": [{"type": "text", "text": "ok"}],
                    "stop_reason": "end_turn",
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                }
            ).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args, **kwargs):  # quiet stderr
            pass

    return H


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: stub-gateway.py <port> <log_path>", file=sys.stderr)
        return 2
    port = int(sys.argv[1])
    log_path = sys.argv[2]
    server = HTTPServer(("127.0.0.1", port), make_handler(log_path))
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
