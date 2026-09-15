import json
import shutil
import traceback
import urllib.parse
from http.server import BaseHTTPRequestHandler

from flowera_agent import record_task, replay_task
from flowera_agent.computer import demonstration, recording, replay
from flowera_agent.computer.errors import ComputerError
from flowera_agent.logging_utils import log
from flowera_agent.messaging import save_request


# ============================================================
# HTTP API
#
# What the control plane reaches this desktop through, on :8081:
#
#   POST /request                        a human request (chat, query reply) -
#                                        also how a ticket is opened: a
#                                        "/record <task>" request is routed to
#                                        record_task.py the same way any other
#                                        request reaches its harness
#   GET  /recordings                     every recording's meta, and the
#                                        demonstration being recorded
#   GET  /recordings/<id>                one recording: meta, events, and the
#                                        program a replay of it would run
#   GET  /recordings/<id>/frames/<name>  one of its screenshots
#   GET  /recordings/<id>/export         the whole recording, as a zip
#   POST /recordings/<id>/update         {title, description}
#   POST /recordings/<id>/delete
#   POST /recordings/<id>/replay         {task_id, from, params}: run it as a task
#   POST /demonstration/start            {title, description}: an ad-hoc
#                                        recording, no ticket - the recordings
#                                        page's own "Record a demonstration"
#   POST /demonstration/pause            {reason}
#   POST /demonstration/resume
#   POST /demonstration/stop
#   GET  /ticket                         the ticket open on this desktop, if
#                                        any - the GUI screen's toolbar
#   POST /ticket/begin                   start capturing for the open ticket
#   POST /ticket/pause                   {reason}
#   POST /ticket/resume
#   POST /ticket/note                    {text}: a person's own explanation,
#                                        recorded at this moment
#   POST /ticket/end                     stop capturing (if started) and
#                                        finish the ticket's task
#
# Served by a ThreadingHTTPServer (app.py): exporting a large recording must
# not hold up the next chat request.
# ============================================================

MAX_BODY = 1024 * 1024


def _text(value, limit):
    return str(value or "").strip()[:limit]


