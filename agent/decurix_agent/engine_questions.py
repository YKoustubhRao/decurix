"""
A question an engine asks in the middle of a task: answered from what the person already said, or kept once they
answer it (docs/skills.md, docs/learning-and-verification.md).

Split out of skill_task.py (follow-up O-10): skill_task runs taught tasks and checks their reports; this module is
messaging's query gate (_question_gate) - a question already settled by the skill's lesson or the worker's kept
lessons is answered from them (at most MAX_SELF_ANSWERS a task, on a confident reading, never a check of this run's
own work), an engine that ran out of room carries on by itself (_carries_on) - and keeps what a person answers on the
skill (learn_answer); a run a person answered was helped, unless the answer is one the rules require on every run
(_required_every_run: the plan's yes as put, an OTP or other secret given and nothing more; note_approval: the gate's
yes).

This module never imports skill_task: what the lesson says for this run (_criteria, _for_this_run) and the question
it asks are skill_task's, handed over at its import (use_run).
"""

import json
import re
import threading
import time
import traceback

from decurix_agent import guidance, lessons, llm_client, messaging, progress, state, study, task_context
from decurix_agent.computer import skills
from decurix_agent.logging_utils import log
from decurix_agent.skill_commands import ABANDON_RE, SKILL_ENGINE


# ============================================================
# What this module calls back in skill_task
#
# What the lesson says for this run (_criteria, _for_this_run) and the question it asks (ask_for_query) are
# skill_task's, which imports this module; so skill_task hands them over at its import (use_run) instead of being
# imported here. Each is looked up in skill_task when called: a test that patches skill_task._criteria patches what
# this module reads.
# ============================================================

_RUN = {}


def use_run(**calls):
    """Take the calls this module makes back into skill_task from skill_task at its import."""

    _RUN.update(calls)


def ask_for_query(*args, **kwargs):
    """Ask the person a question; the task waits for the reply (skill_task.ask_for_query)."""

    return _RUN["ask_for_query"](*args, **kwargs)


def _criteria(*args, **kwargs):
    """What the person said about the task and what done looks like (skill_task._criteria)."""

    return _RUN["_criteria"](*args, **kwargs)


def _for_this_run(*args, **kwargs):
    """Text about the task in this run's values (skill_task._for_this_run)."""

    return _RUN["_for_this_run"](*args, **kwargs)


# ------------------------------------------------------------
# A question an engine asks in the middle of a task
#
# Found live (2026-10-04): taught "if several conversations match, choose the
# first one", the engine met several *people* matching in the new-message box
# and asked which. The person answered "always choose first" - and that went
# to the engine for that run only; nothing kept it, so the next run would ask
# again. Three things now:
#   - what a person answers is kept: on the skill, with its question, when the
#     task is a taught one (learn_answer), and in the worker's memory as a
#     lesson for every task (lessons.py, from the watcher);
#   - before a person is paged, the question is put to the hosted model beside
#     everything they have taught and answered - the skill's lesson and the
#     worker's kept lessons - and one that is already settled is answered from
#     it. Never an approval, never a question this module asks itself, at most
#     MAX_SELF_ANSWERS a task, and only on a confident reading;
#   - the engine is told the same in its own context (_procedure, and worker
#     memory), so it should rarely get as far as asking.
# ------------------------------------------------------------

MAX_SELF_ANSWERS = 2
MAX_RUN_ANSWERS = 12
SELF_ANSWER_CONFIDENCE = 0.8

# A question about this run's own work: the review's points (the soul's knowledge.go knowledgeQuestion), a report the
# final check refused (verify.py). Only the person answers these (_question_gate).
WORK_CHECKED = re.compile(r"An independent check of the work still questions|^\W*I (?:do not think|cannot confirm) "
                          r"this is done|An independent review of this work", re.IGNORECASE | re.MULTILINE)

