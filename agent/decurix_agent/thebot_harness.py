import json
import os
import re
import time
import urllib.error
import urllib.request

from decurix_agent import config, guidance, playbooks, progress, state, usage
from decurix_agent.computer import approval
from decurix_agent.identity import get_identity_context
from decurix_agent.logging_utils import log
from decurix_agent.memory_context import get_memory_context
from decurix_agent.messaging import (
    ask_for_query,
    backend_failure_note,
    write_final_reply,
)
from decurix_agent.ollama_client import ensure_model_resident
from decurix_agent.readiness import wait_for_http_ok

# The embedding model soul uses (ollama.EmbedModel on the Go side).
EMBED_MODEL = os.environ.get("EMBED_MODEL", "nomic-embed-text")


# ============================================================
# TheBot harness
#
# Unlike task_runner.py's hand-rolled loop and claude_harness.py's
# CLI-per-task model, TheBot's `soul` binary is a persistent HTTP server
# (started once as a background daemon by start_agent_terminal.sh, not
# shelled out to per task) with its own atomizer/scheduler/skills loop
# inside it. This harness only ever submits a request over HTTP and follows
# the real job to completion - it does not reimplement any of TheBot's
# reasoning. Exposes the same perform_task/continue_after_query signatures
# so harness.py can dispatch to it interchangeably with the other two.
#
# The job is followed over soul's own SSE stream rather than polled, so
# every reasoning step it emits reaches the Decurix chat live through
# progress.emit. Polling remains the fallback for a stream that drops
# before the job reports a final event.
#
# soul reports each job's outcome as a structured field - answered,
# needs_approval, needs_input or failed - and keeps a paused job's
# checkpoint per session. So a human's reply to a blocked desktop is just
# the next request in the same session: soul resumes the paused job where
# it stopped (running the approved command, or handing the operator the
# answer it asked for) instead of starting the task over.
# ============================================================

APPROVAL_MARKER = "NEEDS_APPROVAL:"

POLL_INTERVAL_SECONDS = 2

# soul bounds a job at 25 minutes itself (THEBOT_JOB_TIMEOUT_SECONDS) and
# reports what it finished when that runs out; this is only the backstop
# for a soul that stopped responding altogether.
TASK_TIMEOUT_SECONDS = 1800

SAFETY_PREAMBLE = f"""
Approval rules for this desktop.

You act for the human. What their request asks for is theirs to decide, and
by asking they have decided it: do it, without asking again. That includes
deleting the record they name, submitting the form or ticket they describe,
saving the settings they list, renaming or removing the file they point at.
Asking "shall I?" about the very thing they asked for wastes their time.

Stop for a yes in three cases only:
1. Money: paying, buying, booking, transferring.
2. Speaking for them: sending an email or a message to other people, or
   publishing something publicly.
3. Something consequential the request did NOT ask for: deleting or
   overwriting more than what was named, or anything you are not sure about.

On the screen - in an app or a web page - you never ask for 1 or 2 yourself,
neither before you start nor before the final click. Do all the work (open
the page, find the record, fill in what is needed) and make the click: the
gui command is what decides. It refuses a click that pays, sends, buys or
publishes and asks the human at that moment, with the details in front of
them; a click it lets through (submitting a form or a support ticket the
request asked for, saving, deleting the named record) needed no yes.
Away from the screen (a shell command, a script), ask with HELP, or begin
your reply with exactly "{APPROVAL_MARKER}" followed by what you want to do
and why; a human will review it on the Decurix dashboard.

Drafting an email into the Outbox (office email) is not sending it: a person
reviews and sends it there, so draft one without asking.
""".strip()


# A pause the model wrote itself (an answer starting with APPROVAL_MARKER) is
# a pause here and nowhere else: soul finished that job as "answered" and
# holds no checkpoint, so a reply sent with reply=True resumes nothing and is
# run as a request of its own. Measured: `google "sanmayee is great"` came
# back as "NEEDS_APPROVAL: I want to Google ...", the human said "proceed",
# and soul answered the single word "proceed" with "I can't assist with that
# request". So the task and the question are kept here, and the reply goes
# back as the original task with the human's answer in its context.
_written_pause = None


