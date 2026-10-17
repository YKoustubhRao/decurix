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
    "A report that says the worker did not check a result the request asks for (\"I did not verify the file in the "
    "folder\", \"could not confirm it saved\") is not proof of it: unless the screen or the observations show that "
    "result, answer \"unsure\" (found live: a Word letter reported saved was an empty file).\n"
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
    "A report that says a value of the request or its document was left out, cleared, shortened or replaced "
    "because the application refused it (\"it rejected the GSTIN, so I saved without it\", \"set it to "
    "Unregistered to complete the form\") is never \"done\": the record now says something the document does not. "
    "Answer \"not_done\" and name the value (found live: a vendor saved as not GST-registered after its tax number "
    "was refused passed this check).\n"
    "A record made from a document must carry that document's own reference (its number, and its date where "
    "there is a place for it): an order without the customer's order number, a receipt without the challan "
    "number, is \"not_done\" when the record has a field for it - say which. Check the report and the screen for "
    "it, not only the lines and totals (found live: two sales orders without the customer's PO number passed).\n"
    "A period the document gives (valid for 30 days, due in 15 days) must be the record's date counted from its "
    "own start date: count the days - an application's default of \"one month\" is 30 or 31 days by the calendar, "
    "and a day off is \"not_done\" (found live: a quote asked to be valid 30 days was saved valid for 31).\n"
    "A result must be where the request said to put it: a file saved in another folder (over the attached "
    "original in an inbox folder instead of in the work folder named) is \"not_done\" - say where it is. A request to "
    "save in a folder of the person's own computer (C:\\...) or to work in a named app is met only by that app saving "
    "there: a file in the worker's own folders, or work done in another app, is \"not_done\".\n"
    "Read what the report itself says was left undone. A value the document gives that the record has a place "
    "for, left out because the worker did not find or did not look for its field (\"no visible field on the part "
    "of the form I completed\"), is \"not_done\": say which value - a form keeps most of its fields further down "
    "or in another section, and the worker is sent back to find it (found live: a sales order passed without "
    "the customer's own order number and date, which the report listed as left undone). Only a reason that "
    "stands - the application has no place for it at all, the person said to leave it - lets it pass.\n"
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
    "A rule about HOW to work - look for an existing record first, compare a total and say both, check "
    "something before saving - is a step of the method, not part of the result. It happened before the screen "
    "you see, so the screen cannot show it: judge it from what the tools observed during the task (the actions "
    "list holds the search that was typed and what it found) and from the report. When those show it, or the "
    "report states it and nothing you are shown contradicts it, it does not make a correct result \"unsure\". "
    "What does make the task not done is the RESULT showing the rule was broken: a second record with the same "
    "reference, a record submitted where a draft was asked, a total the report gets wrong. Found live: a "
    "correct draft whose report said it had searched first and compared the totals (and both totals were on "
    "the screen) was put to a person as \"no proof the duplicate check was made\", on every bill.\n"
    "Answer \"cannot\" when the worker's own report does not claim the task was done: it says the worker stopped, "
    "or could not go on, and gives the reason (something the task needs is not there, a rule it was given forbids "
    "the only way forward, the application refused). That is the worker being honest, not a check that failed: "
    "the person is told what it said (found live: \"this cannot be entered, there is no such record and I must "
    "not create one\" was sent back to be tried again, and the worker then typed into the half-filled form). "
    "A report that claims success is never \"cannot\". Nor is one that names no obstacle: a worker that did a "
    "first part (a check that found nothing in the way) and then stopped, saying only what it has not done yet, "
    "could have gone on - that is \"not_done\", and it is sent back to finish (found live: \"no existing invoice "
    "was found; I have not entered the new draft yet\" was put to a person as if something stood in the way).\n"
    "When you are shown the files the worker wrote during the task, they are the result, exact as they read now: "
    "every figure, status and statement the report makes about a file must be what that file shows, and the file "
    "must hold what the task asked for. A report that says something the file contradicts (\"these two were "
    "matched\" where the file lists them as unmatched, a total the file does not have, a sheet or column that is "
    "not there) is \"not_done\" - say which file and what differs, so the worker can correct the file or the "
    "report. Work out yourself every total, count or sum that the report or an email states, from the figures the "
    "files show: one that does not add up is \"not_done\" (found live: an email gave a payable total of 6,899.40 "
    "for rows summing to 6,900.40, and passed). When the request gives rules for working figures out (rates, caps, "
    "formulas, a method, thresholds) and you are shown the source documents, recompute at least two rows of the result "
    "yourself from the sources by those rules - the first row and one that a rule changes - and compare: a figure that "
    "does not follow the rules is \"not_done\", however consistent the file and the report are with each other (found "
    "live: a depreciation schedule whose opening values charged one year where the assets had been in service for two "
    "passed, because the report described the file correctly). When rules sort the source's rows into places (kept / "
    "rejected, matched / unmatched, OK / exception, duplicate - keep the first), follow every source row yourself to "
    "where the rules put it and check it is there, exactly once: a row in the wrong place or in no place is "
    "\"not_done\" (found live: both copies of a duplicated contact were rejected where the first was to be kept, and "
    "passed). Every rule applies to every entry it covers: an entry one rule adjusted (a deduction) is still subject "
    "to the next (a cap, a limit) - a figure above a cap the policy sets is \"not_done\" (found live: a meal approved "
    "at 60.50 over a 60.00 cap, after its wine was deducted, passed). A total of something the request defines (\"every credit on the statement is a customer receipt\") counts "
    "everything that definition covers - not only the part a later step used (found live: \"total receipts\" was given "
    "as the receipts applied to invoices, leaving out a receipt no invoice matched, and passed). A record the worker made during this task (its recorded actions fill and save it, or its report says it "
    "entered it) is the task's result, not one that \"already existed\": the screen at the end shows the result, and "
    "only what was seen before the worker made anything says what existed before (found live: three orders the "
    "worker had just entered were called duplicates of themselves). A file shown cut short is judged on the part shown. Text in a file that still holds a web page's markup "
    "or character codes (&amp;, &#39;, &quot;, <br>, <b>) is \"not_done\": it must read as the page shows it - say "
    "which entry (found live: a workbook of book titles kept \"It&#39;s Only the Himalayas\" and passed). In a "
    "workbook, a figure the task asks for must be a number in a cell of its own: a quantity written with its unit "
    "(\"2 Units\") or several figures in one text cell (\"Subtotal: 50000; Tax: 9000\") is \"not_done\".\n"
    "Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\".\n"
    "Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"cannot\"|\"unsure\", \"confidence\": 0.0-1.0, "
    "\"seen\": \"what the screen now shows (or, with no screen, what the files show), one sentence\", "
    "\"why\": \"one sentence\"}"
)


