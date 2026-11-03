import sys
import threading
import time
import traceback

from decurix_agent import (ask_task, attachments, config, guidance, lessons, long_task, playbooks, procedures, progress, record_task, replay_task,
                           skill_task, state, task_context, tool_task, usage, workflow_task)
from decurix_agent.computer import approval, browser
from decurix_agent.harness import continue_after_query, perform_task
from decurix_agent.logging_utils import log
from decurix_agent.messaging import REPLY_MARK, resend_unsent_loop, write_final_reply


def _run_handler(target, args):
    try:
        return target(*args)
    finally:
        # Learned skills/replay can pause without going through harness.py.
        workflow_task.engine_returned()


def _called_off(reply):
    """Ends the task a person called off at one of its questions, without another action."""

    state.waiting_for_query = False
    workflow_task.cancel_active()
    state.paused_skill = None
    state.paused_replay = None
    progress.emit("Stopping, as you said. Nothing more is done.")
    # A stop is not a finished task (review 2026-11-03, E-36): not done, never "final", and not judged.
    write_final_reply(f"Stopped, as you said: I did nothing more after your \"{' '.join(str(reply).split())[:100]}\". "
                      f"Anything done before the question is still as it was left.", kind="error", checked=False,
                      gated=True)


# ============================================================
# Command watcher
# ============================================================

# How often to say "still waiting" while requests sit queued behind a
# backend that has not come up. The watcher polls every second; the chat
# wants a heartbeat, not a step line per second.
QUEUE_NOTICE_SECONDS = 30

_last_queue_notice = None

# Command files already reported as waiting behind a running task.
_deferred_logged = set()


def _report_queued(name):
    """
    Say, at most every QUEUE_NOTICE_SECONDS, that requests are waiting for
    the reasoning backend.

    The human has already seen their request appear in the chat, which opens
    a turn and starts the "Working..." card, so silence here would read as a
    hang. This is the only thing that distinguishes "queued, will run" from
    "stuck".
    """

    global _last_queue_notice

    now = time.monotonic()

    if (
        _last_queue_notice is not None
        and now - _last_queue_notice < QUEUE_NOTICE_SECONDS
    ):
        return

    _last_queue_notice = now

    log(
        f"Reasoning backend not ready. "
        f"Leaving {name} queued."
    )

    progress.emit(
        "Waiting for the model backend to come up - "
        "your request is queued and will run as soon as it is reachable."
    )


def _perform_following_procedure(content, task_file):
    """
    An engine's task, with the standing procedure it asks for written under it.
    Recurring work the person taught by saying how it is done: a request that
    gives the procedure has it kept, and a later "as usual" gets it back under
    its own words - for the engine, and for the check of what it hands in
    (procedures.py). Done on the task's own thread: finding the procedure asks
    the hosted model, and found in review (2026-10-14) asking it inside the
    watcher's pass held every other request and reply - and a model that waits
    out a rate limit for minutes reads as a watcher stuck.
    """

    applied = procedures.apply(content)
    if applied == content:
        if procedures.sets_procedure(content):
            threading.Thread(target=procedures.note, args=(content, task_context.current() or ""), daemon=True).start()
    else:
        content = applied
        with state.state_lock:
            state.current_task = content
    return perform_task(content, task_file)


def _needs_no_backend(content):
    """Whether a request is handled without the reasoning backend ("/replay", "/record", "/learn", "/skill", a recorded task done again, or a question for a web app)."""

    return (workflow_task.needs_no_backend(content) or bool(replay_task.parse(content)) or record_task.parse(content) is not None or skill_task.handles(content)
            or skill_task.mimics(content) or _asks_a_web_app(content))


def _asks_a_web_app(content):
    """
    Whether a request is only a question for a web app (ask_task.py). One that
    brought files is not: the files are part of the job, and only an engine
    can read them.
    """

    return not attachments.files(content) and ask_task.handles(content)


def _can_run_without_backend(content):
    """Whether this command can run now when the model backend is unavailable."""

    if state.waiting_for_query:
        # A reply to an ordinary agent question has to resume that agent, but
        # replay and teaching questions are deterministic. Never leave a
        # person's Learn answer waiting for Ollama: it only saves the answer
        # and either asks the next fixed question or writes the skill.
        return workflow_task.query_control(content) or state.paused_replay is not None or state.paused_skill is not None

    return _needs_no_backend(content)


