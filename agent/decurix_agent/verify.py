import json
import os
import re

from decurix_agent import llm_client
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

DONE, NOT_DONE, UNSURE, CANNOT = "done", "not_done", "unsure", "cannot"

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
    "Go through the request part by part before you answer. Each thing it names - which company, account, "
    "person or record, a date, a reference, the state to leave the result in (a draft, not submitted) - must be "
    "shown, or reported, with that value. A report or a screen that has ANOTHER value for a part of the request "
    "than the request gave is \"not_done\" however complete the rest is: say which part, what was asked and what "
    "was done instead (found live: asked for one company, a record saved under a similarly named other one was "
    "reported in passing and passed).\n"
    "Answer \"not_done\" when you can see it was not: an error, a validation message, the wrong page or record, "
    "another value than asked, a half-filled form, a sign-in page, a CAPTCHA, nothing changed, fewer items than "
    "were asked for.\n"
    "When the task names a document and you are shown what it says, the values entered or reported must be the "
    "document's: a customer, reference, product, quantity or amount that is not in it (or differs from it) is "
    "\"not_done\", however complete the screen looks - except that a thing the document describes is entered "
    "as the application's own record of it (see below), which is not a difference.\n"
    "The same value written in the format its field uses is not a difference: a date field whose placeholder "
    "reads yyyy-dd-mm (year-day-month) shows 14 March 1998 as 1998-14-03; a number may show thousands "
    "separators or a currency. Judge the value, not its format (found live: a correct date of birth was "
    "called wrong).\n"
    "A thing a document describes in its own words is entered as the application's own record of it: the worker "
    "picks, from what the application offers, the existing record - a company, a product, an account, a person - "
    "that is the same thing, and that record's name is usually shorter or worded differently than the document's "
    "line (a document's \"Executive A4 Printing Paper 75 GSM (Box of 10 Reams)\" is the record \"A4 Printing "
    "Paper\"). That is this run's value entered correctly, not another value - above all when the person who "
    "taught the task said to pick the matching existing record. Only a record for a different thing is a "
    "mismatch (found live: correct entries were refused for their records' names, and each retry entered the "
    "document a second time).\n"
    "Answer \"cannot\" when the worker's own report does not claim the task was done: it says the worker stopped, "
    "or could not go on, and gives the reason (something the task needs is not there, a rule it was given forbids "
    "the only way forward, the application refused). That is the worker being honest, not a check that failed: "
    "the person is told what it said (found live: \"this cannot be entered, there is no such record and I must "
    "not create one\" was sent back to be tried again, and the worker then typed into the half-filled form). "
    "A report that claims success is never \"cannot\".\n"
    "When you are shown the files the worker wrote during the task, they are the result, exact as they read now: "
    "every figure, status and statement the report makes about a file must be what that file shows, and the file "
    "must hold what the task asked for. A report that says something the file contradicts (\"these two were "
    "matched\" where the file lists them as unmatched, a total the file does not have, a sheet or column that is "
    "not there) is \"not_done\" - say which file and what differs, so the worker can correct the file or the "
    "report. A file shown cut short is judged on the part shown.\n"
    "Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\".\n"
    "Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"cannot\"|\"unsure\", \"confidence\": 0.0-1.0, "
    "\"seen\": \"what the screen now shows (or, with no screen, what the files show), one sentence\", "
    "\"why\": \"one sentence\"}"
)


def available():
    return llm_client.available()