# ============================================================
# What the chat and the desktop's card show while soul works
#
# soul's job log is written for whoever debugs soul: `scheduler: "chatgpt
# \"where is x\"" [general] -> skill=general model=qwen2.5-coder:3b`,
# `memory: nothing in L1 yet`. Forwarded as it was, that is what the card's
# "Working on" line and the chat's live box showed a person. Each line is put
# into words here, or left out when it only describes soul's own plumbing;
# the raw line is still in soul's job log and in /tmp/decurix-startup.log.
# ============================================================

# Plumbing: which classifier matched, which model a step went to, memory and
# skill-library lookups that found nothing, workspace ingestion.
_INTERNAL_STEP = re.compile(
    r"^(?:memory: (?:checking L1|nothing in L1)"
    r"|skill library: (?:checking for|no matching|skipping|match failed)"
    r"|scheduler: \".*\" \[[\w-]+\] -> skill="
    r"|scheduler: rerouting to the "
    r"|Answering directly - no task steps needed"
    r"|rag(?:-progress)?: |l1: |stored: |failed to store: "
    r"|could not judge whether this is worth remembering"
    r"|feedback: (?:found|could not consult)"
    r"|skill:[\w-]+ -> model replied: "
    r"|coding-architecture: =+"
    r"|no skill matched task type)"
)

_STEP_REWRITES = [
    (re.compile(r"^skill:[\w-]+ -> \$ (.+)$", re.DOTALL), lambda m: f"Running: {m.group(1)}"),
    (re.compile(r"^skill:[\w-]+ -> exit 0(?: \u00b7 (.*))?$", re.DOTALL),
     lambda m: f"Output: {m.group(1)}" if m.group(1) else "The command finished"),
    (re.compile(r"^skill:[\w-]+ -> exit (\d+)(?: \u00b7 (.*))?$", re.DOTALL),
     lambda m: f"The command failed (exit {m.group(1)})" + (f": {m.group(2)}" if m.group(2) else "")),
    (re.compile(r"^skill:general -> this needs a look at the machine.*$"),
     lambda m: "This needs a look at the machine - working it out command by command"),
    (re.compile(r"^scheduler: step (\d+)/(\d+) \[[\w-]+\] (.*)$", re.DOTALL),
     lambda m: f"Step {m.group(1)} of {m.group(2)}: {m.group(3)}"),
    (re.compile(r"^memory: recalled (\d+) fact\(s\) from L1$"),
     lambda m: f"Remembering {m.group(1)} thing(s) from earlier conversations"),
    (re.compile(r"^websearch: querying (\S+) for: (.*)$", re.DOTALL),
     lambda m: f"Searching {m.group(1)} for: {m.group(2)}"),
    (re.compile(r"^skill:[\w-]+ -> (.*)$", re.DOTALL), lambda m: m.group(1)),
    (re.compile(r"^(?:scheduler|atomizer|safety|websearch|feedback|skill library|memory|reasoning|coding-[a-z]+): (.*)$", re.DOTALL),
     lambda m: m.group(1)),
]


def readable_step(line):
    """A soul job-log line as a person reads it in the chat, or None to leave it out."""

    text = (line or "").strip().replace('\\"', '"')
    if not text or _INTERNAL_STEP.match(text):
        return None

    for pattern, rewrite in _STEP_REWRITES:
        match = pattern.match(text)
        if match:
            text = rewrite(match).strip()
            break

    return text[:1].upper() + text[1:] if text else None


def _emit_step(line):
    log(f"TheBot: {line}")
    text = readable_step(line)
    if text:
        progress.emit(text)


def _post_json(url, payload):
    body = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.loads(response.read().decode("utf-8"))


def _get_json(url):
    request = urllib.request.Request(url, method="GET")
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.loads(response.read().decode("utf-8"))


