import hmac
import ipaddress
import json
import shutil
import traceback
import urllib.parse
from http.server import BaseHTTPRequestHandler

from decurix_agent import config, playbooks, practice, record_task, replay_task, skill_bundle, skill_task
from decurix_agent.computer import (
    catalog,
    demonstration,
    ingest,
    lint,
    modes,
    recording,
    replay,
    skillmd,
    skills,
    workflow,
)
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log
from decurix_agent.messaging import save_request


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
#   GET  /recordings/<id>                one recording: meta, events, the
#                                        program a replay of it would run, and
#                                        its lint (lint.py: how far to trust it)
#   GET  /recordings/<id>/frames/<name>  one of its screenshots
#   GET  /recordings/<id>/export         the whole recording, as a zip
#   GET  /recordings/<id>/workflow       its subtasks with their completion
#                                        criteria (workflow.py), as data and as
#                                        the text a model is given
#   GET  /skills/<name>/skill.md         a skill as a SKILL.md (skillmd.py)
#   GET  /skills/<name>/bundle           a skill as a bundle for the org's registry (skill_bundle.py)
#   POST /skills/install                 {bundle, version, sha, replace}: a skill from the registry
#   POST /recordings/import              {title, data}: a Chrome DevTools Recorder
#                                        export, a BrowserGym / AgentLab trajectory
#                                        or an OpenHands event list saved as a
#                                        recording (ingest.py, ingest_agents.py);
#                                        what it skipped and any warnings come back
#   POST /recordings/<id>/update         {title, description}
#   POST /recordings/<id>/delete
#   POST /recordings/<id>/replay         {task_id, from, params}: run it as a task
#   POST /recordings/<id>/learn          {task_id, sessions, name}: teach a skill from
#                                        it (and any others named) as a task -
#                                        the desktop then asks its questions in
#                                        the chat
#   GET  /skills                         every skill taught on this desktop
#   GET  /skills/<name>                  one skill, with its steps
#   POST /skills/<name>/delete
#   POST /skills/<name>/run              {task_id, params}: do it as a task
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
#   POST /ticket/attach                  {files: [paths]}: documents added to
#                                        the open ticket, kept with its recording
#   POST /ticket/end                     stop capturing (if started) and
#                                        finish the ticket's task
#   GET  /health                         {"status": "ok"} and nothing else: the
#                                        one route served without the token
#
# Served by a ThreadingHTTPServer (app.py): exporting a large recording must
# not hold up the next chat request.
#
# Who may call (refusal() below, before any route runs). /request carries a
# person's replies, approvals included, so a caller that is not the control
# plane must not get through:
#   - the token: on a desktop created with DECURIX_AGENT_TOKEN, every route but
#     /health needs "Authorization: Bearer <token>" (compared in constant time).
#     That keeps out the other desktops on the shared Docker network.
#   - a browser: a request with an Origin or Sec-Fetch-* header was made by a
#     web page (a page in this desktop's Chrome can POST to it as text/plain,
#     no CORS needed), and is refused; so is a write whose body is not JSON,
#     and a Host that is a DNS name (a rebinding page reaches this port by one;
#     the control plane calls by IP or container name).
# Those last checks hold on a desktop without a token too (created before
# tokens existed): it is otherwise served as before until it is recreated.
# The honest limit: the worker's own shell runs as the same Linux user and can
# read the token from the environment, so nothing here stops the worker itself
# from calling this API. That is what the approval gate's own rules are for
# (a reply must name the open question: messaging.save_request).
# ============================================================

MAX_BODY = 1024 * 1024

# The one route a caller without the token is answered on. It says nothing but that the agent is up.
HEALTH = ["health"]


def _keep_execution(task_id, data):
    """Keep the control plane's execution id that a teach, replay or run-skill request names (H-CP-10), as
    /request does: its run's label names that attempt (retention). A value of another shape is ignored."""

    from decurix_agent import task_context
    if task_context.valid(task_id):
        task_context.set_execution(task_id, data.get("execution_id"))


def _note_secret_params(task_id, declared, params):
    """The values of the parameters a skill or a recording marks secret (`declared`: its params list), given with the
    run: noted as the task's secret values before anything keeps or shows the request (C14), the way /request's
    "secret" is - the "/skill ... key=..." line was logged, and the guided run kept the value, raw (review round 5,
    R4-B2). Returns whether any was given (the saved message is then marked secret)."""

    from decurix_agent import task_context
    secret = {str(p.get("name")) for p in declared or () if isinstance(p, dict) and p.get("secret")}
    found = [str(v) for k, v in (params or {}).items() if str(k) in secret and str(v if v is not None else "").strip()]
    if found and task_context.valid(task_id):
        task_context.note_secret_values(task_id, found)
    elif found:
        from decurix_agent.computer import privacy
        privacy.remember_literals(found, "")
    return bool(found)


