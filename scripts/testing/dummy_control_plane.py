#!/usr/bin/env python3
"""
Dummy control plane: a stdlib-only stand-in for the real Flowera Go control
plane, used only by scripts/*/test-vm.* to test one desktop image quickly
without building/running the whole flowera stack.

A desktop's agent makes two kinds of outbound call to CONTROL_PLANE_URL:

  POST /api/querytext    (agent/flowera_agent/messaging.py) - sent both for a
                         genuine query and for the final "task
                         completed" reply (ask_for_query/write_final_reply)
  POST /api/agent/events (agent/flowera_agent/progress.py) - one line per step
                         while a task runs, whichever harness is driving it

This server accepts both, prints them to the console so you can watch a test
desktop's agent work in real time, and answers every other path with a
permissive 200 so nothing the agent does ever blocks on a missing endpoint.

No task-delivery endpoint is needed here: test-vm.sh/.ps1 deliver the task
the same way the real control plane does, by POSTing straight to the
desktop's own vm_server.py on port 8081 (see internal/desktop/manager.go's
SendRequest) - this process only ever receives calls, never makes them.
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    def _reply(self, status=200, body=None):
        payload = json.dumps(body if body is not None else {"status": "ok"}).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) if length else b""

        if self.path == "/api/querytext":
            try:
                data = json.loads(raw.decode("utf-8"))
            except (ValueError, UnicodeDecodeError):
                data = {}

            print("=" * 60, flush=True)
            print(f"[{data.get('vm_name', '?')}] {data.get('filename', '')}", flush=True)
            print("-" * 60, flush=True)
            print(data.get("content", raw.decode("utf-8", "replace")), flush=True)
            print("=" * 60, flush=True)

            self._reply(200, {"status": "received"})
            return

        if self.path == "/api/agent/events":
            try:
                data = json.loads(raw.decode("utf-8"))
            except (ValueError, UnicodeDecodeError):
                data = {}

            # One line each, unboxed: these arrive steadily while a task runs,
            # and the point of watching them is seeing the shape of the work.
            print(f"  · [{data.get('vm_name', '?')}] {data.get('text', '')}", flush=True)

            self._reply(200, {"status": "received"})
            return

        # Every other path (memory writes, etc.) - accept and ignore, so
        # nothing the agent does ever hangs or logs a control-plane error
        # just because this stub doesn't implement it.
        self._reply(200, {"status": "ok"})

    def do_GET(self):
        self._reply(200, {"status": "ok", "note": "dummy control plane for test-vm"})

    def log_message(self, fmt, *args):
        pass  # keep stdout to the /api/querytext output above


if __name__ == "__main__":
    server = ThreadingHTTPServer(("0.0.0.0", 8080), Handler)
    print("Dummy control plane listening on :8080 - watching for /api/querytext ...", flush=True)
    server.serve_forever()