def _stream_job(base, job_id):
    """
    Follow one job over soul's SSE stream, reporting each log line as a
    live step, and return the final job snapshot.

    Returns None if the stream ended without ever delivering a final
    event, which leaves the caller to fall back to polling rather than
    treating a dropped connection as a failed task - the job itself keeps
    running inside soul either way.
    """

    url = base + f"/api/jobs/{job_id}/stream"

    request = urllib.request.Request(url, method="GET")

    deadline = time.monotonic() + TASK_TIMEOUT_SECONDS

    # The read timeout is the whole task budget on purpose: soul sends no
    # keep-alive frames, so a genuinely long-running step looks exactly
    # like an idle connection from here.
    with urllib.request.urlopen(request, timeout=TASK_TIMEOUT_SECONDS) as response:
        for raw in response:
            if time.monotonic() > deadline:
                return None

            line = raw.decode("utf-8", "replace").strip()

            if not line.startswith("data:"):
                continue

            try:
                event = json.loads(line[len("data:"):].strip())
            except ValueError:
                continue

            if event.get("type") == "log":
                _emit_step(event.get("line") or "")

            elif event.get("type") == "final":
                return event.get("job") or {}

    return None


def _poll_job(base, job_id):
    """
    Wait for a job to finish by polling, for when the stream is unusable.
    """

    deadline = time.monotonic() + TASK_TIMEOUT_SECONDS

    while time.monotonic() < deadline:
        time.sleep(POLL_INTERVAL_SECONDS)

        try:
            job = _get_json(base + f"/api/jobs/{job_id}")
        except (urllib.error.URLError, OSError, ValueError) as exc:
            log(f"Could not poll TheBot job {job_id}: {exc}")
            # The engine has gone, or has come back without the job (it keeps
            # jobs in memory): either way this job will never finish.
            raise EngineStopped() from exc

        if job.get("status") in ("done", "error"):
            return job

    return None


class EngineStopped(Exception):
    """TheBot's engine went away in the middle of a job."""


# What a person is told then. Found live: the engine's process was ended from
# outside (an antivirus took its file) a minute into a task, and the report
# read "did not finish within 1800 seconds" and blamed the model's key.
ENGINE_STOPPED = (
    "TheBot's engine stopped in the middle of this task, so the task was not finished. Nothing more was done "
    "after the last step shown above. The engine is started again by itself; send the request again."
)


def _cancel_job(base, job_id):
    """
    Stop a job this harness has given up waiting for, so it does not keep
    running commands on the desktop after the human was told it failed.
    """

    try:
        _post_json(base + f"/api/jobs/{job_id}/cancel", {})
    except (urllib.error.URLError, OSError, ValueError) as exc:
        log(f"Could not cancel TheBot job {job_id}: {exc}")


def _build_context(extra=""):
    """
    The standing context soul folds into every model call's system frame:
    who this desktop is, its persistent worker memory, and the approval
    rules it runs under.

    Sent as its own field, never glued onto the task. It used to be glued
    on - task first, then identity, memory and the safety rules - and soul
    classified, planned and remembered that whole blob as the human's
    request. "hello, whats your name?" took 148 seconds and came back as
    "NEEDS_APPROVAL: Before sending any real-world consequences..." - an
    answer to the safety rule, not to the question.
    """

    parts = [get_identity_context("TheBot (soul)")]

    memory_context = get_memory_context()
    if memory_context:
        parts.append(
            "Persistent worker memory for this desktop:\n\n" + memory_context
        )

    taught = taught_context()
    if taught:
        parts.append(taught)

    # A taught task this request is (skill_task._by_model): the procedure the
    # model is to follow, this run's values, how to look at the demonstration.
    guided = guidance.text()
    if guided:
        parts.append(guided)

    # What worked the last time this desktop did a request like this one
    # (playbooks.py): the way through, so it is not found out again. Only for
    # a hosted brain: a small local model's window has no room for it, and
    # its turns are not what is being paid for.
    try:
        run = guidance.get() or {}
        known = (playbooks.context(run.get("task") or state.current_task or "", skill=run.get("skill"))
                 if config.HOSTED_BRAIN else "")
    except Exception as exc:  # a way to save turns must never cost the task
        log(f"TheBot: could not look for a playbook: {exc}")
        known = ""
    if known:
        parts.append(known)

    parts.append(SAFETY_PREAMBLE)

    if extra:
        parts.append(extra)

    return "\n\n".join(parts)


