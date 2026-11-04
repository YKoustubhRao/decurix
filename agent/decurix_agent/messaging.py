import json
import threading
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
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(path, text)
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


def _minimal(payload):
    """The least an outcome must say: who, which task, its kind, a few words, and its verdict cut to the bone (its
    verdict itself never changed)."""

    from decurix_agent import verdict as verdicts
    keep = {k: payload[k] for k in ("desktop_id", "vm_name", "filename", "kind", "task_id", "engine", "checked")
            if k in payload}
    text = cut_text(payload.get("reply") or payload.get("content") or "", 600)
    keep.update(content=text, reply=text)
    if isinstance(payload.get("verdict"), dict):
        keep["verdict"] = verdicts.bound(payload["verdict"], limit=2000)
    return keep


def _fallback(payload, fallback, saved):
    """The short plain version sent when the control plane refused a task's outcome or a question."""

    whole = payload.get("reply") or payload.get("content") or ""
    if not fallback and len(whole) <= 600:
        return _with_text(payload, whole), saved  # nothing to leave out: the plain text again
    saved = saved or save_whole(time.strftime("%Y%m%d-%H%M%S-") + uuid.uuid4().hex[:6], whole)
    text = (fallback or cut_text(whole, 600)) + _where(saved)
    return _with_text(payload, cut_text(text, SHORT_TEXT)), saved


def save_request(request, task_id="", secret=False):
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

    # The request's secrets are remembered before its first echo, and the log gets it withheld (review 2026-11-03
    # rework: it was logged raw; watchers remembers them again when the request starts, which is harmless).
    try:
        from decurix_agent.computer import privacy
        privacy.remember_secrets(str(request or ""))
    except Exception:  # noqa: BLE001 - withheld below all the same
        pass
    # A message the person marked secret as a whole is never logged, in any form (review round 2, B1).
    log("Received request: [withheld: marked secret]" if secret else f"Received request: {_withheld(request)}")
    log(f"Saved to {path}")

    return path


# ============================================================
# Control plane communication
# ============================================================

def send_query(filename, content, kind="", reply="", engine=None, quiet=False, interrupted=False, checked=False,
               task_id=None, with_usage=True, fallback=None, still_working=False, secret=False, verdict=None):
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
        secret    - true on a question raised for a secret (a password or code
                    field, a step the recorder kept private): the person's reply
                    may be the secret, and the control plane keeps none of it.
                    A structural fact from where the question came from, never a
                    reading of its words (review 2026-11-03, X-6)
        verdict   - on a final or error: the structured verdict (decurix.verdict/v1,
                    verdict.py), beside `checked`, which always equals
                    verdict == "done" (C5)
        skill     - on a final or error: {name, version} of the taught skill that
                    drove the task, when one did (task_context.skill, R7)

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

    if kind == "approval":
        # The approval gate words and signs the question for the action it holds; the control plane shows the person
        # those words and binds their reply to that request. Without it a reply to this note grants nothing.
        try:
            from decurix_agent.computer import approval
            gate = approval.signed_question()
        except Exception:  # noqa: BLE001 - no gate's question: the note still goes, and its reply grants nothing
            gate = None
        if gate:
            payload["gate"] = gate
    if quiet:
        payload["quiet"] = True
    if interrupted:
        payload["interrupted"] = True
    if still_working:
        # The desktop is still busy after this reply (a planned run's receipt): the chat's turn stays open.
        payload["still_working"] = True
    if secret:
        payload["secret"] = True
    # Whether the result went through the final check (verify.py, the skill
    # gate): the task records keep it as data, so an audit can tell a checked
    # "done" from one nobody looked at.
    if kind in ("final", "error"):
        payload["checked"] = bool(checked)
        if verdict is not None:
            # Within what the control plane reads, its verdict never changed (verdict.bound; review round 2, B7).
            from decurix_agent import verdict as verdicts
            payload["verdict"] = verdicts.bound(verdict)
        ran = task_context.skill(task_id or task_context.current())
        if ran:
            payload["skill"] = ran

    if with_usage:
        payload.update(usage.snapshot())

    saved = None
    if not _fits(payload):
        payload, saved = _shortened(payload)
        log(f"A {kind or 'note'} too long for the control plane was shortened; the whole of it is in {saved}.")
    short_sent = minimal_sent = False

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
        if outcome == "refused" and kind in ("final", "error") and not minimal_sent:
            minimal_sent = True
            log(f"The control plane refused the short {kind} too ({detail}); sending the least it needs.")
            payload = _minimal(payload)
            continue
        if outcome == "refused" or time.monotonic() + wait > deadline:
            log(f"Failed to send query: {detail}")
            if kind in ("final", "error"):
                # Kept and sent again either way: a task must never stay "running" because its outcome did not land.
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


