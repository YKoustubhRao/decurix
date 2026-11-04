"""
The record ledger: what each record the task enters was meant to hold, what its form held when it was saved, and what
the application showed afterwards - per field, kept by the harness, never by the model.

A record is declared before it is typed (gui record): its key, where it comes from (a file, or the screen), the scope
it belongs in (the entity, book or period the request names) and its fields' intended values. Code checks each value
stands in the source (or in what the task read, or in the request) - a value in none of them is refused with its field
named. Before the form is saved, its fields are matched to the intent by label and compared: a field whose value is not
the intent's blocks the save. After the save, the page is read back and each intended value is marked seen or absent.

Why (2026-10-26, after ~220 runs of two suites): every outcome error that recurred - a form's default saved in place of
the document's value, another record's figure in this one's form, a save reported that was never read back, the wrong
book open - was decided nowhere in code. The ledger makes each a fact a check can read.
"""

import json
import re
import time
from pathlib import Path

from decurix_agent import config, task_context, values
from decurix_agent.computer import identity
from decurix_agent.computer.errors import ComputerError

LEDGER_DIR = config.DECURIX_DIR / "computer" / "evidence"
MAX_FIELDS = 60
MAX_VALUE = 400
# How much of a look's text the read-back searches (fields in view or not, and the page's words).
MAX_PAGE_TEXT = 60000
# A label's words that say nothing about which field it is.
# ("to" is not one: "To Date" is another field than "Date" - _LABEL_QUALIFIERS.)
_LABEL_FILLER = {"of", "the", "a", "an", "in", "rs", "inr", "usd", "per", "dd", "mm", "yyyy", "yy", "and"}
_LABEL_SYNONYMS = {"no": "number", "num": "number", "#": "number", "nbr": "number", "ref": "reference",
                   "amt": "amount", "qty": "quantity", "dt": "date", "a/c": "account", "ac": "account"}
# A word that makes a label another field's ("Due date" is not "Date"): a label with one of these never stands for a
# label without it.
_LABEL_QUALIFIERS = {"due", "from", "to", "start", "end", "mailing", "alias", "other", "secondary", "alternate",
                     "shipping", "billing", "old", "new", "previous", "next", "opening", "closing", "parent"}
_NUMBER_TOKEN = re.compile(r"(?<![\w.])[-+]?\d[\d,]*(?:\.\d+)?(?![\w])")
_DATE_TOKEN = re.compile(r"\b(\d{1,2})[/.\- ]([A-Za-z]{3,9}|\d{1,2})[a-z]*[/.\- ](\d{2,4})\b|\b(\d{4})-(\d{2})-(\d{2})\b")


# ------------------------------------------------------------------ values
#
# What counts as the same value is decided in one place, decurix_agent/values.py (W0.2, W0.3): the screen's checks,
# this ledger and a taught run's checks share it. The names below are kept for the ledger's callers.

_text_key = values.text_key
as_number = values.parse_number


def as_date(value, order=None):
    """The day a value names as YYYY-MM-DD, or None - None also for a digit date whose day and month could be either
    way when no date order is known (values.parse_date). `order` defaults to the source documents' (task_context)."""

    return values.as_iso(value, task_context.source_date_order() if order is None else order)


def compare_shown(shown, wanted, source_order=None, label=""):
    """values.compare of what the application shows (`shown`, read under the application's date order) against what
    the record wants (`wanted`, read under its source's order: `source_order`, else the task's) - two orders, never one
    for both (review round 2, B4). `label`: the form field's label, whose stated unit a bare figure carries
    (values.field_units, review round 3, R5)."""

    return values.compare(shown, wanted, task_context.app_date_order(),
                          source_order or task_context.source_date_order(), values.field_units(label))


def same_value(a, b, order=None):
    """Two values of one field (values.same_value): `a` as the application shows it, `b` as the record's source writes
    it, each under its own date order - or both under `order` when given (two values written the same way)."""

    if order is not None:
        return values.same_value(a, b, order)
    return compare_shown(a, b).same


