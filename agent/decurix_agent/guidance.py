# ============================================================
# A taught task handed to the engine
#
# While a skill is still being learned (experience.stage), or when its replay
# got stuck, the engine does the task with the skill as its guide
# (skill_task._by_model). What it is told - the procedure, this run's
# values, how to look at the demonstration - is kept here for the life of the
# task, and the engine puts it into what its model reads (thebot_harness's
# standing context). The same record is what
# messaging's final gate checks the engine's "done" against before anyone is
# told it succeeded (skill_task._gate).
#
# Module state, like record_task's ticket: one task at a time runs, and a new
# task (watchers) clears it.
#
# One part of it is also kept in a file: this run's values. The engine works
# the screen through `gui`, a process of its own, and when it replays the
# recording or the skill it was handed (`gui replay ID`, `gui do NAME`) the
# replay must type this run's values - not the ones the person typed when
# they recorded it. Found live: asked for an order for "Globex" in Germany,
# matched to a recording of one for "Initech" in Japan, the engine ran
# `gui replay` on the recording as it was and saved a second order for
# Initech before it did the one it was asked for.
# ============================================================

import json
import re

from decurix_agent import config, task_context
from decurix_agent.logging_utils import log

_current = None

RUN_FILE = config.DECURIX_DIR / "computer" / "guided-run.json"


def begin(skill, task, text, values=None, mode="model", **extra):
    """Hand `skill` to the engine for `task`; `text` is what its model is told."""

    global _current
    _current = dict({"skill": skill, "task": task, "text": text, "values": dict(values or {}), "mode": mode,
                     "attempt": 1}, **extra)

    run = extra.get("run") or {}
    _keep({
        "task_id": task_context.current(),
        "skill": skill,
        # A task done from a recording nobody has taught yet (skill_task's one-off).
        "recording": (run.get("mimic") or {}).get("id") if run.get("one_off") else None,
        "values": dict(values or {}),
    })

    return _current


def get():
    return _current


# A person's "no" to an approval this task asked for: (task id, their words).
_refused = None

# A reply that only calls the task off, whatever was asked.
STOP_ONLY = re.compile(r"^\W*(stop|cancel|abort|quit)(\s+(it|this|that|the task|now|please))*\W*$", re.IGNORECASE)
# Words that call a task off only when nothing was asked: to an open question ("mark it B2C, or leave it?") they are an
# ordinary answer. Found (audit, 2026-10-24): "leave it" to an either/or question called the whole task off.
LETTING_GO = re.compile(r"^\W*(never ?mind|leave it|forget it)(\s+(it|this|that|the task|now|please))*\W*$", re.IGNORECASE)


def calls_off(reply, question=""):
    """
    Whether a message, whole, only calls the task off. A plain stop always does. "Never mind" and its kind do when no
    question is open (question: the one open, "" for none); to an open question they are read once against it
    (approval.read_choice) - an answer unless the reading says they end the task. With no model: an answer.
    """

    text = str(reply or "")
    if STOP_ONLY.match(text):
        return True
    if not LETTING_GO.match(text):
        return False
    if not str(question or "").strip():
        return True
    from decurix_agent.computer import approval
    said = approval.read_choice(text, question, {
        "stop": "they call the whole task off",
        "answer": "it answers the question (one of the options it offers, such as leaving that one thing out)",
    })
    return said["choice"] == "stop"


def open_question():
    """The question the person was last asked and has not answered yet, "" when none is open."""

    from decurix_agent import state
    return (getattr(state, "last_query_text", "") or "") if getattr(state, "waiting_for_query", False) else ""


# What the person was asked during the task being worked on, and what they answered (note_exchange).
_exchanges = {"task": None, "said": []}
MAX_EXCHANGES = 12