def withheld(text):
    """_withheld, for the agent's own log: a request or a reply printed in a banner is withheld first - the log is a
    file the worker's tools and the support zip read (review 2026-11-03, F raw log). Never raises."""

    return _withheld(text)


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
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(path, payload, atomic=True)
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
        if outcome == "refused" and not path.name.endswith(".min.json"):
            # Kept once more as the least it must say, sent at the next round (review round 2, B7).
            log(f"The control plane refused the kept outcome {path.name} ({detail}); keeping its least form to send.")
            try:
                smaller = path.with_name(path.name[:-len(".json")] + ".min.json")
                from decurix_agent.computer import privacy
                privacy.write_scrubbed(smaller, _minimal(payload))
            except OSError as exc:
                log(f"Could not keep the least form of {path.name}: {exc}")
            path.unlink(missing_ok=True)
            continue
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


def ask_for_query(problem, kind="query", engine=None, secret=False, _date_query=False):
    """
    Ask the human/control plane a query.

    `kind` is "approval" when the agent stopped itself before something
    consequential (a payment, an outgoing message, a gated on-screen
    action) and "query" when it is stuck. Both pause the task the same way;
    the task records count them separately, since one is the agent doing
    its job and the other is the agent failing to.

    `secret`: the question was raised for a secret field or a private step (a structural fact the caller has, never
    read from the question's words): its reply is withheld whole wherever it is kept (send_query's "secret").
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

    # A save waiting on which order a date is written in (task_context.ask_date_order) is asked as a question of its
    # own, in place of this one - never added to it: one reply to two questions answered both, and an engine question
    # with numbered choices set the date's order with its own answer (review round 3, R3). Only the reply to the date's
    # own query settles it (watchers); the engine is then told this question was not put to the person.
    # An approval question or a question raised for a secret is replaced the same way, on purpose: the replaced
    # approval's held action is withdrawn by the date reply (no yes), and the engine asks again (review round 4).
    if not _date_query:
        try:
            pending = task_context.date_question()
            if pending and not pending.get("query_id"):
                log("A save waits on which way a date is written: that is asked first, on its own; the engine's "
                    "question is not put to the person now.")
                query_id = ask_for_query(task_context.date_question_text(pending), kind="query", engine=engine,
                                         _date_query=True)
                task_context.mark_date_asked(task_context.current(), query_id)
                return query_id
        except Exception as exc:  # noqa: BLE001 - the question still goes out
            log(f"Could not ask the date question: {exc}")

    if _query_gate is not None and not _date_query:
        try:
            if _query_gate(problem, kind, engine):
                return None
        except Exception as exc:
            log(f"The question was not checked against what was taught ({exc}); asking the person.")

    # A stage of a long request asks without stopping the stages that do not need the answer (long_task.py).
    try:
        from decurix_agent import long_task
        if not _date_query and long_task.take_question(problem, kind):
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
    # The local copy and the log get what the chat gets: no secret (round-5 recheck: the file was written unredacted).
    content = _withheld(content)

    path = config.QUERYTEXT_DIR / filename

    from decurix_agent.computer import privacy
    privacy.write_scrubbed(path, content)

    state.last_query_text, state.last_query_kind = problem, kind
    state.last_query_secret = bool(secret)
    state.asked_query_id = query_id

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
        secret=secret,
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
                      task_id=None, gated=False, verdict=None, stopped=False):
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

    `verdict` is the structured verdict of the checks that settled it (verdict.py, R6), sent as given; `checked` then
    follows it, and a caller's `checked` that disagrees makes it not checked (fail closed, logged). Without one the
    verdict is derived (verdict.derive): checked=True is a `legacy` done, which never counts towards trust.
    `stopped`: the person said no and the work stopped there (write_stopped) - never done.
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
            # A check that breaks must not lose the report - nor pass it off as checked: it goes out not checked
            # (checked=False), saying so first, where the person reads before the worker's own words (review
            # 2026-11-03, E-35: the note came last, after a report that read as done).
            log(f"The final check failed, reporting unchecked: {exc!r}")
            checked = False
            if kind == "final":
                reply = (f"Not checked - the final check itself failed ({type(exc).__name__}). Look at the screen "
                         f"and the files before relying on this.\n\n{reply}")

    # A save that waited on which way a date is written, the person not yet answering: the task is not checked, and
    # the report says the question (review round 3, R3) - whatever the engine or a check made of the rest.
    if not stopped and not interrupted:
        try:
            pending = task_context.date_question(task_id or task_context.current())
            if pending:
                from decurix_agent import verdict as verdicts
                reply = ("Not checked - a save waited on a question to you that has no answer yet: "
                         + task_context.date_question_text(pending) + "\n\n" + str(reply or ""))
                checked, gated = False, True
                if isinstance(verdict, dict) and verdict.get("verdict") == verdicts.DONE:
                    verdict = verdicts.make(verdicts.UNCHECKED, verdict.get("by") or "none",
                                            why="a save waited on a date question with no answer",
                                            checks=verdict.get("checks") or ())
        except Exception as exc:  # noqa: BLE001 - the report still goes out
            log(f"Could not look for an open date question: {exc}")

    checked, verdict = _settle_verdict(reply, kind, checked, gated, verdict, stopped)

    # The run now has an outcome: its evidence is kept until the control plane has pulled it (retention.py, W0.8).
    # Before the report is sent, so the control plane's pull, which the report starts, finds the mark.
    from decurix_agent import retention
    retention.mark_labelled(task_id or task_context.current(), "final")
    retention.write_manifest(task_id or task_context.current())

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
    log(_withheld(final_note))

    if not quiet:
        path = config.QUERYTEXT_DIR / filename

        from decurix_agent.computer import privacy
        privacy.write_scrubbed(path, _withheld(final_note))

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
        verdict=verdict,
    )

    if sent:
        log("Final reply sent to control plane.")
    else:
        log("WARNING: could not send final reply to control plane.")

    facts_thread = _keep_facts(task_id, quiet=quiet, interrupted=interrupted)

    # The task is over: its secret values as they are let go (privacy.remember_literals; the word hashes stay) - once
    # its facts thread is done with them, if it is still running (review round 7: a model call slower than WAIT_S made
    # its secret check after the values were gone).
    ended = task_id or task_context.current()
    if facts_thread is not None and facts_thread.is_alive():
        def let_go_after():
            facts_thread.join()
            _let_go_of_values(ended)
        threading.Thread(target=let_go_after, daemon=True, name="let-go-after-facts").start()
    else:
        _let_go_of_values(ended)

    if task_id and task_id != task_context.current():
        # The report of a task that is not the one in hand (a stopped run's, sent on its own task): the task in hand
        # keeps its state.
        return
    state.current_task = None
    state.current_task_file = None
    state.waiting_for_query = False
    task_context.clear_active()
    # No task is in hand any more: what arrives next (a marked request on an idle desktop) is never filed under the
    # task that just ended (review round 3, R2-B1).
    try:
        task_context.start("")
    except Exception as exc:  # noqa: BLE001 - the report is out
        log(f"Could not let go of the ended task: {exc}")


def _settle_verdict(reply, kind, checked, gated, given, stopped):
    """(checked, verdict) for an outcome: a stop is "stopped"; a given verdict is sent when valid and `checked` follows
    it - one that is invalid, or that the caller's `checked` disagrees with, is sent as "unchecked" and not checked, and
    logged; with none, it is derived. Never raises: a verdict that cannot be built is "unchecked"."""

    from decurix_agent import verdict as verdicts
    try:
        why = " ".join(str(reply or "").split())[:300]
        if stopped:
            return False, verdicts.derive(False, kind, stopped=True, why=why)
        if given is None:
            derived = verdicts.derive(bool(checked), kind, gated=gated, why=why)
            return derived["verdict"] == verdicts.DONE, derived
        problems = verdicts.validate(given)
        if problems:
            log(f"The verdict given for this report is not valid ({'; '.join(problems)[:300]}); sent as unchecked.")
            by = given.get("by") if isinstance(given, dict) and given.get("by") in verdicts.BY else "none"
            return False, verdicts.make(verdicts.UNCHECKED, by, why="the check's own verdict could not be read")
        if given["verdict"] != verdicts.DONE:
            if checked and kind != "error":
                log(f"A report sent as checked has the verdict {given['verdict']}; sent as not checked.")
            return False, given  # (checked=True on an error is "a check saw it not done": the verdict says so now)
        if not checked:
            log("A done verdict came with a report not marked checked; the two disagree, so it is sent as not checked.")
            return False, verdicts.make(verdicts.UNCHECKED, given["by"], why="the verdict (done) and the report "
                                        "disagree on whether it was checked", checks=given.get("checks") or ())
        return True, given
    except Exception as exc:  # noqa: BLE001 - an outcome is never lost to its verdict
        log(f"Could not build this report's verdict: {exc}")
        return False, {"schema": "decurix.verdict/v1", "verdict": "unchecked", "by": "none", "why": "", "basis": [],
                       "checks": [], "evidence": []}


def write_stopped(text, engine=None, task_id=None, send=None):
    """
    The one way to report that the person stopped the work (E-36): kind "error", not checked, verdict "stopped" -
    never done, whatever was checked before the stop. `send` is the caller's own write_final_reply (the name its
    module holds), so the report goes the way that module's reports go.
    """

    return (send or write_final_reply)(text, kind="error", engine=engine, task_id=task_id, stopped=True, checked=False,
                                       gated=True)


def _let_go_of_values(task_id):
    """The task's secret values as they are, let go (privacy.clear_literals). Never raises."""

    try:
        from decurix_agent.computer import privacy
        privacy.clear_literals(task_id)
        task_context._secret_values.pop(task_id, None)
    except Exception as exc:  # noqa: BLE001
        log(f"Could not let go of the task's secret values: {exc}")