HANDLED_SUFFIX = ".done"

# How long the command loop may go without a pass before the watchdog says so.
# A pass takes milliseconds; one reply sat unhandled for four minutes on a real
# desktop with nothing in the log to say why.
STALL_SECONDS = 45

# When the command loop last started a pass, and the file it is handling now.
_heartbeat = time.monotonic()
_in_flight = None

STALE_REPLY = (
    "Your answer reached this desktop after it had restarted, so the question it answered is gone and I did not "
    "run it as a new request. Send the original request again (for a lesson, `/learn` on the same recording "
    "carries on from the last answer it kept)."
)

INTERRUPTED = (
    "This desktop restarted while it was working on this, so the task stopped part way and anything after that "
    "point was not done. Send the request again to start it over."
)


def _orphaned_question(name):
    """Whether a command file not marked as a reply was saved after the open question was asked."""

    try:
        saved = time.mktime(time.strptime(name[:15], "%Y%m%d_%H%M%S"))
    except (ValueError, OverflowError):
        return False
    return bool(state.query_asked_at) and saved > state.query_asked_at + 1


def is_reply_file(name):
    """Whether a command file was saved while the agent was waiting on a question (messaging.save_request)."""

    return REPLY_MARK in (name or "")


def _report_interrupted():
    """
    Tell the control plane about a task an earlier life of this agent started
    and never finished: the desktop restarted under it (its X session ended
    and the container came back on its own). Without this the task stayed
    "running" for good and the chat said "Working..." over nothing.
    """

    record = task_context.take_interrupted()
    if record is None:
        return

    # A long request's planned run is on disk, stage by stage: it carries on (long_task.py).
    try:
        if long_task.resume_interrupted(record):
            return
    except Exception as exc:
        log(f"Could not resume the long request of task {record['task_id']}: {exc}")

    log(f"Task {record['task_id']} was interrupted by a restart; reporting it.")
    task_context.start(record["task_id"], request=record.get("request"))
    state.current_task = record.get("request") or None
    note = workflow_task.interrupted_note(record["task_id"]) or INTERRUPTED
    if skill_task.parse_learn(record.get("request") or "") is not None:
        note += " A lesson keeps every answer you gave, so the same `/learn` carries on from there."
    try:
        write_final_reply(note, kind="error", interrupted=True)
    except Exception as exc:
        log(f"Could not report the interrupted task: {exc}")
    finally:
        state.current_task = None
        task_context.start("")


def _answer_stale_reply(path, content):
    """A reply whose question no longer exists: say so on its task instead of running it as a request."""

    log(f"{path.name} answers a question this agent no longer has (it restarted). Not running it.")
    task_context.start(task_context.task_id_from_filename(path.name), request=content)
    state.current_task = content
    try:
        write_final_reply(STALE_REPLY, kind="error")
    finally:
        state.current_task = None
        task_context.start("")


def _thread_stacks():
    frames = sys._current_frames()
    parts = []
    for thread in threading.enumerate():
        frame = frames.get(thread.ident)
        if frame is not None:
            parts.append(f"--- {thread.name}\n" + "".join(traceback.format_stack(frame)[-8:]))
    return "\n".join(parts)


# How often the watchdog looks, and how much later than that a look may come
# before it counts as the process having been frozen.
WATCHDOG_TICK = 5
FROZEN_SLACK = 10