# How many taught skills the standing context lists. In name order, never
# ranked by the request: the context is part of soul's system frame, and a
# frame that changed with every request would cost a local brain its prompt
# cache.
MAX_TAUGHT_IN_CONTEXT = 8

WEB_ASK_HINT = (
    "To put one question to ChatGPT, Google, DuckDuckGo, Wikipedia or YouTube and read "
    "its answer, run: gui ask chatgpt \"QUESTION\" (or google/duckduckgo/wikipedia/youtube). "
    "It opens the service in the desktop's browser and prints what it answered; "
    "for several questions, run it once per question."
)


def taught_context():
    """
    The skills a person taught this desktop, each with what they said about
    it in their own words, for soul's system frame. Found live: a person
    taught "query them one by one" and the model never saw it - worker memory
    had only the skill's name and values, and the words were filed as what
    "finished" looks like. Read from the skills themselves every task, so a
    skill taught before this is covered too.
    """

    from decurix_agent.computer import skill_engine, skills

    try:
        ready = sorted(skills.search("", limit=100), key=lambda s: s["name"])[:MAX_TAUGHT_IN_CONTEXT]
    except (OSError, ValueError) as exc:
        log(f"TheBot: could not list taught skills: {exc}")
        ready = []

    lines = [
        "Tasks a person taught on this desktop by demonstration. When a request is one of "
        "these, do it the way the person said - their words are how they want it done, even "
        "when the request is worded differently. A taught task works in the app or site it "
        "was taught in (its \"works in\"): a request for the same kind of work in another "
        "system - a different app or site, named in the request or already open - is not "
        "that task; do it there with gui. `gui skill NAME` shows one's steps, "
        "`gui do NAME --param name=value` runs it as recorded.",
    ]
    lines += ["- " + skill_engine.summary_line(skill) for skill in ready]
    if not ready:
        lines = []

    lines.append(WEB_ASK_HINT)
    return "\n".join(lines)


def _answer_context(asked, reply):
    """
    What the human said to a pause the model wrote itself, for the context
    of the task sent again - never glued onto the task, which is what soul
    triages and plans (see _build_context).
    """

    if approval.is_approval(reply):
        return (
            "Earlier in this task you asked the human for approval: "
            f"\"{asked}\". They replied \"{reply}\" - that is approval. "
            "Do the task now, and do not ask for approval for it again."
        )

    return (
        f"Earlier in this task you asked the human: \"{asked}\". "
        f"They replied: \"{reply}\". Do the task with that answer in mind."
    )


def _run_thebot(request, reply, extra_context=""):
    """
    Submit one request to TheBot's /api/chat and follow the job until it
    finishes.

    `reply` says whether the request answers the job this desktop's
    session has paused: True resumes it, False tells soul to drop it and
    treat this as a new request.

    Returns (job, error): the final job snapshot, or a human-readable
    error string when there is no job to report on.
    """

    base = config.THEBOT_URL.rstrip("/")

    try:
        submitted = _post_json(
            base + "/api/chat",
            {
                "prompt": request,
                "context": _build_context(extra_context),
                "workdir": str(config.WORK_DIR),
                # One session per desktop, for the whole life of the
                # container: soul keeps the conversation's history and the
                # paused job (if any) under it.
                "sessionId": config.DESKTOP_ID or "decurix-desktop",
                "reply": reply,
                # Said yes already, as a kind of task: soul does not ask about the request's own words again.
                "approved": bool(state.request_preapproved) and not reply,
            },
        )
    except (urllib.error.URLError, OSError, ValueError) as exc:
        return None, f"could not submit task to TheBot: {exc}"

    job_id = submitted.get("id")
    if not job_id:
        return None, f"TheBot did not return a job id: {submitted}"

    try:
        job = _stream_job(base, job_id)
    except (urllib.error.URLError, OSError, ValueError) as exc:
        log(f"TheBot job stream for {job_id} failed, polling instead: {exc}")
        job = None

    if job is None:
        try:
            job = _poll_job(base, job_id)
        except EngineStopped:
            return None, ENGINE_STOPPED

    if job is None:
        _cancel_job(base, job_id)
        return None, (
            f"TheBot job {job_id} did not finish within "
            f"{TASK_TIMEOUT_SECONDS} seconds"
        )

    return job, None


