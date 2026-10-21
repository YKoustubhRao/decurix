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
# The task text the judge is given (a long stage's instruction, inputs, binding answers, the system's read-back, the
# code checks' evidence), and how many things it may ask to look up when a part it needs was not shown.
MAX_TASK_CHARS = 20000
MAX_LOOK = 6

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
    "read, and each must answer what was asked.\n"
    "A report that says the worker did not check a result the request asks for is not proof of it: unless the "
    "screen or the observations show that result, answer \"unsure\".\n"
    "A screen left over from before is not this task's result: when it shows the demonstration's own values (or "
    "any others) where this run's should be, it is \"not_done\". A taught task's details that belonged to the "
    "demonstration's own result (a person's city, a company, a date, an amount) are examples, not requirements: "
    "never answer not_done or unsure because such a detail differs.\n"
    "Read the report's own evidence against the request: when a figure the report or the files give contradicts "
    "what the request asks, the answer is \"not_done\" however the report sums itself up - say what was asked and "
    "what the evidence shows.\n"
    "When the request names the app to do the work in, the tools' record must show that app's own window opened (its "
    "name in the window's title): work done in another app in its place - another program of the same kind - is not "
    "what was asked; answer \"cannot\" and say which app was used instead, unless the report says the named app was not "
    "available and the person agreed. A file whose content was made with tools (office, python) and that was then "
    "opened in the named app's window, checked there and saved from it IS done in that app; made with tools and never "
    "opened and saved in that app, it is not. An app named only as a file's format (\"the template (<app>)\", \"a "
    "<app> file\") or offered as a choice (\"if you use <app> there\") is not an app the work must be done in.\n"
    "Go through the request part by part before you answer. Each thing it names - which company, account, person or "
    "record, a date, a reference, the state to leave the result in (a draft, not submitted) - must be shown, or "
    "reported, with that value; ANOTHER value for a part of the request is \"not_done\" however complete the rest is: "
    "say which part, what was asked and what was done instead.\n"
    "A value of the request or its document that was left out, cleared, shortened or replaced because the application "
    "refused it is never \"done\": the record now says something the document does not - name the value. A value the "
    "record has a place for, left out because the worker did not find its field, is \"not_done\" too; only a reason "
    "that stands (no place for it at all, the person said to leave it) lets it pass. A record made from a document "
    "carries that document's own reference (its number, and its date) where the record has a field for it.\n"
    "A period the document gives (valid for N days) is counted from the record's own date, never taken from an "
    "application's default.\n"
    "A result must be where the request said to put it: a file saved in another folder is \"not_done\" - say where it "
    "is. A request to save in a folder of the person's own computer (C:\\...) is met only by a file saved there.\n"
    "Answer \"not_done\" when you can see it was not: an error, a validation message, the wrong page or record, "
    "another value than asked, a half-filled form, a sign-in page, a CAPTCHA, nothing changed, fewer items than "
    "were asked for.\n"
    "When the task names a document and you are shown what it says, the values entered or reported must be the "
    "document's: a value that is not in it, or differs from it, is \"not_done\" - except that a thing the document describes is entered as the application's own record of the same "
    "thing, whose name is usually shorter or worded differently (a document's \"Executive A4 Printing Paper 75 GSM "
    "(Box of 10 Reams)\" is the record \"A4 Printing Paper\"): that is the value entered correctly. Only a record for a "
    "different thing is a mismatch. The same value written in its field's format (a date in the field's own order, "
    "thousands separators, a currency) is not a difference: judge the value, not its format.\n"
    "A rule about HOW to work (look for an existing record first, compare before saving) is a step of the method: judge "
    "it from what the tools observed and the report, never as missing from the final screen. What makes the task not "
    "done is the RESULT showing the rule was broken (a second record with the same reference, a record submitted where "
    "a draft was asked).\n"
    "Answer \"cannot\" only when the worker's own report says it stopped and gives the reason (something the task needs "
    "is not there, a rule it was given forbids the only way forward, the application refused): that is the worker being "
    "honest, and the person is told what it said. A report that claims success is never \"cannot\"; nor is one that "
    "names no obstacle - that is \"not_done\".\n"
    "When you are shown the files the worker wrote, they are the result, exact as they read now. Every figure, status "
    "and statement the report makes about a file must be what that file shows; a report the file contradicts is "
    "\"not_done\" - say which file and what differs. Work out yourself every total, count or sum the report or an email "
    "states, from the figures the files show. When the request gives rules for working figures out and you are shown "
    "the sources, recompute at least two rows from the sources by those rules - the first, and one a rule changes - and "
    "compare. A figure the result presents as a source's own must equal what that source states. When rules sort the "
    "source's rows into places, every source row must be where the rules put it, exactly once (a copy shown as its own "
    "row marked as the duplicate, with its amounts in no total, is counted once). Every rule applies to every entry it "
    "covers, in the request's order. A total of something the request defines counts everything the definition "
    "covers. A record the worker made during this task is the task's result, not one that \"already existed\". A file "
    "shown cut short is judged on the part shown. Text taken from a web page reads as the page shows it (no markup or "
    "character codes), and in a workbook a figure the task asks for is a number in a cell of its own.\n"
    "A refusal about how an item is classified or decided (its category, priority, status, approval) must rest on the "
    "item's own source - its message, document or record - and say the source's own words that require the change. The "
    "worker's working notes are not a source: a note that disagrees with the item's classification is a fault in the "
    "notes - say so, and that the notes are to be corrected, never the answer the source supports. When the worker kept "
    "an answer against an earlier check and quotes the source, judge the quote against the source.\n"
    "Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\". When you are unsure only "
    "because a part of a file you need is not shown (a row, a person, a section), add \"look\": up to 6 exact words to "
    "find in the files (a row's reference, a name, a heading) - you will be shown the lines that hold them.\n"
    "Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"cannot\"|\"unsure\", \"confidence\": 0.0-1.0, "
    "\"seen\": \"what the screen now shows (or, with no screen, what the files show), one sentence\", "
    "\"why\": \"one sentence\", \"look\": [optional]}"
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
_SUM = re.compile(rf"(?<![\w.,/-])((?:{_AMOUNT})(?:\s*[-+×x*@]\s*(?:{_AMOUNT}))+)\s*=\s*({_AMOUNT})(?![\w%/.]|,\d)", re.I)
# One token of a written sum: an amount (with its currency) or an operator; a product's "x", "×", "*" or "@".
_SUM_TOKEN = re.compile(rf"\s*(?:({_AMOUNT})|([-+×x*@]))", re.I)