# Who may be answered: an engine working a task. A question from a replay, a
# lesson, a skill's own run, a ticket or a web lookup is that module's, and
# its reply goes back to it, not to an engine.
ENGINES_THAT_ASK = (None, "", "thebot", "azure", "claude", "ollama")

ANSWERED_SYSTEM = """A computer agent working for a person wants to stop its task and ask that person a question. You are given what the person has already told it - what they taught about this task (their own words, the rules agreed, the questions they answered when teaching and during earlier runs) and the lessons kept from their answers in earlier tasks - and the agent's question. Decide whether the person has already answered it.

They have when they answered the same question, or the same kind of choice arising somewhere else (told to take the first of several matching conversations, the first of several matching people in another list is the same choice).

They have NOT when: nothing they said touches it; the earlier answer was about one task's own value (a particular person, amount, file or date) and this one's differs; the agent is asking for approval, a password, a sign-in, a code, a check that it is a human, or a value it was not given; or something has gone wrong (an error, a model or page that does not answer, a control it cannot find); or what the person said about this situation is to ask them - then asking is exactly what they want, and the question goes to them; or the agent says that the thing it needs (a record, an item, a customer, a file) is not among what the application offers, and asks which to use instead - a rule the person gave about what counts as the same kind of thing answers that only when it names this very thing and an entry that is offered; a general "use the closest one" never chooses a record for a thing the person has not spoken of. When in doubt they have not: the person is then asked, which costs little.

Reply with JSON only: {"answered": true, "answer": "<what the agent should do, in a sentence or two, in the person's terms>", "from": "<the person's words this rests on, quoted>", "confidence": 0.0-1.0} or {"answered": false}"""

_self_answered = {"task": None, "count": 0}

# An "answer" that only sends the question back to the person.
ASK_THEM = re.compile(r"\b(ask|check with|confirm with|wait for|refer (?:it )?to|escalate to)\s+(?:the\s+)?"
                      r"(person|user|human|me|them|him|her|you|someone)\b", re.IGNORECASE)


def guided_skill_title():
    """The taught task the engine is on right now, by its name for a person, or ""."""

    run = (guidance.get() or {}).get("run")
    if not run or run.get("one_off"):
        return ""
    return run["skill"].get("title") or run["skill"]["name"]


def _already_told(run):
    """Everything the person has said that could settle a question: this task's lesson, then the kept lessons."""

    parts = []
    if run is not None and not run.get("one_off"):
        taught = _for_this_run(_criteria(run["skill"]), run["skill"], run["values"])
        if taught.strip():
            parts.append(f"What the person taught about this task (\"{run['skill'].get('title') or run['skill']['name']}\"):\n{taught}")
    kept = lessons.known()
    if kept:
        parts.append(f"Lessons kept from the person's answers in earlier tasks:\n{kept}")
    return "\n\n".join(parts)


