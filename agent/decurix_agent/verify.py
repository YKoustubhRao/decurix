import json
import re

from decurix_agent import llm_client, usage
from decurix_agent.logging_utils import log


# ============================================================
# Checking a task was really done, before anyone is told it was
#
# The rule: a worker may ask, or say it failed, but never report a success it
# has not seen. A replay's own checks (replay.py) prove only the steps whose
# results it could look for; a model finishing a task with "done" proves
# nothing at all. Found live: a taught search ran 8 of 9 steps, 7 of them with
# nothing to check, and was reported to the person as replayed.
#
# So a task that was not fully checked on the screen is judged here: the
# hosted model is shown the screen now, the screen the person's own
# demonstration ended on, what they said "done" means, and what the worker
# claims - and says done, not done, or unsure. Anything but a confident
# "done" is not a success: the caller asks the person, never assumes. The
# judgement is asymmetric on purpose: a false "unsure" costs one question, a
# false "done" is a failure reported as a success.
# ============================================================

DONE_CONFIDENCE = 0.8

DONE, NOT_DONE, UNSURE = "done", "not_done", "unsure"

JUDGE_SYSTEM = (
    "You check a computer worker's work before anyone is told it succeeded. You are shown the task as it was "
    "asked, what the person who taught it said about it, what the worker reports, the screen as it is NOW, and "
    "often the screen at the end of the person's own demonstration of the same task - which was done with other "
    "values, so compare the kind of result, not the exact words.\n"
    "Answer \"done\" only when what you are shown clearly proves the task was carried out for THIS request: the "
    "right page or record, with this request's values, saved or sent or answered as the task asks. Evidence is "
    "the screen now AND what the tools observed during the task (earlier screens are gone, but what was read off "
    "them is listed). For a task that asks for information, each answer in the report must match what the tools "
    "read, and each must answer what was asked - an answer to a different question (a value garbled on the way, "
    "\"if you mean 7 x 8\" for 7+8) is not done.\n"
    "Answer \"not_done\" when you can see it was not: an error, a validation message, the wrong page or record, "
    "another value than asked, a half-filled form, a sign-in page, a CAPTCHA, nothing changed, fewer items than "
    "were asked for.\n"
    "Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\".\n"
    "Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"unsure\", \"confidence\": 0.0-1.0, "
    "\"seen\": \"what the screen now shows, one sentence\", \"why\": \"one sentence\"}"
)


def available():
    return llm_client.available()


def judge(task, claim="", criteria="", values=None, steps="", live_png=None, reference_png=None, observed=None):
    """
    {"verdict", "confidence", "seen", "why"} for whether `task` was done. With
    no model, or when the model cannot be read, the verdict is "unsure" - the
    caller then asks a person.
    """

    if not available():
        return {"verdict": UNSURE, "confidence": 0.0, "seen": "", "why": "there is no model here to check the screen with"}

    lines = [f"The task as asked: {task.strip()[:1500]}"]
    if values:
        lines.append("This run's values: " + "; ".join(f"{k} = {v}" for k, v in values.items() if v not in (None, ""))[:1200])
    if criteria:
        lines.append(f"What the person who taught this task said about it (and what done looks like): {criteria.strip()[:1500]}")
    if steps:
        lines.append(f"What the worker did: {steps.strip()[:2000]}")
    if observed:
        lines.append("What the worker's tools did and read off the screen during this task, as it happened (observed, "
                     "not claimed - a report that matches these is backed by them):\n" + "\n".join(observed)[-6000:])
    lines.append(f"What the worker reports: {(claim or '(nothing)').strip()[:2500]}")

    content = [{"type": "text", "text": "\n".join(lines)}]
    if live_png:
        content += [{"type": "text", "text": "The screen NOW:"}, llm_client.image_part(live_png)]
    else:
        content.append({"type": "text", "text": "(The screen now could not be read.)"})
    if reference_png:
        content += [{"type": "text", "text": "The screen at the end of the person's own demonstration (other values):"},
                    llm_client.image_part(reference_png)]

    try:
        message, spent = llm_client.chat([{"role": "system", "content": JUDGE_SYSTEM},
                                          {"role": "user", "content": content}], max_tokens=600)
        _count(spent)
        found = _parse(message.get("content") or "")
    except (llm_client.LLMError, ValueError) as exc:
        log(f"verify: could not judge: {exc}")
        return {"verdict": UNSURE, "confidence": 0.0, "seen": "", "why": f"the check could not be made ({exc})"}

    if found["verdict"] == DONE and found["confidence"] < DONE_CONFIDENCE:
        found["why"] = f"not sure enough ({found['confidence']:.2f}): {found['why']}"
        found["verdict"] = UNSURE

    log(f"verify: {found}")
    return found


def _parse(text):
    match = re.search(r"\{.*\}", text or "", re.DOTALL)
    if not match:
        raise ValueError(f"the check did not answer in JSON: {text[:200]!r}")
    data = json.loads(match.group(0))
    verdict = str(data.get("verdict") or "").strip().lower().replace(" ", "_")
    if verdict not in (DONE, NOT_DONE, UNSURE):
        verdict = UNSURE
    try:
        confidence = max(0.0, min(1.0, float(data.get("confidence", 0))))
    except (TypeError, ValueError):
        confidence = 0.0
    return {"verdict": verdict, "confidence": confidence,
            "seen": " ".join(str(data.get("seen") or "").split())[:400],
            "why": " ".join(str(data.get("why") or "").split())[:400]}


def _count(spent):
    try:
        usage.add(input_tokens=spent.get("prompt_tokens") or 0, output_tokens=spent.get("completion_tokens") or 0)
    except Exception:
        pass