def stands_in(value, text, value_order=None, text_order=None):
    """Whether a value is written in a text: as a number (any formatting), a date (any format), or its words.
    `value_order` / `text_order`: the date orders the value and the text are written in (default: the source's for the
    value, the application's for the text - a page read back)."""

    if not text:
        return False
    n = as_number(value)
    if n is not None:
        for tok in _NUMBER_TOKEN.findall(text):
            if as_number(tok) == n:
                return True
        if "." not in str(value) and len(str(value).strip()) >= 4:
            # A code of digits (a document number) may stand joined to letters ("INV1188") or a dash.
            return bool(re.search(r"(?<!\d)" + re.escape(str(value).strip()) + r"(?!\d)", text))
        return False
    if values.parse_date(value):
        # The date written exactly as the value writes it is shown (a read-back of what was typed); a date written
        # otherwise is read under each side's order (review round 2, B4).
        literal = " ".join(str(value).split())
        if re.search(r"(?<![\w/.-])" + re.escape(literal) + r"(?![\w/-])", " ".join(text.split())):
            return True
        value_order = value_order or task_context.source_date_order()
        text_order = text_order or task_context.app_date_order()
        return any(values.same_value(value, m.group(0), a_order=value_order, b_order=text_order) is True
                   for m in _DATE_TOKEN.finditer(text))
    key = _text_key(value)
    if len(key) < 2:
        return False
    if re.search(r"\d", key):
        # An identifier ("INV-1234", "B9", "12 MG Road") stands in the text only as a whole token: its letters and
        # digits in order (punctuation and spaces between them aside), with no letter or digit glued on at either end.
        # Recheck (2026-11-02): "INV-1234" was found in "Invoice INV-12345" - another document's number counted as
        # read back.
        return identity.holds_identifier(text, value)
    # Words stand in the text whole and in order (identity.holds). Found in review (2026-11-03, D-13): the squashed
    # letters were looked for anywhere, so "Paid" was read back on a page that says "Unpaid", and "Ravi" in "Ravindra".
    return identity.holds(text, value)


_JOINED_TOKEN = r"[A-Za-z0-9][A-Za-z0-9/\-]*"


def written_whole(value, text):
    """
    None when a value stands in the text on its own; else the larger token the text writes it inside ("ABC-1234" for
    "3306", "INV1188" for "1188"). A code of digits is the document's only as the document writes it: the whole token.
    Found live (2026-10-26): the save was stopped because the form held '1234' where the record said 'ABC-1234', and
    the worker declared the record again with '1234' - accepted, because the digits stood joined in 'ABC-1234' - and
    four vouchers went in under the wrong number. The declaration is where it is decided.
    """

    v = str(value or "").strip()
    if not text or "." in v or not v.isdigit() or len(v) < 4:
        return None
    if re.search(r"(?<![A-Za-z0-9/\-])" + re.escape(v) + r"(?![A-Za-z0-9/\-])", text):
        return None
    # The larger token holding the digits; one with a letter in it (a document number), not a date or a span of years.
    for m in re.finditer(_JOINED_TOKEN, text):
        tok = m.group(0)
        if re.search(r"(?<![0-9])" + re.escape(v) + r"(?![0-9])", tok) and re.search(r"[A-Za-z]", tok):
            return tok
    return None


# ------------------------------------------------------------------ labels

def label_words(label):
    words = []
    for w in re.findall(r"[a-z0-9/#]+", str(label or "").lower()):
        w = _LABEL_SYNONYMS.get(w, w)
        if w not in _LABEL_FILLER:
            words.append(w)
    return words


def label_matches(intent_label, form_label):
    """Whether a form's field label names the intent's field: the same words, or one's words all in the other's, with
    a trailing number (a row's "Amount 2") kept apart from another row's."""

    a, b = label_words(intent_label), label_words(form_label)
    if not a or not b:
        return False
    if a == b:
        return True
    na = [w for w in a if w.isdigit()]
    nb = [w for w in b if w.isdigit()]
    if na != nb:
        return False
    sa, sb = set(a), set(b)
    if not (sa <= sb or sb <= sa):
        return False
    return not ((sa ^ sb) & _LABEL_QUALIFIERS)


def match_fields(intent, form):
    """
    [(intent label, form label, form value)] for each intent field a form field names, each form field used once:
    exact label first, then contained labels. Intent fields no form field names are left out.
    """

    out, used = [], set()
    for exact in (True, False):
        for label in intent:
            if any(label == m[0] for m in out):
                continue
            for i, (flabel, fvalue) in enumerate(form):
                if i in used:
                    continue
                if (label_words(label) == label_words(flabel)) if exact else label_matches(label, flabel):
                    out.append((label, flabel, fvalue))
                    used.add(i)
                    break
    return out


# ------------------------------------------------------------------ the ledger file

