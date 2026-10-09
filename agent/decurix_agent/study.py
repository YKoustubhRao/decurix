import json
import re

from decurix_agent import attachments, llm_client, usage
from decurix_agent.computer import evidence, learning, replay
from decurix_agent.logging_utils import log


# ============================================================
# Studying a demonstration before asking about it
#
# learning.py asks what a recording cannot say, from rules: when is it done,
# which typed values change, which clicks were positions. That is enough to
# replay a task, not to do one whose next run is different - a list instead
# of one item, a customer that does not exist yet, a page with a banner on
# it. What a person means lives in exactly those cases, and a person teaching
# does not think to say them unless asked.
#
# So when a hosted model is here, a lesson starts with the model studying the
# demonstration - its steps, the person's notes, and the screens it was done
# on - the way an apprentice would: what the task achieves, what each step is
# for, what each typed value is and where a run gets it, what "done" looks
# like on the screen, which steps were incidental, and the decisions a
# different run will meet. It asks the person only what it could not work
# out (at most MAX_QUESTIONS, most important first, one at a time), and folds
# each answer back into what it understood, asking one follow-up when an
# answer opens something it must know. What it ends with - `understanding` on
# the skill - is what the model doing the task later is told
# (skill_task._procedure), what the check of a finished run is measured
# against (skill_task._criteria), and what the person approves before the
# skill is saved.
#
# Every call here is best-effort: a lesson without a model, or whose model
# fails, is the rule-based lesson it always was.
# ============================================================

MAX_QUESTIONS = 5
MAX_FOLLOW_UPS = 3
# Every press of the demonstration is shown, ringed (see _pictures), plus the
# screen it began on and the one it ended on - up to this many pictures.
MAX_PICTURES = 14

STUDY_SYSTEM = """You are an apprentice learning a computer task by watching a person do it (one or a few demonstrations) on a desktop. Later you will do this task on your own, with other values, on screens that may look different, and your work will be checked. What you work out now, and what you ask now, is all you will have.

Study the recording you are given - the numbered steps (what was clicked, typed, in which window), the person's own notes, and the screenshots - and work out:
- summary: what the task achieves, in two or three sentences, in the person's terms.
- steps: what each step is FOR (not what it clicks), by step number. For every press you are shown the screen at the moment the person pressed, with a red ring on the exact spot: look at what is under the ring and around it, and say what that thing IS in the task ("the first conversation in the search results", "the result for the company that was searched for") - the step list only names the text or control that happened to be under the pointer, which on another run will read differently. If the list's name for a press and what the ring is on disagree, trust the picture and say so.
- inputs: for each typed value (named by its parameter), what it is and where a future run gets it (the request, a document, always the same...). When the documents the person worked from are shown (the files attached to their ticket), find each typed value in them and say which document and which label or line it was read from ("the name on the source document", "the quantity column, one per entry"): a future run brings its own documents of the same kind and reads the same things from those. A value that is in a document varies.
- varies: for each input, true if it will be different on another run, false if it stays the same.
- done_when: what must be visible on the screen, or true, when the task is finished - concrete enough to check from a screenshot, and true of EVERY run, not only this demonstration. Write an input as its parameter in braces ("a profile page for {typed_text} is open"), never as the value the person happened to use, and leave out details that only this demonstration's result has (this person's city, this order's total, today's date): another run's result will not show them. The same goes for the summary.
- incidental: step numbers that look accidental or not needed (a stray click, scrolling around, closing a popup that may not appear).
- rules: leave empty unless the notes already state one; each rule is one imperative sentence.
- questions: what you must ask the person, because you cannot work it out and getting it wrong would make a future run fail or do the wrong thing. Think about: what the request will look like next time (one item or a list? done once per item?), decisions (what if the record is not found, already exists, or the page differs?), which values come from where, preconditions (signed in, which account, which page to start from), steps that send, pay, submit or delete and what to check before them, and anything in the notes you do not understand. Most important first, at most 5. Each is one short, concrete question that can be answered in a line, naming steps by number and what is on the screen. Do not ask what you can see or work out, about passwords, about screen coordinates, or whether to save the task - those are handled separately.

Reply with JSON only:
{"summary": "...", "steps": {"1": "..."}, "inputs": {"param": "..."}, "varies": {"param": true}, "done_when": "...", "incidental": [5], "rules": [], "questions": [{"id": "q1", "text": "...", "why": "..."}]}"""