def _keep_facts(task_id, quiet=False, interrupted=False):
    """
    What the sources said that a later task will need, kept in the worker's memory (kept_facts.py) - after the report
    went out, so the person never waits on it (review 2026-11-03, E-30: the report waited up to WAIT_S for a model
    call), and before this task lets go of the desktop, for at most kept_facts.WAIT_S: a worker restarted for its next
    task as soon as it is free would otherwise lose them. Given the whole request (the client may be named past its
    first lines). Not after a demonstration (it ends quietly: the person's own work, and another client's, is taught,
    not learned about - found live 2026-10-23, a taught example's figures were kept as facts) nor a command (/learn,
    /skill). Nothing either from a task any message of which was marked or withheld as secret: its request is that
    message, and a fact distilled from it - an unlabelled secret written as a "place" - would enter later tasks'
    prompts (review round 7, R6-B1), as secret replies keep no lessons. Returns the facts thread (None when none was
    started) so the task's values are let go only once it is done with them. Never raises.
    """

    if interrupted or quiet or str(state.current_task or "").lstrip().startswith("/"):
        return None
    if task_id and task_id != task_context.current():
        # The report of another task (a stopped run's, sent on its own task): its reading is not the request in hand,
        # so nothing is kept from pairing the two (review 2026-11-03 rework).
        return None
    try:
        from decurix_agent import kept_facts
        task = task_id or task_context.current()
        held = task_context.held_values(task) if task else []
        if held or getattr(state, "reply_secret", False):
            log("Facts: none kept - this task's messages held a secret")
            return None
        request = task_context.current_request() or str(state.current_task or "")
        thread = kept_facts.after_task(task, request, held=held)
        if thread is not None:
            thread.join(kept_facts.WAIT_S)
        return thread
    except Exception as exc:  # noqa: BLE001 - what is kept for later must never cost this report
        log(f"Could not keep this task's facts: {exc}")
        return None