def _question_gate(problem, kind, engine):
    """messaging's query gate: True when what the person already said answered the engine's question and nobody was asked."""

    if (kind != "query" or engine not in ENGINES_THAT_ASK
            or state.paused_skill is not None or state.paused_replay is not None):
        return False

    # A planned run's stage takes its answers its own way: the stage runs again with the answer in its prompt
    # (long_task). An answer sent into the engine instead was taken as a new request - found live (2026-10-25): it
    # was answered "Understood ..." as a chat question, that became the stage's report, and the request's second
    # stage (a message to the client) never ran.
    from decurix_agent import long_task
    if long_task.active() or task_context.current_stage()[0]:
        return False

    # A check of this run's own work (the review's points, a report the final check refused) is never answered from
    # lessons: a rule kept from another run cannot vouch for this run's figures. Found live (2026-10-25): "an
    # independent check still questions these" - a bill left out - was answered from a lesson about which files to
    # ignore, nobody was asked, and the report went out a bill short.
    if WORK_CHECKED.search(problem or ""):
        return False

    if _carries_on(problem):
        return True

    guided = guidance.get()
    run = (guided or {}).get("run")

    # What the person's reply, if they are asked, will be the answer to.
    if run is not None:
        guided["asked"] = problem

    task = task_context.current() or state.current_task
    if _self_answered["task"] != task:
        _self_answered.update(task=task, count=0)
    if _self_answered["count"] >= MAX_SELF_ANSWERS or not llm_client.available():
        return False

    told = _already_told(run)
    if not told:
        return False

    values = f"This run's values: {json.dumps(run['values'], ensure_ascii=False)}\n\n" if run is not None else ""
    try:
        data = study._call(ANSWERED_SYSTEM, f"{told}\n\nThe task: {(state.current_task or '')[:600]}\n\n{values}"
                                            f"The agent's question:\n{problem[:2000]}", max_tokens=400)
    except (llm_client.LLMError, ValueError) as exc:
        log(f"Could not check the question against what the person already said: {exc}")
        return False

    answer = " ".join(str(data.get("answer") or "").split())[:500]
    try:
        confidence = float(data.get("confidence") or 0)
    except (TypeError, ValueError):
        confidence = 0.0
    if data.get("answered") is not True or not answer or confidence < SELF_ANSWER_CONFIDENCE:
        return False
    # "Ask the person" is not an answer to a question for the person. Found
    # live: a lesson said to ask when nothing offered was the same kind of
    # thing; the agent asked; this gate answered it "stop and ask the person",
    # and the task ended with nobody asked.
    if ASK_THEM.search(answer):
        log(f"What the person said is to ask them: the question goes to them ({answer[:200]!r}).")
        return False

    _self_answered["count"] += 1
    if run is not None:
        guided["asked"] = None
    source = " ".join(str(data.get("from") or "").split())[:200]
    progress.emit(f"Not asking you - you already told me: {answer}" + (f" (from: {source})" if source else ""))
    log(f"Answered the engine's question from what the person already said: {problem[:200]!r} -> {answer!r}")

    # On a thread of its own: the engine is still inside the call that asked.
    threading.Thread(target=_answer_engine, args=(answer,), daemon=True).start()
    return True


# An engine that ran out of steps with its work part done carries on by itself, a few times a task, as a planned run's
# stage does (long_task.OUT_OF_ROOM, MAX_CONTINUATIONS). Found live (2026-10-21): a month's return on a
# portal is longer than one engine budget; the engine stopped after three of eight invoices and asked the person to
# "start a new run", which would only have sent the whole task again.
MAX_CARRY_ON = 3
CARRY_ON = ("Carry on from where you stopped. First look at what is already done - on the screen and in the files - and "
            "do not do any of it again; then do what is still missing, and report when everything the request asks is "
            "done.")
_carried_on = {"task": None, "count": 0}


def _carries_on(problem):
    """True when the engine's question is that it ran out of room, and it was told to carry on (nobody is asked)."""

    from decurix_agent import long_task

    if not long_task.OUT_OF_ROOM.match(problem or "") or long_task.active() or task_context.current_stage()[0]:
        return False  # (a planned run's stage carries on by its own rules)
    task = task_context.current() or state.current_task
    if _carried_on["task"] != task:
        _carried_on.update(task=task, count=0)
    if _carried_on["count"] >= MAX_CARRY_ON:
        return False
    _carried_on["count"] += 1
    progress.emit(f"I ran out of steps before finishing - carrying on where I stopped ({_carried_on['count']} of "
                  f"{MAX_CARRY_ON}).")
    log(f"Engine ran out of room; carrying on ({_carried_on['count']}/{MAX_CARRY_ON}): {problem[:200]!r}")
    threading.Thread(target=_carry_on, daemon=True).start()
    return True


def _carry_on():
    from decurix_agent.harness import continue_after_query

    try:
        continue_after_query(CARRY_ON)
    except Exception as exc:
        log(traceback.format_exc())
        ask_for_query(f"I ran out of steps and could not carry on by myself ({exc}). Tell me how to go on.",
                      engine=SKILL_ENGINE)