def judge(task, claim="", criteria="", values=None, steps="", live_png=None, reference_png=None, observed=None,
          screen_text="", chosen=(), produced="", screen=True):
    """
    {"verdict", "confidence", "seen", "why"} for whether `task` was done. With
    no model, or when the model cannot be read, the verdict is "unsure" - the
    caller then asks a person. `produced` is what the files the task wrote say
    now (produced.text); `screen=False` is a task that never touched the
    screen, judged on its files alone.
    """

    if not available():
        return {"verdict": UNSURE, "confidence": 0.0, "seen": "", "why": "there is no model here to check the screen with"}

    lines = [f"The task as asked: {task.strip()[:1500]}"]
    if values:
        typed = {k: v for k, v in values.items() if v not in (None, "") and k.split("__")[0] not in chosen}
        looked_up = {k: v for k, v in values.items() if v not in (None, "") and k.split("__")[0] in chosen}
        if typed:
            lines.append("This run's values: " + "; ".join(f"{k} = {v}" for k, v in typed.items())[:3000])
        if looked_up:
            # Apart from the values to compare, and never as "k = v". Found
            # live after the note beside each was added: with "item = Office
            # Mesh Chairs" among the run's values, the check still refused a
            # correct record "Ergonomic Office Chair" as not the value.
            lines.append("Things this run had to find in the application's own lists and choose (NOT values to "
                         "compare by name - the worker searched and took the existing record of each thing, and a "
                         "record has its own name; judge only whether the record taken is that kind of thing): "
                         + "; ".join(f"for {k}, the document says: {v}" for k, v in looked_up.items())[:2000])
    if criteria:
        lines.append(f"What the person who taught this task said about it (and what done looks like): {criteria.strip()[:6000]}")
    if steps:
        # Not "what the worker did". Found live: these are the demonstration's
        # steps, whose clicks on list entries name the demonstration's own
        # choices; under that label the check kept reporting that a correct
        # run had "selected the wrong company and the wrong products".
        lines.append("The steps of the person's demonstration, which this run followed with its own values (a click "
                     "on a list entry here names the DEMONSTRATION's choice, not this run's - what this run chose "
                     f"is in the tools' record below): {steps.strip()[:2000]}")
    if observed:
        lines.append("What the worker's tools did and read off the screen during this task, as it happened (observed, "
                     "not claimed - a report that matches these is backed by them; a look at the screen listed here "
                     "shows what was on it at that moment, a page since closed included):\n" + "\n".join(observed)[-24000:])
    if screen_text:
        lines.append("The text the fields on the screen hold NOW, read from the apps themselves (exact - where it and "
                     "the picture's small print disagree, this is right; a report that says a text was changed is "
                     "backed only if this shows it):\n" + screen_text)
    lines.append(f"What the worker reports: {(claim or '(nothing)').strip()[:4000]}")
    if produced:
        lines.append("The files the worker wrote during this task, as they read NOW (the result itself - the report "
                     "must agree with them):\n" + produced)
    doc_text, doc_pictures = _documents(task)
    if doc_text:
        lines.append("The documents the task names, as they read (the values must come from these):\n" + doc_text)

    content = [{"type": "text", "text": "\n".join(lines)}]
    for picture in doc_pictures:
        content += [{"type": "text", "text": "A page of a document the task names:"}, llm_client.image_part(path=picture)]
    if live_png:
        content += [{"type": "text", "text": "The screen NOW:"}, llm_client.image_part(live_png)]
    elif not screen:
        content.append({"type": "text", "text": "(This task did not work on the screen: judge it on the files and on "
                                                "what the tools read.)"})
    else:
        content.append({"type": "text", "text": "(The screen now could not be read.)"})
    if reference_png:
        content += [{"type": "text", "text": "The screen at the end of the person's own demonstration (other values):"},
                    llm_client.image_part(reference_png)]

    try:
        message, spent = llm_client.chat([{"role": "system", "content": JUDGE_SYSTEM},
                                          {"role": "user", "content": content}], max_tokens=600)
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
    if verdict not in (DONE, NOT_DONE, UNSURE, CANNOT):
        verdict = UNSURE
    try:
        confidence = max(0.0, min(1.0, float(data.get("confidence", 0))))
    except (TypeError, ValueError):
        confidence = 0.0
    return {"verdict": verdict, "confidence": confidence,
            "seen": " ".join(str(data.get("seen") or "").split())[:400],
            "why": " ".join(str(data.get("why") or "").split())[:400]}