def _sum_terms(left):
    """[amount, op, amount, op, ...] of the left side of a written sum, read token by token - or None when any part of
    it is not an amount or an operator in turn (then the sum is not decided: a blocking fact must be certain). Found live
    (round 5 P6 on pc3, 2026-10-21): "60 x Rs 35 = Rs 2,100" was read as one number 6035 - the currency's lower-case
    letters fell outside the old split - and a right line was refused twice."""

    tokens, pos = [], 0
    while pos < len(left):
        m = _SUM_TOKEN.match(left, pos)
        if not m or m.end() == pos:
            return None
        tokens.append(("n", m.group(1)) if m.group(1) is not None else ("op", m.group(2)))
        pos = m.end()
    kinds = [k for k, _ in tokens]
    if not kinds or kinds[0] != "n" or kinds[-1] != "n" or any(a == b for a, b in zip(kinds, kinds[1:])):
        return None
    return [v for _, v in tokens]


def _amount(text):
    from decimal import Decimal
    # The currency first: the dot of "Rs." is no decimal point (found live: "Rs. 35" was read as 0.35).
    bare = re.sub(r"(?i)^\s*(?:[$€£¥₹]|USD|EUR|GBP|INR|Rs\.?)\s*", "", text)
    digits = re.sub(r"[^\d.\-]", "", bare.replace(",", ""))
    return Decimal(digits)


# One item of a priced list: "40 @ 175.75", "24 x 149", "30 × Rs 95".
_ITEM = re.compile(r"(?<![\w.,])(\d(?:[\d,]*\d)?(?:\.\d+)?)\s*(?:@|×|x|\*)\s*(?:Rs\.?\s*|₹\s*|INR\s*|USD\s*|\$\s*)?"
                   r"(\d(?:[\d,]*\d)?(?:\.\d+)?)(?![\d])", re.I)
# A stated total tied to the items that follow it: "should total 13129.00 ex GST from ...", "totals Rs 500 for: ...".
_TOTAL_OF = re.compile(r"(?i)\btotal(?:s|led|ling)?\b(?:\s+(?:of|is|to|would be|should be|comes to))?\s+"
                       r"(?:Rs\.?\s*|₹\s*|INR\s*|USD\s*|\$\s*)?(\d(?:[\d,]*\d)?(?:\.\d+)?)([^.\n]{0,40}?)\s(?:from|for|of|over)\b:?")
_TAX_RATE = re.compile(r"(\d{1,2}(?:\.\d+)?)\s*%")


