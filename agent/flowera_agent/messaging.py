import json
import time
import urllib.error
import urllib.request
import uuid

from flowera_agent import config, state
from flowera_agent.logging_utils import log, timestamp


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

    path = config.COMMAND_DIR / filename

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

def send_helptext(filename, content, kind="", message=""):
    """
    Send a help request to the Flowera control plane.

    The existing Go control plane receives:
        POST /api/helptext

    with:
        desktop_id
        vm_name
        filename
        content
        kind      - "help" or "final"
        message   - just the agent's own words, no headings

    `content` is the whole formatted note, which the dashboard's Help rail
    still shows verbatim. `kind`/`message` are what the chat renders: the
    control plane has no business re-parsing the note's headings to find the
    one sentence a human wants to read, so the agent says which is which.
    Both are optional - an older control plane ignores them, and a newer one
    falls back to parsing `content` when they are absent.
    """

    payload = {
        "desktop_id": config.DESKTOP_ID,
        "vm_name": config.DESKTOP_NAME,
        "filename": filename,
        "content": content,
        "kind": kind,
        "message": message,
    }

    body = json.dumps(payload).encode("utf-8")

    url = config.CONTROL_PLANE_URL.rstrip("/") + "/api/helptext"

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
        log(f"Failed to send help request: {exc}")
        return False


def ask_for_help(problem):
    """
    Ask the human/control plane for help.
    """

    help_id = uuid.uuid4().hex

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    content = f"""
FLOWERA HELP REQUEST

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{state.current_task or "unknown"}

Problem:
{problem}

Help ID:
{help_id}

The agent is currently blocked.

Please send instructions through the Flowera control plane.
""".strip()

    path = config.HELPTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    state.last_help_id = help_id

    log("========================================")
    log("AGENT NEEDS HELP")
    log(content)
    log(f"Help request written to: {path}")
    log("========================================")

    sent = send_helptext(
        filename,
        content,
        kind="help",
        message=problem,
    )

    if sent:
        log("Help request sent to control plane.")
    else:
        log("WARNING: could not contact control plane.")

    state.waiting_for_help = True

    return help_id

# ============================================================
# Final task message
# ============================================================

def write_final_message(message):
    """
    Write the final result into helptext/ so the existing
    helptext watcher sends it to the Flowera control plane.
    """

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    final_message = f"""
FLOWERA TASK COMPLETED

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{state.current_task or "unknown"}

Status:
COMPLETED

Final message:
{message}

Completed at:
{timestamp()}
""".strip()

    path = config.HELPTEXT_DIR / filename

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
        kind="final",
        message=message,
    )

    if sent:
        log("Final message sent to control plane.")
    else:
        log("WARNING: could not send final message to control plane.")

    state.current_task = None
    state.current_task_file = None
    state.waiting_for_help = False