# ============================================================
# What a desktop keeps from a person's reply, copied to the control plane (R8; PLAN-P2 C7, D4)
#
# A correction, a run answer or a lesson the worker keeps from what a person said lived only on the desktop: deleting
# or recreating the desktop lost it, and no record said who taught it. Each is now posted as it is kept, citing the
# control plane's judgement of the reply it came from (derived_from) - the control plane decides the author from that
# judgement, never from this desktop. With no reply to cite nothing is posted. It never blocks the run: a thread, a
# 5-second timeout, and an unposted line kept in the task's run folder (learned.jsonl) to be tried again at the next
# post or when the agent starts. A 409 (the control plane does not take the citation) is dropped, not retried.
# ============================================================

LEARNED_KINDS = ("correction", "run_answer", "lesson")
LEARNED_TIMEOUT = 5
LEARNED_FILE = "learned.jsonl"
_learned_lock = threading.Lock()


def post_learned(kind, skill, version, question, text, task_id=None, derived_from=""):
    """Post what was kept from a reply (C7) in the background; the thread, or None when there is nothing to post.
    `derived_from` is the judgement of the very reply it was kept from, captured with that reply (watchers): never the
    task's latest, which a later reply may have replaced (review round 2, B7)."""

    task_id = task_id or task_context.current()
    if kind not in LEARNED_KINDS or not task_context.valid(task_id):
        return None
    if not task_context.valid_judgement(derived_from):
        log(f"Kept on this desktop only: no reply to cite for the {kind}.")
        return None
    # Withheld whole, then cut (review round 2, B4): a secret across the cut leaves nothing of itself.
    body = {"task_id": task_id, "kind": kind, "skill": str(skill or ""), "skill_version": int(version or 0),
            "derived_from": derived_from,
            "question": cut_text(_withheld(" ".join(str(question or "").split())), 2000),
            "text": cut_text(_withheld(" ".join(str(text or "").split())), 4000)}
    try:
        from decurix_agent.computer import privacy
        body = privacy.scrub(body)
    except Exception:  # noqa: BLE001 - withheld above already
        pass
    if not isinstance(body, dict) or "held_back" in body:
        log(f"The {kind} could not be checked for secrets, so it is kept on this desktop only.")
        return None
    _queue_learned(task_id, body)
    thread = threading.Thread(target=resend_learned, daemon=True, name="post-learned")
    thread.start()
    return thread


