"""
"/learn": a skill taught from demonstrations, by asking the person who gave them what the recordings cannot say
(docs/skills.md, "Learn").

Split out of skill_task.py (follow-up O-10): skill_task runs what was taught and checks it; this module holds the
lesson itself - the recordings it is taught from (_sources), the model's study of them settling what it can
(_settle_from_study), each question put to the person and their reply taken in (_ask, _learn_reply), and what is kept
when the skill is saved (_remember, _learned_text). skill_task's perform and resume call _learn and _learn_reply.

This module never imports skill_task: the report and the question it sends are skill_task's, handed over at its
import (use_run).
"""

import re
import threading

from decurix_agent import messaging, progress, state, study
from decurix_agent.computer import learning, recording, skills
from decurix_agent.logging_utils import log
from decurix_agent.messaging import remember
from decurix_agent.skill_commands import _Refused, ABANDON_RE, LEARN_ENGINE


# ============================================================
# What this module calls back in skill_task
#
# The report a lesson ends with (write_final_reply) and its questions (ask_for_query) are skill_task's, which
# imports this module; so skill_task hands them over at its import (use_run) instead of being imported here. Each
# is looked up in skill_task when called: a test that patches skill_task.write_final_reply patches what this module
# reads.
# ============================================================

_RUN = {}


def use_run(**calls):
    """Take the calls this module makes back into skill_task from skill_task at its import."""

    _RUN.update(calls)


def write_final_reply(*args, **kwargs):
    """Report how a task ended, to the person and the control plane (skill_task.write_final_reply)."""

    return _RUN["write_final_reply"](*args, **kwargs)


def ask_for_query(*args, **kwargs):
    """Ask the person a question; the task waits for the reply (skill_task.ask_for_query)."""

    return _RUN["ask_for_query"](*args, **kwargs)


# ============================================================
# Learning
# ============================================================

def _sources(ids):
    """[(meta, events)] for the recordings named, or the latest finished demonstration when none is."""

    if not ids:
        latest = next((m for m in recording.sessions()
                       if m.get("kind") == "demonstration" and m.get("status") == "complete" and m.get("event_count")), None)
        if latest is None:
            raise _Refused("There is no finished demonstration on this desktop to learn from. Record one, then send "
                           "`/learn <recording>`.")
        ids = [latest["id"]]

    sources = []
    for session_id in ids:
        try:
            meta, events = recording.load(session_id)
        except (FileNotFoundError, ValueError):
            raise _Refused(f"There is no recording {session_id} on this desktop.")

        if meta.get("status") == "recording":
            raise _Refused(f"{session_id} is still being recorded - end it first, then teach it.")

        sources.append((meta, events))

    return sources


def _learn(ids, name):
    sources = _sources(ids)
    draft = skills.draft_for([meta["id"] for meta, _ in sources])

    if draft is not None:
        log(f"Continuing the unfinished skill {draft['name']}")
        return _ask(draft, "Picking up where we left off.")

    try:
        skill = learning.build(sources, name=name)
    except ValueError as exc:
        raise _Refused(f"I cannot learn from that: {exc}.")

    if study.available():
        # The model studies the demonstration first, and asks what it could
        # not work out (study.py) - a lesson for a task whose next run will be
        # different, not only a replay of this one.
        progress.emit("Studying your recording to work out what the task is and what I need to ask.")
        understanding = study.study(skill)
        if understanding is not None:
            skill["understanding"] = understanding
            _settle_from_study(skill)

    _ask(skill)


def _in_words(value, text):
    words = " ".join(str(value or "").lower().split())
    return len(words) >= 2 and words in " ".join(str(text or "").lower().split())


def _settle_from_study(skill):
    """
    What the study already settles is not asked: each value gets the name of
    what it is (learning.rename_params), a value the person's own request
    holds - "send Koustubh OG "hello man"" - changes with the request, and a
    skill the person did not name gets the study's short name for the task
    rather than the request with its values in it.
    """

    understanding = skill["understanding"]
    learning.rename_params(skill, understanding.pop("names", None) or {})

    request = skill.get("goal") or ""
    for param in skill.get("params") or []:
        if (param.get("varies") is None and understanding.get("varies", {}).get(param["name"]) is True
                and not param.get("file") and _in_words(param.get("example"), request)):
            param["varies"] = True
            param["default"] = None
            learning._follow_param(skill, param)
            understanding.setdefault("settled", []).append(param["name"])

    named = understanding.pop("name", "")
    if named and (not skill.get("named") or skill.get("title") == request):
        learning.rename(skill, named)
        skill["named"] = False


def _ask(skill, preface="", question=None):
    """Save the draft and put its next question to the person."""

    question = question or learning.next_question(skill)
    state.paused_skill = {"mode": "learn", "skill": skill, "question": question}

    skills.save(skill)
    ask_for_query(f"{preface} {question['text']}".strip(), kind="query", engine=LEARN_ENGINE)


