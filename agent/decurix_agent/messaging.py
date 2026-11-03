import json
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from decurix_agent import config, progress, state, task_context, usage
from decurix_agent.logging_utils import log, log_banner, timestamp


# ============================================================
# Existing request functionality
# ============================================================

# In the name of a command file saved while the agent was waiting on a
# question, naming the task that asked it: it is that question's answer
# (save_request). The mark outlives the process, so an
# agent that restarted in between can tell an answer to a question it no
# longer has from a new request (watchers.is_reply_file).
REPLY_MARK = "-reply"

# How long a task's outcome is sent again while the control plane cannot be reached (it is restarting).
SEND_RETRY_SECONDS = 180

# What the control plane takes in one note (internal/httpapi/querynotes.go): the note's content in bytes, and the
# whole request body. A message longer than that is never sent as it is: its whole text is saved in the work folder
# (REPORTS_DIR) and the chat gets the first SHORT_TEXT characters, cut where a line or sentence ends, and where the
# rest is. Found live (round 5 P3, 2026-10-20): an 18 KB final report was refused with a 400 and the person never
# heard how the run ended.
CONTENT_LIMIT = 16384
BODY_LIMIT = 32768
SHORT_TEXT = 6000
REPORTS_DIR = "decurix-reports"

# Kinds whose refusal is never the end of it: the person must hear a task's outcome, and a question must be asked.
_MUST_REACH = {"final", "error", "query", "approval"}


def cut_text(text, limit):
    """`text` in at most `limit` characters, cut where a paragraph, a line or a sentence ends - never mid-sentence."""

    text = text or ""
    if len(text) <= limit:
        return text
    head = text[:limit]
    for mark in ("\n\n", "\n", ". "):
        at = head.rfind(mark)
        if at >= limit // 3:
            return head[:at + (1 if mark == ". " else 0)].rstrip()
    at = head.rfind(" ")
    return head[:at if at > 0 else limit].rstrip()


def _fits(payload):
    """Whether the control plane takes this note as it is (CONTENT_LIMIT, BODY_LIMIT), with room to spare."""

    content = (payload.get("content") or "").encode("utf-8")
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    return len(content) <= CONTENT_LIMIT - 256 and len(body) <= BODY_LIMIT - 512


def save_whole(name, text):
    """Keep a message's whole text in the work folder's REPORTS_DIR; its path, or None when it cannot be written."""

    try:
        folder = config.WORK_DIR / REPORTS_DIR
        folder.mkdir(parents=True, exist_ok=True)
        path = folder / (name if name.endswith(".txt") else name + ".txt")
        path.write_text(text, encoding="utf-8")
        return path
    except OSError as exc:
        log(f"Could not keep the whole message: {exc}")
        return None


def _where(saved):
    return (f"\n\n(Too long for the chat: the whole of it is in {saved}.)" if saved
            else "\n\n(Too long for the chat: the rest was left out.)")


