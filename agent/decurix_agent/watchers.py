import sys
import threading
import time
import traceback

from decurix_agent import (ask_task, attachments, config, guidance, lessons, playbooks, procedures, progress, record_task, replay_task,
                           skill_task, state, task_context, tool_task, usage)
from decurix_agent.computer import approval
from decurix_agent.harness import continue_after_query, perform_task
from decurix_agent.logging_utils import log
from decurix_agent.messaging import REPLY_MARK, send_query, write_final_reply


def _called_off(reply):
    """Ends the task a person called off at one of its questions, without another action."""

    state.waiting_for_query = False
    progress.emit("Stopping, as you said. Nothing more is done.")
    write_final_reply(f"I did nothing more after your \"{' '.join(str(reply).split())[:100]}\". "
                      f"Anything done before the question is still as it was left.")


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


def _needs_no_backend(content):
    """Whether a request is handled without the reasoning backend ("/replay", "/record", "/learn", "/skill", a recorded task done again, or a question for a web app)."""

    return (bool(replay_task.parse(content)) or record_task.parse(content) is not None or skill_task.handles(content)
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
        return state.paused_replay is not None or state.paused_skill is not None

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

    log(f"Task {record['task_id']} was interrupted by a restart; reporting it.")
    task_context.start(record["task_id"], request=record.get("request"))
    state.current_task = record.get("request") or None
    note = INTERRUPTED
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

        # A task queued before a question was asked is not its answer.
        # Only messages marked as replies at receipt may resume that task.
        if state.waiting_for_query and not is_reply_file(path.name):
            continue

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

            # A reply belongs to the paused task. A desktop whose
            # task started before the control plane sent IDs takes
            # the one its reply carries.
            if not state.current_task_id:
                task_context.start(task_context.task_id_from_filename(path.name))

            # If what it is waiting on is an on-screen action the
            # approval gate stopped, this reply is what settles it:
            # a yes becomes a one-shot grant for exactly that action,
            # anything else withdraws it. Done here, before the
            # harness sees the reply, so whichever engine repeats
            # the action finds the grant already in place.
            outcome = None
            try:
                outcome = approval.resolve_reply(content)
                if outcome:
                    log(f"On-screen approval {outcome} by the human's reply.")
            except OSError as exc:
                log(f"Could not settle the on-screen approval: {exc}")

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

            if state.paused_replay is not None:
                target, args = replay_task.resume, (content, outcome)
            elif state.paused_skill is not None:
                target, args = skill_task.resume, (content, outcome)
            else:
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
                if guidance.STOP_ONLY.match(content or ""):
                    target, args = _called_off, (content,)

            thread = threading.Thread(
                target=target,
                args=args,
                daemon=True,
            )

            thread.start()

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

        # A reply that finds nothing waiting for it answers a question
        # an earlier life of this agent asked: the desktop restarted in
        # between. Run as a request it was handed to the engine as a
        # task of its own ("keep" put to TheBot as a chat message). The
        # slot claimed above keeps anything else from starting while
        # this is said, outside the lock.
        if is_reply_file(path.name):
            _mark_handled(path)
            _answer_stale_reply(path, content)
            continue

        # Reports from here on belong to this request's task record,
        # and so does what the engine uses.
        task_id = task_context.task_id_from_filename(path.name)
        task_context.start(task_id, request=content)
        task_context.mark_active(task_id, content)
        attachments.keep_originals(content, task_id)
        usage.reset()
        playbooks.begin()

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
        if replay_task.parse(content):
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
            target = perform_task
            # Recurring work the person taught by saying how it is done: a request
            # that gives the procedure has it kept, and a later "as usual" gets it
            # back under its own words - for the engine, and for the check of
            # what it hands in (procedures.py).
            if state.backend_ready:
                applied = procedures.apply(content)
                if applied == content:
                    if procedures.sets_procedure(content):
                        threading.Thread(target=procedures.note, args=(content, task_id), daemon=True).start()
                else:
                    content = applied
                    with state.state_lock:
                        state.current_task = content

        thread = threading.Thread(
            target=target,
            args=(
                content,
                path,
            ),
            daemon=True,
        )

        thread.start()


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


# ============================================================
# Query text watcher (defunct, useful to have)
# ============================================================

def querytext_watcher():
    """
    Existing functionality:
    watches querytext/ and sends newly-created or
    modified .txt files to the control plane.

    This remains useful for agent-generated queries.
    """

    log(
        f"Watching querytext directory: "
        f"{config.QUERYTEXT_DIR}"
    )

    seen = {}

    while True:

        try:

            for path in config.QUERYTEXT_DIR.glob("*.txt"):

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
                        f"Could not read query file "
                        f"{path}: {exc}"
                    )
                    continue

                send_query(
                    path.name,
                    content,
                )

        except Exception as exc:
            log(
                f"Query text watcher error: "
                f"{exc}"
            )

        time.sleep(1)