def _learn_reply(paused, reply):
    skill, question = paused["skill"], paused["question"]

    # Only a bare "stop" / "cancel" ends a lesson: an answer may well start
    # with the word ("stop and tell me" to "what if it already exists?").
    # And not when the question itself offers stopping as an answer - found
    # live: "should the task stop or start a new chat?" answered `stop` threw
    # the lesson away instead of teaching it to stop.
    offered = question["kind"] in ("study", "goal") and re.search(
        r"\b(stop|cancel|abort|quit)\b", question.get("text") or "", re.I)
    if ABANDON_RE.match(reply or "") and len((reply or "").split()) <= 2 and not offered:
        skills.save(skill)
        messaging.write_stopped(
            f"Stopped. What you have told me about \"{skill['title']}\" is kept - send `/learn {skill['sources'][0]}` "
            "to carry on where we left off.", engine=LEARN_ENGINE, send=write_final_reply)
        return

    # A correction to what the model understood, given at the last question,
    # is not a name: names are short, sentences are not.
    if question["kind"] == "confirm" and skill.get("understanding") and len((reply or "").split()) > MAX_NAME_WORDS:
        progress.emit("Taking in your correction.")
        study.absorb(skill, "Is this how the task should be done? (the summary and rules above)", reply)
        _ask(skill, "Got it - here it is again.")
        return

    # The last question takes a name, which moves the skill to another file.
    drafted_as = skill["name"]
    result = learning.answer(skill, question, reply)

    if not result["understood"]:
        _ask(skill, result["note"], question=question)
        return

    # What the person told the model that studied the demonstration is folded
    # into what it understood; an answer that opens something it must know
    # gets one follow-up, asked next.
    if skill.get("understanding") is not None and question["kind"] in ("study", "goal") \
            and not learning.NOTHING.match(reply or "") \
            and not (question["kind"] == "goal" and skill["answers"].get("goal") == (reply or "").strip()[:300]
                     and learning.YES.match(reply or "") and len((reply or "").split()) <= 3):
        progress.emit("Taking in your answer.")
        follow_up = study.absorb(skill, question["text"], reply)
        if follow_up:
            _add_follow_up(skill, question, follow_up)

    if question["kind"] == "goal" and learning.YES.match(reply or "") and len((reply or "").split()) <= 3:
        study.confirm(skill)  # (a plain yes to the read-back: the brief's guesses are the person's word now)

    decision = result["decision"]

    if decision == "save":
        skills.save(skill)
        if skill["name"] != drafted_as:
            skills.delete(drafted_as)
        # The skill is already durable and ready to run. Sending its optional
        # summary to the control plane must never leave the teaching UI in
        # "Processing" behind a slow or unreachable memory endpoint.
        _remember_async(skill)
        write_final_reply(_learned_text(skill), kind="final", engine=LEARN_ENGINE)
    elif decision == "discard":
        skills.delete(skill["name"])
        write_final_reply(f"Threw away what I had learned about \"{skill['title']}\". The recording is untouched.",
                          kind="final", engine=LEARN_ENGINE)
    elif decision == "hold":
        skills.save(skill)
        write_final_reply(f"I could not tell whether to save \"{skill['title']}\", so I left it unfinished. "
                          f"Send `/learn {skill['sources'][0]}` to answer that last question.",
                          kind="final", engine=LEARN_ENGINE)
    else:
        _ask(skill, result["note"])


MAX_NAME_WORDS = 6


def _add_follow_up(skill, question, text):
    """Ask `text` right after `question` (study.absorb's follow-up)."""

    asked = skill["understanding"].setdefault("questions", [])
    new = {"id": f"f{skill['understanding'].get('follow_ups', len(asked))}", "text": text, "why": "follow-up"}
    at = next((i + 1 for i, q in enumerate(asked) if f"study:{q['id']}" == question["id"]), 0)
    asked.insert(at, new)


def _remember_async(skill):
    """Record optional learned memory without delaying the completed lesson."""

    threading.Thread(target=_remember, args=(skill,), daemon=True).start()


def _remember(skill):
    """Put what was just learned into the desktop's worker memory (its card's "learned memory")."""

    steps = skill["steps"]
    params = skills.required(skill)
    gated = sum(1 for s in steps if s.get("consequential"))
    by_hand = sum(1 for s in steps if s["kind"] == "manual")

    lines = [f"Taught by demonstration ({len(skill.get('sources') or [])} recording(s), {len(steps)} steps)."]

    said = skills.explanation(skill)
    if said:
        lines.append(f"The person who taught it said: {said.rstrip('. ')}.")

    understood = skill.get("understanding") or {}
    if understood.get("summary"):
        lines.append(f"What it does: {understood['summary']}")
    # What was asked and answered while it was taught, word for word: the
    # lesson is the person's, and memory is where every engine reads it.
    for said in (understood.get("dialogue") or [])[:12]:
        lines.append(f"Asked when it was taught: {said['q']} - the person answered: {said['a']}.")

    if params:
        wanted = ", ".join(f'{p["name"]} ({p["label"]})' for p in params)
        lines.append(f"Needs a value for: {wanted}.")

    if gated:
        lines.append(f"Stops for approval before {gated} step(s), every run.")

    if by_hand:
        lines.append(f"{by_hand} step(s) are left to a person, such as a sign-in.")

    example = "".join(f' {p["name"]}="..."' for p in params)
    lines.append(f"Run it with `/skill {skill['name']}{example}`, or `gui do {skill['name']}` from a shell.")

    remember(f"Skill: {skill.get('title') or skill['name']}", " ".join(lines)[:3800], category="skill")


def _learned_text(skill):
    params = "".join(f' {p["name"]}="{"a;b;c" if p.get("list") else "..."}"' for p in skills.required(skill))
    gated = sum(1 for s in skill["steps"] if s.get("consequential"))

    text = f"Learned \"{skill['title']}\" as `{skill['name']}`: {len(skill['steps'])} steps. Run it with `/skill {skill['name']}{params}`."

    if gated:
        text += f" It will stop for your approval before {gated} of them, every time."

    return text + " If it gets stuck or is unsure it will ask you, and remember a correction that works."