ABSORB_SYSTEM = """You are an apprentice learning a computer task from a person (you studied their demonstration earlier; your understanding so far is given). The person has just answered one of your questions.

Fold their answer into your understanding: correct the summary, step purposes, inputs, varies and done_when where it changes them, and add rules - each one plain imperative sentence a future run can follow ("Ask each question in a new chat, one at a time, and report every answer." / "If the customer does not exist, create it before the order."). Keep what the answer does not touch. Never drop an earlier rule unless the person contradicts it.

Rules is the whole list, rewritten: each rule once, merged with the earlier ones - no two rules saying the same thing. Only state what the person said or what the demonstration shows; do not invent requirements (for example, do not require "the same conversation" unless the person said so).

If the answer is unclear, or opens something you must know to do the task right, write ONE short follow_up question; otherwise follow_up is "". Do not ask again what was answered.

You are also given the questions still waiting to be asked. List in "settled" the ids of those that the answers so far already answer, so they are not asked.

Reply with JSON only, the whole understanding without "questions":
{"summary": "...", "steps": {"1": "..."}, "inputs": {"param": "..."}, "varies": {"param": true}, "done_when": "...", "incidental": [], "rules": ["..."], "follow_up": "", "settled": ["q3"]}"""

KEYS = ("summary", "steps", "inputs", "varies", "done_when", "incidental", "rules")


def available():
    return llm_client.available()


def _step_line(step, params):
    line = f"{step['n']}. {step.get('summary') or step['kind']}"
    if step.get("window"):
        line += f" [window: {step['window']}]"
    if step.get("param") and step["param"] in params:
        line += f" [typed value {json.dumps(params[step['param']].get('example') or '', ensure_ascii=False)} -> parameter {step['param']}]"
    if step.get("expect"):
        line += " [then: " + "; ".join(replay.describe_check(c) for c in step["expect"])[:200] + "]"
    if step.get("consequential"):
        line += " [stopped for the person's approval]"
    if step["kind"] == "manual":
        line += " [a private step the person does by hand]"
    if step.get("intent"):
        line += f" [the person said: {step['intent']}]"
    return line


PRESSES = ("click", "double_click", "right_click", "drag")


