import re
import threading
import traceback

from decurix_agent import state, usage
from decurix_agent.computer import approval, replay
from decurix_agent.logging_utils import log
from decurix_agent.messaging import ask_for_query, write_final_reply


# ============================================================
# A replay as a task
#
# "/replay <recording>" sent to a desktop - by the recordings page's Replay
# button, or typed into the chat - runs that recording (computer/replay.py)
# rather than an engine. The command watcher dispatches it like any other
# request, so it queues behind a running task, carries its task ID, and
# reports its steps, pauses and outcome through the same messaging calls
# every harness uses. The task records show it as engine "replay", with no
# model and no tokens.
#
# This is not a harness: harness.py still decides which engine answers
# everything else, and nothing here asks which one that is.
# ============================================================

ENGINE = "replay"

COMMAND_RE = re.compile(r"^/replay\s+(\S+)(.*)$", re.DOTALL)
OPTION_RE = re.compile(r"""([A-Za-z_]\w*)=(?:"([^"]*)"|'([^']*)'|(\S+))""")

# A reply that stops a replay paused on a step a person had to do by hand (kept for its plain words; what a reply
# decides is read by by_hand_choice).
STOP_RE = re.compile(r"^\s*(no\b|stop|cancel|abort|don'?t|do not|never ?mind)", re.IGNORECASE)

# What a reply to "do this step by hand, then tell me" decides, read once (approval.read_choice). The word list above
# read "no problem, done" and "don't worry, logged in, carry on" as stop, and "not yet, the code hasn't come" as done
# (audit, 2026-10-24). Code decides only a reply that is, whole, one of these phrases.
BY_HAND_CHOICES = {
    "done": "they did the step, so the replay goes on",
    "not_done": "the step is not done yet, or could not be done - the replay must wait",
    "stop": "stop the replay here",
}
BY_HAND_PLAIN = {
    "done": ["done", "it's done", "its done", "it is done", "all done", "did it", "i did it", "done it", "ok", "okay",
             "yes", "carry on", "continue", "go on", "go ahead", "proceed", "resume", "finished", "next"],
    "stop": ["no", "stop", "stop it", "stop the replay", "cancel", "cancel it", "abort", "don't", "do not", "quit",
             "never mind", "nevermind"],
    "not_done": ["not yet", "wait", "not done", "not done yet", "hold on", "one minute", "give me a minute"],
}


def by_hand_choice(reply, asked=""):
    """What a reply to a step a person was asked to do by hand decides: {"choice": done|not_done|stop|unclear, ...}."""

    return approval.read_choice(reply, asked or "Do this step by hand on the screen, then tell me to carry on.",
                                BY_HAND_CHOICES, BY_HAND_PLAIN)

_held = {}
_held_lock = threading.Lock()


def parse(content):
    """(session, params, start) of a "/replay" request, or None for any other request."""

    match = COMMAND_RE.match((content or "").strip())
    if not match:
        return None

    params, start = {}, 1

    for option in OPTION_RE.finditer(match.group(2)):
        name = option.group(1)
        value = next(v for v in option.groups()[1:] if v is not None)
        if name == "from":
            start = int(value) if value.isdigit() else 1
        else:
            params[name] = value

    return match.group(1), params, start


def hold_params(task_id, params):
    """
    Keep a replay's parameters in memory until the replay runs. They come
    with the control plane's request and never go into the command file, the
    chat or the task record, so a password given for a replay is written down
    nowhere.
    """

    with _held_lock:
        _held[task_id] = {str(k): str(v) for k, v in (params or {}).items()}


def _take_params(task_id):
    with _held_lock:
        return _held.pop(task_id, {})


def perform(content, task_file=None):
    """Run a "/replay" request as the task the watcher dispatched it as."""

    state.begin(content, task_file)
    state.paused_replay = None

    # Known, not unknown: a replay reads no prompt and costs nothing.
    usage.add(input_tokens=0, output_tokens=0, cost_usd=0)

    session_id, params, start = parse(content)
    params.update(_take_params(state.current_task_id))

    try:
        _, program = replay.load_program(session_id)
    except (FileNotFoundError, ValueError):
        write_final_reply(f"There is no recording {session_id} on this desktop.", kind="error", engine=ENGINE)
        return

    _run(program, params, start)


def _run(program, params, start):
    try:
        result = replay.run(program, params=params, start=start)
    except Exception as exc:
        log(traceback.format_exc())
        write_final_reply(f"The replay stopped on an internal error: {exc}", kind="error", engine=ENGINE)
        return

    log(f"Replay of {program['session']} ended {result['outcome']}: {result['text']}")

    if result["outcome"] in (replay.NEEDS_APPROVAL, replay.NEEDS_HUMAN):
        state.paused_replay = {
            "program": program,
            "params": params,
            "outcome": result["outcome"],
            "step": result["step"],
            "resume_from": result["resume_from"],
            "text": result["text"],
        }
        kind = "approval" if result["outcome"] == replay.NEEDS_APPROVAL else "query"
        ask_for_query(result["text"], kind=kind, engine=ENGINE)
        return

    if result["outcome"] == replay.COMPLETED_UNVERIFIED:
        # Ran to the end without seeing every result: not a success until it is
        # shown to be one (skill_task.check_unchecked; imported here, as
        # skill_task imports this module).
        from decurix_agent import skill_task
        skill_task.check_unchecked(result["text"], engine=ENGINE)
        return

    kind = "final" if result["outcome"] in replay.SUCCESS else "error"
    write_final_reply(result["text"], kind=kind, engine=ENGINE)


def resume(reply, approval_outcome=None):
    """
    The human's reply to a paused replay. The watcher has already settled an
    on-screen approval with it (approval_outcome is "granted", "declined" or
    None), so the step the replay stopped on goes through when it repeats it.
    """

    paused = state.paused_replay
    state.paused_replay = None
    state.waiting_for_query = False

    if paused is None:
        write_final_reply("There was no paused replay to continue.", kind="error", engine=ENGINE)
        return

    if paused["outcome"] == replay.NEEDS_APPROVAL:
        stop = approval_outcome == "declined" or (approval_outcome is None and not approval.says_yes(reply))
    else:
        said = by_hand_choice(reply, paused.get("text") or "")
        if said["choice"] not in ("done", "stop"):
            # Not done yet, or not to be told: the replay waits on the same step and the person is asked again - a step
            # nobody did is never carried on from.
            state.paused_replay = paused
            ask_for_query("I will carry on once that step is done - tell me when it is, or say stop."
                          + (f"\n\n{paused['text']}" if paused.get("text") else ""), kind="query", engine=ENGINE)
            return
        stop = said["choice"] == "stop"

    if stop:
        # A stop is not a finished task (review 2026-11-03, E-36): it is reported as not done, never "final".
        write_final_reply(
            f"Stopped, as you said: the replay stopped before step {paused['step']}. Nothing from that step on was "
            f"done.", kind="error", engine=ENGINE, checked=False, gated=True,
        )
        return

    _run(paused["program"], paused["params"], paused["resume_from"])