def watchdog():
    """
    Say, with every thread's stack, when the command loop stops passing. It
    changes nothing: a loop that is blocked is blocked on something this
    cannot safely take away. What it buys is the next occurrence explaining
    itself in the log, and a line in the chat so a person is not left
    watching "Working..." with no idea why.
    """

    reported = None
    resumed = 0.0
    tick = time.monotonic()
    while True:
        time.sleep(WATCHDOG_TICK)
        now = time.monotonic()
        # A sleep that took far longer than asked means the whole process was
        # frozen (idle-pause's `docker pause`), and the monotonic clock ran on
        # meanwhile. Time frozen is not time stuck: found live, every desktop
        # woken from a pause could say "stuck handling a message" before its
        # command loop had run once since.
        if now - tick > WATCHDOG_TICK + FROZEN_SLACK:
            resumed = now
        tick = now
        stalled = now - max(_heartbeat, resumed)
        if stalled < STALL_SECONDS:
            reported = None
            continue
        if reported == _heartbeat:
            continue
        reported = _heartbeat
        try:
            log(f"Command watcher has not passed in {stalled:.0f}s (handling {_in_flight or 'nothing'}). Threads:\n"
                + _thread_stacks())
            # Between messages nobody is waiting, and there is no message to be stuck on: only the log hears of it.
            if _in_flight:
                progress.emit("This desktop's agent is stuck handling a message - restart the desktop if it does not "
                              "recover within a minute.")
        except Exception:
            pass


def _mark_handled(path):
    """
    Rename a dispatched request file so no later pass - and no later life of
    this process - picks it up again.

    state.processed_commands is memory; the command directory is the desktop's
    persistent volume. Every restart used to replay every request the desktop
    had ever been sent: measured running the GUI benchmark, which restarts a
    desktop between tasks, each new task queued behind replays of all the
    earlier ones, and the answers came back about signing in to an admin panel
    for a spreadsheet task. Renamed rather than deleted, so the history of what
    the desktop was asked stays on disk.
    """

    try:
        path.rename(path.with_name(path.name + HANDLED_SUFFIX))
    except OSError as exc:
        log(f"Could not mark {path.name} as handled: {exc}")


