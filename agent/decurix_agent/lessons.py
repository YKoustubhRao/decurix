"""
What a person tells a worker while it works, kept.

A worker stops and asks: which of two matches, what a field means, whether a
result is right ("I cannot confirm this is done ... tell me what is wrong").
The person's answer used to reach the task it was asked in and nothing else,
so the next task met the same doubt and asked again. Found live (2026-10-04):
"always choose first", answered during a run, was in no file afterwards.

Every such exchange now goes through note(): the question the worker asked,
what the person replied, and the task it happened in. With a hosted model the
exchange is distilled into one instruction worth keeping - general where the
answer was general ("when several people match a name, take the first"),
nothing at all where it was only this run's ("yes", "the second one, he is at
Google") or is already known. Without a model the exchange is kept as it was
said. It is written to the worker's memory on the control plane
(messaging.remember, category "lesson"): shown in the card's learned-memory
list, written back into ~/.decurix/memory, and read by every engine on every
later task (memory_context) - and by the check that answers a repeated
question without paging the person (skill_task._question_gate).

Best-effort and off the task's own thread: a lesson that could not be kept
never delays or fails the reply it came from.
"""

import re
import threading

from decurix_agent import config, llm_client, study
from decurix_agent.computer import approval, privacy
from decurix_agent.logging_utils import log
from decurix_agent.messaging import remember

CATEGORY = "lesson"
MAX_KNOWN_CHARS = 3000
MAX_LESSON_CHARS = 600

# A reply that only steers the run it was given in: nothing to learn from.
OPERATIONAL = re.compile(
    r"^\W*(yes|y|yep|yeah|ok(?:ay)?|sure|no|nope|n|done|retry|try again|again|redo|skip|stop|cancel|abort|quit|"
    r"continue|carry on|go on|go ahead|proceed|keep|save|discard|manual|restart|click anyway|never ?mind|"
    r"all change|all same|\d+)\W*$", re.IGNORECASE)

# A question whose answer is a secret.
SECRET_ASKED = re.compile(
    r"\b(password|passcode|passphrase|pin|otp|one[- ]time (?:code|password)|verification code|security code|"
    r"2fa|cvv|api key|secret|token)\b", re.IGNORECASE)

DISTILL_SYSTEM ="""A computer worker stopped during a task to ask the person it works for a question, and the person answered. Decide what, if anything, the worker should remember from this for future tasks, and write it as one instruction.

Keep a lesson when the answer tells the worker how the person wants things done, in a way that will hold again: a preference or rule ("when several match, take the first"), how something works here (where a thing is found, what a field means, which account or format to use), a correction of a mistake it made, a fact about the person's systems or people that it will need again.

Keep nothing when: the answer only settles this one run (a bare yes or no, an approval, "the second one", a value for this task alone such as one name, amount or file); it gives a password, code or other secret; it is already covered by what the worker knows (listed below); or it is unclear what was meant.

Write the lesson so it stands on its own, without the question: say when it applies and what to do, in the person's terms, in one or two sentences. Generalise only as far as the person's words allow - "always" from them means always; an answer about one case stays about that kind of case. Do not add advice of your own.

Reply with JSON only: {"keep": true, "title": "<3-8 words>", "lesson": "<the instruction>"} or {"keep": false, "why": "<a few words>"}"""


def known(limit=MAX_KNOWN_CHARS):
    """The lessons already in this worker's memory, newest first, as text (up to `limit` characters)."""

    parts, used = [], 0
    try:
        paths = sorted(config.MEMORY_DIR.glob("*.txt"), key=lambda p: p.name, reverse=True)
    except OSError:
        return ""

    for path in paths:
        try:
            content = path.read_text(encoding="utf-8")
        except OSError:
            continue
        if not re.search(rf"^Category:\s*{CATEGORY}\s*$", content, re.MULTILINE | re.IGNORECASE):
            continue
        # The entry's own text, past its header lines.
        body = content.split("\n\n", 1)[1].strip() if "\n\n" in content else content.strip()
        if not body or used + len(body) > limit:
            continue
        parts.append("- " + " ".join(body.split()))
        used += len(body)

    return "\n".join(parts)


def _squash(text, limit):
    return " ".join(str(text or "").split())[:limit]


def worth_keeping(question, reply, kind="query", outcome=None):
    """Whether an exchange can hold a lesson at all (no model: the cheap rules)."""

    reply = (reply or "").strip()
    if not (question or "").strip() or not reply:
        return False
    if OPERATIONAL.match(reply):
        return False
    # A secret is never a lesson: not kept, and not sent anywhere to be read.
    if SECRET_ASKED.search(question) or privacy.credential_in(reply):
        return False
    # A yes or a no to an approval is one action's, never a standing rule
    # (a standing approval would be the worker switching off its own gate).
    if (kind == "approval" or outcome) and (approval.is_approval(reply) or approval.is_refusal(reply)):
        return False
    return True


def distil(question, reply, task="", skill=""):
    """
    The lesson in an exchange, as (title, text), or None when there is none.
    With a hosted model it is distilled; without, the exchange is kept as said.
    """

    question, reply = _squash(question, 1500), _squash(reply, 800)

    if llm_client.available():
        content = (f"The task: {_squash(task, 600) or '(unknown)'}\n"
                   + (f"It is the taught task: {skill}\n" if skill else "")
                   + f"\nThe worker asked:\n{question}\n\nThe person answered:\n{reply}\n\n"
                   f"What the worker already knows:\n{known() or '(nothing yet)'}")
        try:
            data = study._call(DISTILL_SYSTEM, content, max_tokens=400)
        except (llm_client.LLMError, ValueError) as exc:
            log(f"lessons: the model could not be asked ({exc}); keeping the exchange as it was said")
        else:
            if data.get("keep") is not True:
                log(f"lessons: nothing to keep from {reply[:80]!r} ({_squash(data.get('why'), 120)})")
                return None
            lesson = _squash(data.get("lesson"), MAX_LESSON_CHARS)
            if not lesson:
                return None
            title = _squash(data.get("title"), 80) or lesson[:60]
            return title, (f"{lesson}\n\nLearned when the worker asked: \"{_squash(question, 300)}\" "
                           f"and the person answered: \"{_squash(reply, 300)}\"."
                           + (f" (Task: {_squash(skill or task, 120)})" if skill or task else ""))

    title = _squash(question, 60)
    return title, (f"When the worker asked: \"{_squash(question, 500)}\" the person answered: \"{reply}\"."
                   + (f" (Task: {_squash(skill or task, 120)})" if skill or task else ""))


def _keep(question, reply, task, skill):
    try:
        found = distil(question, reply, task=task, skill=skill)
        if found is None:
            return
        title, text = found
        if remember(f"Lesson: {title}", text, category=CATEGORY):
            log(f"lessons: kept {title!r}")
    except Exception as exc:
        log(f"lessons: could not keep what the person said: {exc}")


def note(question, reply, kind="query", outcome=None, task="", skill="", mechanical=False):
    """
    Keep what a person answered to a question this worker asked, if there is
    something in it to keep. Returns the thread doing it, or None when the
    exchange holds nothing (a yes, a retry, an approval).

    `mechanical` marks a question a replay or a skill's own run asked (a
    value it needs, a step that is stuck): its answer is nearly always that
    run's alone, so it is only looked at when there is a model to tell - with
    none, keeping it as said would fill the memory with one-off values.
    """

    if not worth_keeping(question, reply, kind=kind, outcome=outcome):
        return None
    if mechanical and not llm_client.available():
        return None

    thread = threading.Thread(target=_keep, args=(question, reply, task, skill), daemon=True)
    thread.start()
    return thread