def _answer_engine(answer):
    from decurix_agent.harness import continue_after_query

    try:
        continue_after_query(f"The person already answered this earlier, so they were not asked again: {answer}")
    except Exception as exc:
        log(traceback.format_exc())
        ask_for_query(f"I could not carry on after answering my own question from what you told me earlier ({exc}). "
                      "Tell me what to do next.", engine=SKILL_ENGINE)


def _plan_approved_as_is(asked, reply):
    """
    Whether a reply approves the plan of the work exactly as it was put, adding nothing: a routine yes, not help.
    Found (2026-10-31): every run puts its plan to the person first, so with every answer counted as help no taught
    skill could ever run cleanly three times and be trusted - each run of a routine task cost the model's whole way
    through, for good. Anything the reply adds (a source, a rule, a correction, a condition) is still help.
    """

    from decurix_agent.thebot_harness import PLAN_QUESTION
    from decurix_agent.computer import approval

    if not str(asked or "").startswith(PLAN_QUESTION) or not str(reply or "").strip():
        return False
    said = approval.read_choice(reply, asked, {
        "as_planned": "it approves the plan exactly as written and adds, changes, asks or answers nothing else",
        "changed": "it adds anything (a source, a rule, a fact, a correction), changes the plan, sets a condition, "
                   "answers something, or refuses",
    }, plain={"as_planned": ["yes", "ok", "okay", "fine", "go ahead", "proceed", "looks good", "plan is fine",
                             "plan is fine, go ahead", "yes, go ahead", "ok, go ahead", "the plan is fine"]})
    return said["choice"] == "as_planned" and not said.get("instruction")


# A word of a reply that may be a secret value: it holds a digit (the control plane's rule for a reply to a question
# asked for a secret, withhold.Reply). Masked before the reply is read, so no code reaches a model.
_DIGIT_WORD = re.compile(r"\S*\d\S*")
_MASK = "[withheld]"
_MASKS = re.compile(r"\[withheld[^\]]*\]|\[held back[^\]]*\]", re.IGNORECASE)


def _letter_words(text):
    """The words of `text` with every withheld marker taken out."""

    return re.findall(r"[^\W\d_]+", _MASKS.sub(" ", str(text or "")))


def _secret_given(asked, reply):
    """
    Whether a reply to a question raised for a secret gives that value and nothing more: the OTP of a filing, a
    password, a PIN - a value the person gives on every run, which no document holds, so not help (owner's ruling
    2026-11-10). `secret` is the caller's, from structure (soul marked the question, or the control plane withheld the
    reply), never from words. Found live (GSTR-1 curve, 2026-11-10): every month asks for the OTP by design, that answer
    made every run "helped", and the skill could never earn trust.

    Read with every word that holds a digit masked and every known secret withheld (privacy.scrub): a reply of nothing
    else, or one word, is the value; anything else is read once (approval.read_choice) - a reply that also corrects,
    adds data, sets a condition or redirects the work is help. A reply that cannot be read safely, or no model to read
    it, is help: the run then only waits longer for trust, never earns it wrongly.
    """

    from decurix_agent.computer import approval, privacy

    masked = " ".join(_DIGIT_WORD.sub(_MASK, str(reply or "")).split())
    if not masked:
        return False
    shown = privacy.scrub(masked)
    if not isinstance(shown, str):
        return False
    if not _letter_words(shown):
        # Nothing but the value: a code alone, or one word the worker knows as a secret. Several words withheld whole
        # are a reply nobody may read here: help.
        return len(_letter_words(masked)) <= 1
    question = privacy.scrub(" ".join(str(asked or "").split())[:2000])
    try:
        said = approval.read_choice(shown, question if isinstance(question, str) else "", {
            "value": "it gives the value the question asks for (a code, password or PIN - shown withheld) and perhaps "
                     "where it came from, and asks, adds, corrects or changes nothing else",
            "more": "besides or instead of that value it gives any other value or fact, corrects something, adds work, "
                    "sets a condition, or says to do something differently",
        })
    except Exception as exc:  # noqa: BLE001 - a reply that cannot be read is help (the safe way)
        log(f"Could not read the reply to a question asked for a secret: {exc}")
        return False
    return said.get("choice") == "value" and not said.get("instruction")


