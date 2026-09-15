import json
import time
import urllib.error
import urllib.request
import uuid

from flowera_agent import config, progress, state, task_context, usage
from flowera_agent.logging_utils import log, timestamp


# ============================================================
# Existing request functionality
# ============================================================

def save_request(request, task_id=""):
    """
    Save an incoming request/task into command/, with the control plane's
    task ID (if it sent one) in the file name for the watcher to pick up.
    """

    filename = task_context.command_filename(
        time.strftime("%Y%m%d_%H%M%S")
        + "_"
        + uuid.uuid4().hex[:8],
        task_id,
    )

    path = config.COMMAND_DIR / filename

    path.write_text(
        request,
        encoding="utf-8"
    )

    log(f"Received request: {request}")
    log(f"Saved to {path}")

    return path


# ============================================================
# Control plane communication
# ============================================================

def send_query(filename, content, kind="", reply="", engine=None):
    """
    Send a query to the Flowera control plane.

    The existing Go control plane receives:
        POST /api/querytext

    with:
        desktop_id
        vm_name
        filename
        content
        kind      - "query", "approval", "final" or "error"
        reply     - just the agent's own words, no headings
        task_id   - the control plane's task this ends or pauses
        engine, model, input_tokens, output_tokens, cost_usd
                  - what the task records keep about the execution

    `content` is the whole formatted note, which the dashboard's Query rail
    still shows verbatim. `kind`/`reply` are what the chat renders: the
    control plane has no business re-parsing the note's headings to find the
    one sentence a human wants to read, so the agent says which is which.
    Both are optional - an older control plane ignores them, and a newer one
    falls back to parsing `content` when they are absent.
    """

    # The chat closes a turn on this note; let the turn's last progress lines
    # land first.
    progress.flush()

    payload = {
        "desktop_id": config.DESKTOP_ID,
        "vm_name": config.DESKTOP_NAME,
        "filename": filename,
        "content": content,
        "kind": kind,
        "reply": reply,
        "task_id": task_context.current(),
        # Reporting which engine ran is not branching on it: every harness
        # sends the same fields. A replay (replay_task.py) ran no engine at
        # all, and says so.
        "engine": engine or config.AGENT_HARNESS,
    }

    payload.update(usage.snapshot())

    body = json.dumps(payload).encode("utf-8")

    url = config.CONTROL_PLANE_URL.rstrip("/") + "/api/querytext"

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
        log(f"Failed to send query: {exc}")
        return False


def ask_for_query(problem, kind="query", engine=None):
    """
    Ask the human/control plane a query.

    `kind` is "approval" when the agent stopped itself before something
    consequential (a payment, an outgoing message, a gated on-screen
    action) and "query" when it is stuck. Both pause the task the same way;
    the task records count them separately, since one is the agent doing
    its job and the other is the agent failing to.
    """

    query_id = uuid.uuid4().hex

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    content = f"""
FLOWERA QUERY REQUEST

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{state.current_task or "unknown"}

Problem:
{problem}

Query ID:
{query_id}

The agent is currently blocked.

Please send instructions through the Flowera control plane.
""".strip()

    path = config.QUERYTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    state.last_query_id = query_id

    log("========================================")
    log("AGENT HAS A QUERY")
    log(content)
    log(f"Query written to: {path}")
    log("========================================")

    sent = send_query(
        filename,
        content,
        kind=kind,
        reply=problem,
        engine=engine,
    )

    if sent:
        log("Query sent to control plane.")
    else:
        log("WARNING: could not contact control plane.")

    state.waiting_for_query = True

    return query_id

# ============================================================
# Backend failures, in words a human can act on
# ============================================================

def backend_failure_note(what, url, error):
    """
    Turn a raw backend exception into a query worth reading.

    A query note is the one place a human actually looks when a desktop
    stops, and it used to hand them the engine's own words verbatim:
    `ollama: request failed: Post "http://host.docker.internal:11434
    /api/chat": context deadline exceeded`. That sentence says a machine
    was too slow, which is fixable in about ten seconds, but it reads
    like the control plane is broken. Lead with what happened and what to
    do about it; keep the original text underneath for whoever wants it.
    """

    text = str(error)
    lowered = text.lower()

    timed_out = (
        "deadline exceeded" in lowered
        or "timed out" in lowered
        or "timeout" in lowered
    )

    unreachable = (
        "connection refused" in lowered
        or "could not reach" in lowered
        or "no route to host" in lowered
        or "name or service not known" in lowered
        or "connection reset" in lowered
    )

    if timed_out:
        cause = (
            f"{what} at {url} accepted the request but did not finish "
            "answering in time. That is nearly always a machine too slow "
            "for the model it was given rather than a broken setup - and "
            "the first request after a restart also pays to load the "
            "model into memory before it reads a single word."
        )
        fix = (
            "Send the request again now that the model is warm. If it "
            "keeps timing out, point OLLAMA_MODEL at a smaller model in "
            "the control plane's .env, or raise OLLAMA_TIMEOUT_SECONDS."
        )

    elif unreachable:
        cause = f"{what} at {url} could not be reached at all."
        fix = (
            "Check that it is running, and that it listens on more than "
            "loopback (OLLAMA_HOST=0.0.0.0) so a container can reach it. "
            "Then send the request again."
        )

    elif "not found" in lowered and "model" in lowered:
        cause = (
            f"{what} at {url} is running, but does not have the model "
            "this desktop was configured to use."
        )
        fix = (
            "Pull that model on that server, or set OLLAMA_MODEL to one "
            "it already has."
        )

    else:
        cause = f"{what} at {url} failed."
        fix = "Send the request again, or check that server's own logs."

    return f"{cause}\n\n{fix}\n\nTechnical detail:\n{text}"


# ============================================================
# Final task reply
# ============================================================

def write_final_reply(reply, kind="final", engine=None):
    """
    Write the final result into querytext/ so the existing
    querytext watcher sends it to the Flowera control plane.

    `kind` is "final" for a finished task and "error" for one that ran to
    the end without managing it - the chat flags the latter as failed
    rather than presenting it as an ordinary answer. Either way the task is
    over and nothing is waiting on the human.
    """

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    status = "FAILED" if kind == "error" else "COMPLETED"

    final_note = f"""
FLOWERA TASK COMPLETED

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{state.current_task or "unknown"}

Status:
{status}

Final reply:
{reply}

Completed at:
{timestamp()}
""".strip()

    path = config.QUERYTEXT_DIR / filename

    path.write_text(
        final_note,
        encoding="utf-8"
    )

    log("========================================")
    log("TASK COMPLETED")
    log(final_note)
    log(f"Final reply written to: {path}")
    log("========================================")

    sent = send_query(
        filename,
        final_note,
        kind=kind,
        reply=reply,
        engine=engine,
    )

    if sent:
        log("Final reply sent to control plane.")
    else:
        log("WARNING: could not send final reply to control plane.")

    state.current_task = None
    state.current_task_file = None
    state.waiting_for_query = False