def note_exchange(asked, reply, judgement_id="", secret=False):
    """
    Keep one question of this task with the person's reply, for the final check (said_this_task), and on disk for the
    task's other processes: the question, the reply (withheld like every store), the reply's judgement on the control
    plane (`judgement_id`, captured with this very reply) and its text's hash. What is on disk is the worker's own
    user's file: it backs nothing by itself - person_record.answers confirms each row from the control plane, and the
    text it then uses is the control plane's (review round 10, R10-B1). A secret reply carries no hash.
    """

    tid = task_context.current()
    if _exchanges["task"] != tid:
        _exchanges.update(task=tid, said=[])
    reply = " ".join(str(reply or "").split())
    if reply:
        _exchanges["said"].append((" ".join(str(asked or "").split())[:400], reply[:800]))
        del _exchanges["said"][:-MAX_EXCHANGES]
        try:
            from decurix_agent import person_record
            from decurix_agent.computer import evidence, privacy
            if tid:
                evidence.NOTES_DIR.mkdir(parents=True, exist_ok=True)
                row = {"asked": " ".join(str(asked or "").split())[:400], "reply": reply[:800],
                       "judgement": str(judgement_id or "") if task_context.valid_judgement(judgement_id) else ""}
                if row["judgement"] and not secret:
                    row["sha256"] = person_record.text_hash(reply)
                privacy.write_scrubbed(evidence.NOTES_DIR / f"{tid}.said", row, append=True, end="\n",
                                       exempt=("sha256", "judgement"))
        except Exception as exc:  # noqa: BLE001 - the in-process copy stays
            log(f"guidance: could not keep the person's answer for the task's other processes: {exc}")


def kept_replies(task_id=None):
    """The task's kept replies as rows {asked, reply, judgement, sha256}, from every process ([] when none). Rows of
    the worker's own file: person_record.answers confirms them; nothing here backs anything."""

    tid = task_id or task_context.current()
    out = []
    try:
        from decurix_agent.computer import evidence
        for line in (evidence.NOTES_DIR / f"{tid}.said").read_text(encoding="utf-8").splitlines():
            try:
                data = json.loads(line)
            except ValueError:
                continue
            if isinstance(data, dict) and data.get("reply"):
                out.append({k: str(data.get(k) or "") for k in ("asked", "reply", "judgement", "sha256")})
    except (OSError, TypeError):
        pass
    return out


def exchanges(task_id=None):
    """[(question, reply)] of the task, as kept on disk by every process ([] when none)."""

    return list(dict.fromkeys((r["asked"], r["reply"]) for r in kept_replies(task_id)))


def answers(task_id=None):
    """The person's answers during the task, as one text, as kept on disk ("" when none). Not a source a pick may
    rest on: that is person_record.answers, confirmed from the control plane."""

    return "\n".join(dict.fromkeys(reply for _, reply in exchanges(task_id)))


def forget_exchanges(task_id):
    """The task is over: its kept questions and answers go with its other secret stores (privacy.clear_literals)."""

    try:
        from decurix_agent.computer import evidence
        (evidence.NOTES_DIR / f"{task_id}.said").unlink()
    except (OSError, TypeError):
        pass
    if _exchanges["task"] == task_id:
        _exchanges.update(task=None, said=[])


