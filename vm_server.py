from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
import threading
import time
import urllib.request
import urllib.error
import socket
import uuid
from pathlib import Path


# ============================================================
# Configuration
# ============================================================

DESKTOP_ID = os.environ.get("DESKTOP_ID", "")

DESKTOP_NAME = os.environ.get(
    "DESKTOP_NAME",
    socket.gethostname()
)

CONTROL_PLANE_URL = os.environ.get(
    "CONTROL_PLANE_URL",
    "http://host.docker.internal:8080"
)

# The Flowera engine (atomizer + scheduler + skills) runs inside this same
# container, started by start_agent_terminal.sh before this script's
# terminal opens - see /opt/flowera-engine. This replaces the old direct
# Ollama COMMAND/DONE/HELP loop that used to live in this file: the engine
# owns talking to a model now (it reaches the real Ollama server via its
# own OLLAMA_HOST env var, set on the container by the control plane), this
# script's job is only to submit tasks to it and relay its progress back.
ENGINE_URL = os.environ.get(
    "ENGINE_URL",
    "http://127.0.0.1:4478"
)

COMMAND_DIR = Path("/home/headless/command")
HELPTEXT_DIR = Path("/home/headless/helptext")
FINAL_DIR = Path("/home/headless/final")
WORK_DIR = Path("/home/headless/work")

COMMAND_DIR.mkdir(parents=True, exist_ok=True)
HELPTEXT_DIR.mkdir(parents=True, exist_ok=True)
FINAL_DIR.mkdir(parents=True, exist_ok=True)
WORK_DIR.mkdir(parents=True, exist_ok=True)


# ============================================================
# Agent state
# ============================================================

state_lock = threading.Lock()

current_task = None
current_task_file = None
waiting_for_help = False
last_help_id = None

processed_commands = set()


# ============================================================
# Utility
# ============================================================

def timestamp():
    return time.strftime("%Y-%m-%d %H:%M:%S")


def log(message):
    print(
        f"[{timestamp()}] {message}",
        flush=True
    )


def safe_filename(name):
    return "".join(
        c if c.isalnum() or c in "-_." else "_"
        for c in name
    )


# ============================================================
# Existing message functionality
# ============================================================

def save_message(message):
    """
    Save an incoming message/task into command/.
    """

    filename = (
        time.strftime("%Y%m%d_%H%M%S")
        + "_"
        + uuid.uuid4().hex[:8]
        + ".txt"
    )

    path = COMMAND_DIR / filename

    path.write_text(
        message,
        encoding="utf-8"
    )

    log(f"Received message: {message}")
    log(f"Saved to {path}")

    return path


# ============================================================
# Control plane communication
# ============================================================