def _with_text(payload, text):
    """The payload with `text` as the chat's reply, and in the note in place of the whole reply (or as the note)."""

    new = dict(payload)
    reply, content = payload.get("reply") or "", payload.get("content") or ""
    if reply:
        new["reply"] = text
    new["content"] = content.replace(reply, text) if reply and reply in content else text
    if not _fits(new):
        new["content"] = text
    if not _fits(new):
        new["content"] = cut_text(text, SHORT_TEXT // 2)
    return new


def _shortened(payload, saved=None):
    """A note that fits, its whole text kept in the work folder: (payload, saved path)."""

    whole = payload.get("reply") or payload.get("content") or ""
    saved = saved or save_whole(time.strftime("%Y%m%d-%H%M%S-") + uuid.uuid4().hex[:6], whole)
    return _with_text(payload, cut_text(whole, SHORT_TEXT) + _where(saved)), saved


def _fallback(payload, fallback, saved):
    """The short plain version sent when the control plane refused a task's outcome or a question."""

    whole = payload.get("reply") or payload.get("content") or ""
    if not fallback and len(whole) <= 600:
        return _with_text(payload, whole), saved  # nothing to leave out: the plain text again
    saved = saved or save_whole(time.strftime("%Y%m%d-%H%M%S-") + uuid.uuid4().hex[:6], whole)
    text = (fallback or cut_text(whole, 600)) + _where(saved)
    return _with_text(payload, cut_text(text, SHORT_TEXT)), saved


def save_request(request, task_id=""):
    """
    Save an incoming request/task into command/, with the control plane's
    task ID (if it sent one) in the file name for the watcher to pick up.
    """

    stem = time.strftime("%Y%m%d_%H%M%S") + "_" + uuid.uuid4().hex[:8]
    # A reply must name the question it answers: the task whose question is open. One without a task ID, or with
    # another task's, is not its answer whatever it says - otherwise anything that reached :8081 could answer an
    # approval "yes" (review 2026-11-02). The control plane sends a reply with the waiting task's ID only after
    # checking that note is this desktop's newest open one.
    if state.waiting_for_query and task_id and task_id == state.current_task_id:
        stem += REPLY_MARK

    filename = task_context.command_filename(stem, task_id)

    path = config.COMMAND_DIR / filename

    # The watcher globs *.txt every second. Publishing that name before the
    # write finishes lets it read an empty request and remember the filename
    # as processed forever. Write to a name outside the glob, then publish it
    # in one rename after all bytes are present.
    pending = path.with_name(path.name + ".tmp")
    pending.write_text(request, encoding="utf-8")
    pending.replace(path)

    log(f"Received request: {request}")
    log(f"Saved to {path}")

    return path


# ============================================================
# Control plane communication
# ============================================================

def send_query(filename, content, kind="", reply="", engine=None, quiet=False, interrupted=False, checked=False,
               task_id=None, with_usage=True, fallback=None, still_working=False):
    """
    Send a query to the Decurix control plane.

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
        quiet     - true when the note is only the chat's reply and the task's
                    outcome, and the Query rail should not get an entry
        interrupted - true when this reports a task an earlier life of the
                    agent never finished; the control plane drops it when that
                    task was already closed (a restart it was asked for)

    `content` is the whole formatted note, which the dashboard's Query rail
    still shows verbatim. `kind`/`reply` are what the chat renders: the
    control plane has no business re-parsing the note's headings to find the
    one sentence a human wants to read, so the agent says which is which.
    Both are optional - an older control plane ignores them, and a newer one
    falls back to parsing `content` when they are absent.

    A note too long for the control plane is sent shortened, its whole text kept in the work folder (_shortened). A
    task's outcome or a question the control plane refuses is sent once more as `fallback` (or its first lines),
    plain and short, with where the whole text is: never dropped without a word.
    """

    # The chat closes a turn on this note; let the turn's last progress lines
    # land first.
    progress.flush()

    # A secret the request gave never reaches the chat, the task record or the rail (computer/privacy.py). Fails
    # closed: a text that cannot be checked is held back, never sent as it was (review 2026-11-02).
    content, reply, fallback = _withheld(content), _withheld(reply), _withheld(fallback)
    payload = {
        "desktop_id": config.DESKTOP_ID,
        "vm_name": config.DESKTOP_NAME,
        "filename": filename,
        "content": content,
        "kind": kind,
        "reply": reply,
        # A message a long request took mid-run is answered on its own task (long_task.reply_on_task).
        "task_id": task_id or task_context.current(),
        # Reporting which engine ran is not branching on it: every harness
        # sends the same fields. A replay (replay_task.py) ran no engine at
        # all, and says so.
        "engine": engine or config.AGENT_HARNESS,
    }

    if quiet:
        payload["quiet"] = True
    if interrupted:
        payload["interrupted"] = True
    if still_working:
        # The desktop is still busy after this reply (a planned run's receipt): the chat's turn stays open.
        payload["still_working"] = True
    # Whether the result went through the final check (verify.py, the skill
    # gate): the task records keep it as data, so an audit can tell a checked
    # "done" from one nobody looked at.
    if kind in ("final", "error"):
        payload["checked"] = bool(checked)

    if with_usage:
        payload.update(usage.snapshot())

    saved = None
    if not _fits(payload):
        payload, saved = _shortened(payload)
        log(f"A {kind or 'note'} too long for the control plane was shortened; the whole of it is in {saved}.")
    short_sent = False

    # A control plane that is restarting (an update, a crash it comes back from) is unreachable for seconds: the
    # outcome is sent again until it answers. Found live (2026-10-15): a task's final reply met a restart, was never
    # sent again, and the control plane showed the task running for good. A refused or unreachable connection, or a
    # server error (5xx: a proxy in front of a restarting control plane), is retried - any other answer is the control
    # plane's word. A task's outcome that still could not be delivered is kept and sent again (resend_unsent): found
    # live (2026-10-20), the report of a request stopped a moment before a control-plane restart never reached the
    # chat, which said "running" for good.
    deadline = time.monotonic() + SEND_RETRY_SECONDS
    wait = 2.0
    while True:
        outcome, detail = _post_querytext(payload)
        if outcome == "sent":
            return True
        if outcome == "refused" and kind in _MUST_REACH and not short_sent:
            short_sent = True
            log(f"The control plane refused the {kind} ({detail}); sending a short plain version.")
            payload, saved = _fallback(payload, fallback, saved)
            continue
        if outcome == "refused" or time.monotonic() + wait > deadline:
            log(f"Failed to send query: {detail}")
            if outcome != "refused" and kind in ("final", "error"):
                _keep_unsent(payload)
            return False
        log(f"Could not reach the control plane ({detail}); sending again in {int(wait)} s.")
        time.sleep(wait)
        wait = min(wait * 2, 20.0)


HELD_BACK = "[held back: this text could not be checked for secrets]"


def _withheld(text):
    """text with the request's secrets and any credential it spells out withheld (computer/privacy.py), for anything
    that leaves the desktop: the chat, the task record, the rail, the worker's memory. Never raises, and never lets a
    text it could not check through: that is HELD_BACK."""

    if text is None or text == "":
        return text
    try:
        from decurix_agent.computer import privacy
        return privacy.withhold_known(text)
    except Exception as exc:  # noqa: BLE001 - fail closed
        log(f"Could not withhold secrets from a note, so it is held back: {exc}")
        return HELD_BACK


def _post_querytext(payload):
    """POST one note to the control plane: ("sent", ""), ("refused", why) for its own no, or ("unreachable", why)."""

    request = urllib.request.Request(
        config.CONTROL_PLANE_URL.rstrip("/") + "/api/querytext",
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            if 200 <= response.status < 300:
                return "sent", ""
            return "unreachable", f"control plane returned HTTP {response.status}"
    except urllib.error.HTTPError as exc:
        return ("unreachable" if exc.code >= 500 else "refused"), str(exc)
    except Exception as exc:  # noqa: BLE001 - every way of not getting through is "not yet"
        return "unreachable", str(exc)


def _unsent_dir():
    return config.DECURIX_DIR / "unsent"


def _keep_unsent(payload):
    """Keep a task's outcome the control plane could not be told, to send again (resend_unsent)."""

    try:
        folder = _unsent_dir()
        folder.mkdir(parents=True, exist_ok=True)
        # Named by the moment to the nanosecond: sent again in the order they happened.
        path = folder / (f"{time.time_ns():020d}_" + uuid.uuid4().hex[:8] + ".json")
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(payload), encoding="utf-8")
        tmp.replace(path)
        log(f"Kept the undelivered outcome of task {payload.get('task_id') or '?'} to send again: {path.name}")
    except OSError as exc:
        log(f"Could not keep the undelivered outcome: {exc}")


# How often kept outcomes are sent again, and for how long one is kept trying.
RESEND_SECONDS = 30
RESEND_FOR_SECONDS = 24 * 3600


def resend_unsent():
    """Send again every kept outcome, oldest first; one delivered (or refused by the control plane) is let go."""

    folder = _unsent_dir()
    if not folder.is_dir():
        return 0
    sent = 0
    for path in sorted(folder.glob("*.json")):
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            path.unlink(missing_ok=True)
            continue
        outcome, detail = _post_querytext(payload)
        if outcome == "unreachable":
            try:
                too_old = time.time() - path.stat().st_mtime > RESEND_FOR_SECONDS
            except OSError:
                too_old = True
            if too_old:
                log(f"Giving up sending {path.name} after a day: {detail}")
                path.unlink(missing_ok=True)
            break  # the control plane is not there: the rest wait for the next round, in order
        if outcome == "refused":
            log(f"The control plane refused the kept outcome {path.name}: {detail}")
        else:
            sent += 1
            log(f"Sent the kept outcome {path.name}.")
        path.unlink(missing_ok=True)
    return sent


def resend_unsent_loop():
    """Send kept outcomes again now and every RESEND_SECONDS (the agent's start runs this on a thread of its own)."""

    while True:
        try:
            resend_unsent()
        except Exception as exc:  # noqa: BLE001 - this loop must outlive anything one round meets
            log(f"Sending kept outcomes again failed: {exc}")
        time.sleep(RESEND_SECONDS)


def remember(title, summary, category="workflow"):
    """
    Add an entry to this desktop's worker memory on the control plane - the
    "learned memory" list on its dashboard card - which then writes it back
    into ~/.decurix/memory, where every harness reads it on its next task.

    Best-effort and never raises: a lesson that could not be recorded must not
    fail the task that produced it. Returns whether it was recorded.
    """

    # Memory is read by every later task and shown on the dashboard: no secret goes in it (computer/privacy.py).
    body = urllib.parse.urlencode({
        "desktop_id": config.DESKTOP_ID,
        "title": _withheld(title) or "",
        "summary": _withheld(summary) or "",
        "category": category,
    }).encode("utf-8")

    request = urllib.request.Request(
        config.CONTROL_PLANE_URL.rstrip("/") + "/api/desktops/memory",
        data=body,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )

    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            return 200 <= response.status < 300
    except Exception as exc:
        log(f"Failed to record learned memory: {exc}")
        return False


BLANK_PROBLEM = (
    "I stopped without saying why, so I do not know what is in the way. "
    "Tell me what to try next, or send the request again with more detail."
)


def ask_for_query(problem, kind="query", engine=None):
    """
    Ask the human/control plane a query.

    `kind` is "approval" when the agent stopped itself before something
    consequential (a payment, an outgoing message, a gated on-screen
    action) and "query" when it is stuck. Both pause the task the same way;
    the task records count them separately, since one is the agent doing
    its job and the other is the agent failing to.
    """

    # A model that answers `HELP:` with nothing after it has still stopped the
    # task. The chat shows only the reply text, so an empty one would leave a
    # blocked desktop with no visible question at all.
    problem = (problem or "").strip() or BLANK_PROBLEM

    # A payment run put to the person for approval shows, per payee, the account it pays to and where that account
    # came from - an account taken from a message is said loudly (payees.py). Found live (2026-10-20): a run paying a
    # vendor to an account from a lookalike email was put up for approval with nothing to show it.
    try:
        from decurix_agent import payees
        problem = payees.annotate_approval(problem)
    except Exception as exc:  # noqa: BLE001 - the question still goes out
        log(f"Could not show where the payments go: {exc}")

    if _query_gate is not None:
        try:
            if _query_gate(problem, kind, engine):
                return None
        except Exception as exc:
            log(f"The question was not checked against what was taught ({exc}); asking the person.")

    # A stage of a long request asks without stopping the stages that do not need the answer (long_task.py).
    try:
        from decurix_agent import long_task
        if long_task.take_question(problem, kind):
            return None
    except Exception as exc:
        log(f"The long request could not park this question ({exc}); asking as usual.")

    # A question that tells the person something with figures that do not add up is not passed on as fact: the
    # figures are checked, and what they really come to is said with it (verify.false_sums; a planned stage is sent
    # back to recheck instead, long_task._send_back_false_figures).
    try:
        from decurix_agent import verify
        wrong = verify.false_sums(problem)
        if wrong:
            problem += ("\n\n(Checked before asking you: " + "; ".join(wrong)[:500]
                        + ". The figures above may be mistaken - nothing was changed because of them.)")
    except Exception as exc:  # noqa: BLE001 - the check never stops a question
        log(f"Could not check the question's figures: {exc}")

    query_id = uuid.uuid4().hex

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    content = f"""
DECURIX QUERY REQUEST

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{_task_name()}

Problem:
{problem}

Query ID:
{query_id}

The agent is currently blocked.

Please send instructions through the Decurix control plane.
""".strip()

    path = config.QUERYTEXT_DIR / filename

    path.write_text(
        content,
        encoding="utf-8"
    )

    state.last_query_text, state.last_query_kind = problem, kind

    log_banner("AGENT HAS A QUERY", content, f"Query written to: {path}")

    # Waiting before the question goes out, not after: a person answering
    # the moment it appears (or a scripted reply) can reach the watcher before
    # send_query returns, and a reply that finds the agent not waiting is
    # held behind the paused task as if it were a new one.
    state.waiting_for_query = True
    state.query_asked_at = time.time()

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

# What a finished task passes before it is reported (skill_task._gate): a
# success an engine claims for a taught task is checked first. Set at import
# by skill_task, so this module needs to know nothing about skills.
_final_gate = None


def set_final_gate(gate):
    """`gate(reply, kind, engine)` -> True when it took the report over (it reports itself, checked=True)."""

    global _final_gate
    _final_gate = gate


# What a question passes before a person is paged with it
# (skill_task._question_gate): one the person already answered when they
# taught the task is answered from the lesson instead.
_query_gate = None


def set_query_gate(gate):
    """`gate(problem, kind, engine)` -> True when it answered the question itself (nobody is asked)."""

    global _query_gate
    _query_gate = gate


# How much of the task a note repeats.
TASK_ECHO_CHARS = 1000


def _task_name():
    """The task, as a note names it: a stage's whole prompt is not its name (found live: a run's report went out with
    the last stage's 9,000-word prompt in front of it, and past the control plane's size limit)."""

    task = state.current_task or "unknown"
    return cut_text(task, TASK_ECHO_CHARS) + " ..." if len(task) > TASK_ECHO_CHARS else task


def write_final_reply(reply, kind="final", engine=None, quiet=False, interrupted=False, checked=False, fallback=None,
                      task_id=None, gated=False):
    """
    Write the final result into querytext/ so the existing
    querytext watcher sends it to the Decurix control plane.

    `kind` is "final" for a finished task and "error" for one that ran to
    the end without managing it - the chat flags the latter as failed
    rather than presenting it as an ordinary answer. Either way the task is
    over and nothing is waiting on the human.

    `quiet` finishes the task and answers in the chat without a note for the
    Query rail (nor a file in querytext/, which its watcher would send as
    one): for a result nobody needs paging about, such as a finished
    recording that now sits in the desktop card's recordings list.

    `checked` says the result was judged by a check (recorded on the task as such) and skips the final gate; `gated`
    skips the gate for a report the gate (or a run's own checks) already settled WITHOUT judging it done - recorded as
    not checked (verify.outcome).
    """

    # Durable workflow stages report through the same harness as ordinary work.
    # Consume even a checked stage report: only the run's own artifact checks
    # can advance the workflow, and stage replies must never close its chat turn.
    if not interrupted:
        from decurix_agent import workflow_task
        if workflow_task.final_gate(reply, kind, engine):
            return

    if _final_gate is not None and not checked and not gated and not interrupted:
        try:
            if _final_gate(reply, kind, engine):
                return
        except Exception as exc:
            # A check that breaks must not lose the report - nor pass it off as
            # checked: it goes out saying it was not.
            log(f"The final check failed, reporting unchecked: {exc!r}")
            if kind == "final":
                reply = f"{reply}\n\n(Not checked: the check itself failed. Look at the screen before relying on this.)"

    # ddmmyyyyhhmmss.txt
    filename = time.strftime("%d%m%Y%H%M%S") + ".txt"

    status = "FAILED" if kind == "error" else "COMPLETED"

    final_note = f"""
DECURIX TASK COMPLETED

Desktop:
{config.DESKTOP_NAME}

Desktop ID:
{config.DESKTOP_ID}

Task:
{_task_name()}

Status:
{status}

Final reply:
{reply}

Completed at:
{timestamp()}
""".strip()

    log("========================================")
    log("TASK COMPLETED")
    log(final_note)

    # What the sources said that a later task will need, kept in the worker's memory (kept_facts.py) before the report
    # goes out - for at most kept_facts.WAIT_S: a worker restarted for its next task as soon as it reports would
    # otherwise lose them.
    # Not after a demonstration (it ends quietly: the person's own work, and another client's, is taught, not learned
    # about - found live 2026-10-23, a taught example's figures were kept as facts) nor a command (/learn, /skill).
    if not interrupted and not quiet and not str(state.current_task or "").lstrip().startswith("/"):
        try:
            # (task_context is this module's own import: a local import of it here made it a local name for the whole
            # function, and every ending that skips this block - a /command, a quiet one - failed further down.)
            from decurix_agent import kept_facts
            thread = kept_facts.after_task(task_id or task_context.current(), _task_name())
            if thread is not None:
                thread.join(kept_facts.WAIT_S)
        except Exception as exc:  # noqa: BLE001 - what is kept for later must never cost this report
            log(f"Could not keep this task's facts: {exc}")

    if not quiet:
        path = config.QUERYTEXT_DIR / filename

        path.write_text(
            final_note,
            encoding="utf-8"
        )

        log(f"Final reply written to: {path}")

    log("========================================")

    sent = send_query(
        filename,
        final_note,
        kind=kind,
        reply=reply,
        engine=engine,
        quiet=quiet,
        interrupted=interrupted,
        checked=checked,
        fallback=fallback,
        task_id=task_id,
    )

    if sent:
        log("Final reply sent to control plane.")
    else:
        log("WARNING: could not send final reply to control plane.")

    if task_id and task_id != task_context.current():
        # The report of a task that is not the one in hand (a stopped run's, sent on its own task): the task in hand
        # keeps its state.
        return
    state.current_task = None
    state.current_task_file = None
    state.waiting_for_query = False
    task_context.clear_active()


def post_question(problem, kind="query"):
    """
    Put a question to the person without stopping the work: a long request's
    stage that needs a decision waits, the stages that do not go on
    (long_task.py). The control plane opens an intervention as for any
    question; this agent keeps working and reads the reply when it comes.
    """

    problem = (problem or "").strip() or BLANK_PROBLEM
    filename = time.strftime("%d%m%Y%H%M%S") + "_q.txt"
    return send_query(filename, problem, kind=kind, reply=problem)


def reply_on_task(task_id, text, kind="final", still_working=False):
    """
    Answer a message a long request took while it ran - an amendment, a
    status question - on that message's own task, leaving the long request's
    task and this agent's state as they are. `still_working`: the long request
    goes on after this reply, so the chat keeps showing the desktop at work.
    """

    filename = time.strftime("%d%m%Y%H%M%S") + "_r.txt"
    return send_query(filename, text, kind=kind, reply=text, quiet=True, checked=False, task_id=task_id,
                      with_usage=False, still_working=still_working)