def current_skill():
    """The skill the task being worked on runs: this process's guided run's, else the guided-run file's (a `gui`
    command's process has no guided run of its own; the final gate, in the agent's process, reads its own). None."""

    if _current and _current.get("skill"):
        return _current["skill"]
    try:
        kept = json.loads(RUN_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return kept.get("skill") if isinstance(kept, dict) and kept.get("task_id") == task_context.current() else None


def said_this_task():
    """
    What the person said during this task, for the final check's judge: it decides over the request where they
    disagree, and what they asked for is part of the work. "" when nothing was said. Found live (2026-10-25): told
    "every bill must have its rows ... (454 two rows)", the worker reported done three minutes later with one row,
    and the final check - shown only the request - passed it.
    """

    if _exchanges["task"] != task_context.current() or not _exchanges["said"]:
        return ""
    return ("What the person said during this task - it decides over the request where they disagree, and what they "
            "asked for is part of the work:\n" + "\n".join(f"- asked: {q} - they answered: {a}"
                                                            for q, a in _exchanges["said"]))


# A no that is "not yet": the person asks for something first and says to come back. Found live (2026-10-27): "No -
# do the serial check first ... Once you have told me that, ask again" - the worker did the check and asked again,
# and the harness closed the task as "Stopped, as you said" with the return unfiled.
_NOT_YET = re.compile(r"(?i)\b(first\b|before (you|that|filing|sending|pressing|clicking)|then ask|ask (me )?again|"
                      r"come back|not yet|once you|after (you|that)|until you)")


def not_yet(reply):
    """Whether a refusal asks for something first and the action again after: the gate's own reading says so (an
    "if" with a condition, or something the person wants to see). A reading that is a plain no (or yes) decides: the
    words never turn it into "not yet" (review 2026-11-03, E-28: "No, not yet - I will do it myself first" read as a
    no by the model was overridden by a word, and the gate could send the worker back to do what the person stopped).
    Only with no reading at all, or an unclear one, do the words decide."""

    from decurix_agent.computer import approval

    text = " ".join(str(reply or "").split())
    try:
        said = approval.last_reading(text) or {}
    except Exception:  # noqa: BLE001 - no reading kept: the words decide
        said = {}
    if said.get("decision") == "if" or said.get("wants"):
        return True
    if said.get("decision") in ("no", "yes"):
        return False
    return bool(_NOT_YET.search(text))


def note_reply(reply, outcome):
    """
    What a person answered to an approval the screen gate asked for (the
    watcher tells it before the engine sees the reply). A refusal is kept for
    the task's final check; anything the person says after it - a yes to a
    later approval, an instruction, an answer - means the task goes on, and
    takes it back.
    """

    global _refused

    from decurix_agent.computer import approval

    # A bare "stop" to any question of the task ends it the same way. Found
    # live: told `stop` at a question, the engine wound up, the final check
    # found the work unfinished - and sent it back to finish what the person
    # had just called off.
    if (outcome == "declined" and approval.is_refusal(reply) and not not_yet(reply)) or calls_off(reply, open_question()):
        _refused = (task_context.current(), " ".join(str(reply).split())[:300], _screen_actions())
    else:
        _refused = None


def _screen_actions():
    """
    How much work the task being worked on has recorded so far: its screen actions and the outputs of its other
    commands (a workbook corrected is work as much as a click) - None when that cannot be read. Found live
    (2026-10-23): told "No - fix the workbook first", the worker fixed the workbook, and with only screen actions
    counted the task was still closed as "Stopped there".
    """

    try:
        from decurix_agent.computer import evidence
        task_id = task_context.current()
        done = len(evidence._task_events(task_id))
        try:
            with open(evidence.NOTES_DIR / f"{task_id}.outputs", encoding="utf-8") as f:
                done += sum(1 for _ in f)
        except OSError:
            pass
        return done
    except Exception:  # noqa: BLE001 - unknown: the refusal stands as said
        return None


def refused():
    """
    The person's words when they said no to an approval of the task being worked on, else None. A no ends the task
    only when the work stopped there: found live (2026-10-23), "No - not yet. Open that document
    ... then redo the table" was taken for calling the task off (no word of a word list said otherwise), the worker
    went on as told - and the final check closed the task as "Stopped there, as you said". Work done on the screen
    after the no shows the task went on with what the person said: the no was that action's, not the task's.
    """

    if _refused is None or _refused[0] != task_context.current():
        return None
    words, at = _refused[1], (_refused[2] if len(_refused) > 2 else None)
    now = _screen_actions()
    if at is not None and now is not None and now > at:
        return None
    return words


def clear_refusal():
    """The refusal has been acted on for its own part of the task (one stage of a long request): the rest goes on."""

    global _refused
    _refused = None


def text():
    return (_current or {}).get("text") or ""


def end():
    global _current
    _current = None

    try:
        RUN_FILE.unlink()
    except OSError:
        pass


def _keep(record):
    try:
        RUN_FILE.parent.mkdir(parents=True, exist_ok=True)
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(RUN_FILE, record, exempt=("values",))  # (the taught values are compared later)
    except (OSError, TypeError, ValueError):
        pass


def run_values(skill=None, recording=None):
    """
    This run's values for the skill or recording an engine is about to replay:
    what it was handed for the task it is on now, or {} when it was handed
    nothing, something else, or for another task. Read from any process.
    """

    kept = _kept()
    if kept is None:
        return {}

    if (skill and kept.get("skill") == skill) or (recording and kept.get("recording") == recording):
        values = kept.get("values")
        return dict(values) if isinstance(values, dict) else {}

    return {}


def handed(skill):
    """
    Whether the task being worked on was handed to the engine as this skill
    (by the person, or by the reading of their request). Read from any process.
    """

    kept = _kept()
    return kept is not None and kept.get("skill") == skill


def _kept():
    """The run kept in RUN_FILE when it is the task being worked on's, else None."""

    try:
        kept = json.loads(RUN_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not isinstance(kept, dict) or not kept.get("task_id") or kept.get("task_id") != task_context.current():
        return None
    return kept


def taught_values_check(skill, given):
    """
    What to say instead of replaying `skill` with `given`, or "" to go ahead: a value the engine passed that is the
    person's own from when they taught it (the parameter's example), that the harness did not hand this run, and that
    nothing the task has been given or has read holds - the request, the files read, what its commands printed, the
    pages the tools read. Found live (2026-10-22): asked for another client's return "the way I showed
    you", the engine passed every figure of the teaching client's return and never opened this client's workbook; the
    replay typed them faithfully. Said once for a value in a task: the same value passed again (the person may have
    given it) goes ahead, and the final check still traces what the work states.
    """

    task = task_context.current()
    if not task or not given:
        return ""
    handed_values = run_values(skill=skill.get("name"))
    params = {p.get("name"): p for p in skill.get("params") or [] if p.get("varies", True) and not p.get("list")}
    suspects = {}
    for name, value in given.items():
        param = params.get(name)
        if param is None or str(handed_values.get(name, "")).strip() == str(value).strip():
            continue
        from decurix_agent import binding
        if binding.is_example(value, param.get("example")):
            suspects[name] = str(value)
    if not suspects:
        return ""

    from decurix_agent import long_task
    from decurix_agent.computer import evidence
    try:
        sources = long_task.work_sources(task_context.current_request())
    except Exception as exc:  # noqa: BLE001 - a check that cannot be made stops nothing
        log(f"guidance: could not gather this task's sources: {exc}")
        return ""
    unheld = {name: value for name, value in suspects.items() if sources.unsourced(value)}
    if not unheld:
        return ""

    told_path = evidence.NOTES_DIR / f"{task}.examples"
    try:
        told = set(json.loads(told_path.read_text(encoding="utf-8")))
    except (OSError, ValueError, TypeError):
        told = set()
    keys = {f"{skill.get('name')}|{name}|{value}" for name, value in unheld.items()}
    if keys <= told:
        return ""
    try:
        told_path.parent.mkdir(parents=True, exist_ok=True)
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(told_path, sorted(told | keys))
    except OSError:
        pass

    same = ", ".join(f"{name}={value}" for name, value in suspects.items())
    return (f"Not replayed: {', '.join(f'{n}={v}' for n, v in unheld.items())} "
            f"{'is' if len(unheld) == 1 else 'are'} what the person typed when they TAUGHT this, from their own example "
            "document - nothing this task has read holds it. This run's values come from this run's own source (the "
            "document, workbook or page this request is about): read it first (office read PATH, or gui observe), work "
            "out any total with a command, then pass what it says. "
            + (f"Every value you passed that equals the taught example is suspect: {same}. " if len(suspects) > len(unheld) else "")
            + "If no source of this task holds a value, ask the person (HELP) instead of using the example.")


def persons_words():
    """
    What a person has said that bears on the task being worked on, as text:
    the lesson of the skill it was handed as (their explanation, what was
    agreed, their answers and later corrections), what they answered in
    earlier tasks (the worker's memory) and the request itself. Read from any
    process; "" when nothing is known.
    """

    parts = [task_context.current_request()]

    try:
        kept = json.loads(RUN_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        kept = None
    name = kept.get("skill") if isinstance(kept, dict) and kept.get("task_id") == task_context.current() else None
    if name:
        try:
            from decurix_agent.computer import skills
            skill = skills.load(name) or {}
            parts.append(skills.explanation(skill) or "")
            for key in ("understanding", "answers", "dialogue", "corrections", "run_answers"):
                if skill.get(key):
                    parts.append(json.dumps(skill[key], ensure_ascii=False))
        except Exception:  # a skill that cannot be read says nothing
            pass

    try:
        from decurix_agent import memory_context
        parts.append(memory_context.get_memory_context() or "")
    except Exception:
        pass

    return "\n".join(part for part in parts if part)


def with_run_values(given, params, skill=None, recording=None):
    """
    `given` (what an engine passed a replay) with this run's own value for
    every parameter of `params` it did not give. A value the engine gave is
    kept: it may be working one item of a list.
    """

    mine = run_values(skill=skill, recording=recording)
    names = {p.get("name") for p in params or []}
    out = dict(given or {})

    for name, value in mine.items():
        if name in names and name not in out and value not in (None, ""):
            out[name] = value

    return out