def _pressed_at(step):
    """Where on the screen a step pressed: its point, else the middle of the control it was on; None for no press."""

    if step.get("kind") not in PRESSES:
        return None
    if step.get("point"):
        return step["point"]
    box = (step.get("locate") or {}).get("box")
    if box and len(box) == 4:
        return [box[0] + box[2] // 2, box[1] + box[3] // 2]
    return None


def _chosen(steps, budget):
    """
    Which steps' screens are shown when there are more presses than pictures:
    the first step, then presses with no named control under them (only the
    picture says what they were on), then presses that changed the window,
    then the rest - and of each kind, the earliest first.
    """

    ranked, last_window = [], None
    for index, step in enumerate(steps):
        window = step.get("window") or step.get("phase_label")
        moved = bool(window and window != last_window)
        last_window = window or last_window

        pressed = _pressed_at(step)
        if index == 0:
            rank = 0
        elif pressed and not step.get("target"):
            rank = 1
        elif pressed and moved:
            rank = 2
        elif pressed:
            rank = 3
        elif moved:
            rank = 4
        else:
            continue
        ranked.append((rank, index))

    keep = sorted(index for _, index in sorted(ranked)[:budget])
    return [steps[index] for index in keep]


def _pictures(skill):
    """
    What the person saw as they did the task: the screen each press was made
    on, with a red ring on the spot, where it began and where it ended.

    It used to be the screens where the window changed, five at most. Said by
    the person who runs this (2026-10-03): "some of the clicks I do you don't
    see where I clicked or why". Their demonstration's click on the first
    conversation of a search was in the list as `click text "This message has
    been deleted."` - the words under the pointer - and no picture of it was
    shown, so nothing said it was the first result.
    """

    steps = skill.get("steps") or []
    parts = []

    for step in _chosen(steps, MAX_PICTURES - 1):
        png = evidence.step_frame(step, "before")
        if png is None:
            continue
        caption = f"before step {step['n']} ({step.get('summary') or step['kind']})"
        pressed = _pressed_at(step)
        if pressed:
            png = evidence.mark(png, pressed)
            caption += " - the red ring is where the person pressed"
        parts += [{"type": "text", "text": f"Screen {caption}:"}, llm_client.image_part(png)]

    if steps:
        png = evidence.step_frame(steps[-1], "after") or evidence.step_frame(steps[-1], "before")
        if png is not None:
            parts += [{"type": "text", "text": f"Screen after the last step ({steps[-1]['n']}):"}, llm_client.image_part(png)]

    return parts


def _documents(skill):
    """
    The files attached to the demonstration's ticket, as the model reads them:
    what the person had in front of them while they typed. Without them a
    lesson sees "typed Gemini Furniture into Customer" and cannot say where a
    later run finds its customer.
    """

    try:
        return attachments.parts(
            skill.get("documents"),
            "The documents the person worked from in this demonstration (attached to their ticket). They are this "
            "demonstration's own - another run will bring other documents of the same kind:")
    except Exception as exc:
        log(f"study: could not read the lesson's documents: {exc}")
        return []


def _described(skill):
    params = {p["name"]: p for p in skill.get("params") or []}
    lines = [f"The task as the person named it: {skill.get('goal') or skill.get('title')}",
             f"Demonstrations: {skill.get('demos', 1)}", "Steps:"]
    lines += [_step_line(step, params) for step in skill.get("steps") or []]

    notes = [n["text"] for n in skill.get("notes") or [] if n.get("text")]
    if notes:
        lines.append("The person's own notes while recording: " + " | ".join(notes))
    if skill.get("documents"):
        lines.append("Documents attached to the ticket (shown below): " + attachments.names(skill["documents"]))
    if skill.get("tidied"):
        lines.append("Already tidied from the recording: " + "; ".join(skill["tidied"]))
    learned = [p for p in params.values() if p.get("varies")]
    if learned:
        lines.append("Differed between demonstrations (so they vary): " + ", ".join(p["name"] for p in learned))
    loops = [loop for loop in skill.get("loops") or []]
    if loops:
        lines.append(f"Repeated blocks found: {len(loops)} (asked about separately).")
    return "\n".join(lines)


def _token_budget(skill):
    # Reasoning-model completion budgets include reasoning as well as JSON.
    # Long demonstrations need room for one purpose per step plus inputs.
    return min(24000, max(6000, 4000 + 80 * len(skill.get("steps") or [])))


def _call(system, content, max_tokens=6000):
    message, spent = llm_client.chat([{"role": "system", "content": system}, {"role": "user", "content": content}],
                                     max_tokens=max_tokens, response_format={"type": "json_object"})
    try:
        usage.add(input_tokens=spent.get("prompt_tokens") or 0, output_tokens=spent.get("completion_tokens") or 0)
    except Exception:
        pass
    text = message.get("content") or ""
    match = re.search(r"\{.*\}", text, re.DOTALL)
    if not match:
        raise ValueError(f"the model did not answer in JSON: {text[:200]!r}")
    return json.loads(match.group(0))


def _clean(data, skill):
    """An understanding as the rest of Decurix reads it: known keys, strings where strings go."""

    numbers = {str(s["n"]) for s in skill.get("steps") or []}
    names = {p["name"] for p in skill.get("params") or []}

    def text(value, limit=800):
        return " ".join(str(value or "").split())[:limit]

    return {
        "summary": text(data.get("summary"), 1200),
        "steps": {str(k): text(v, 300) for k, v in (data.get("steps") or {}).items() if str(k) in numbers and v},
        "inputs": {k: text(v, 300) for k, v in (data.get("inputs") or {}).items() if k in names and v},
        "varies": {k: bool(v) for k, v in (data.get("varies") or {}).items() if k in names and v is not None},
        "done_when": text(data.get("done_when"), 600),
        "incidental": [int(n) for n in data.get("incidental") or [] if str(n).isdigit() and str(n) in numbers],
        "rules": [text(r, 400) for r in data.get("rules") or [] if text(r)][:20],
    }


def study(skill):
    """
    The model's first understanding of a draft skill, with the questions it
    wants to ask - or None when there is no model or it could not be read.
    """

    if not available():
        return None
    try:
        data = _call(STUDY_SYSTEM, [{"type": "text", "text": _described(skill)}] + _documents(skill) + _pictures(skill),
                     max_tokens=_token_budget(skill))
    except (llm_client.LLMError, ValueError) as exc:
        log(f"study: could not study {skill.get('name')}: {exc}")
        return None

    understanding = _clean(data, skill)
    questions = []
    for n, q in enumerate(data.get("questions") or [], 1):
        words = " ".join(str((q or {}).get("text") or "").split())
        if words:
            questions.append({"id": f"q{n}", "text": words[:400], "why": " ".join(str(q.get("why") or "").split())[:300]})
    understanding["questions"] = questions[:MAX_QUESTIONS]
    understanding["follow_ups"] = 0
    understanding["dialogue"] = []
    log(f"study: {skill.get('name')}: {json.dumps(understanding)[:1500]}")
    return understanding


def absorb(skill, question, answer):
    """
    Fold a person's answer into the skill's understanding (in place). Returns
    a follow-up question to ask next, or "" - never more than MAX_FOLLOW_UPS
    in a lesson. A model that cannot be reached keeps the answer word for
    word as a rule, so nothing the person said is lost.
    """

    understanding = skill.get("understanding")
    if understanding is None:
        return ""

    understanding.setdefault("dialogue", []).append({"q": question, "a": answer})

    if not available():
        understanding.setdefault("rules", []).append(f"(Asked: {question}) The person said: {answer}")
        return ""

    current = {k: understanding.get(k) for k in KEYS}
    answers = skill.get("answers") or {}
    waiting = [{"id": q["id"], "text": q["text"]} for q in understanding.get("questions") or []
               if f"study:{q['id']}" not in answers and q["text"] != question]
    content = [{"type": "text", "text": (
        f"The task: {skill.get('goal') or skill.get('title')}\n"
        f"Steps:\n" + "\n".join(f"{s['n']}. {s.get('summary')}" for s in skill.get("steps") or []) + "\n\n"
        f"Your understanding so far:\n{json.dumps(current, ensure_ascii=False)}\n\n"
        f"Earlier questions and answers: {json.dumps(understanding['dialogue'][:-1], ensure_ascii=False)}\n\n"
        f"Questions still waiting to be asked: {json.dumps(waiting, ensure_ascii=False)}\n\n"
        f"You asked: {question}\nThe person answered: {answer}")}]

    try:
        data = _call(ABSORB_SYSTEM, content, max_tokens=_token_budget(skill))
    except (llm_client.LLMError, ValueError) as exc:
        log(f"study: could not take in the answer: {exc}")
        understanding.setdefault("rules", []).append(f"(Asked: {question}) The person said: {answer}")
        return ""

    updated = _clean(data, skill)
    # The rules are the model's own merged list. What the person actually
    # said is never lost to a rewording: it stays in `dialogue`, word for
    # word, and every later run's model reads it (skill_task._criteria).
    if not updated["rules"]:
        updated["rules"] = understanding.get("rules") or []
    understanding.update(updated)

    # Questions the answers so far already settle are not asked.
    settled = {str(i) for i in data.get("settled") or []}
    if settled:
        understanding["questions"] = [q for q in understanding.get("questions") or []
                                      if q["id"] not in settled or f"study:{q['id']}" in answers]

    follow_up = " ".join(str(data.get("follow_up") or "").split())[:400]
    if follow_up and understanding.get("follow_ups", 0) < MAX_FOLLOW_UPS:
        understanding["follow_ups"] = understanding.get("follow_ups", 0) + 1
        return follow_up
    return ""


def text(skill):
    """What was understood, for the person to approve before the skill is saved."""

    return learning.understood_text(skill)
