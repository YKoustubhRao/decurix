import re
import threading

from decurix_agent import attachments, progress, state, usage
from decurix_agent.computer import demonstration, recording
from decurix_agent.computer.errors import ComputerError
from decurix_agent.logging_utils import log
from decurix_agent.messaging import write_final_reply


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
    """
    The task a "/record" request names ("" when it names none), or None for any
    other request. The files the ticket was opened with are not part of its
    name (files()).
    """

    match = COMMAND_RE.match(attachments.text(content).strip())
    if not match:
        return None

    return " ".join((match.group(1) or "").split())


def files(content):
    """The documents a "/record" request brought: what the person will work from."""

    return attachments.files(content)


def status():
    """
    The ticket open on this desktop, for the GUI toolbar: {"task_id",
    "title", "session_id", "files", "recording", "paused", "events"} - or
    None when there is none. session_id is None until begin() has been called.
    """

    with _ticket_lock:
        ticket = dict(_ticket, files=list(_ticket.get("files") or [])) if _ticket else None

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
        given = list(ticket.get("files") or [])

    _keep_files(meta["id"], given)

    # The chat (and the desktop's card) say what the ticket is doing at every
    # turn, not only when an action lands: a person watching from elsewhere
    # otherwise sees "Ticket open" and then nothing until the first click.
    progress.emit(f"Recording \"{ticket['title']}\" - every click and keystroke on the screen is being captured.")

    return status()


def pause(reason=""):
    """Pause capturing, for a private step such as signing in."""

    _require_recording()
    demonstration.pause(reason)
    progress.emit("Recording paused - nothing on the screen is kept until Continue is pressed.")
    return status()


def resume():
    """Resume capturing after pause()."""

    _require_recording()
    demonstration.resume()
    progress.emit("Recording again.")
    return status()


def note(text):
    """
    Add the open ticket's own written explanation of what is happening right
    now - a "reason", sitting at this moment in its recording alongside
    whatever action the person takes around it. Returns the recorded event.
    """

    _require_recording()
    return demonstration.note(text)


def attach(paths):
    """
    Add documents to the open ticket (the toolbar's Attach): files the control
    plane has just written into this desktop. They are kept with the ticket's
    recording, so a lesson taught from it reads them beside what was typed.
    """

    paths = [str(p) for p in paths or [] if str(p).strip()]

    with _ticket_lock:
        ticket = _ticket
        if ticket is None:
            raise ComputerError("no ticket is open - create one first")
        ticket["files"] = list(dict.fromkeys((ticket.get("files") or []) + paths))
        session_id, given = ticket.get("session_id"), list(ticket["files"])

    if session_id:
        _keep_files(session_id, given)

    if paths:
        progress.emit(f"Attached to the ticket: {attachments.names(paths)}.")

    return status()


def _keep_files(session_id, paths):
    """Write the ticket's documents into its recording's meta; never fails the ticket."""

    if not paths:
        return

    try:
        recording.update(session_id, files=paths)
    except (OSError, ValueError) as exc:
        log(f"Could not keep the ticket's files with {session_id}: {exc}")


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
        session_id, given = ticket.get("session_id"), list(ticket.get("files") or [])

    if recording_now:
        try:
            demonstration.stop()
        except ComputerError:
            pass  # already stopped - e.g. the recorder had already finished on its own
        # Written again once the recording is whole: in broker mode the recorder writes the recording as the screen's
        # user and stop() hands the whole of it back, replacing the worker's meta.json - and with it the files that
        # begin() and attach() wrote there. Found live (2026-11-08): a ticket opened with a document was taught with
        # no document, so the study never read what the person worked from.
        _keep_files(session_id, given)

    progress.emit("Ending the ticket and saving the recording..." if recording_now
                  else "Ending the ticket - recording was never started.")
    _ended.set()

    # perform()'s own thread clears the ticket a moment later, once its wait
    # loop wakes; the caller does not wait for that.
    return status()


def perform(content, task_file=None):
    """Open a ticket for a "/record" request's task, until end() finishes it."""

    global _ticket

    state.begin(content, task_file)

    # Known, not unknown: nobody read a prompt and nothing was spent.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)

    title = parse(content) or "Untitled demonstration"
    task_id = state.current_task_id or ""

    given = files(content)

    with _ticket_lock:
        _ticket = {"task_id": task_id, "title": title, "session_id": None, "files": given}
    _ended.clear()

    log(f"Ticket opened for task {task_id or '(none)'}: {title}" + (f" with {given}" if given else ""))
    progress.emit(
        f"Ticket open for \"{title}\". Press Record on the desktop's screen when you're ready to start capturing, "
        "and End when you're done."
        + (f" Attached: {attachments.names(given)} - in this desktop's work/inbox folder." if given else "")
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

    # Quiet: the recording is not something to page anyone about. It is in the
    # desktop card's recordings list, where it can be replayed, learned from
    # or downloaded, so no note goes to the Query rail.
    write_final_reply(
        f"Recorded {steps} of \"{title}\" as {session_id}. "
        "Replay it, teach me from it or download it from Recordings on the desktop's card.",
        kind="final", engine=ENGINE, quiet=True,
    )