# How soul words a failure that was really its Ollama backend giving up:
# "could not reason about the failure (ollama: request failed: Post
# ".../api/chat": context deadline exceeded) - stopping here". That is a
# true sentence about a slow machine, and it reaches a human as the entire
# explanation of why their desktop stopped.
# soul says this, and only this, when its own reasoning call errored out -
# reasonAboutFailure returns it exclusively on a non-nil error from the
# provider, so the phrase alone is conclusive.
_BACKEND_FAILURE_PHRASES = (
    "could not reason about the failure",
    "could not reason about the task",
    "the reasoning model failed",
)

# Otherwise a provider error has to look like one: the provider named, and
# a transport failure alongside it. Matching a bare "request failed" or
# "connection refused" would relabel a task's own failed HTTP call - a web
# fetch soul made on purpose - as the reasoning model being down, and send
# the human a note about tuning OLLAMA_MODEL for something unrelated.
_BACKEND_FAILURE_SHAPES = (
    "deadline exceeded",
    "request failed",
    "connection refused",
    "no such host",
    "timed out",
    "returned http",
)


def _looks_like_backend_failure(text):
    lowered = (text or "").lower()

    if any(phrase in lowered for phrase in _BACKEND_FAILURE_PHRASES):
        return True

    # soul names its provider in every error it wraps ("ollama: ...",
    # "openai returned HTTP 503: ..." - a hosted brain reaches the control
    # plane's proxy through soul's OpenAI-compatible provider).
    if "ollama" not in lowered and "openai" not in lowered:
        return False

    return any(shape in lowered for shape in _BACKEND_FAILURE_SHAPES)


def _brain_name():
    """What this desktop reasons with, as a person would say it."""

    if config.BRAIN == "claude":
        return f"Claude ({config.BRAIN_MODEL or 'hosted'})"
    if config.BRAIN == "azure":
        return f"Azure OpenAI ({config.BRAIN_MODEL or 'hosted'})"
    return config.OLLAMA_MODEL


def _backend_note(reason):
    if config.HOSTED_BRAIN:
        # Nothing about a local model applies: no Ollama to tune, no .env.
        # What goes wrong with a hosted one is its key, its quota or the
        # network, all fixed on the dashboard or by waiting.
        return (
            f"TheBot's model, {_brain_name()}, did not answer through the "
            "control plane.\n\n"
            "If this keeps happening, check its key and model on the "
            "dashboard (New desktop > Model > Change) - a rate limit or a "
            "spent quota also shows up this way - then send the request "
            "again.\n\n"
            f"Technical detail:\n{reason}"
        )

    return backend_failure_note(
        f"TheBot's reasoning model ({config.OLLAMA_MODEL})",
        config.OLLAMA_URL,
        reason,
    )


APPROVAL_ASKED_RE = re.compile(r"\bneeds? (your )?approval\b|\bplease approve\b|\bapprove (if|this|it|the)\b|"
                               r"\b(your|for) (approval|permission|go-ahead)\b", re.IGNORECASE)


def _escalate(reason):
    """
    Send one reason to the human, rewritten if it is really about the
    backend rather than about the task.
    """

    if _looks_like_backend_failure(reason):
        ask_for_query(_backend_note(reason))
        return

    # A question that asks for a yes to something consequential is an
    # approval, whichever way the model worded it: the chat and the task
    # record tell the two apart ("Needs approval" / "Needs your answer").
    ask_for_query(reason, kind="approval" if APPROVAL_ASKED_RE.search(reason) else "query")