def _saved_secret(path, secret):
    """What the watcher reads beside a saved message: whether it is secret (no judgement: not a person's reply)."""

    from decurix_agent import state as agent_state
    agent_state.message_facts[path.name] = {"secret": bool(secret), "judgement": ""}


def _text(value, limit):
    return str(value or "").strip()[:limit]


def _host_is_ours(host):
    """Whether a Host header names this desktop the way its callers do: an IP address, or a name with no dot (a
    container name, localhost). A DNS name is how a rebinding web page reaches this port."""

    host = (host or "").strip()
    if not host:
        return True  # HTTP/1.0 without a Host: not something a browser sends
    if host.startswith("["):
        name = host[1:].split("]", 1)[0]
    else:
        name = host.rsplit(":", 1)[0] if host.count(":") == 1 else host
    try:
        ipaddress.ip_address(name)
        return True
    except ValueError:
        return "." not in name and name != ""


def refusal(method, parts, headers, token=None):
    """
    Why a request to this API may not be served - (status, reason) - or None when it may.

    `headers` is the request's headers (anything with .get); `token` defaults to this desktop's own
    (config.agent_token()), "" when it was created without one.
    """

    token = config.agent_token() if token is None else token

    # A web page: its fetch, form or beacon always names its origin on a write, and Chromium marks every request
    # it makes with Sec-Fetch-*. Nothing that legitimately calls this API is a browser.
    if headers.get("Origin") is not None or any(headers.get(h) is not None for h in
                                                ("Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest")):
        return 403, "this API is not for web pages"
    if not _host_is_ours(headers.get("Host")):
        return 403, "this API is reached by address, not by a DNS name"

    if method != "GET":
        ctype = (headers.get("Content-Type") or "").split(";", 1)[0].strip().lower()
        try:
            length = int(headers.get("Content-Length") or "0")
        except ValueError:
            length = -1
        if length < 0:
            return 400, "invalid content length"
        # The control plane sends every write as JSON, a body or not (desktop.Manager.AgentRequest); a page's simple
        # POST is text/plain or a form.
        if ctype != "application/json":
            return 415, "the body must be JSON (Content-Type: application/json)"

    if list(parts) != HEALTH and not token:
        # Fails closed (review 2026-11-02): a desktop made without a token would take requests and a person's
        # replies from anything on the network.
        return 401, "this desktop was created without an agent token: recreate it"
    if list(parts) != HEALTH:
        given = headers.get("Authorization") or ""
        scheme, _, value = given.partition(" ")
        if scheme.lower() != "bearer" or not hmac.compare_digest(value.strip().encode("utf-8"), token.encode("utf-8")):
            return 401, "this desktop's agent token is required"

    return None


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

        if length < 0:
            raise ValueError("invalid content length")

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

    def _refused(self, parts):
        """Answer a request refusal() turns away, and say whether it did. Nothing in it is read first."""

        refused = refusal(self.command, parts, self.headers)
        if refused is None:
            return False
        status, reason = refused
        log(f"HTTP {self.command} {urllib.parse.urlsplit(self.path).path} refused ({status}): {reason}")
        self.close_connection = True  # its body, if any, is never read
        self._error(status, reason)
        return True

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

        if self._refused(parts):
            return

        def route():
            if parts == HEALTH:
                return self._json(200, {"status": "ok"})

            if parts == ["workflows"]:
                from decurix_agent.workflow_api import listing
                return self._json(200, listing())

            if parts == ["recordings"]:
                return self._json(200, {"recordings": recording.sessions(), "demonstration": demonstration.status()})

            if parts == ["demonstration"]:
                return self._json(200, {"demonstration": demonstration.status()})

            if parts == ["ticket"]:
                return self._json(200, {"ticket": record_task.status()})

            if len(parts) == 2 and parts[0] == "recordings":
                meta, events = recording.load(parts[1])
                program = replay.compile_program(meta, events)
                return self._json(200, {
                    "meta": meta,
                    "events": events,
                    "program": program,
                    "lint": lint.lint(program),
                    "demonstration": demonstration.status(),
                })

            if parts == ["skills"]:
                return self._json(200, {"skills": [skills.summary(s) for s in skills.all_skills()]})

            # What this desktop worked out for itself and kept (playbooks.py).
            if parts == ["playbooks"]:
                return self._json(200, {"playbooks": playbooks.listing()})

            if len(parts) == 2 and parts[0] == "playbooks":
                found = playbooks.detail(parts[1]) if playbooks.NAME.match(parts[1]) else None
                if found is None:
                    raise FileNotFoundError(parts[1])
                return self._json(200, {"playbook": found})

            if len(parts) == 3 and parts[0] == "playbooks" and parts[2] == "qualification-source":
                return self._json(200, playbooks.qualification_source(parts[1]))

            if parts == ["practice-sources"]:
                return self._json(200, practice.sources())
            if len(parts) == 3 and parts[0] == "skills" and parts[2] == "practice-source":
                return self._json(200, practice.skill_source(parts[1]))
            if len(parts) == 3 and parts[0] == "recordings" and parts[2] == "practice-source":
                return self._json(200, practice.recording_source(parts[1]))
            if len(parts) == 3 and parts[0] == "skills" and parts[2] == "bundle":
                return self._json(200, skill_bundle.export(parts[1]))
            if len(parts) == 2 and parts[0] == "skills":
                return self._json(200, {"skill": skills.summary(skills.load(parts[1]), detail=True)})

            if len(parts) == 3 and parts[0] == "skills" and parts[2] == "skill.md":
                return self._skill_md(parts[1])

            if len(parts) == 3 and parts[0] == "recordings" and parts[2] == "workflow":
                meta, events = recording.load(parts[1])
                program = replay.compile_program(meta, events)
                return self._json(200, {"workflow": workflow.build(program), "text": workflow.render(program)})

            if len(parts) == 4 and parts[0] == "recordings" and parts[2] == "frames":
                return self._frame(parts[1], parts[3])

            if len(parts) == 3 and parts[0] == "recordings" and parts[2] == "export":
                return self._export(parts[1])

            if parts == ["mode"]:
                return self._json(200, modes.stage_summary())

            if parts == ["catalog"]:
                return self._json(200, {"catalog": catalog.get_catalog()})

            if len(parts) == 2 and parts[0] == "catalog":
                item = catalog.get_catalog_skill(parts[1])
                if not item:
                    raise FileNotFoundError(f"no catalog skill {parts[1]}")
                return self._json(200, {"skill": item})

            return False

        self._guarded(route)

    def _skill_md(self, name):
        # The skill as a SKILL.md (skillmd.py): what any agent that reads skills can be handed.
        body = skillmd.render(skills.load(name)).encode("utf-8")

        self.send_response(200)
        self.send_header("Content-Type", "text/markdown; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Content-Disposition", f'attachment; filename="{name}-SKILL.md"')
        self.end_headers()
        self.wfile.write(body)

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

        if self._refused(parts):
            return

        if parts == ["request"]:
            return self._request()

        def route():
            data = self._body()

            if parts == ["skills", "install"]:
                return self._json(200, skill_bundle.install(data.get("bundle"), version=data.get("version"),
                                                            sha=data.get("sha"), replace=bool(data.get("replace"))))

            if len(parts) == 2 and parts[0] == "demonstration":
                return self._demonstration(parts[1], data)

            if parts == ["recordings", "import"]:
                return self._import(data)

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

                if action == "learn":
                    return self._learn(session_id, data)

            if len(parts) == 3 and parts[0] == "playbooks" and parts[2] == "delete":
                if not playbooks.NAME.match(parts[1]) or not playbooks.forget(parts[1]):
                    raise FileNotFoundError(parts[1])
                return self._json(200, {"status": "deleted"})

            if len(parts) == 3 and parts[0] == "skills":
                if parts[2] == "delete":
                    skills.load(parts[1])
                    skills.delete(parts[1])
                    return self._json(200, {"status": "deleted"})

                if parts[2] == "run":
                    return self._run_skill(parts[1], data)

            if parts == ["mode"]:
                stage = data.get("stage")
                if not stage:
                    raise ValueError("stage is required")
                modes.set_stage(stage)
                return self._json(200, modes.stage_summary())

            if parts == ["catalog", "install"]:
                name = data.get("name")
                if not name:
                    raise ValueError("name is required")
                target_name = data.get("target_name")
                installed = catalog.install_catalog_skill(name, target_name)
                return self._json(200, {"status": "ok", "skill": installed})

            return False

        self._guarded(route)

    def _import(self, data):
        payload = data.get("data")

        if isinstance(payload, str):
            try:
                payload = json.loads(payload)
            except ValueError:
                raise ValueError("that is not a JSON recording")

        # A Chrome Recorder export and a BrowserGym trajectory are objects; an OpenHands
        # trajectory is a list of events.
        if not isinstance(payload, (dict, list)):
            raise ValueError("data must be the recording's JSON")

        try:
            result = ingest.import_recording(payload, title=_text(data.get("title"), 200))
        except ingest.IngestError as exc:
            # Words written for a person: what to export, or what was in the way.
            raise ValueError(str(exc))

        return self._json(200, dict(result, status="ok"))

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
        elif action == "attach":
            given = data.get("files")
            record_task.attach([str(p)[:1000] for p in given[:20]] if isinstance(given, list) else [])
        elif action == "end":
            record_task.end()
        else:
            return False

        return self._json(200, {"status": "ok", "ticket": record_task.status()})

    def _replay(self, session_id, data):
        meta, events = recording.load(session_id)

        params = data.get("params") or {}
        if not isinstance(params, dict):
            raise ValueError("params must be an object of name: value")

        start = int(data.get("from") or 1)
        task_id = _text(data.get("task_id"), 64)
        _keep_execution(task_id, data)
        content = f"/replay {session_id}" + (f" from={start}" if start > 1 else "")

        # The request the watcher reads names only the recording: the values
        # stay in this process's memory until the replay takes them. The ones its program marks secret are the task's
        # secret values first (review round 5, R4-B2).
        secret = _note_secret_params(task_id, replay.compile_program(meta, events).get("params"), params)
        replay_task.hold_params(task_id, params)
        path = save_request(content, task_id, secret=secret)
        _saved_secret(path, secret)

        return self._json(200, {"status": "received", "file": path.name, "content": content})

    def _learn(self, session_id, data):
        recording.load(session_id)

        also = data.get("sessions") or []
        if not isinstance(also, list) or not all(recording.valid(str(s)) for s in also):
            raise ValueError("sessions must be a list of recording ids")

        sessions = [session_id] + [str(s) for s in also if str(s) != session_id]
        for other in sessions[1:]:
            recording.load(other)

        content = skill_task.learn_request(sessions, _text(data.get("name"), 80))
        task_id = _text(data.get("task_id"), 64)
        _keep_execution(task_id, data)
        path = save_request(content, task_id)

        return self._json(200, {"status": "received", "file": path.name, "content": content})

    def _run_skill(self, name, data):
        skill = skills.load(name)

        if skill.get("status") != skills.READY:
            raise ComputerError("that skill was never finished being taught")

        params = data.get("params") or {}
        if not isinstance(params, dict):
            raise ValueError("params must be an object of name: value")

        content = skill_task.request_text(name, params)
        task_id = _text(data.get("task_id"), 64)
        _keep_execution(task_id, data)
        # The parameters the skill marks secret are the task's secret values before the request is saved, logged or
        # kept (review round 5, R4-B2).
        secret = _note_secret_params(task_id, skill.get("params"), params)
        path = save_request(content, task_id, secret=secret)
        _saved_secret(path, secret)

        return self._json(200, {"status": "received", "file": path.name, "content": content})

    def _request(self):
        """
        A person's request or reply. A reply is taken as one only when it names
        the task whose question is open (messaging.save_request): the control
        plane sends a reply with that task's ID, after checking the note it
        answers is this desktop's newest open one.
        """

        try:
            data = self._body()

            content = data.get("content", "")
            if not isinstance(content, str) or not content:
                raise ValueError("missing content")

            task_id = data.get("task_id", "")
            if not isinstance(task_id, str):
                raise ValueError("task_id must be a string")

            # A message the person marked secret (F-2): remembered before anything keeps or shows it, so every later
            # line, record and recording withholds it (privacy.remember_marked).
            from decurix_agent import task_context
            from decurix_agent.computer import privacy
            if data.get("secret", False) is True:
                privacy.remember_marked(content, task_id if task_context.valid(task_id) else "")
                task_context.note_secret_values(task_id, privacy.marked_values(content))
            # A secret the words spell out ("password: ...") is one too, marked or not (C14): soul is told of it.
            task_context.note_secret_values(task_id, privacy.request_secrets(content))

            # What the control plane says beside the words (C6): the org's date order, and the judgement this message
            # is when it is a person's reply. Kept by task before the request is saved, so the watcher and every
            # later process read them; a value of another shape is ignored, never an error.
            if task_context.valid(task_id):
                task_context.set_date_order(task_id, data.get("date_order"))
                task_context.set_reply_judgement(task_id, data.get("judgement_id"))
                task_context.set_execution(task_id, data.get("execution_id"))

            # The control plane's record for this request; it comes back
            # on every report about it (task_context.py). What it said about this one message - secret or not, and the
            # judgement it is - is kept by the message's own file, so the watcher reads it for exactly this message
            # (review round 2: one secret fact; a lesson cites its own reply's judgement).
            secret = data.get("secret", False) is True
            path = save_request(content, task_id, secret=secret)
            from decurix_agent import state as agent_state
            judgement = data.get("judgement_id")
            agent_state.message_facts[path.name] = {
                "secret": secret,
                "judgement": judgement if isinstance(judgement, str) and task_context.valid_judgement(judgement) else ""}

            self._json(200, {"status": "received", "file": path.name})

        except Exception as exc:
            log(f"POST /request failed: {exc}")
            self._error(400, str(exc))
