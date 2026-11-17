"""
A question an engine asks in the middle of a task: answered from what the person already said, or kept once they
answer it (docs/skills.md, docs/learning-and-verification.md).

Split out of skill_task.py (follow-up O-10): skill_task runs taught tasks and checks their reports; this module is
messaging's query gate (_question_gate) - a question already settled by the skill's lesson or the worker's kept
lessons is answered from them (at most MAX_SELF_ANSWERS a task, on a confident reading, never a check of this run's
own work), an engine that ran out of room carries on by itself (_carries_on), a worded ask to press a control the
approval gate itself holds is answered that the gate asks the person at the press (_answers_a_held_press) - and keeps
what a person answers on the skill (learn_answer); a run a person answered was helped, unless the answer is one the
rules require on every run (_required_every_run: the plan's yes as put, an OTP or other secret given and nothing more; note_approval: the gate's
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

    # A repair whose actions are spent is over: its question is no question for the person - the engine reports, and the
    # run hands the rest to the model (repairs.after_repair). Found in the learning curves (2026-11-17): a repair that
    # spent its actions asked the person for "a new session or removal of the action limit", each answer started it
    # again, and the case ended on questions with no voucher entered.
    from decurix_agent import repairs
    if repairs.budget_refusal():
        log("Engine questions: a repair past its action budget asked a question - told to report instead")
        _answer_engine_spent()
        return True

    # The same question asked again, after it was answered twice in this task, is not put to the person a third time:
    # the engine is given their answer and told to finish or say what is missing. Found in the learning curves
    # (2026-11-17): a repair asked one question again and again, each time answered "I don't know that", until the case
    # ran out of time with the return never filed.
    repeats = [reply for asked, reply in guidance.exchanges() if _same_question(asked, problem)]
    if len(repeats) >= REPEAT_ASKS:
        log(f"Engine questions: asked {len(repeats)} times already - not asked again")
        _answer_engine_repeat(len(repeats), repeats[-1])
        return True

    # A check of this run's own work (the review's points, a report the final check refused) is never answered from
    # lessons: a rule kept from another run cannot vouch for this run's figures. Found live (2026-10-25): "an
    # independent check still questions these" - a bill left out - was answered from a lesson about which files to
    # ignore, nobody was asked, and the report went out a bill short.
    if WORK_CHECKED.search(problem or ""):
        return False

    if _carries_on(problem):
        return True

    # A worded ask to press a control the gate itself puts to the person: the gate's card is the one approval
    # (_answers_a_held_press). Before guided["asked"] is set: no person answers this, so nothing here is help.
    if _answers_a_held_press(problem):
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


# How many times one question is put to the person in a task before the engine is given their answer instead.
REPEAT_ASKS = 2


def _same_question(asked, problem):
    """Whether two questions are the same one: the same words, in the opening the exchange keeps (400 characters)."""

    a = " ".join(str(asked or "").lower().split())[:300]
    b = " ".join(str(problem or "").lower().split())[:300]
    if not a or not b:
        return False
    if a == b:
        return True
    wa, wb = set(a.split()), set(b.split())
    return len(wa & wb) >= 0.85 * max(len(wa), len(wb))


def _answer_engine_spent():
    """Tell an engine whose repair spent its actions to end it with its report, instead of asking a person."""

    from decurix_agent.harness import continue_after_query
    try:
        continue_after_query("This repair has used all its actions, so it is over and nobody is asked: end it now with "
                             "your report as your final answer - what you did, whether the stopped step can go on, and "
                             "what stops it if not. The rest of the task is taken up after your report.")
    except Exception as exc:  # noqa: BLE001
        log(traceback.format_exc())
        ask_for_query(f"I could not end a repair that used its actions ({exc}). Tell me what to do next.",
                      engine=SKILL_ENGINE)


def _answer_engine_repeat(times, reply):
    """Give the engine, instead of asking a person again, what they already answered, and tell it to finish."""

    from decurix_agent.harness import continue_after_query
    try:
        state.answer_from_earlier = True
        continue_after_query(f"You have asked the person this {times} times in this task, and they answered: {reply} - "
                             "they were not asked again. Do not ask it again: finish what you can with what you have, "
                             "or report plainly that it cannot be done and exactly what is missing.")
    except Exception as exc:  # noqa: BLE001
        log(traceback.format_exc())
        ask_for_query(f"I could not carry on after a question asked {times} times ({exc}). Tell me what to do next.",
                      engine=SKILL_ENGINE)


def _answer_engine(answer):
    from decurix_agent.harness import continue_after_query

    try:
        state.answer_from_earlier = True
        continue_after_query(f"The person already answered this earlier, so they were not asked again: {answer}")
    except Exception as exc:
        log(traceback.format_exc())
        ask_for_query(f"I could not carry on after answering my own question from what you told me earlier ({exc}). "
                      "Tell me what to do next.", engine=SKILL_ENGINE)


# ------------------------------------------------------------
# A worded ask to press a control the approval gate itself holds
#
# Found live (2026-11-11, freeze 2.3, a return-filing curve): the taught worker's lesson said to ask before its saving
# and filing presses, so it asked in words ('I am about to click "Save to portal" ... May I proceed?'), and the gate
# then asked again with its own card at the press. Every approval cost two questions, the worded ones counted toward
# the case's question cap, and the run ran out of questions before its last press. Owner's ruling (2026-11-11): where
# the gate itself will hold the press the question asks for, the question is answered here and nobody is asked; the
# person sees one approval, the gate's card. Nothing here touches the gate - the press still goes through it, and only
# a yes the gate makes from the person's signed reply lets it go - and nothing here is a yes: no reply is noted
# (approval.note_reply, guidance.note_exchange), so none can stand as one. Whether the gate holds the press is the
# gate's own code (_gate_holds), never a copy of its rules. A hold that only the press itself can find - a form
# carrying the person's code, a replayed step, the plan's ask-first list - is not known here: that question goes to
# the person, as before.
# ------------------------------------------------------------

MAX_GATE_HELD_ANSWERS = 12
GATE_HELD_ANSWER = ("The approval gate itself asks the person when you press {named} - press it, and the person "
                    "approves it there, on the gate's own question. Your question was not put to them. If it also asked "
                    "anything besides approval to press {named}, ask that again on its own.")
_gate_held = {"task": None, "count": 0}


def _gate_holds(element, observation):
    """
    Why the approval gate holds a press of this on-screen control, decided without pressing by the code the press goes
    through, or None: what the person said about it (core._person_hold: their confirmed approvals of a taught task, a
    step they approved while teaching it, a no to it in this task), or the gate's own judgement of it (approval.assess)
    in the context that lifts the most - a press that submits no form and a link the page says only goes somewhere -
    so a hold found here stands at the press whatever the page then says.
    """

    from decurix_agent.computer import approval, core
    held = core._person_hold(element, observation)
    if held:
        return held
    return approval.assess("click", element, context={"submits": False, "navigates": True})


def _gate_held_press(problem, observation=None):
    """
    The labels of the controls the engine's question asks to press (approval.asks_to_press: the object of a press
    verb, the longest label where one holds another), when the gate holds a press of every control on the screen that
    bears each of them (_gate_holds); [] otherwise. The screen is the one the engine last looked at (core's saved
    observation): a control not on it is not known to be held, and the question goes to the person.
    """

    from decurix_agent.computer import approval, core
    observation = core._load_state() if observation is None else observation
    if not isinstance(observation, dict):
        return []

    def label(element):
        return " ".join(str(element.get("name") or "").split())

    elements = [e for e in observation.get("elements") or [] if isinstance(e, dict) and label(e)]
    asked = [e for e in elements if approval.asks_to_press(problem, e)]
    # 'click "Save to portal"' also reads as a press of "Save": the longer label is the one asked for.
    asked = [e for e in asked if not any(label(o) != label(e) and approval.names_control(label(o), e) for o in asked)]
    labels = list(dict.fromkeys(label(e) for e in asked))
    for name in labels:
        same = [e for e in elements if approval.names_control(name, e)
                and approval.names_control(label(e), {"name": name})]
        if not all(_gate_holds(e, observation) for e in same):
            return []
    return labels


def _answers_a_held_press(problem):
    """True when the engine's question asks to press controls the gate itself holds (_gate_held_press) and it was
    answered here, that the gate asks the person at the press; at most MAX_GATE_HELD_ANSWERS a task."""

    task = task_context.current() or state.current_task
    if _gate_held["task"] != task:
        _gate_held.update(task=task, count=0)
    if _gate_held["count"] >= MAX_GATE_HELD_ANSWERS:
        return False
    try:
        labels = _gate_held_press(problem)
    except Exception as exc:  # noqa: BLE001 - not known to be held: the person is asked, as before
        log(f"Could not tell whether the approval gate holds the press asked about ({exc}); asking the person.")
        return False
    if not labels:
        return False
    if not _bare_press_question(problem, labels):
        log("The engine's question asks to press a control the gate holds but says more than the press (a figure, a "
            "doubt, a second sentence): it goes to the person.")
        return False
    _gate_held["count"] += 1
    named = " and ".join(f'"{name}"' for name in labels)
    progress.emit(f"Not asking you twice: the approval gate asks you itself when {named} is pressed.")
    log(f"The engine asked to press {named}, which the approval gate holds: answered that the gate asks the person "
        f"({problem[:200]!r}).")
    # On a thread of its own: the engine is still inside the call that asked.
    threading.Thread(target=_answer_held_press, args=(GATE_HELD_ANSWER.format(named=named),), daemon=True).start()
    return True


# The most words a question may keep besides its press phrase and still be a bare "may I press X" (_bare_press_question):
# "May I", "Should I go ahead and", "Shall I now".
MAX_BARE_PRESS_WORDS = 6


def _bare_press_question(problem, labels):
    """
    Whether the engine's question is only a request to press the controls `labels` ("May I click "Save to portal"?"):
    with each press phrase (approval.asks_to_press's verb, bridge and label) taken out, what is left is one sentence of
    at most MAX_BARE_PRESS_WORDS words, with no figure and no other quoted text. Anything more - a figure the person
    should see, a doubt, a second sentence - is put to the person (review of freeze 2, A8: a question carrying a doubt
    beside the press was answered locally).
    """

    from decurix_agent.computer import approval

    text = " ".join(str(problem or "").split())
    for name in sorted(labels, key=len, reverse=True):
        label = " ".join(str(name).split())
        text = re.sub(r"\b" + approval._PRESS_VERB + approval._PRESS_BRIDGE + re.escape(label) + r"(?!\w)[\"'“”‘’]?",
                      " ", text, flags=re.IGNORECASE)
    rest = text.strip()
    if re.search(r"\d|[\"“”‘’]|(?<![^\W\d_])'|'(?![^\W\d_])", rest):
        return False
    if len(re.findall(r"[.?!;:]+(?=\s+\S)", rest)) > 0:
        return False
    return len(re.findall(r"[^\W_]+(?:'[^\W_]+)?", rest)) <= MAX_BARE_PRESS_WORDS


def _answer_held_press(answer):
    from decurix_agent.harness import continue_after_query

    try:
        continue_after_query(answer)
    except Exception as exc:
        log(traceback.format_exc())
        ask_for_query(f"I could not carry on after the approval gate's answer to my question ({exc}). Tell me what to "
                      "do next.", engine=SKILL_ENGINE)


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
    reply), never from words. Found live (a monthly return's curve, 2026-11-10): every month asks for the OTP by design, that answer
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
    run["skill"]["run_answers"] = fresh["run_answers"]
    from decurix_agent import learned
    try:
        skills.save(fresh, actor="correction")
    except learned.Locked as exc:
        log(f"Skill {fresh['name']}: the person's answer holds for this run only: {exc}")
        return
    log(f"Skill {fresh['name']}: kept the person's answer to {said['q'][:120]!r}")
    if not secret:
        messaging.post_learned("run_answer", fresh["name"], fresh.get("version", 1), said["q"], said["a"],
                               derived_from=judgement_id)