def _path(task_id):
    return LEDGER_DIR / f"{task_id}.records.json"


def load(task_id=None):
    """The task's records, in the order declared ([] when none)."""

    task_id = task_id or task_context.current()
    if not task_id:
        return []
    try:
        data = json.loads(_path(task_id).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    return data if isinstance(data, list) else []


def _save(task_id, records):
    LEDGER_DIR.mkdir(parents=True, exist_ok=True)
    # The task's secret values withheld as they are; no shape matching (a record's 16-digit account is no card here).
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(_path(task_id), records, atomic=True,
                           shapes=False, json_kw={"ensure_ascii": False, "indent": 0})


def _key_number_on_form(rec, form):
    """
    Whether the form shows a record key's number - after a letter prefix ("B9", "INV-1234") - in the record's own
    number field. Only for finding which declared record a form is (a form shows the voucher's number, not the
    worker's key); never for whether a value is the document's (stands_in, same_value).

    The number counts only in the field that holds it on the record: the one declared field whose intent is the key
    or its number, found on the form by its label. Any other field showing the same digits ("Qty 9" for record B9) is
    not the record's number. Recheck (2026-11-02): matched anywhere in the form's text, record B9 was bound to a form
    whose quantity was 9. When the record has no such field, or more than one, which field holds the number cannot be
    told, and the number binds nothing.
    """

    m = re.fullmatch(r"\s*[A-Za-z]{1,6}[-/ ]?(\d+)\s*", str(rec.get("key") or ""))
    if not m or not form:
        return False
    n = as_number(m.group(1))
    if n is None:
        return False
    own = [label for label, f in (rec.get("fields") or {}).items()
           if isinstance(f, dict) and f.get("intent") not in (None, "")
           and (same_value(f["intent"], m.group(1), order=_source_order(rec))
                or same_value(f["intent"], rec.get("key"), order=_source_order(rec)))]
    if len(own) != 1:
        return False
    return any(label_matches(own[0], flabel) and as_number(str(fvalue if fvalue is not None else "")) == n
               for flabel, fvalue in form)


def open_record(records, text=None, form=None):
    """
    The record being entered now: among those declared and not yet saved, the latest whose key stands in `text` (the
    form's values, the page) when given; else the latest whose declared fields fit the form (`form`: its (label, value)
    pairs - at least half of the record's fields found on it by label, two at the least); else, with no form given,
    the latest. None when none is open. Found live (2026-10-27): a customer form opened while a bill's record was
    declared was held against the bill's record - its "Name" against the bill's party - and could not be saved.
    """

    unsaved = [rec for rec in records if not rec.get("saved_at")]
    if text:
        for rec in reversed(unsaved):
            if stands_in(rec.get("key", ""), text) or _key_number_on_form(rec, form):
                return rec
    if form is not None:
        pairs = [(str(l), str(v if v is not None else "")) for l, v in form]
        for rec in reversed(unsaved):
            fields = {l: f for l, f in rec.get("fields", {}).items() if f.get("intent")}
            if fields and len(match_fields(fields, pairs)) >= max(2, (len(fields) + 1) // 2):
                return rec
        return None
    return unsaved[-1] if unsaved else None


# ------------------------------------------------------------------ declaring

def is_picture(source):
    """Whether a record's source is a picture (a photographed or scanned document): its text is what OCR reads."""

    from decurix_agent.tools import office
    return Path(str(source)).suffix.lower() in office.IMAGE_TYPES


def _source_text(source):
    """The text of a record's source: a file read by the office tools (a picture: what OCR reads off it), or the
    task's looks at the screen."""

    if source == "screen":
        from decurix_agent.computer import evidence
        looks = evidence.looks()
        return "\n".join(looks[-6:])
    from decurix_agent.tools import office
    try:
        if is_picture(source):
            return office._ocr_file(office._path(source))
        return office.read_document(source)["text"]
    except Exception as exc:  # noqa: BLE001 - what cannot be read is said
        raise ComputerError(f"cannot read the record's source {source}: {exc}")


def chosen_values(task_id=None):
    """
    The entries this task chose from the application's own lists (gui select), and the records its identifiers resolved
    to: values the application holds, so a record may name them even when no document writes them that way. Found live
    (2026-10-26): a customer's ledger, found by its tax number, was named otherwise than the bill wrote it; declared
    with the ledger's name the record was refused, declared with the bill's the save was stopped.
    """

    from decurix_agent.computer import evidence
    out = []
    for event in evidence._task_events(task_id):
        if event.get("outcome") in (None, "done") and event.get("kind") == "select" and event.get("option"):
            out.append(str(event["option"]))
        m = re.search(r'took the entry "([^"]{1,120})"|Selected the entry "([^"]{1,120})"', str(event.get("summary") or ""))
        if m:
            out.append(m.group(1) or m.group(2))
    try:
        from decurix_agent.computer import resolve
        for fact in resolve.facts(task_id):
            if fact.get("status") == "matched" and fact.get("name"):
                out.append(str(fact["name"]))
    except Exception:  # noqa: BLE001
        pass
    return list(dict.fromkeys(" ".join(v.split()) for v in out if v and v.strip()))


def _elsewhere_text():
    """What else a value may legitimately come from: the request, the person's words and what the task read."""

    parts = [task_context.current_request() or ""]
    try:
        from decurix_agent.computer import evidence
        parts.append(evidence.read()[-MAX_PAGE_TEXT:])
        try:
            with open(evidence.NOTES_DIR / f"{task_context.current()}.outputs", encoding="utf-8") as f:
                parts.append(f.read()[-MAX_PAGE_TEXT:])
        except OSError:
            pass
    except Exception:  # noqa: BLE001
        pass
    return "\n".join(parts)


def declare(key, source, fields, scope="", task_id=None):
    """
    Keep a record's intent: {key, source, scope, fields{label: {intent, from}}}. Every value must stand in the source
    ("source"), else in what the task read or the request ("seen"); otherwise the whole declaration is refused with
    the fields named. Returns the record and the lines to show.
    """

    task_id = task_id or task_context.current()
    if not task_id:
        raise ComputerError("no task is being worked on - a record is declared for the task that enters it")
    key = " ".join(str(key or "").split())
    if not key:
        raise ComputerError("a record needs a key (its own number or name): gui record \"1188\" ...")
    if not isinstance(fields, dict) or not fields:
        raise ComputerError("a record needs its fields: a JSON object {\"Label\": \"value\", ...} on stdin (gui record KEY --from FILE --fields -)")
    if len(fields) > MAX_FIELDS:
        raise ComputerError(f"a record has at most {MAX_FIELDS} fields")
    source = str(source or "").strip()
    if not source:
        raise ComputerError("a record needs its source: --from PATH (the document it comes from) or --from screen")
    text = _source_text(source)
    # The order this source writes its digit dates in, from its own dates that cannot be read the other way (a bill
    # writing 28/09/2026 is day first), else the task's: kept on the record, so the save reads its dates by it.
    from decurix_agent import binding
    source_order = binding.date_order([text]) or task_context.source_date_order()
    picture = is_picture(source)
    elsewhere = chosen = None
    kept, unsourced, partial = {}, [], []
    for label, value in fields.items():
        label = " ".join(str(label).split())
        value = " ".join(str(value if value is not None else "").split())[:MAX_VALUE]
        if not label:
            continue
        if not value:
            kept[label] = {"intent": "", "from": "empty"}
            continue
        whole = written_whole(value, text)
        if whole:
            partial.append(f"{label}: '{value}' - the document writes it as '{whole}'")
            continue
        if stands_in(value, text, source_order, source_order):
            kept[label] = {"intent": value, "from": "source"}
            continue
        if chosen is None:
            chosen = chosen_values(task_id)
        if any(compare_shown(c, value, source_order).same is True for c in chosen):
            kept[label] = {"intent": value, "from": "chosen"}  # an entry of the application's own list
            continue
        if elsewhere is None:
            elsewhere = _elsewhere_text()
        if stands_in(value, elsewhere, source_order):
            kept[label] = {"intent": value, "from": "seen"}
        elif picture:
            # OCR of a photograph misses words and figures; what the worker read off the picture is kept as read off a
            # picture (uncertain), for the second read and the reviewer to weigh - never refused outright.
            kept[label] = {"intent": value, "from": "picture"}
        else:
            unsourced.append(f"{label}: {value}")
    if partial:
        raise ComputerError("not declared - a code is the document's only whole, as the document writes it: "
                            + "; ".join(partial) + ". Declare the whole token and type it whole; if the application's "
                            "field cannot hold it, leave the record undeclared and ask the person - never a part of it.")
    if unsourced:
        raise ComputerError("not declared - these values stand neither in the source nor in anything this task read "
                            "or was told: " + "; ".join(unsourced) + ". Declare each value as the DOCUMENT writes it "
                            "(a place as 'Kerala', not the application's option label '32-Kerala'; a party as the "
                            "document names it): the application's own wording of an option or an entry chosen from "
                            "its list is accepted at the save. A value the document does not give at all is not typed.")
    records = load(task_id)
    records = [r for r in records if not (r.get("key") == key and not r.get("saved_at"))]  # declared again: replaced
    rec = {"key": key, "source": source, "scope": " ".join(str(scope or "").split()), "fields": kept,
           "source_order": source_order, "declared_at": time.time(), "saved_at": None, "saved": "", "window": "", "readback": {}, "checks": 0}
    records.append(rec)
    _save(task_id, records)
    lines = [f"Record {key} declared from {source}" + (f" in {rec['scope']}" if rec["scope"] else "")
             + f": {len(kept)} field(s)"]
    seen = [l for l, f in kept.items() if f["from"] in ("seen", "chosen")]
    if seen:
        lines.append("(from the screen, the application's own lists, the request or the person rather than the source: "
                     + ", ".join(seen) + ")")
    unread = [f"{l}={f['intent']}" for l, f in kept.items() if f["from"] == "picture"]
    if unread:
        lines.append("(read off the picture and NOT confirmed by OCR - look at the picture again for each before saving: "
                     + ", ".join(unread) + ")")
    lines.append("Its form is checked against these before it is saved, and the page read back after.")
    return rec, lines


# ------------------------------------------------------------------ before a save

def scope_missing(scope, title, page_text):
    """
    The scope's words the window does not show, or [] when it does: the window shows the scope when a run of two or
    more of its words in a row (one of them a word of letters, four or more) stands in the title or the page - the
    entity's name - or, for a scope of one word, that word. Found live (2026-10-26): a scope written as "<client>
    October 2026 sales bill 1188" was held against a window naming only the client, and the save was stopped for words
    no window would show; a run of the name is what identifies the window, not every word.
    """

    words = [w for w in re.findall(r"[a-z0-9]+", str(scope or "").lower()) if len(w) >= 3 and w not in _LABEL_FILLER]
    shown = " " + re.sub(r"[^a-z0-9]+", " ", f"{title or ''} {page_text or ''}".lower()) + " "
    missing = [w for w in words if f" {w} " not in shown]
    if not missing or len(words) == 1:
        return missing
    for n in range(len(words), 1, -1):
        for i in range(0, len(words) - n + 1):
            run = words[i:i + n]
            if any(w.isalpha() and len(w) >= 4 for w in run) and f" {' '.join(run)} " in shown:
                return []
    return missing


# An identifier: a code of letters and digits (a tax number, an account number), or an email.
_IDENT_RE = re.compile(r"^(?:(?=[A-Za-z0-9]*\d)(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{10,20}|[^\s@]+@[^\s@]+\.[A-Za-z]{2,})$")


def _source_order(rec):
    return rec.get("source_order") or task_context.source_date_order()


def _undecided(flabel, fvalue, want, why):
    """The line a save waits on when a field cannot be compared - and, for a date whose order is not known, the
    question to the person it waits on (task_context.ask_date_order: both readings, an option each)."""

    if str(why).startswith("date_order:"):
        side, value = ("app", fvalue) if why.endswith(":a") else ("source", want)
        question = task_context.ask_date_order(task_context.current(), side, value)
        if question:
            return (f"{flabel} holds {fvalue!r} and the record says {want!r}, and which day {value!r} is cannot be told "
                    "from here - this needs the person. The agent puts the two readings to them itself, as a question "
                    "of its own, when you next ask them anything (HELP: say the save waits on this date) or stop; "
                    "do not retype the field (a date written the same way is no answer).")
        return (f"{flabel} holds {fvalue!r} and the record says {want!r}: the dates cannot be compared under the date "
                "order known for this task - this needs the person.")
    if why == "unit":
        return (f"{flabel} holds {fvalue!r} and the record says {want!r}: a unit is written on one side only and the "
                "field's label does not state it, so they cannot be compared. When the field's label (or the text "
                "beside it) carries the unit, declare the record's figure without the unit (gui record declare); "
                "otherwise ask the person which unit the field takes.")
    return (f"{flabel} holds {fvalue!r} and the record says {want!r}, and they cannot be compared ({why}: a number "
            "against text, or separators that read two ways) - type the value as the document writes it, or ask the "
            "person.")


def presave(rec, form_fields):
    """
    The intent against a form about to be saved: (blocking, advisory) lines. A matched field whose value is not the
    intent's blocks; an intent field the form does not name, or a filled form field the intent does not cover, is
    advisory. Also returns how many intent fields the form matched (the diff is "clean" when nothing blocks).
    """

    fields = {l: f for l, f in rec.get("fields", {}).items() if f.get("intent")}
    matched = match_fields(fields, [(str(l), str(v if v is not None else "")) for l, v in form_fields])
    blocking, advisory = [], []
    chosen = None
    for label, flabel, fvalue in matched:
        want = fields[label]["intent"]
        if not str(fvalue).strip():
            if _IDENT_RE.match(str(want).strip()):
                # A party saved without the identifier its document gives is a record nothing can resolve later -
                # found live (2026-10-27): a customer record made with name and group only, its identifier left out, and the
                # resolution guard had nothing to see.
                blocking.append(f"{flabel} is empty where the record says {want!r} - a record is saved with its identifier")
            else:
                advisory.append(f"{flabel} is empty where the record says {want}")
        elif (decided := compare_shown(fvalue, want, _source_order(rec), flabel)).same is not True:
            if chosen is None:
                chosen = chosen_values()
            # The rescue is tried for an undecided pair too ("1188" against "INV-1188" is undecided, not "differs"),
            # before the save waits on a question (review round 3, nit 4).
            if any(same_value(fvalue, c, order=task_context.app_date_order() or "") for c in chosen):
                # The application's own entry for what the document names (a party found by its identifier under
                # another spelling): the application holds it so, and that is right - and it is what the page will
                # show after the save, so the read-back looks for it (records.readback).
                advisory.append(f"{flabel} holds the application's own entry {fvalue!r} for the document's {want!r}")
                fields[label]["entered"] = str(fvalue)
                continue
            if decided.same is None:
                blocking.append(_undecided(flabel, fvalue, want, decided.why))
            else:
                blocking.append(f"{flabel} holds {fvalue!r} where the record says {want!r}")
    named = {m[0] for m in matched}
    missing = [l for l in fields if l not in named]
    if missing:
        advisory.append("not found on this form by label: " + ", ".join(f"{l}={fields[l]['intent']}" for l in missing))
    used = {m[1] for m in matched}
    extra = [f"{l}={v}" for l, v in form_fields if str(v if v is not None else "").strip() and l not in used
             and str(l) not in used]
    if extra:
        advisory.append("on the form but not in the record: " + ", ".join(str(x)[:80] for x in extra[:12]))
    return blocking, advisory, len(matched)


def note_presave(rec, blocking, task_id=None):
    """Keep the last pre-save diff on the record (what blocked, how many checks)."""

    task_id = task_id or task_context.current()
    records = load(task_id)
    for r in records:
        if r.get("key") == rec.get("key") and r.get("declared_at") == rec.get("declared_at"):
            r["checks"] = int(r.get("checks") or 0) + 1
            r["last_blocked"] = blocking[:12]
            for label, f in rec.get("fields", {}).items():
                if f.get("entered") and label in r.get("fields", {}):
                    r["fields"][label]["entered"] = f["entered"]
    _save(task_id, records)


# ------------------------------------------------------------------ after a save

def readback(rec, title, page_text, change, task_id=None):
    """Mark the record saved and each intended value seen or absent on the page the save left. Returns the lines."""

    task_id = task_id or task_context.current()
    records = load(task_id)
    seen, absent = [], []
    for label, f in rec.get("fields", {}).items():
        if not f.get("intent"):
            continue
        # (The application's own entry for the document's value, when the form held one, is what the page shows.)
        shown = (stands_in(f["intent"], page_text, _source_order(rec))
                 or (f.get("entered") and stands_in(f["entered"], page_text, task_context.app_date_order())))
        (seen if shown else absent).append(label)
    key_shown = stands_in(rec.get("key", ""), page_text)
    for r in records:
        if r.get("key") == rec.get("key") and r.get("declared_at") == rec.get("declared_at"):
            r["saved_at"] = time.time()
            r["saved"] = " ".join(str(change or "").split())[:300]
            r["window"] = str(title or "")[:120]
            r["readback"] = {"seen": seen, "absent": absent, "key_shown": key_shown}
    _save(task_id, records)
    total = len(seen) + len(absent)
    line = f"Record {rec.get('key')}: saved; the page now shows {len(seen)} of {total} declared value(s)"
    if absent:
        line += " - NOT shown: " + ", ".join(f"{l}={rec['fields'][l]['intent']}" for l in absent[:10])
        line += ". Open the saved record and check those before going on; a value the application changed is not what the document says."
    elif total:
        line += " - all of them."
    return [line]


# ------------------------------------------------------------------ listing

def status_of(rec):
    if not rec.get("saved_at"):
        return "declared, not saved" + (" (last save blocked: " + "; ".join(rec["last_blocked"]) + ")" if rec.get("last_blocked") else "")
    rb = rec.get("readback") or {}
    if rb.get("absent"):
        return "saved; not read back: " + ", ".join(rb["absent"])
    return "saved and read back" if rb.get("seen") else "saved"


def listing(task_id=None):
    """The ledger as lines: one per record with its status, then its fields."""

    records = load(task_id)
    if not records:
        return ["No record declared in this task (gui record KEY --from FILE --fields -)."]
    out = [f"{len(records)} record(s) declared in this task:"]
    for rec in records:
        out.append(f"- {rec['key']} (from {rec.get('source')}" + (f"; scope {rec['scope']}" if rec.get("scope") else "")
                   + f"): {status_of(rec)}")
        rb = rec.get("readback") or {}
        for label, f in rec.get("fields", {}).items():
            if not f.get("intent"):
                continue
            mark = "seen" if label in rb.get("seen", []) else ("ABSENT" if label in rb.get("absent", []) else "-")
            out.append(f"    {label}: {f['intent']} [{f.get('from')}; read back: {mark}]")
    return out


def all_read_back(task_id=None):
    """Whether the task declared at least one record and every one was saved and read back with each of its declared
    values shown (the ledger's "matched"): the independent check a run's trust may rest on (experience.INDEPENDENT)."""

    try:
        found = load(task_id)
    except Exception:  # noqa: BLE001 - a ledger that cannot be read proves nothing
        return False
    if not found:
        return False
    for rec in found:
        rb = rec.get("readback") or {}
        wanted = [l for l, f in (rec.get("fields") or {}).items() if isinstance(f, dict) and f.get("intent")]
        if not rec.get("saved_at") or rb.get("absent") or not wanted or set(wanted) - set(rb.get("seen") or []):
            return False
    return True


def facts(task_id=None):
    """What a check can hold against the task: records declared and never saved, and values not read back."""

    out = []
    for rec in load(task_id):
        if not rec.get("saved_at"):
            out.append(f"record {rec['key']} was declared but never saved")
        elif (rec.get("readback") or {}).get("absent"):
            out.append(f"record {rec['key']} was saved but the page did not show: "
                       + ", ".join(f"{l}={rec['fields'][l]['intent']}" for l in rec["readback"]["absent"]))
    return out


def declared_sources(task_id=None):
    """The sources (files) records were declared from, each once."""

    return list(dict.fromkeys(r.get("source") for r in load(task_id) if r.get("source") and r["source"] != "screen"))


def parse_fields(text):
    """The fields given on stdin: a JSON object, or lines of "Label: value"."""

    text = (text or "").strip()
    if not text:
        return {}
    if text[0] in "{[":
        try:
            data = json.loads(text)
        except ValueError as exc:
            raise ComputerError(f"the fields are not valid JSON: {exc}")
        if not isinstance(data, dict):
            raise ComputerError("the fields must be a JSON object of label: value")
        return {str(k): ("" if v is None else str(v)) for k, v in data.items()}
    out = {}
    for line in text.splitlines():
        if ":" in line:
            k, v = line.split(":", 1)
            out[k.strip()] = v.strip()
    return out


def refusal_marker(task_id, what):
    """A file that says the task was stopped over `what` once (so it is never stopped twice over it)."""

    safe = re.sub(r"[^A-Za-z0-9]+", "_", str(what))[:80]
    return Path(LEDGER_DIR) / "stops" / f"{task_id}-{safe}"


def stop_once(task_id, what):
    """True the first time the task is stopped over `what`; False after."""

    mark = refusal_marker(task_id, what)
    if mark.exists():
        return False
    try:
        mark.parent.mkdir(parents=True, exist_ok=True)
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(mark, "1")
    except OSError:
        return False
    return True