def send_helptext(filename, content):
    """
    Send a message to the Flowera control plane.

    The existing Go control plane receives:
        POST /api/helptext

    with:
        desktop_id
        vm_name
        filename
        content

    Despite the name (kept for compatibility with the existing control
    plane route and /dashboard view), this is the general message-relay
    path used for the atomized plan, the final result, AND genuine help
    requests - the control plane just displays whatever content arrives.
    """

    payload = {
        "desktop_id": DESKTOP_ID,
        "vm_name": DESKTOP_NAME,
        "filename": filename,
        "content": content,
    }

    body = json.dumps(payload).encode("utf-8")

    url = CONTROL_PLANE_URL.rstrip("/") + "/api/helptext"

    request = urllib.request.Request(
        url,
        data=body,
        headers={
            "Content-Type": "application/json",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(
            request,
            timeout=10,
        ) as response:

            if response.status < 200 or response.status >= 300:
                raise RuntimeError(
                    f"control plane returned HTTP {response.status}"
                )

            return True

    except Exception as exc:
        log(f"Failed to send message to control plane: {exc}")
        return False


def ask_for_help(problem):
    """
    Report a problem to the human via the control plane. Unlike the old
    COMMAND/DONE/HELP loop this replaced, this is fire-and-report, not a
    blocking wait for a human reply - the engine has no notion of a
    mid-task human handoff, so perform_task always clears current_task
    afterward and the desktop is ready for the next task immediately.
    """

    global waiting_for_help
    global last_help_id

    help_id = uuid.uuid4().hex

    filename = time.strftime("%d%m%Y%H%M%S") + "_help.txt"

    content = f"""
FLOWERA TASK PROBLEM

Desktop:
{DESKTOP_NAME}

Desktop ID:
{DESKTOP_ID}

Task:
{current_task or "unknown"}

Problem:
{problem}

Help ID:
{help_id}
""".strip()

    path = HELPTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    last_help_id = help_id

    log("========================================")
    log("TASK PROBLEM")
    log(content)
    log(f"Written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        content,
    )

    if sent:
        log("Problem report sent to control plane.")
    else:
        log("WARNING: could not contact control plane.")

    waiting_for_help = True

    return help_id


# ============================================================
# Atomized plan + final task messages
# ============================================================

def send_plan_message(task, plan):
    """
    Sent once the engine's atomizer finishes decomposing the task - the
    "Atomised tasks when atomiser completes" message.
    """

    filename = time.strftime("%d%m%Y%H%M%S") + "_plan.txt"

    if plan:
        plan_lines = "\n".join(
            f"{i + 1}. [{step.get('type', '?')}] {step.get('step', '')}"
            for i, step in enumerate(plan)
        )
    else:
        plan_lines = "(single atomic step - no further breakdown)"

    content = f"""
FLOWERA ATOMIZED PLAN

Desktop:
{DESKTOP_NAME}

Desktop ID:
{DESKTOP_ID}

Task:
{task}

Plan:
{plan_lines}

Planned at:
{timestamp()}
""".strip()

    path = HELPTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    log("========================================")
    log("ATOMIZED PLAN")
    log(content)
    log(f"Written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        content,
    )

    if sent:
        log("Atomized plan sent to control plane.")
    else:
        log("WARNING: could not send atomized plan to control plane.")


def write_final_message(message):
    """
    Send the "actual end result when the job completes" message - the
    engine's real composed final answer, not a locally-generated summary.
    """

    global current_task
    global current_task_file
    global waiting_for_help

    filename = time.strftime("%d%m%Y%H%M%S") + "_final.txt"

    final_message = f"""
FLOWERA TASK COMPLETED

Desktop:
{DESKTOP_NAME}

Desktop ID:
{DESKTOP_ID}

Task:
{current_task or "unknown"}

Status:
COMPLETED

Final message:
{message}

Completed at:
{timestamp()}
""".strip()

    path = HELPTEXT_DIR / filename

    path.write_text(
        final_message,
        encoding="utf-8"
    )

    log("========================================")
    log("TASK COMPLETED")
    log(final_message)
    log(f"Final message written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        final_message,
    )

    if sent:
        log("Final message sent to control plane.")
    else:
        log("WARNING: could not send final message to control plane.")

    current_task = None
    current_task_file = None
    waiting_for_help = False


# ============================================================
# Flowera engine
# ============================================================

def wait_for_engine():
    """
    Wait until the Flowera engine (node server.js, started by
    start_agent_terminal.sh) is up and can reach Ollama.
    """

    url = ENGINE_URL.rstrip("/") + "/api/health"

    while True:

        try:
            request = urllib.request.Request(
                url,
                method="GET",
            )

            with urllib.request.urlopen(
                request,
                timeout=5,
            ) as response:

                if response.status == 200:
                    log(
                        f"Flowera engine available: "
                        f"{ENGINE_URL}"
                    )
                    return

        except Exception:
            pass

        log("Waiting for Flowera engine...")

        time.sleep(2)


def submit_to_engine(task):
    """
    POST /api/chat - starts an engine job, returns its id.
    """

    payload = json.dumps({"prompt": task}).encode("utf-8")

    request = urllib.request.Request(
        ENGINE_URL.rstrip("/") + "/api/chat",
        data=payload,
        headers={
            "Content-Type": "application/json",
        },
        method="POST",
    )

    with urllib.request.urlopen(
        request,
        timeout=15,
    ) as response:

        result = json.loads(
            response.read().decode("utf-8")
        )

    if "id" not in result:
        raise RuntimeError(
            f"engine did not return a job id: {result}"
        )

    return result["id"]


def stream_engine_job(job_id):
    """
    Reads the engine's SSE stream (GET /api/jobs/:id/stream) line by line
    and yields each event as a dict. A "backlog" event (sent on connect,
    or on a reconnect mid-job) wraps a full list of prior events plus the
    job's current status - unwrapped here so the caller only ever deals
    with plain event dicts, live or replayed.
    """

    url = ENGINE_URL.rstrip("/") + f"/api/jobs/{job_id}/stream"

    request = urllib.request.Request(
        url,
        method="GET",
    )

    with urllib.request.urlopen(
        request,
        timeout=600,
    ) as response:

        for raw_line in response:

            line = raw_line.decode(
                "utf-8",
                errors="replace",
            ).rstrip("\n")

            if not line.startswith("data: "):
                continue

            payload = line[len("data: "):]

            try:
                event = json.loads(payload)
            except json.JSONDecodeError:
                continue

            if event.get("type") == "backlog":

                for sub_event in event.get("events", []):
                    yield sub_event

                if event.get("status") != "running":
                    yield {
                        "type": "status",
                        "status": event.get("status"),
                    }

                continue

            yield event


def _handle_engine_event(task, event):
    """
    Applies one engine event. Returns True if this was a terminal event
    (the caller should stop reading the stream).
    """

    event_type = event.get("type")

    if event_type == "plan":
        send_plan_message(task, event.get("plan", []))
        return False

    if event_type == "final":
        write_final_message(
            event.get("finalAnswer") or "(engine returned no final answer)"
        )
        return True

    if event_type == "status" and event.get("status") == "error":
        ask_for_help(
            "The Flowera engine reported an error while running this "
            "task - check the engine's own log (/tmp/flowera-engine.log) "
            "for detail."
        )
        return True

    return False


# ============================================================
# Task execution
# ============================================================

def perform_task(task, task_file):
    global current_task
    global current_task_file
    global waiting_for_help

    current_task = task
    current_task_file = task_file
    waiting_for_help = False

    log("========================================")
    log("STARTING TASK (Flowera engine)")
    log(task)
    log("========================================")

    try:
        job_id = submit_to_engine(task)
        log(f"Engine job id: {job_id}")

        finished = False

        for event in stream_engine_job(job_id):
            if _handle_engine_event(task, event):
                finished = True
                break

        if not finished:
            ask_for_help(
                "The Flowera engine's event stream ended without ever "
                "reporting a final result."
            )

    except Exception as exc:
        ask_for_help(f"Could not run this task via the Flowera engine: {exc}")

    finally:
        # No mid-task human handoff exists for engine-run tasks (unlike
        # the old COMMAND/DONE/HELP loop's blocking HELP) - always leave
        # the desktop ready to accept the next task rather than getting
        # stuck waiting for a reply nothing will ever send.
        current_task = None
        current_task_file = None


# ============================================================
# Command watcher
# ============================================================

def command_watcher():
    """
    Continuously watch command/ for new task files.
    """

    global processed_commands

    log(
        f"Watching command directory: "
        f"{COMMAND_DIR}"
    )

    while True:

        try:

            files = sorted(
                COMMAND_DIR.glob("*.txt"),
                key=lambda p: p.stat().st_mtime,
            )

            for path in files:

                if path.name in processed_commands:
                    continue

                # Ignore files generated by the agent.
                if path.name.startswith("result_"):
                    processed_commands.add(
                        path.name
                    )
                    continue

                try:
                    content = path.read_text(
                        encoding="utf-8"
                    ).strip()

                except Exception as exc:
                    log(
                        f"Could not read {path}: "
                        f"{exc}"
                    )
                    continue

                if not content:
                    processed_commands.add(
                        path.name
                    )
                    continue

                processed_commands.add(
                    path.name
                )

                # Do not start another task while one
                # is already running.
                if current_task is not None:

                    log(
                        "Task already running. "
                        f"Leaving {path.name} "
                        "for later."
                    )

                    processed_commands.discard(
                        path.name
                    )

                    continue

                thread = threading.Thread(
                    target=perform_task,
                    args=(
                        content,
                        path,
                    ),
                    daemon=True,
                )

                thread.start()

        except Exception as exc:
            log(
                f"Command watcher error: {exc}"
            )

        time.sleep(1)


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


# ============================================================
# Help text watcher (defunct, useful to have)
# ============================================================

def helptext_watcher():
    """
    Existing functionality:
    watches helptext/ and sends newly-created or
    modified .txt files to the control plane.

    This remains useful for agent-generated help requests.
    """

    log(
        f"Watching helptext directory: "
        f"{HELPTEXT_DIR}"
    )

    seen = {}

    while True:

        try:

            for path in HELPTEXT_DIR.glob("*.txt"):

                try:
                    stat = path.stat()

                except FileNotFoundError:
                    continue

                signature = (
                    stat.st_mtime_ns,
                    stat.st_size,
                )

                if seen.get(path.name) == signature:
                    continue

                seen[path.name] = signature

                try:
                    content = path.read_text(
                        encoding="utf-8"
                    )

                except Exception as exc:
                    log(
                        f"Could not read help file "
                        f"{path}: {exc}"
                    )
                    continue

                send_helptext(
                    path.name,
                    content,
                )

        except Exception as exc:
            log(
                f"Helptext watcher error: "
                f"{exc}"
            )

        time.sleep(1)


# ============================================================
# Startup
# ============================================================

def main():

    log("========================================")
    log("Flowera VM Agent")
    log("========================================")

    log(
        f"Desktop ID: {DESKTOP_ID}"
    )

    log(
        f"Desktop name: {DESKTOP_NAME}"
    )

    log(
        f"Control plane: "
        f"{CONTROL_PLANE_URL}"
    )

    log(
        f"Engine: "
        f"{ENGINE_URL}"
    )

    # Wait until the Flowera engine (and, transitively, Ollama) is alive.
    wait_for_engine()

    # Start command watcher.
    threading.Thread(
        target=command_watcher,
        daemon=True,
    ).start()

    # helptext_watcher is intentionally NOT started here (kept defunct, as
    # in the original file): send_plan_message / write_final_message /
    # ask_for_help already call send_helptext() inline at write time.
    # Starting the watcher too would have it notice the same files a
    # second time and send every message twice.

    server = HTTPServer(
        ("0.0.0.0", 8081),
        Handler,
    )

    log(
        "VM agent listening on "
        "0.0.0.0:8081"
    )

    server.serve_forever()


if __name__ == "__main__":
    main()
