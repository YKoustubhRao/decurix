import json
import os
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
    "A screen left over from before is not this task's result: when it shows the demonstration's own values (or "
    "any others) where this run's should be, it is \"not_done\".\n"
    "What a taught task's \"done\" looks like was written from one demonstration. Its details that belonged to "
    "that demonstration's own result - a person's city, a company, a date, an amount - are examples and do NOT "
    "apply to this run: a search taught on one person whose profile showed \"Udupi, Karnataka\" is done for "
    "another person when THAT person's profile is open, whatever city it shows. Never answer not_done or unsure "
    "because such a detail differs or is missing (found live: a correct run was refused for exactly that).\n"
    "Answer \"not_done\" when you can see it was not: an error, a validation message, the wrong page or record, "
    "another value than asked, a half-filled form, a sign-in page, a CAPTCHA, nothing changed, fewer items than "
    "were asked for.\n"
    "When the task names a document and you are shown what it says, the values entered or reported must be the "
    "document's: a customer, reference, product, quantity or amount that is not in it (or differs from it) is "
    "\"not_done\", however complete the screen looks.\n"
    "The same value written in the format its field uses is not a difference: a date field whose placeholder "
    "reads yyyy-dd-mm (year-day-month) shows 14 March 1998 as 1998-14-03; a number may show thousands "
    "separators or a currency. Judge the value, not its format (found live: a correct date of birth was "
    "called wrong).\n"
    "Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\".\n"
    "Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"unsure\", \"confidence\": 0.0-1.0, "
    "\"seen\": \"what the screen now shows, one sentence\", \"why\": \"one sentence\"}"
)


def available():
    return llm_client.available()


def judge(task, claim="", criteria="", values=None, steps="", live_png=None, reference_png=None, observed=None,
          screen_text=""):
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
    if screen_text:
        lines.append("The text the fields on the screen hold NOW, read from the apps themselves (exact - where it and "
                     "the picture's small print disagree, this is right; a report that says a text was changed is "
                     "backed only if this shows it):\n" + screen_text)
    lines.append(f"What the worker reports: {(claim or '(nothing)').strip()[:2500]}")
    doc_text, doc_pictures = _documents(task)
    if doc_text:
        lines.append("The documents the task names, as they read (the values must come from these):\n" + doc_text)

    content = [{"type": "text", "text": "\n".join(lines)}]
    for picture in doc_pictures:
        content += [{"type": "text", "text": "A page of a document the task names:"}, llm_client.image_part(path=picture)]
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


# A document named by its path in a task; the same shape TheBot's operator
# refuses to type values from before reading (internal/skills/grounding.go).
# A native Windows worker's documents are named C:\...\po.pdf.
DOCUMENT_PATH = re.compile(r"(?:~|/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.(?:pdf|xlsx|xlsm|docx|csv|eml|png|jpe?g)\b", re.I)
MAX_DOCUMENT_CHARS = 4000
MAX_DOCUMENT_PICTURES = 2


def _documents(task):
    """
    (text, [picture paths]) of the documents the task names, read the way the
    worker reads them. Found live: a PO was entered with a customer and lines
    the PDF never had, and the check - shown only the screen - saw a saved
    quotation and passed it. Unreadable documents are left out, never guessed.
    """

    from decurix_agent.tools import office

    texts, pictures = [], []
    for path in dict.fromkeys(DOCUMENT_PATH.findall(task or "")):
        try:
            doc = office.read_document(path)
        except Exception as exc:  # a missing or unreadable file: judged without it
            log(f"verify: could not read {path}: {exc}")
            continue
        texts.append(f"--- {path}\n{(doc.get('text') or '').strip()}")
        for image in doc.get("images") or []:
            if len(pictures) < MAX_DOCUMENT_PICTURES and os.path.isfile(image):
                pictures.append(image)
    return "\n".join(texts)[:MAX_DOCUMENT_CHARS], pictures


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