def _required_every_run(asked, reply, secret=False):
    """
    Whether a person's answer to an engine's question is one the rules require on every run, so the run was not
    helped (owner's ruling 2026-11-10): the plan's yes as it was put (_plan_approved_as_is), or the value asked for a
    secret and nothing more (_secret_given). Every other answer to the engine is help - a corrected value, data the
    documents should have held, "do X instead". (The other required confirmations never reach here as help: a gate's
    yes settles the approval (note_approval), and the date order, a field's unit, a hole in a series and picked
    entries are answered to questions of their own - watchers, final_gate._confirm_reply, _picks_reply.)
    """

    if _plan_approved_as_is(asked, reply):
        return True
    return bool(secret) and _secret_given(asked, reply)


def note_approval(outcome):
    """
    A person's reply settled an on-screen approval the engine of a taught run met (watchers, `outcome` "granted" or
    "declined"). The gate's yes is one action's, required every run for a control the lesson or the case lists: not
    help. Anything else to an approval - a no, a yes with a condition, "use the other button" - is the person
    directing the work: the run was helped (owner's ruling 2026-11-10). Only a reply to a question asked as an approval
    (state.last_query_kind); a reply to another question that a gate also read decides nothing here.
    """

    if outcome in (None, "granted") or getattr(state, "last_query_kind", "") != "approval":
        return
    run = (guidance.get() or {}).get("run")
    if run is not None:
        run["asked_person"] = True


def learn_answer(reply, secret=False, judgement_id=""):
    """
    Keep a person's answer to a question the engine asked during a taught
    task (the watcher calls this as it hands the reply to the engine), so no
    later run asks it again. A secret reply is kept only as withheld, never its
    words (review round 2, B3). `judgement_id`: the reply's own (watchers).
    """

    guided = guidance.get()
    run = (guided or {}).get("run")
    asked = (guided or {}).pop("asked", None) if guided else None
    reply = " ".join((reply or "").split())[:500]

    if run is not None and asked and not _required_every_run(asked, reply, secret):
        # A run a person had to answer was helped, however well it then went (_gate). Found live: three runs
        # in a row, two of them answered by a person, counted as three clean runs and the skill was trusted. An
        # answer the rules require on every run (the plan's yes, an OTP) is not help (_required_every_run).
        run["asked_person"] = True

    if run is None or run.get("one_off") or not asked or not reply:
        return
    if ABANDON_RE.match(reply) and len(reply.split()) <= 2:
        # Only a reply that is nothing but "stop": "stop using 18%, use 12% from now" is a rule to keep (audit).
        return

    try:
        fresh = skills.load(run["skill"]["name"])
    except FileNotFoundError:
        return

    if secret:
        reply = "[withheld: a secret]"
    said = {"at": int(time.time()), "q": " ".join(asked.split())[:500], "a": reply}
    kept = [x for x in fresh.get("run_answers") or [] if (x.get("q"), x.get("a")) != (said["q"], said["a"])]
    fresh["run_answers"] = (kept + [said])[-MAX_RUN_ANSWERS:]
    skills.save(fresh)
    run["skill"]["run_answers"] = fresh["run_answers"]
    log(f"Skill {fresh['name']}: kept the person's answer to {said['q'][:120]!r}")
    if not secret:
        messaging.post_learned("run_answer", fresh["name"], fresh.get("version", 1), said["q"], said["a"],
                               derived_from=judgement_id)