def _fail_on_backend(reason):
    """
    End the task with a backend failure note, rather than pausing it.

    A job that failed because the model did not answer is finished inside
    soul - nothing is paused for a reply to resume - and the note itself
    tells the human to send the request again. Raising a query instead left
    the desktop waiting, so the human's next, unrelated request was taken
    as the reply to the one that failed.
    """

    write_final_reply(_backend_note(reason), kind="error")


def _count(job):
    """
    Add what a finished job's model calls used to the task's totals, and note
    the commands that worked. soul reports both with the job (a soul from an
    older image reports neither, and the figures stay unknown as before).
    """

    spent = job.get("usage") or {}
    if spent.get("calls"):
        usage.add(input_tokens=spent.get("promptTokens"), output_tokens=spent.get("responseTokens"))
    try:
        playbooks.note_job(job)
    except Exception as exc:
        log(f"TheBot: could not note this job's commands: {exc}")


def _handle_job(job, error, resuming=False):
    if error:
        # _run_thebot's own error return means the request never produced
        # a job to report on at all (soul unreachable, no job id, or the
        # job never finished) - by construction a backend failure, not
        # something to run through _looks_like_backend_failure's text
        # matching (that heuristic is for soul's own authored answers,
        # which this error string never is).
        if error == ENGINE_STOPPED:
            # Not the model's doing and not a setup to fix: said as it is.
            write_final_reply(error, kind="error")
            return
        if resuming:
            # The documented exception: a failure while resuming after a
            # human's reply keeps the task paused, so a resend of that
            # same reply still carries the original job rather than
            # dispatching an unrelated new one.
            ask_for_query(
                "The agent tried to continue after your reply, but "
                "TheBot's backend failed again.\n\n" + _backend_note(error)
            )
            return
        _fail_on_backend(error)
        return

    _count(job)

    outcome = job.get("outcome") or ""
    answer = (job.get("finalAnswer") or "").strip()
    question = (job.get("question") or "").strip() or answer

    if outcome == "needs_approval":
        # soul's own safety gate stopped a consequential action, and holds
        # the job paused. The human's reply comes back through
        # continue_after_query and resumes it.
        ask_for_query(question, kind="approval")
        return

    if outcome == "needs_input":
        # The person has just called the task off: what the engine asks next
        # is not put to them. Found live: told `stop`, the engine stopped -
        # and then asked whether its stopping counted as the task done.
        from decurix_agent import guidance
        called_off = guidance.refused()
        if called_off:
            write_final_reply(f"Stopped, as you said (\"{called_off}\"). {answer or ''}".strip())
            return
        _escalate(question)
        return

    if outcome == "failed":
        if _looks_like_backend_failure(answer):
            _fail_on_backend(answer)
            return
        write_final_reply(answer or "TheBot could not finish this.", kind="error")
        return

    # Answered - or a soul too old to report an outcome, whose only signal
    # was a flag on its first result.
    results = job.get("results") or []
    meta = (results[0].get("Meta") if results else None) or {}
    if not outcome and meta.get("needsUserInput"):
        _escalate(meta.get("failureReasoning") or answer or "TheBot could not resolve this on its own.")
        return

    # The model can still pause on its own, in words, with the marker the
    # approval rules above ask for.
    if answer.startswith(APPROVAL_MARKER):
        global _written_pause
        asked = answer[len(APPROVAL_MARKER):].strip()
        # state.current_task is still the original request while a reply
        # is handled, so a second written pause keeps the first task.
        _written_pause = {"task": state.current_task, "asked": asked}
        ask_for_query(asked, kind="approval")
        return

    write_final_reply(answer or "(no result text)")