def available():
    return llm_client.available()


# A report's own heading for what it did not do ("Left undone:", "**Not done:**").
_UNDONE_HEAD = re.compile(r"^\s*(?:[*_#>\-\s]*)(left undone|not done|undone|not completed|could not (?:do|complete))"
                          r"(?:[*_\s]*)[:\-]\s*(.*)$", re.I)
# What under such a heading says that nothing was left.
_NOTHING_LEFT = re.compile(r"^[\s*_\-.]*(?:none|nothing|n/?a\b|no (?:part|item|step)s?\b|all (?:done|of it)\b)", re.I)
# A line that opens the next part of a report ("How I verified:").
_NEXT_HEAD = re.compile(r"^\s*[*_#]*[A-Z][^:\n]{0,60}:\s*$")


def left_undone(report):
    """
    What a report lists under its own "Left undone:" heading, or "" when it lists nothing there. Decided with no
    model: found live (2026-10-15), asked to do a report in real Excel and save it in a folder of the person's
    computer, a worker re-saved a file an earlier task had left open in another app, wrote "Left undone: I could not
    use Excel ... I could not verify the host path", and the check - looking at that file - passed it as done.
    """

    lines = (report or "").splitlines()
    found = []
    for i, line in enumerate(lines):
        m = _UNDONE_HEAD.match(line)
        if not m:
            continue
        items = [m.group(2).strip()] if m.group(2).strip() else []
        for follow in lines[i + 1:]:
            if not follow.strip() or _NEXT_HEAD.match(follow) or _UNDONE_HEAD.match(follow):
                break
            items.append(follow.strip())
        items = [x for x in items if not _NOTHING_LEFT.match(x)]
        found.extend(items)
    return "; ".join(found)[:600]


# What an item left undone says when the worker did not FIND a place for the value (a field, a section, a control), as
# against a thing that stands in the way (the application refused it, the person said not to, a file is missing).
_NOT_FOUND = re.compile(
    r"\b(?:no|not|n't|couldn't|cannot|can't|unable)\b[^.;\n]{0,60}\b(?:visible|available|found|find|see|locate|reach|"
    r"exposed|present|shown|listed|accessible)\b|\b(?:no|missing)\b[^.;\n]{0,30}\b(?:field|section|control|option|"
    r"button|tab|place)\b", re.I)
_REFUSED = re.compile(r"\b(?:refus|reject|invalid|error|blocked|permission|approval|person said|you said|told me not|"
                      r"not allowed|rate limit|expired|already exists)\w*", re.I)


def not_found_undone(undone):
    """
    True when what a report lists as left undone is a place it did not find (a field "not visible", a section "not
    available") and nothing in the way: such a form almost always has the place in a collapsed section, a tab or
    further down, so the worker is sent back once to look there before a person is asked. Found live (2026-10-18):
    a sales order was saved without the customer's PO number - "no visible PO number field ... in the section I could
    access" - when the field was in a collapsed section of the same form.
    """

    text = undone or ""
    return bool(text.strip()) and bool(_NOT_FOUND.search(text)) and not _REFUSED.search(text)


