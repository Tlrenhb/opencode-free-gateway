#!/usr/bin/env python3
"""Minimal mock upstream for QA of the relay gateway.
Echoes every request into mock-requests.log; serves canned responses so we
can verify passthrough, header construction, retries and SSE streaming.
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = "/root/ocfreelay-go/qa-reports/mock-requests.log"

def log_entry(method, path, headers, body):
    entry = {
        "t": time.time(),
        "method": method,
        "path": path,
        "headers": dict(headers),
        "body": body,
    }
    with open(LOG, "a") as f:
        f.write(json.dumps(entry) + "\n")

class H(BaseHTTPRequestHandler):
    # HTTP/1.0: connection closed after each response -> clean EOF for
    # streaming copies (no keep-alive deadlock in the relay's io.Copy).
    protocol_version = "HTTP/1.0"

    def _handle(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode("utf-8", "replace") if length else ""
        log_entry(self.command, self.path, self.headers, body)

        # --- trigger routes (matched on body content).
        # First-hit triggers fire once; retried requests succeed, so we can
        # observe worker rotation (worker1 fails -> worker2 succeeds). ---
        counters = self.server.counters
        if "x-trigger-429free" in body:
            n = counters["429free"] = counters.get("429free", 0) + 1
            if n == 1:
                self._json(429, {"error": {"message": "FreeUsageLimitError: free quota exceeded"}})
                return
        if "x-trigger-429" in body:
            n = counters["429"] = counters.get("429", 0) + 1
            if n == 1:
                self._json(429, {"error": {"message": "rate limited, slow down"}})
                return
        if "x-trigger-500" in body:
            n = counters["500"] = counters.get("500", 0) + 1
            if n == 1:
                self._json(500, {"error": {"message": "internal boom"}})
                return
        if "x-trigger-500-all" in body:
            self._json(500, {"error": {"message": "always boom"}})
            return
        if "x-trigger-400" in body:
            self._json(400, {"error": {"message": "bad request from client"}})
            return

        # --- auth behaviour: upstream requires a bearer key ---
        auth = self.headers.get("Authorization", "")
        if not auth.startswith("Bearer "):
            self._json(401, {"error": {"message": "missing api key"}})
            return

        # --- streaming chat ---
        if self.path.startswith("/v1/chat/completions") and "stream=true" in self.path:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            self.wfile.write(b'data: {"id":"chatcmpl-mock1","choices":[{"delta":{"role":"assistant"},"index":0}]}\n\n')
            self.wfile.write(b'data: {"id":"chatcmpl-mock1","choices":[{"delta":{"content":"hello"},"index":0}]}\n\n')
            self.wfile.write(b'data: [DONE]\n\n')
            self.wfile.flush()
            return

        # --- non-stream chat with usage ---
        if self.path.startswith("/v1/chat/completions"):
            self._json(200, {
                "id": "chatcmpl-mock2",
                "object": "chat.completion",
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "pong"}}],
                "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
            })
            return

        # --- models list (no usage field!) ---
        if self.path.startswith("/v1/models"):
            self._json(200, {"object": "list", "data": [{"id": "mock-model", "object": "model"}]})
            return

        self._json(200, {"ok": True, "echo": body})

    def _json(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = _handle
    do_POST = _handle
    do_PUT = _handle
    do_DELETE = _handle

    def log_message(self, *a):
        pass

if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9902
    srv = ThreadingHTTPServer(("127.0.0.1", port), H)
    srv.counters = {}
    print(f"mock upstream on {port}", flush=True)
    srv.serve_forever()