def false_item_totals(text):
    """
    Each stated total of a list of "qty @ rate" items that the items do not come to, as a sentence. Found live
    (2026-10-20, P2): a stage told the person a saved order was wrong because the lines "should total 13129.00 ex GST
    from WB-500 40 @ 175.75, MUG-12 24 @ 149, NB-A5 30 @ 95" - they come to 13,456.00, the record's own total. A total
    stated with a tax rate nearby may be with or without it.
    """

    from decimal import Decimal, InvalidOperation

    out = []
    for m in _TOTAL_OF.finditer(text or ""):
        rest = text[m.end():]
        end = re.search(r"\n|\.\s+[A-Z]|;|\s-\s|\s\u2013\s", rest)
        span = rest[:end.start() if end else 400][:400]
        items = _ITEM.findall(span)
        if len(items) < 2:
            continue
        # "totals of 2 orders from ...": a count of things, not an amount.
        if re.match(r"\s*[A-Za-z]+s\b", m.group(2) or "") and not re.match(r"(?i)\s*(?:ex|excl\w*|incl\w*|plus|with)\b",
                                                                           m.group(2) or ""):
            continue
        try:
            total = sum((_amount(q) * _amount(r) for q, r in items), Decimal(0))
            stated = _amount(m.group(1))
        except (InvalidOperation, ValueError):
            continue
        sentence = text[max(0, m.start() - 200):m.end() + len(span)]
        rates = [Decimal(1)] + [1 + Decimal(t) / 100 for t in _TAX_RATE.findall(sentence)]
        if any(abs(total * rate - stated) <= Decimal("0.01") for rate in rates):
            continue
        shown = ", ".join(f"{q} @ {r}" for q, r in items)
        out.append(f"the total {m.group(1)} stated for {shown} is wrong: those come to {total:,.2f}")
    return out


def false_sums(text):
    """
    Each sum a text writes out that does not come to what it says, as a sentence; [] when every one does. Decided with
    no model. Found live (2026-10-14): a report read "750.00 + 300.00 + 84.00 = 1143.00" (it is 1134.00), so an
    invoice whose lines do not add up to its total was registered OK - and the model check took the report's word.
    A priced list with a stated total ("should total N from 40 @ 175.75, 24 @ 149") is checked too (false_item_totals).
    """

    from decimal import Decimal, InvalidOperation

    out = false_item_totals(text)
    for m in _SUM.finditer(text or ""):
        left, stated = m.group(1), m.group(2)
        parts = _sum_terms(left)
        if parts is None:
            continue  # not read with certainty: not decided
        try:
            # Multiplication first, then sums and differences, left to right.
            terms, ops = [_amount(parts[0])], []
            for op, value in zip(parts[1::2], parts[2::2]):
                if op.lower() in ("×", "x", "*", "@"):
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
          screen_text="", chosen=(), produced="", screen=True, deliverables=(), look_up=None):
    """
    {"verdict", "confidence", "seen", "why"} for whether `task` was done. With
    no model, or when the model cannot be read, the verdict is "unsure" - the
    caller then asks a person. `produced` is what the files the task wrote say
    now (produced.text); `screen=False` is a task that never touched the
    screen, judged on its files alone. `look_up(terms)`: the lines of the
    task's files that hold those terms - when the judge is unsure only because
    a part it needs was not shown, it names what to find and judges once more
    with those lines (the judge asks for evidence; nothing is guessed for it).
    """

    if not available():
        return {"verdict": UNSURE, "confidence": 0.0, "seen": "", "why": "there is no model here to check the screen with"}

    # The task is the stage's instruction AND what the caller adds after it - the person's binding answers, the
    # system's own read-back, the code checks' evidence, the other stages' context. Found (2026-10-21): cut at 1,500
    # characters, everything after a long instruction and its inputs list never reached the judge.
    body = task.strip()
    if len(body) > MAX_TASK_CHARS:
        body = body[:MAX_TASK_CHARS] + f"\n[cut here: {len(body) - MAX_TASK_CHARS} more characters not shown]"
    lines = [f"The task as asked: {body}"]
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
    # Where the person's shared folder is on this desktop, in plain words: a path under it is that folder, never "the
    # wrong folder" (found live, 2026-10-19). Whether a file is where the request said is decided in code
    # (produced.missing_on_host / misplaced); this keeps the model from contradicting it.
    from decurix_agent import shared
    note = shared.mapping_note()
    if note:
        lines.append(note + " Do not judge a file's location by how its path is spelled: a file the request asks to be "
                     "saved in that folder and found under that path IS saved where asked.")
    if deliverables:
        # Found live (2026-10-19): a correct, cleanly saved register was refused because the screen still showed a
        # range the worker had filled by mistake and cleared, and the action log showed the mistake.
        lines.append("The file(s) the request asks for - " + ", ".join(os.path.basename(str(d)) for d in deliverables)
                     + " - are saved, and as they read above they ARE that part of the result. Decide that part from "
                     "the saved file(s) alone. A mistake the action log shows and the saved file no longer has, or an "
                     "unsaved change still on the screen, is not a reason for not_done: mention it in 'seen'. Judge "
                     "not_done for what is wrong or missing in the saved file(s), for another part of the request "
                     "not done, or for a report the files contradict.")
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

    def ask(parts):
        try:
            message, spent = llm_client.chat([{"role": "system", "content": JUDGE_SYSTEM},
                                              {"role": "user", "content": parts}], max_tokens=600)
            found = _parse(message.get("content") or "")
        except (llm_client.LLMError, ValueError) as exc:
            log(f"verify: could not judge: {exc}")
            return {"verdict": UNSURE, "confidence": 0.0, "seen": "", "why": f"the check could not be made ({exc})",
                    "look": []}
        if found["verdict"] == DONE and found["confidence"] < DONE_CONFIDENCE:
            found["why"] = f"not sure enough ({found['confidence']:.2f}): {found['why']}"
            found["verdict"] = UNSURE
        return found

    found = ask(content)
    if found["verdict"] == UNSURE and found.get("look") and look_up is not None:
        try:
            closer = look_up(found["look"])
        except Exception as exc:  # noqa: BLE001 - a closer look is extra evidence, never a reason to fail the check
            log(f"verify: could not look closer: {exc}")
            closer = ""
        if closer:
            log(f"verify: unsure, looking closer at {found['look']}")
            found = ask(content + [{"type": "text", "text": (
                "You were unsure and asked to see: " + "; ".join(found["look"]) + ". The lines of the files that hold "
                "them, looked up for you (exact):\n" + closer + "\nJudge again with these. Do not ask to look again.")}])
    found.pop("look", None)
    log(f"verify: {found}")
    return found