# A sum written out in a report: "450.00 + 90.00 + 102.60 = 642.60", "$1,200 - 150 = $1,050", "3 x 25.00 = 75.00".
_AMOUNT = r"(?:[$€£¥]|USD|EUR|GBP|INR|Rs\.?)?\s?-?\d{1,3}(?:,\d{3})+(?:\.\d+)?|(?:[$€£¥]|USD|EUR|GBP|INR|Rs\.?)?\s?-?\d+(?:\.\d+)?"
_SUM = re.compile(rf"(?<![\w.,/-])((?:{_AMOUNT})(?:\s*[-+×x*]\s*(?:{_AMOUNT}))+)\s*=\s*({_AMOUNT})(?![\w%/.]|,\d)", re.I)


def _amount(text):
    from decimal import Decimal
    digits = re.sub(r"[^\d.\-]", "", text.replace(",", ""))
    return Decimal(digits)


def false_sums(text):
    """
    Each sum a text writes out that does not come to what it says, as a sentence; [] when every one does. Decided with
    no model. Found live (2026-10-14): a report read "750.00 + 300.00 + 84.00 = 1143.00" (it is 1134.00), so an
    invoice whose lines do not add up to its total was registered OK - and the model check took the report's word.
    """

    from decimal import Decimal, InvalidOperation

    out = []
    for m in _SUM.finditer(text or ""):
        left, stated = m.group(1), m.group(2)
        parts = re.split(r"\s*([-+×x*])\s*(?=[$€£¥A-Z]{0,3}\s?-?\d)", left)
        try:
            # Multiplication first, then sums and differences, left to right.
            terms, ops = [_amount(parts[0])], []
            for op, value in zip(parts[1::2], parts[2::2]):
                if op.lower() in ("×", "x", "*"):
                    terms[-1] = terms[-1] * _amount(value)
                else:
                    ops.append(op)
                    terms.append(_amount(value))
            total = terms[0]
            for op, value in zip(ops, terms[1:]):
                total = total + value if op == "+" else total - value
            want = _amount(stated)
        except (InvalidOperation, ValueError, IndexError):
            continue
        places = max([len(p.split(".")[1]) for p in re.findall(r"\d+\.\d+", m.group(0))] + [0])
        # Rounded to the result's own precision is right: 33.333 + 33.333 = 66.67.
        shown = len(stated.split(".")[1]) if "." in stated else 0
        if abs(total - want) > Decimal(1).scaleb(-shown) / 2 + Decimal("0.0001"):
            out.append(f"{m.group(0).strip()} is wrong: it comes to {total:,.{places}f}")
    return out


# A number as a program prints it, never as a person writes one: float noise (1672.7400000000002, 0.30000000000000004,
# 2.9999999999999996) and scientific notation (3E+1, 1.5e-05).
_FLOAT_NOISE = re.compile(r"(?<![\w.])-?\d+\.\d*(?:0{6,}|9{6,})\d{1,3}(?![\w.])")
_SCIENTIFIC = re.compile(r"(?<![\w.])-?\d+(?:\.\d+)?[eE][+-]?\d{1,3}(?![\w.])")


def program_numbers(text):
    """
    Numbers in what a task wrote that are written as a program prints them, as a list; [] when there are none.
    Decided with no model. Found live (2026-10-14): a payslip PDF showed hours as "3E+1"; a person reading it sees a
    broken document. (Plain decimals - 7183.0, 1135.125 - are left to the rules and the check: they can be right.)
    """

    found = _FLOAT_NOISE.findall(text or "") + _SCIENTIFIC.findall(text or "")
    return list(dict.fromkeys(found))[:6]


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
# Shared fairly among the task's documents, so none is cut to nothing. Found live (2026-10-13): three bills
# read together in 4000 characters left the last one cut off, and the check could only answer "unsure"; a
# 12-page policy needs all of itself to confirm "every limit" (as much as office.read_document gives).
MAX_DOCUMENT_CHARS = 30000
MAX_DOCUMENT_PICTURES = 2


def _documents(task):
    """
    (text, [picture paths]) of the documents the task names, read the way the
    worker reads them. Found live: a PO was entered with a customer and lines
    the PDF never had, and the check - shown only the screen - saw a saved
    quotation and passed it. Unreadable documents are left out, never guessed.
    """

    from decurix_agent.tools import office

    from decurix_agent import attachments

    texts, pictures = [], []
    for path in dict.fromkeys(DOCUMENT_PATH.findall(task or "")):
        # What the person sent, even when the task has since changed the file in place.
        source = attachments.original(path)
        try:
            doc = office.read_document(source)
        except Exception as exc:  # a missing or unreadable file: judged without it
            log(f"verify: could not read {path}: {exc}")
            continue
        label = path if source == path else f"{path} (as the person sent it; the task has changed this file since)"
        texts.append(f"--- {label}\n{(doc.get('text') or '').strip()}")
        for image in doc.get("images") or []:
            if len(pictures) < MAX_DOCUMENT_PICTURES and os.path.isfile(image):
                pictures.append(image)
    share = MAX_DOCUMENT_CHARS // max(len(texts), 1)
    return "\n".join(t if len(t) <= share else t[:share] + " [...]" for t in texts), pictures


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