class Handler(BaseHTTPRequestHandler):

    def log_message(self, format, *args):
        log(
            "HTTP "
            + format % args
        )

    def _parts(self):
        path = urllib.parse.urlsplit(self.path).path
        return [urllib.parse.unquote(part) for part in path.strip("/").split("/") if part]

    def _body(self):
        length = int(self.headers.get("Content-Length", "0") or "0")

        if length > MAX_BODY:
            raise ValueError("request too large")

        raw = self.rfile.read(length) if length else b""
        data = json.loads(raw.decode("utf-8")) if raw else {}

        if not isinstance(data, dict):
            raise ValueError("expected a JSON object")

        return data

    def _json(self, status, payload):
        body = json.dumps(payload).encode("utf-8")

        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, reason):
        self._json(status, {"status": "error", "error": reason})

    def _guarded(self, route):
        """Run a route, turning what it raises into a status an HTTP client can act on."""

        try:
            if route() is False:
                self._error(404, "not found")
        except FileNotFoundError as exc:
            self._error(404, str(exc) or "not found")
        except ComputerError as exc:
            self._error(409, str(exc))
        except ValueError as exc:
            self._error(400, str(exc))
        except Exception as exc:
            log(f"{self.command} {self.path} failed:\n{traceback.format_exc()}")
            self._error(500, f"{type(exc).__name__}: {exc}")

    # ---- GET ----

    def do_GET(self):
        parts = self._parts()

        def route():
            if parts == ["recordings"]:
                return self._json(200, {"recordings": recording.sessions(), "demonstration": demonstration.status()})

            if parts == ["demonstration"]:
                return self._json(200, {"demonstration": demonstration.status()})

            if parts == ["ticket"]:
                return self._json(200, {"ticket": record_task.status()})

            if len(parts) == 2 and parts[0] == "recordings":
                meta, events = recording.load(parts[1])
                return self._json(200, {
                    "meta": meta,
                    "events": events,
                    "program": replay.compile_program(meta, events),
                    "demonstration": demonstration.status(),
                })

            if len(parts) == 4 and parts[0] == "recordings" and parts[2] == "frames":
                return self._frame(parts[1], parts[3])

            if len(parts) == 3 and parts[0] == "recordings" and parts[2] == "export":
                return self._export(parts[1])

            return False

        self._guarded(route)

    def _frame(self, session_id, name):
        path = recording.frame_file(session_id, name)
        if path is None:
            raise FileNotFoundError("no such frame")

        self.send_response(200)
        self.send_header("Content-Type", "image/png")
        self.send_header("Content-Length", str(path.stat().st_size))
        # Named by content, so a frame never changes.
        self.send_header("Cache-Control", "private, max-age=31536000, immutable")
        self.end_headers()

        with open(path, "rb") as f:
            shutil.copyfileobj(f, self.wfile)

    def _export(self, session_id):
        # Everything that can fail cleanly does so before a header is sent.
        meta, events = recording.load(session_id)
        program = json.dumps(replay.compile_program(meta, events), indent=1).encode("utf-8")

        self.send_response(200)
        self.send_header("Content-Type", "application/zip")
        self.send_header("Content-Disposition", f'attachment; filename="{session_id}.zip"')
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True

        try:
            recording.export(session_id, self.wfile, extra={"program.json": program})
        except Exception as exc:
            # The headers are gone; a zip cut short is the only way left to fail.
            log(f"Export of {session_id} failed part way: {exc}")

    # ---- POST ----

    def do_POST(self):
        parts = self._parts()

        if parts == ["request"]:
            return self._request()

        def route():
            data = self._body()

            if len(parts) == 2 and parts[0] == "demonstration":
                return self._demonstration(parts[1], data)

            if len(parts) == 2 and parts[0] == "ticket":
                return self._ticket(parts[1], data)

            if len(parts) == 3 and parts[0] == "recordings":
                session_id, action = parts[1], parts[2]

                if action == "update":
                    meta = recording.update(
                        session_id,
                        title=_text(data.get("title"), 200) if "title" in data else None,
                        description=_text(data.get("description"), 2000) if "description" in data else None,
                    )
                    return self._json(200, {"status": "ok", "meta": meta})

                if action == "delete":
                    if (demonstration.status() or {}).get("session") == session_id:
                        raise ComputerError("that demonstration is still being recorded - stop it first")
                    recording.delete(session_id)
                    return self._json(200, {"status": "deleted"})

                if action == "replay":
                    return self._replay(session_id, data)

            return False

        self._guarded(route)

    def _demonstration(self, action, data):
        if action == "start":
            meta = demonstration.start(title=_text(data.get("title"), 200), description=_text(data.get("description"), 2000))
        elif action == "pause":
            meta = demonstration.pause(_text(data.get("reason"), 200))
        elif action == "resume":
            meta = demonstration.resume()
        elif action == "stop":
            meta = demonstration.stop()
        else:
            return False

        return self._json(200, {"status": "ok", "meta": meta, "demonstration": demonstration.status()})

    def _ticket(self, action, data):
        if action == "begin":
            record_task.begin()
        elif action == "pause":
            record_task.pause(_text(data.get("reason"), 200))
        elif action == "resume":
            record_task.resume()
        elif action == "note":
            record_task.note(_text(data.get("text"), 1000))
        elif action == "end":
            record_task.end()
        else:
            return False

        return self._json(200, {"status": "ok", "ticket": record_task.status()})

    def _replay(self, session_id, data):
        recording.load(session_id)

        params = data.get("params") or {}
        if not isinstance(params, dict):
            raise ValueError("params must be an object of name: value")

        start = int(data.get("from") or 1)
        task_id = _text(data.get("task_id"), 64)
        content = f"/replay {session_id}" + (f" from={start}" if start > 1 else "")

        # The request the watcher reads names only the recording: the values
        # stay in this process's memory until the replay takes them.
        replay_task.hold_params(task_id, params)
        path = save_request(content, task_id)

        return self._json(200, {"status": "received", "file": path.name, "content": content})

    def _request(self):

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

            content = data.get(
                "content",
                "",
            )

            if not content:
                raise ValueError(
                    "missing content"
                )

            # The control plane's record for this request; it comes back
            # on every report about it (task_context.py).
            path = save_request(
                content,
                data.get("task_id", ""),
            )

            self._json(200, {"status": "received", "file": path.name})

        except Exception as exc:

            log(
                f"POST /request failed: "
                f"{exc}"
            )

            self._error(400, str(exc))