# A document named by its path in a task; the same shape TheBot's operator
# refuses to type values from before reading (internal/skills/grounding.go).
# A native Windows worker's documents are named C:\...\po.pdf.
# (A path starts at "~", "/" or a drive - never at the "/" inside "P5_MIS_Q2/x.xlsx", which was read as "/x.xlsx".)
DOCUMENT_PATH = re.compile(r"(?:~|(?<![\w.~-])/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.(?:pdf|xlsx|xlsm|docx|csv|eml|png|jpe?g)\b",
                           re.I)
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

    def add(path, source, label):
        try:
            doc = office.read_document(source)
        except Exception as exc:  # a missing or unreadable file: judged without it
            log(f"verify: could not read {path}: {exc}")
            return
        texts.append(f"--- {label}\n{(doc.get('text') or '').strip()}")
        for image in doc.get("images") or []:
            if len(pictures) < MAX_DOCUMENT_PICTURES and os.path.isfile(image):
                pictures.append(image)

    for path in dict.fromkeys(DOCUMENT_PATH.findall(task or "")):
        # What the person sent, even when the task has since changed the file in place.
        source = attachments.original(path)
        add(path, source, path if source == path else f"{path} (as the person sent it; the task has changed this file since)")
    # The files the task names by name that are in the worker's shared folder and were there before it began: what it
    # was given to work from, read like any attachment (its request names the folder once, then the files).
    for local in _shared_inputs(task):
        add(local, local, local)
    share = MAX_DOCUMENT_CHARS // max(len(texts), 1)
    return "\n".join(t if len(t) <= share else t[:share] + " [...]" for t in texts), pictures


def _shared_inputs(task):
    """
    The files in the worker's shared folder that the task names and that were there before it began (inputs, not what
    the task made). [] when there is no shared folder.
    """

    from decurix_agent import attachments, produced, shared

    if not shared.active():
        return []
    began = produced.since()
    names = {n.lower() for n in produced.REQUESTED_FILE.findall(attachments.text(task or ""))}
    names -= {n.lower() for n in produced.requested_names(task)}
    out = []
    for path in shared.named(names).values():
        try:
            if began is None or os.path.getmtime(path) <= began:
                out.append(path)
        except OSError:
            continue
    return out


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
    look = data.get("look") if isinstance(data.get("look"), list) else []
    return {"verdict": verdict, "confidence": confidence,
            "seen": " ".join(str(data.get("seen") or "").split())[:400],
            "why": " ".join(str(data.get("why") or "").split())[:400],
            "look": [" ".join(str(t).split())[:80] for t in look if str(t).strip()][:MAX_LOOK]}

