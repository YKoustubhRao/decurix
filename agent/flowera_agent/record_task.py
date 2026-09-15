import re
import threading

from flowera_agent import progress, state, usage
from flowera_agent.computer import demonstration, recording
from flowera_agent.computer.errors import ComputerError
from flowera_agent.logging_utils import log
from flowera_agent.messaging import write_final_reply


# ============================================================
# A ticket: a person doing a task, recorded
#
# "/record <task>" - sent by the GUI screen's toolbar, or typed into the chat
# - opens a ticket for a human to do that task on this desktop's screen. The
# engine never sees it: the command watcher dispatches it here instead of the
# harness, the way it dispatches "/replay" to replay_task.py. So it queues
# behind a running task, carries its task ID, and while it is open every
# other request waits behind it, just as it would behind an agent's task.
#
# Opening a ticket does not itself start capturing - the toolbar's Record
# does that (begin()), so a person can open a ticket, walk over to the
# screen, and start only when ready. From there the toolbar's Stop/Continue
# are demonstration.py's pause/resume, and End (end()) stops capturing (if
# it had started) and finishes the ticket's task - reporting as engine
# "human", with no model and no tokens - which is what makes the recording's
# export downloadable.
#
# Exactly one ticket is open at a time: tasks.Store.Busy refuses a second
# "/record" (and a "/replay") while one is, the same way it refuses a second
# task of any other kind. That is what makes this module's ticket state
# (_ticket) safe to keep as module-private state, the way replay_task.py
# keeps its own held parameters - not in state.py, since nothing outside this
# module reads it directly.
#
# This is not a harness: harness.py still decides which engine answers
# everything else, and nothing here asks which one that is.
# ============================================================

ENGINE = "human"

COMMAND_RE = re.compile(r"^/record(?:\s+(.*))?$", re.DOTALL)

# How often perform()'s wait loop notices a recording that ended by itself -
# the recorder crashed, or something outside this API stopped it (the
# recordings page's own Stop is a plain demonstration.stop() with no idea a
# ticket exists) - and treats that as the ticket ending too.
POLL_SECONDS = 1

_ticket = None
_ticket_lock = threading.Lock()
_ended = threading.Event()


def parse(content):
    """The task a "/record" request names ("" when it names none), or None for any other request."""

    match = COMMAND_RE.match((content or "").strip())
    if not match:
        return None

    return (match.group(1) or "").strip()


def status():
    """
    The ticket open on this desktop, for the GUI toolbar: {"task_id",
    "title", "session_id", "recording", "paused", "events"} - or None when
    there is none. session_id is None until begin() has been called.
    """

    with _ticket_lock:
        ticket = dict(_ticket) if _ticket else None

    if ticket is None:
        return None

    demo = demonstration.status() if ticket.get("session_id") else None

    ticket["recording"] = bool(demo) and not demo.get("paused")
    ticket["paused"] = bool(demo) and bool(demo.get("paused"))
    ticket["events"] = (demo or {}).get("events", 0)

    return ticket


def begin():
    """Start capturing for the open ticket. Raises ComputerError if there is none, or it is already recording."""

    with _ticket_lock:
        ticket = _ticket
        if ticket is None:
            raise ComputerError("no ticket is open - create one first")
        if ticket.get("session_id"):
            raise ComputerError("already recording")

    meta = demonstration.start(title=ticket["title"], task_id=ticket["task_id"])

    with _ticket_lock:
        # Still the same ticket - end() has not raced ahead of us and closed it.
        if _ticket is ticket:
            ticket["session_id"] = meta["id"]

    return status()


def pause(reason=""):
    """Pause capturing, for a private step such as signing in."""

    _require_recording()
    demonstration.pause(reason)
    return status()


def resume():
    """Resume capturing after pause()."""

    _require_recording()
    demonstration.resume()
    return status()


def note(text):
    """
    Add the open ticket's own written explanation of what is happening right
    now - a "reason", sitting at this moment in its recording alongside
    whatever action the person takes around it. Returns the recorded event.
    """

    _require_recording()
    return demonstration.note(text)


def _require_recording():
    with _ticket_lock:
        ticket = _ticket

    if not ticket or not ticket.get("session_id"):
        raise ComputerError("nothing is being recorded")


def end():
    """
    Finish the open ticket: stop capturing if it had started, and end its
    task. Safe to call on a ticket that never started recording - it is then
    dropped with nothing made.
    """

    with _ticket_lock:
        ticket = _ticket
        if ticket is None:
            raise ComputerError("no ticket is open")
        recording_now = bool(ticket.get("session_id"))

    if recording_now:
        try:
            demonstration.stop()
        except ComputerError:
            pass  # already stopped - e.g. the recorder had already finished on its own

    _ended.set()

    # perform()'s own thread clears the ticket a moment later, once its wait
    # loop wakes; the caller does not wait for that.
    return status()


def perform(content, task_file=None):
    """Open a ticket for a "/record" request's task, until end() finishes it."""

    global _ticket

    state.current_task = content
    state.current_task_file = task_file
    state.waiting_for_query = False

    # Known, not unknown: nobody read a prompt and nothing was spent.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)

    title = parse(content) or "Untitled demonstration"
    task_id = state.current_task_id or ""

    with _ticket_lock:
        _ticket = {"task_id": task_id, "title": title, "session_id": None}
    _ended.clear()

    log(f"Ticket opened for task {task_id or '(none)'}: {title}")
    progress.emit(
        f"Ticket open for \"{title}\". Press Record on the desktop's screen when you're ready to start capturing, "
        "and End when you're done."
    )

    _wait_for_end()

    with _ticket_lock:
        ticket, _ticket = _ticket, None

    _report(ticket)


def _wait_for_end():
    while not _ended.wait(timeout=POLL_SECONDS):
        with _ticket_lock:
            ticket = _ticket

        # A recording that ended by itself - the recorder crashed, or
        # something outside this API stopped it - is still an end: nobody
        # will ever call end() for it.
        if ticket and ticket.get("session_id") and demonstration.status() is None:
            _ended.set()


def _report(ticket):
    session_id = ticket.get("session_id")
    title = ticket["title"]

    if not session_id:
        write_final_reply(f"Ended the ticket for \"{title}\" - nothing was recorded.", kind="error", engine=ENGINE)
        return

    try:
        meta = recording.meta(session_id) or {}
    except (OSError, ValueError):
        meta = {}

    count = meta.get("event_count", 0)
    steps = "1 step" if count == 1 else f"{count} steps"
    replay = f"Replay it with `/replay {session_id}`, or download it from the desktop's recordings."

    if meta.get("status") != "complete":
        reason = meta.get("reason") or "the recorder stopped before it finished"
        kept = f" The {steps} recorded before that are kept as {session_id}." if count else ""
        write_final_reply(f"The recording of \"{title}\" did not finish: {reason}.{kept}", kind="error", engine=ENGINE)
        return

    if not count:
        write_final_reply(
            f"The recording of \"{title}\" ended with nothing in it - no action on the screen was recorded.",
            kind="error", engine=ENGINE,
        )
        return

    write_final_reply(f"Recorded {steps} of \"{title}\" as {session_id}. {replay}", kind="final", engine=ENGINE)