def _pass():
    """One look at the command directory: dispatch what can run now, leave the rest for the next pass."""

    global _in_flight

    files = sorted(
        config.COMMAND_DIR.glob("*.txt"),
        key=lambda p: p.stat().st_mtime,
    )

    for path in files:

        if path.name in state.processed_commands:
            continue

        _in_flight = path.name

        # Ignore files generated by the agent.
        if path.name.startswith("result_"):
            state.processed_commands.add(
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
            # A writer may have created the file but not finished writing it.
            # Leave it eligible for the next pass; remembering its name here
            # would strand the complete request for the life of this process.
            continue

        # A message that arrives while a long request is carried - an answer, a change to the task, stop, a
        # question about progress, or something else - is read for which it is (long_task.py), off this thread:
        # reading it may ask the model. Found in round 5's spec: "Bill NS-0457 was cancelled, leave it out of
        # everything", sent ten minutes into a month-end close, would have waited for the whole close to end and
        # then run as a task of its own.
        if long_task.holding():
            # A stopped run's report is being put together: this waits for it, untouched, and is read on a later pass
            # as the new request it is (long_task.stop).
            continue
        if long_task.takes(path.name) and state.backend_ready:
            state.processed_commands.add(path.name)
            threading.Thread(target=long_task.take_message, args=(content, path), daemon=True).start()
            continue

        # A task queued before a question was asked is not its answer.
        # Only messages marked as replies at receipt may resume that task.
        if state.waiting_for_query and not is_reply_file(path.name):
            if not _orphaned_question(path.name):
                continue
            # Saved after the question, yet not as its reply: the control plane planned it as a new task, so it
            # holds no open question for this desktop (the question was asked for a task it had already closed).
            # Found live (2026-10-15): such a request sat unread for 34 minutes behind a question nobody could
            # answer. The stale question is dropped and the request runs.
            log(f"{path.name} came after the open question but not as its reply - the question is stale; dropping it.")
            state.waiting_for_query = False
            state.paused_skill = None
            state.paused_replay = None
            with state.state_lock:
                state.current_task = None

        # The reasoning backend has not been reached yet - Ollama
        # is still starting, or is briefly unreachable. Leave the
        # file in the command directory and come back to it on a
        # later pass, exactly as the "task already running" case
        # below does.
        #
        # Not failing the task here is the whole point: a request
        # that arrives a moment before the model is up used to be
        # burned - dispatched, failed on the first request, and
        # turned into a query asking a human to do something
        # about a backend that was about to come up on its own.
        # Waiting costs nothing and needs no one.
        # Recording, replay and teaching are deterministic. Only a
        # reply that must resume an ordinary agent needs the model.
        if not state.backend_ready and not _can_run_without_backend(content):
            _report_queued(path.name)
            continue

        state.processed_commands.add(
            path.name
        )

        # If the agent is waiting on a query answer,
        # this new command is the reply.
        if state.waiting_for_query:
            answer_waiting(path, content)
            continue

        # Do not start another task while one
        # is already running.
        #
        # The slot is claimed here, under the lock, rather than
        # left to perform_task's own first statement: the watcher
        # would otherwise start a thread and then test
        # current_task again for the next file before that thread
        # had run a single line, and dispatch a second task on top
        # of the first. Nearly unreachable while requests only
        # trickled in one at a time - routine now that a backlog
        # can be released all at once when the backend comes up.
        with state.state_lock:

            if state.current_task is not None:

                # Once per file: the watcher passes every second,
                # and a queued request behind a five-minute model
                # call used to write three hundred of these lines.
                if path.name not in _deferred_logged:
                    _deferred_logged.add(path.name)
                    log(
                        "Task already running. "
                        f"Leaving {path.name} "
                        "for later."
                    )

                state.processed_commands.discard(
                    path.name
                )

                continue

            state.current_task = content

        # The slot claimed above is given back if the request never reached its handler: an exception on the way
        # (a file that would not unpack, a record that would not start) otherwise left current_task set for the life
        # of the agent, and every later request waited behind "Task already running" (review 2026-11-02).
        started = False
        try:
            started = _start_request(path, content)
        finally:
            if not started:
                _release_slot(content)


NOT_THE_REPLY = ("Your message \"{said}\" did not come as the reply to my question, so I have not taken it as the "
                 "answer{granted}. The question is still open - please answer it by replying to it:\n\n{asked}")


def _not_the_reply(content):
    """
    Tell the person a message of theirs was not taken as the answer to the open question (it did not come as its
    reply), and put that question to them again, unchanged, so the answer they send comes as its reply. Called off the
    command watcher's thread (a long request's message thread). Never raises.
    """

    said = " ".join(str(content or "").split())
    said = said[:120] + ("..." if len(said) > 120 else "")
    try:
        from decurix_agent.computer import privacy
        said = privacy.withhold_known(said)
    except Exception:  # noqa: BLE001 - the quote is withheld whole rather than risked
        said = "[withheld]"
    asked, kind = state.last_query_text or "", state.last_query_kind or "query"
    try:
        if state.waiting_for_query and asked:
            long_task._ask_again(NOT_THE_REPLY.format(said=said, asked=asked,
                                                      granted=" - nothing was approved" if kind == "approval" else ""),
                                 kind)
            # What reads the reply (the approval gate, lessons) reads the question itself, not this note around it.
            state.last_query_text, state.last_query_kind = asked, kind
        else:
            progress.emit(f"Your message \"{said}\" did not come as the reply to a question, so it was not taken as "
                          "an answer.")
    except Exception as exc:  # noqa: BLE001
        log(f"Could not tell the person their message was not taken as the answer: {exc}")
        progress.emit(f"Your message \"{said}\" was not taken as the answer to my question - it is still open; "
                      "please answer it by replying to it.")


def answer_waiting(path, content, already_marked=False):
    """
    A request that arrived while the agent waits on a question: its reply.
    Also called by a long request (long_task.py) for a message it read as the
    answer to the question its whole run waits on - with the answer alone,
    `already_marked` when it has renamed the file itself.
    """

    # A reply must name the question it answers: only a message saved as the
    # reply - naming the task whose question is open (messaging.save_request) -
    # answers it. Anything else that reached this desktop (another desktop, a
    # web page, a message a long request read as an answer without that name)
    # answers nothing and grants nothing; the question stays open (review
    # 2026-11-02). It is never dropped in silence either: the person is told it
    # was not taken as the answer, and the open question is put to them again
    # so their reply comes as its reply (recheck 2026-11-02: dropped, a long
    # run waited on an answer the person thought they had given).
    if not is_reply_file(path.name):
        log(f"{path.name} does not name the open question's task - it is not taken as the answer.")
        if not already_marked:
            _mark_handled(path)
        _not_the_reply(content)
        return

    # A reply belongs to the paused task. A desktop whose
    # task started before the control plane sent IDs takes
    # the one its reply carries.
    if not state.current_task_id:
        task_context.start(task_context.task_id_from_filename(path.name))

    if workflow_task.query_control(content):
        if not already_marked:
            _mark_handled(path)
        threading.Thread(target=workflow_task.perform_control, args=(content,), daemon=True).start()
        return

    # If what it is waiting on is an on-screen action the
    # approval gate stopped, this reply is what settles it:
    # a yes becomes a one-shot grant for exactly that action,
    # anything else withdraws it. Done here, before the
    # harness sees the reply, so whichever engine repeats
    # the action finds the grant already in place.
    outcome = None
    try:
        approval.note_reply(content, asked=state.last_query_text)  # (the latest reply: the next gated action's yes when it says so)
        outcome = approval.resolve_reply(content, asked=state.last_query_text if state.last_query_kind == "approval"
                                         else "")
        if outcome:
            log(f"On-screen approval {outcome} by the human's reply.")
    except OSError as exc:
        log(f"Could not settle the on-screen approval: {exc}")

    if not already_marked:
        _mark_handled(path)

    # What a search failed to find binds its field until a person
    # has spoken (core: "Nothing else in its place"). They have.
    try:
        from decurix_agent.computer import core as _core
        _core.forget_failed_search()
    except Exception as exc:
        log(f"Could not clear the failed search: {exc}")

    # A paused replay, or a taught skill's question, takes its
    # own reply; it is still a reply either way, which is all
    # tasks.Plan mirrors.
    # What the person just told the worker is kept for later
    # tasks (lessons.py) - except the answers of a lesson being
    # taught, which the skill itself keeps, question by question.
    teaching = (state.paused_skill or {}).get("mode") == "learn"
    if not teaching:
        # A replay's or a skill run's own question (a value, a
        # stuck step); "is it done?" is not one of those.
        mechanical = (state.paused_replay is not None
                      or (state.paused_skill or {}).get("mode") == "run")
        try:
            lessons.note(state.last_query_text, content, kind=state.last_query_kind, outcome=outcome,
                         task=state.current_task or "", skill=skill_task.guided_skill_title(),
                         mechanical=mechanical)
        except Exception as exc:
            log(f"Could not keep what the person said: {exc}")

    if state.paused_replay is not None or state.paused_skill is not None:
        # A paused replay or taught skill reads "stop" itself: a lesson
        # keeps its draft and says how to carry on, a run records that
        # it was stopped and where. Inside a /workflow stage the stop
        # also ends the workflow, before that report is written, so the
        # report is shown rather than taken for the stage's result and
        # the next stage started.
        if guidance.calls_off(content, guidance.open_question()) and workflow_task.has_active():
            workflow_task.cancel_active()
        if state.paused_replay is not None:
            target, args = replay_task.resume, (content, outcome)
        else:
            target, args = skill_task.resume, (content, outcome)
    else:
        guidance.note_exchange(guidance.open_question() or getattr(state, "last_query_text", ""), content)
        guidance.note_reply(content, outcome)
        # An answer to a question a taught task's engine asked
        # (not a yes or no to an approval) is kept on the skill.
        if outcome is None:
            try:
                skill_task.learn_answer(content)
            except Exception as exc:
                log(f"Could not keep the answer on the skill: {exc}")
        target, args = continue_after_query, (content,)
        # A reply that only calls the task off ends it here: the
        # engine is not handed it. Found live: told `stop` at a
        # question about which record to use, the engine took it
        # as an answer, ran twelve more commands and saved the
        # record with the entry the person had just been asked
        # about. (The engine's own paused job is dropped when the
        # next request starts.)
        if guidance.calls_off(content, guidance.open_question()):
            target, args = _called_off, (content,)

    if workflow_task.has_active():
        target, args = _run_handler, (target, args)

    threading.Thread(target=target, args=args, daemon=True).start()


def _perform_long(content, task_file):
    """
    A request that names several deliverables: planned in stages and carried
    to the end (long_task.py) - or, when the plan comes back as one stage,
    done as any other request.
    """

    if not long_task.perform(content, task_file):
        _perform_following_procedure(content, task_file)


def _start_request(path, content):
    """A new request, with the agent's slot already claimed for it: say which task it is, and hand it to its handler."""

    # A reply that finds nothing waiting for it answers a question
    # an earlier life of this agent asked: the desktop restarted in
    # between. Run as a request it was handed to the engine as a
    # task of its own ("keep" put to TheBot as a chat message). The
    # slot claimed above keeps anything else from starting while
    # this is said, outside the lock.
    if is_reply_file(path.name):
        _mark_handled(path)
        _answer_stale_reply(path, content)
        return

    # The slot was free, so a workflow stage still pointed at is not being worked on: it is closed as unfinished, or it
    # would take this request's report as its own (review 2026-11-03, E-05).
    workflow_task.drop_stale()

    # A bare "stop" with nothing running or waiting has nothing to stop. Found live (2026-10-15): sent after the
    # desktop restarted under an open question, it went to TheBot as a task, the engine answered "Stopped.", and
    # the final check asked the person what to do about a task that could not be finished.
    if guidance.calls_off(content) and not workflow_task.has_active():
        task_id = task_context.task_id_from_filename(path.name)
        task_context.start(task_id, request=content)
        _mark_handled(path)
        write_final_reply("Nothing is running or waiting on you, so there is nothing to stop.")
        return

    # Reports from here on belong to this request's task record,
    # and so does what the engine uses.
    task_id = task_context.task_id_from_filename(path.name)
    # A .zip a person attached (a folder of files, past the control plane's ten-file cap) is unpacked, and the request
    # names the files it holds: from here on it is a request that brought those files.
    content, unpacked = attachments.expand_archives(content)
    task_context.start(task_id, request=content)
    task_context.mark_active(task_id, content)
    # The browser is not kept running on an idle desktop; it starts now, alongside the work (browser.warm).
    browser.warm_soon()
    for note in unpacked:
        progress.emit(note)
    attachments.keep_originals(content, task_id)
    # The secrets the request spells out are remembered (as hashes) before anything can echo them (privacy.py).
    try:
        from decurix_agent.computer import privacy
        privacy.remember_secrets(attachments.text(content))
    except Exception as exc:
        log(f"Could not note the request's secrets: {exc}")
    # On a desktop with a shared folder, the person's files are noted (and copied) before anything can touch them:
    # a source the task changes is put back and reported (source_guard.py).
    try:
        from decurix_agent import source_guard
        source_guard.take(content, task_id, notify=progress.emit)
    except Exception as exc:
        log(f"Could not note the shared folder's files before the task: {exc}")
    usage.reset()
    playbooks.begin()
    try:
        from decurix_agent import methods
        methods.begin()
    except Exception as exc:  # what is learned for later must never cost this task
        log(f"Could not start the task's method record: {exc}")

    _mark_handled(path)

    # A new task: nothing an earlier one was approved for, or was
    # stopped on, carries over into it.
    try:
        approval.reset()
    except OSError as exc:
        log(f"Could not clear on-screen approvals: {exc}")
    guidance.end()

    # "/replay <recording>" runs a recording, "/record <task>"
    # records a person doing it, "/learn <recording>" teaches a
    # skill from one and "/skill <name>" runs it; none of them
    # goes to an engine.
    if workflow_task.handles(content):
        target = workflow_task.perform
    elif replay_task.parse(content):
        target = replay_task.perform
    elif record_task.parse(content) is not None:
        target = record_task.perform
    elif skill_task.handles(content):
        target = skill_task.perform
    elif skill_task.mimics(content):
        # A task a person recorded, asked for again with other
        # values (`google "x"` after a ticket for `google "y"`),
        # is done the way they showed - their steps, with only
        # what differs changed - not worked out again by an
        # engine (computer/mimic.py).
        target = skill_task.perform_mimic
    elif state.backend_ready and long_task.looks_long(content):
        # Several deliverables in one request: a plan of stages, each its own
        # job, carried to the end (long_task.py). Decided in code; a short
        # request never gets here.
        target = _perform_long
    elif state.backend_ready and tool_task.handles(content):
        # A kind of request this desktop has done by hand, written a
        # script for, proven it and used it: the script does it, and
        # the engine only when it cannot (tool_task.py).
        target = tool_task.perform
    elif not _asks_a_web_app(content) and state.backend_ready and skill_task.may_be_taught(content):
        # Not the recorded ticket's words, but maybe its task said
        # another way: with a hosted model, the model reads the
        # request against what was taught here, and the engine
        # gets it when it is none of those (request_reading.py).
        target = skill_task.perform_mimic
    elif _asks_a_web_app(content):
        # `chatgpt "where is x"`, `google x`, `search youtube for x`:
        # the request is the whole plan - open the service with the
        # question, read its answer off the page (ask_task.py).
        target = ask_task.perform
    else:
        target = perform_task if not state.backend_ready else _perform_following_procedure

    if state.backend_ready and target in (_perform_following_procedure, tool_task.perform) and \
            long_task.may_follow_up(content):
        # A short message just after a planned run was reported may change that work (long_task.follow_up): read on
        # the task's own thread, never this one - it asks the model.
        target = _follow_up_or(target)

    thread = threading.Thread(
        target=_guarded(target, task_id),
        args=(content, path),
        daemon=True,
    )

    thread.start()
    return True


def _release_slot(content):
    """Give the task slot back when it is still held for `content` (claimed in _pass)."""

    with state.state_lock:
        if state.current_task is content or state.current_task == content:
            state.current_task = None


def _guarded(target, task_id):
    """
    `target` on the request's own thread, with the slot given back if it raises: the request is reported as stopped
    on an internal error - not done, and not checked - and the next one can run (review 2026-11-02: an exception in a
    handler left "Task already running" for the life of the agent).
    """

    # (Exception only: KeyboardInterrupt and SystemExit end the agent and are never swallowed here - the slot is given
    # back and they go on.)
    def release():
        if task_context.current() in ("", task_id):
            with state.state_lock:
                state.current_task = None

    def run(content, path):
        try:
            target(content, path)
        except Exception as exc:  # noqa: BLE001 - a handler's error is reported, never the end of the agent
            try:
                log(f"The request's handler stopped on an error: {exc!r}\n{traceback.format_exc()}")
                if state.current_task is not None and task_context.current() in ("", task_id):
                    write_final_reply(f"That stopped on an internal error ({type(exc).__name__}) - it is NOT done. "
                                      "Look at what it left before sending it again.", kind="error", gated=True,
                                      task_id=task_id or None)
            except Exception as again:  # noqa: BLE001
                try:
                    log(f"Could not report the handler's error: {again!r}")
                except Exception:  # noqa: BLE001
                    pass
            finally:
                release()
        except BaseException:
            release()
            raise
    run.__wrapped__ = target  # (inspect.unwrap(thread target) is the handler chosen for the request)
    return run


def _follow_up_or(target):
    """`target`, unless the message changes the planned run reported shortly before: then that change is planned and
    carried as a request of its own (the original request, the report, the change)."""

    def run(content, path):
        followed = long_task.follow_up(content)
        if followed:
            progress.emit("This changes the work I reported - redoing what it affects.")
            # The task's request is now the whole of it - the original request (its folders and files), the report and
            # the change: every check a command makes reads its inputs from it (series 16 H3, 2026-10-21: with only
            # the person's one line, office email read every file of the desktop for each draft).
            # It carries on the reported task: the drafts that task made are this one's to replace (tools/outbox.py).
            task_context.start(task_context.current(), request=followed, continues=long_task.reported_task())
            _perform_long(followed, path)
        else:
            target(content, path)
    return run


def command_watcher():
    """
    Continuously watch command/ for new task files.
    """

    global _heartbeat, _in_flight

    log(
        f"Watching command directory: "
        f"{config.COMMAND_DIR}"
    )

    _report_interrupted()

    # A task's outcome the control plane could not be told (it was restarting) is sent again, now and every little
    # while, so no chat is left saying "running" over a task that ended (messaging.resend_unsent).
    threading.Thread(target=resend_unsent_loop, daemon=True, name="resend-unsent").start()

    while True:

        _heartbeat = time.monotonic()
        _in_flight = None

        try:
            _pass()
        except BaseException as exc:
            # Nothing may end this loop: it is the only way a request reaches
            # an engine. Logging itself can fail (a full /tmp), so that must
            # not escape either.
            try:
                log(f"Command watcher error: {exc!r}\n{traceback.format_exc()}")
            except BaseException:
                pass

        time.sleep(1)