def wait_for_backend():
    """
    Wait for both halves of this harness's backend.

    soul is a local daemon that takes a moment to bind :4900, and it
    reasons with the same Ollama server every desktop is handed. Waiting
    for both is what lets watchers.py queue an early request instead of
    dispatching it into "could not reason about the failure ...
    connection refused" and turning a backend that was still starting into
    a query for a human.
    """

    if config.HOSTED_BRAIN:
        # A hosted brain: soul reasons through the control plane's proxy,
        # and Ollama only embeds (best-effort - soul runs without memory
        # rather than not at all), so neither the local server nor its chat
        # model is waited for, and nothing is loaded into it.
        wait_for_http_ok(
            config.THEBOT_URL.rstrip("/") + "/api/health",
            f"TheBot soul at {config.THEBOT_URL}",
        )
        return

    wait_for_http_ok(
        config.OLLAMA_URL.rstrip("/") + "/api/tags",
        f"Ollama at {config.OLLAMA_URL}",
    )

    wait_for_http_ok(
        config.THEBOT_URL.rstrip("/") + "/api/health",
        f"TheBot soul at {config.THEBOT_URL}",
    )

    # Reachable is not loaded. A cold model takes minutes on a small host,
    # longer than soul's own per-call budgets, so a job dispatched into it
    # failed as "could not reason" rather than waiting. Waiting here keeps
    # the request queued (watchers.py holds it while backend_ready is False)
    # and puts the load behind a visible chat step instead of a timeout.
    try:
        ensure_model_resident()
    except Exception as exc:
        log(f"Could not confirm the model is loaded (continuing): {exc}")


def warm_after_ready():
    """
    Load the embedding model too, in the background: soul embeds every
    memory read and write, and its first embed otherwise pays a cold load
    inside the first job.
    """

    try:
        body = json.dumps(
            {"model": EMBED_MODEL, "input": "warm", "keep_alive": "60m"}
        ).encode("utf-8")
        request = urllib.request.Request(
            config.OLLAMA_URL.rstrip("/") + "/api/embed",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=600):
            pass
    except Exception as exc:
        log(f"Could not warm the embedding model (harmless): {exc}")


def perform_task(task, task_file):
    global _written_pause
    _written_pause = None

    state.current_task = task
    state.current_task_file = task_file
    state.waiting_for_query = False

    log("========================================")
    log("STARTING TASK (TheBot harness)")
    log(task)
    log("========================================")

    # soul reports each job's token counts with the job (_count). The local
    # model costs nothing per call; a hosted one's spend is on the provider's
    # own bill, so its cost stays unknown and its tokens are what is tracked.
    usage.set_model(config.BRAIN_MODEL if config.HOSTED_BRAIN else config.OLLAMA_MODEL)
    if not config.HOSTED_BRAIN:
        usage.add(cost_usd=0)

    # A new task, not a reply: if soul still holds a paused job for this
    # desktop (the human moved on without answering it), it is dropped.
    job, error = _run_thebot(task, reply=False)
    _handle_job(job, error)


def continue_after_query(query_response):
    if not state.current_task:
        log(
            "Received a reply to a query but no "
            "active task exists."
        )
        return

    state.waiting_for_query = False

    # What a person answers steers the rest of the task: its way is not the request's (playbooks.py).
    if getattr(state, "last_query_kind", "") != "approval":
        playbooks.answered()

    log("========================================")
    log("RECEIVED HUMAN REPLY TO QUERY (TheBot harness)")
    log(query_response)
    log("========================================")

    global _written_pause
    written, _written_pause = _written_pause, None

    if written:
        # soul holds nothing paused for a pause the model only wrote: the
        # task goes again, with the question and the answer in its context.
        job, error = _run_thebot(
            written["task"], reply=False,
            extra_context=_answer_context(written["asked"], query_response),
        )
        _handle_job(job, error, resuming=True)
        return

    # soul resumes the job it paused for this session, with the reply as
    # the answer - approving the exact command it stopped before, or
    # handing it to the operator that asked. No need to restate the task:
    # the checkpoint still has it.
    job, error = _run_thebot(query_response, reply=True)
    _handle_job(job, error, resuming=True)
