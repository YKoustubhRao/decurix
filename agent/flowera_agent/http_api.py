import json
from http.server import BaseHTTPRequestHandler

from flowera_agent.logging_utils import log
from flowera_agent.messaging import save_message


# ============================================================
# HTTP API
# ============================================================

class Handler(BaseHTTPRequestHandler):

    def log_message(self, format, *args):
        log(
            "HTTP "
            + format % args
        )

    def do_POST(self):

        if self.path != "/message":
            self.send_response(404)
            self.end_headers()
            return

        try:

            content_length = int(
                self.headers.get(
                    "Content-Length",
                    "0",
                )
            )

            raw = self.rfile.read(
                content_length
            )

            data = json.loads(
                raw.decode("utf-8")
            )

            message = data.get(
                "content",
                "",
            )

            if not message:
                raise ValueError(
                    "missing content"
                )

            path = save_message(
                message
            )

            self.send_response(200)

            self.send_header(
                "Content-Type",
                "application/json",
            )

            self.end_headers()

            self.wfile.write(
                json.dumps(
                    {
                        "status": "received",
                        "file": path.name,
                    }
                ).encode("utf-8")
            )

        except Exception as exc:

            log(
                f"POST /message failed: "
                f"{exc}"
            )

            self.send_response(400)
            self.send_header(
                "Content-Type",
                "application/json",
            )
            self.end_headers()

            self.wfile.write(
                json.dumps(
                    {
                        "status": "error",
                        "error": str(exc),
                    }
                ).encode("utf-8")
            )