def _learned_path(task_id):
    folder = task_context.run_dir(task_id)
    return None if folder is None else folder / LEARNED_FILE


def _queue_learned(task_id, body):
    path = _learned_path(task_id)
    try:
        with _learned_lock:
            path.parent.mkdir(parents=True, exist_ok=True)
            from decurix_agent.computer import privacy
            privacy.write_scrubbed(path, body, append=True, json_kw={"ensure_ascii": False}, end="\n")
    except OSError as exc:
        log(f"Could not keep the {body['kind']} to post later: {exc}")


def _post_learned(body):
    """"posted", "refused" (409 or another 4xx: not retried) or "later" (unreachable, 5xx)."""

    request = urllib.request.Request(
        config.CONTROL_PLANE_URL.rstrip("/") + "/api/desktops/learned",
        data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=LEARNED_TIMEOUT) as response:
            return "posted" if 200 <= response.status < 300 else "later"
    except urllib.error.HTTPError as exc:
        if exc.code == 409:
            log(f"The control plane did not take the {body.get('kind')} (409: its citation is not a person's reply on "
                "this task); kept on this desktop only.")
            return "refused"
        return "later" if exc.code >= 500 else "refused"
    except Exception:  # noqa: BLE001 - not reachable now: tried again later
        return "later"


def resend_learned():
    """Try every unposted line of every run folder once; keep what could not be posted yet. Never raises."""

    try:
        root = config.DECURIX_DIR / "runs"
        paths = sorted(root.glob("*/" + LEARNED_FILE)) if root.is_dir() else []
    except OSError:
        return
    for path in paths:
        try:
            with _learned_lock:
                lines = [line for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
            left = []
            for line in lines:
                try:
                    body = json.loads(line)
                except ValueError:
                    continue
                if _post_learned(body) == "later":
                    left.append(line)
            with _learned_lock:
                # (A line queued meanwhile is kept: only the lines read here are settled.)
                now = [line for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
                rest = left + [line for line in now if line not in lines]
                if rest:
                    from decurix_agent.computer import privacy
                    privacy.write_scrubbed(path, "\n".join(rest) + "\n")
                else:
                    path.unlink()
        except Exception as exc:  # noqa: BLE001
            log(f"Could not post what was kept from a reply: {exc}")


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
    # An outcome carries a verdict (R6): an answer on a message's own task is not checked work - unchecked.
    from decurix_agent import verdict as verdicts
    verdict = None
    if kind in ("final", "error"):
        verdict = verdicts.derive(False, kind, why=" ".join(str(text or "").split())[:300])
    return send_query(filename, text, kind=kind, reply=text, quiet=True, checked=False, task_id=task_id,
                      with_usage=False, still_working=still_working, verdict=verdict)
