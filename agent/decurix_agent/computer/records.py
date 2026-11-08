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


def compare_shown(shown, wanted, source_order=None, label="", units=()):
    """values.compare of what the application shows (`shown`, read under the application's date order) against what
    the record wants (`wanted`, read under its source's order: `source_order`, else the task's) - two orders, never one
    for both (review round 2, B4). `label`: the form field's label, whose stated unit a bare figure carries
    (values.field_units, review round 3, R5). `units`: more units the field takes, as the person said
    (unit_answers.answered)."""

    return values.compare(shown, wanted, task_context.app_date_order(),
                          source_order or task_context.source_date_order(),
                          values.field_units(label) | frozenset(units or ()))


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


def saved_record(records, key=None, text=None):
    """The record of this task already saved under `key` (or whose key stands in `text`, a form about to be saved),
    or None - a second save of it makes a second record (review round 9, B)."""

    for rec in reversed([r for r in records if r.get("saved_at")]):
        if (key is not None and rec.get("key") == key) or (text and stands_in(rec.get("key", ""), text)):
            return rec
    return None


def saved_on_form(records, form_fields):
    """The record of this task already saved whose own key field - the declared field whose intent is its key - holds
    its key on this form (`form_fields`: (label, value) pairs), or None. Only that field: a payment's "Against bill"
    naming a bill saved before is another record (review round 9: the key's text anywhere in the form blocked it)."""

    for rec in reversed([r for r in records if r.get("saved_at")]):
        key = rec.get("key") or ""
        own = [label for label, f in (rec.get("fields") or {}).items()
               if isinstance(f, dict) and f.get("intent") and same_value(f["intent"], key, order=_source_order(rec))]
        if any(label_matches(label, flabel) and same_value(str(fvalue if fvalue is not None else ""), key, order="")
               for label in own for flabel, fvalue in form_fields):
            return rec
    return None


def _shown_now(task_id=None):
    """The page the task looked at last, as its look was noted ("" when none)."""

    try:
        from decurix_agent.computer import evidence
        looks = evidence.looks(task_id)
    except Exception:  # noqa: BLE001
        return ""
    return looks[-1] if looks else ""


def _add_to_saved(task_id, records, done, kept):
    """
    A declaration of a record this task saved already. The fields it adds (or gives again where the page did not show
    them) are taken onto the saved record - checked against the source by declare, read back off the page the task
    looks at now - and the undeclared fields they cover are let go: a field set on the form with no declaration can be
    declared after the save, instead of the worker being left only to ask (review round 9). No new field, or a field
    given otherwise than the saved declaration, is refused: a second save of the record makes a second record.
    """

    fields = done.get("fields") or {}
    absent = set((done.get("readback") or {}).get("absent") or [])
    order = _source_order(done)
    changed = [f"{l}: {f['intent']} (declared {fields[l].get('intent')})" for l, f in kept.items()
               if l in fields and f.get("intent") and fields[l].get("intent")
               and not same_value(fields[l]["intent"], f["intent"], order=order)]
    new = {l: f for l, f in kept.items() if f.get("intent") and (l not in fields or l in absent)}
    if changed or not new:
        raise ComputerError("not declared - " + saved_again_text(done)
                            + (" It gives otherwise than declared: " + "; ".join(changed) + "." if changed else ""))
    page = _shown_now(task_id)
    seen = [l for l, f in new.items() if stands_in(f["intent"], page, order)]
    rec = done
    for r in records:
        if r.get("key") == done.get("key") and r.get("declared_at") == done.get("declared_at"):
            r.setdefault("fields", {}).update(new)
            rb = r.setdefault("readback", {})
            rb["seen"] = list(dict.fromkeys(list(rb.get("seen") or []) + seen))
            rb["absent"] = [l for l in list(rb.get("absent") or []) + list(new) if l not in rb["seen"]]
            rb["absent"] = list(dict.fromkeys(rb["absent"]))
            r["undeclared"] = [u for u in r.get("undeclared") or []
                               if not any(label_matches(l, u.split("=", 1)[0]) for l in new)]
            rec = r
    _save(task_id, records)
    lines = [f"Record {done.get('key')} is saved already; its declaration now also covers: "
             + ", ".join(f"{l}={f['intent']}" for l, f in new.items())]
    missing = [l for l in new if l not in seen]
    if missing:
        lines.append("NOT shown on the page the task looked at last: " + ", ".join(missing) + ". Open the saved record, "
                     "look at it, and declare those again to read them back; correct the saved record if they are "
                     "wrong - never enter it again.")
    return rec, lines


def saved_again_text(rec):
    """What a second save or declaration of a saved record is told."""

    where = rec.get("window") or rec.get("saved") or "the application"
    return (f"record {rec.get('key')} was saved in this task already (at {el_quote(where)}) - a second save makes a "
            "second record of the same one. Open the saved record to correct it, and save that; never enter it again.")


def el_quote(text):
    return '"' + " ".join(str(text or "").split())[:120] + '"'


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


_FIELDS_PART = "What every field of the page held (in view or not):\n"
_WORDS_PART = "The words the page showed outside its fields, in its order (in view or not):\n"
_IN_VIEW_PART = "\nThe part in view, as the tool listed it:"


def _key_on_an_app_page(key, source, task_id=None):
    """Where `key` stood on a page of the application this task read (as a page's words, not a form's field - a list
    or a record): the look's lead, or "". A look that names the record's own source (the document open on the
    screen) is the document, not the application."""

    try:
        from decurix_agent.computer import evidence
        looks = evidence.looks(task_id)
    except Exception:  # noqa: BLE001 - nothing read, nothing shown
        return ""
    name = Path(str(source)).name if source and source != "screen" else ""
    # (--from screen: the looks the values were read from - _source_text's - are the source, not the application.)
    looks = looks[:-6] if source == "screen" else looks
    for look in looks:
        if name and name.lower() in look.lower():
            continue
        start = look.find(_WORDS_PART)
        if start == -1:
            continue
        fields = look.find(_FIELDS_PART)
        if fields != -1 and stands_in(key, look[fields + len(_FIELDS_PART):start]):
            # A field of the page holds the key: a search for it, whose page then echoes the query ("No bills match
            # 'X'") - the diligent look before entering, not the application holding it (review round 9, C).
            continue
        words = look[start + len(_WORDS_PART):]
        end = words.find(_IN_VIEW_PART)
        words = words[:end] if end != -1 else words
        if words.strip() and words.strip() != "(none)" and stands_in(key, words):
            return "a list or a record it showed"
    return ""


class Finding(str):
    """A finding's line with its kind as structure (`kind`): what a caller sorts findings by, never their words."""

    kind = ""


def finding(kind, line):
    out = Finding(line)
    out.kind = kind
    return out


# The kinds of what the final check holds against the task's saved records (ledger_checks.findings).
UNDECLARED = "undeclared_field"
UNCOVERED = "record_not_declared"
SAVED_TWICE = "saved_twice"
UNBACKED_PICK = "unbacked_pick"
NOT_BACKED = "report_not_backed"
WORDY_KEY = "identifier_as_words"
SENT_AROUND = "sent_around_ledger"
NOT_MADE = "not_made"


def _page_of(url):
    """(host, path) of an address, the query and a trailing slash aside; ("", "") for none."""

    import urllib.parse
    try:
        parts = urllib.parse.urlsplit(str(url or "").rstrip(";,"))
    except ValueError:
        return "", ""
    return parts.netloc.lower(), parts.path.rstrip("/")


def _pick_of(event, host=""):
    element = event.get("element") or {}
    offered = event.get("offered")
    return {"label": " ".join(str(element.get("name") or "").split()),
            "option": " ".join(str(event.get("option") or "").split()),
            "taken": " ".join(str(event.get("taken") or "").split()),
            "offered": list(offered) if isinstance(offered, list) and offered else None,
            "by_id": event.get("resolved_by") == "identifier",
            "host": _page_of(event.get("url"))[0] or host}


def _selects(task_id=None, events=None):
    """Every select this task made that was done, as picks (_pick_of), oldest first."""

    if events is None:
        from decurix_agent.computer import evidence
        events = evidence._task_events(task_id)
    return [_pick_of(e) for e in events if e.get("outcome") in (None, "done") and e.get("kind") == "select"
            and (e.get("option") or e.get("taken"))]


def picks(task_id=None, events=None, open_form=False):
    """
    The selects this task made into a form it then saved: [{label, option, taken, offered, by_id, host}], oldest
    first. A select counts from the page its form is on until that form's save is answered as saved; one made on a
    page the task then left without saving (a list's filter, its pager, a sort order), or that itself moved the page
    (a pager, a view), was no record's value. `open_form`: also those into the form being filled now, not yet saved.
    Review round 9 (R9-B2): every select of the task counted, list filters and pagers included, and correct work was
    refused done.
    """

    from decurix_agent.computer import core
    if events is None:
        from decurix_agent.computer import evidence
        events = evidence._task_events(task_id)
    out, pending, page = [], [], None
    for event in events:
        if event.get("outcome") not in (None, "done"):
            continue
        kind, change = event.get("kind"), str(event.get("change") or "")
        moved = re.search(r"page is now (\S+)", change)
        to = moved.group(1) if moved else (str(event.get("target") or "") if kind == "open" else "")
        left = False
        if to:
            here = _page_of(to)
            left = page is not None and here != page
            page = here
        if kind in ("click", "key") and core._saved(change):
            out += pending
            pending = []
            continue
        if left or core._FORM_START.search(change) or (kind == "open" and core._FORM_START.search("page is now " + to)):
            pending = []
        if kind == "select" and not left and (event.get("option") or event.get("taken")):
            pending.append(_pick_of(event, host=(page or ("", ""))[0]))
    return out + pending if open_form else out


def picked_options(task_id=None):
    """The options this task took from the application's own lists with gui select (what was asked and the entry
    taken) - the worker's own choice, which the list's offering it never backs (review round 9, A)."""

    out = []
    for p in _selects(task_id):
        out += [p["option"], p["taken"]]
    return list(dict.fromkeys(v for v in out if v))


def _taught_text():
    """The person's own words on this desktop's tasks that taught or ran the skill being run, as the control plane
    keeps them (person_record.taught) - never the skill file, which the worker's user can write and whose steps are
    partly model-written (review round 10, R10-B1)."""

    try:
        from decurix_agent import guidance, person_record
        return "\n".join(person_record.taught(guidance.current_skill()))
    except Exception:  # noqa: BLE001 - what cannot be confirmed backs nothing
        return ""


# The organisation's knowledge as last read, kept for org_knowledge.CACHE_SECONDS whether or not it could be read: a
# control plane that does not answer costs one wait, not one per pick. Kept for one task at a time: the task's own
# items (scope task) are read with the org's, the worker's roles' and its own.
_org_read = {"at": None, "task": None, "text": ""}


def _org_text(task_id=None):
    try:
        from decurix_agent import org_knowledge
        task = task_id or task_context.current() or ""
        now = time.monotonic()
        if (_org_read["at"] is None or _org_read["task"] != task
                or now - _org_read["at"] > org_knowledge.CACHE_SECONDS):
            _org_read.update(at=now, task=task, text="\n".join(org_knowledge.cached(task_id=task or None)))
        return _org_read["text"]
    except Exception:  # noqa: BLE001 - knowledge that cannot be read backs nothing
        return ""


def _answers_text(task_id=None):
    """This task's replies the control plane confirms as the person's (person_record.answers), in its own text."""

    try:
        from decurix_agent import person_record
        return "\n".join(a["text"] for a in person_record.answers(task_id))
    except Exception:  # noqa: BLE001 - what cannot be confirmed backs nothing
        return ""


def _free_text(task_id=None):
    """What people wrote for this task, as one text: the request, the person's replies the control plane confirms, the
    person's words on the skill being run and the organisation's knowledge (given_sources' "free" part)."""

    parts = [task_context.current_request() or "", _answers_text(task_id), _taught_text(), _org_text(task_id)]
    return "\n".join(p for p in parts if p)


def given_sources(task_id=None):
    """
    What a picked option may be backed by, as {"free": text, "files": [text]}. Free: the request, the person's replies
    in this task that the control plane confirms, the person's words on the skill being run (from the control plane),
    and the organisation's knowledge (people write it; a desktop may only read it). Files: the request's own files,
    which back a pick only where they state that field (option_beside_label). Never the application's pages, whose
    option list is the menu; never the worker's own memory, notes or files.
    """

    free = [_free_text(task_id)]
    files = []
    try:
        from decurix_agent.tools import office
        for path in (task_context.current_files() or [])[:12]:
            try:
                files.append(str(office.read_document(str(path)).get("text") or "")[:MAX_PAGE_TEXT])
            except Exception:  # noqa: BLE001 - a file not read backs nothing
                continue
    except Exception:  # noqa: BLE001
        pass
    return {"free": "\n".join(p for p in free if p), "files": [f for f in files if f]}


def given_text(task_id=None):
    """given_sources as one text (the free sources and the files): for a reader that needs one; a pick's backing is
    pick_backed's, which holds a file to the field's own label."""

    sources = given_sources(task_id)
    return "\n".join([sources["free"]] + sources["files"])


# ------------------------------------------------------------------ a record's number or reference
#
# Found live (2026-11-08): a key field typed with a phrase no source held as a number - a supplier's copy printed a
# phrase pointing to another paper where its invoice number belongs, and the worker declared that phrase as the bill's
# number. It was accepted: the phrase DOES stand in the document, beside the field's own label, so the ledger called
# the value backed. Standing in a source is not enough
# for the field that identifies a record: the document there says where the number is, not what it is. Decided by
# structure, not a word list: a field whose label names a number, a reference or an id (label_words: "No.", "#", "Ref"
# are these) holds a code, and a value of two or more words with no digit in it is words, not a code. Only the people's
# own words (the request, the person's answers, what they taught, the organisation's knowledge) can give words there.

_IDENTIFIER_LABEL = frozenset({"number", "reference", "id"})


def identifier_field(label):
    """Whether a field's label says it holds a code the record is found by: its number, reference or id."""

    return bool(set(label_words(label)) & _IDENTIFIER_LABEL)


def words_not_code(value):
    """Whether a value is words and no code: two or more words, and not one digit among them."""

    text = " ".join(str(value or "").split())
    return len(text.split()) >= 2 and not re.search(r"\d", text)


def wordy_identifiers(fields, task_id=None, free=None):
    """
    [(label, value)] of `fields` ((label, value) pairs) whose label names the record's number, reference or id
    (identifier_field) and whose value is words, not a code (words_not_code) - unless what people wrote for this task
    (`free`, default _free_text: the request, the person's answers, their teaching, the organisation's knowledge) gives
    those words whole. A document writing them there does not give the number: it points elsewhere.
    """

    found = [(" ".join(str(label).split()), " ".join(str(value or "").split())) for label, value in fields
             if identifier_field(label) and words_not_code(value)]
    if not found:
        return []
    free = _free_text(task_id) if free is None else free
    return [(label, value) for label, value in found if not identity.holds(free, value)]


def wordy_text(found):
    """What a worker is told for wordy_identifiers' fields: ask the person for each, by the field's name."""

    named = "; ".join(f"{label}: {value!r}" for label, value in found[:6])
    asks = ", ".join(dict.fromkeys(label for label, _ in found[:6]))
    return (f"a record's number, reference or id is a code, and this is words: {named}. Where a document writes words "
            "in that place, it does not give the code - it says where the code is. Ask the person (HELP) for the "
            f"{asks}, naming the field; never type the words, a guess or a made-up code in its place.")


def typed_into_saved(events):
    """[(label, value)] typed or chosen into a form that a save then answered as saved (core._current_form's reading
    of the form, at each save), oldest first."""

    from decurix_agent.computer import core
    out, seen = [], []
    for event in events or []:
        if event.get("outcome") not in (None, "done"):
            continue
        seen.append(event)
        if event.get("kind") in ("click", "key") and core._saved(str(event.get("change") or "")):
            out += core._current_form(seen[:-1])
            seen = []
    return out


def wordy_saved(task_id=None, events=None):
    """wordy_identifiers of the fields this task put into forms it saved: the final check's reading (ledger_checks),
    for a record saved with or without a declaration."""

    if events is None:
        from decurix_agent.computer import evidence
        events = evidence._task_events(task_id)
    return wordy_identifiers(typed_into_saved(events), task_id)


# ------------------------------------------------------------------ a record sent around the screen
#
# Found live (2026-11-08): a worker tried to curl a bill straight into the application's form endpoint. The approval
# gate held that one (the application's host was not this machine), but a send to a server on this machine runs without
# a person (TheBot's safety reads it as development), and a yes to the gate is a yes to the send, never a check of what
# was saved. A record sent so was never declared, typed on the screen or read back: nothing shows what the application
# holds. So a send the task made to an application it entered records in is held against the done.

def _hosts_worked(events):
    """The hosts (lower case, no port) of the pages where this task opened a new record's form or had a save answered."""

    from decurix_agent.computer import core
    import urllib.parse
    hosts = set()
    for event in events or []:
        if event.get("outcome") not in (None, "done"):
            continue
        change = str(event.get("change") or "")
        pages = re.findall(r"page is now (\S+)", change)
        if event.get("kind") == "open" and event.get("target"):
            pages.append(str(event["target"]))
        record_page = core._saved(change) and event.get("kind") in ("click", "key")
        for page in pages + ([str(event.get("url"))] if event.get("url") else []):
            if record_page or core._NEW_RECORD_PAGE.search("page is now " + page.rstrip(";,")):
                try:
                    host = urllib.parse.urlsplit(page.rstrip(";,")).hostname or ""
                except ValueError:
                    host = ""
                if host:
                    hosts.add(host.lower())
    return hosts


def sent_around(commands, events=None, task_id=None):
    """
    The commands among `commands` (shell commands this task ran) that sent data to an application around the screen
    (playbooks.sends_data): every such send when the task declared a record (the ledger is how its records go in), and
    any send naming the host of a page where the task opened a new record's form or saved one. [] when none.
    """

    from decurix_agent import playbooks
    sending = [c for c in commands or () if playbooks.sends_data(c)]
    if not sending:
        return []
    if load(task_id):
        return sending
    hosts = _hosts_worked(events)
    return [c for c in sending
            if any(re.search(r"(?<![\w.-])" + re.escape(h) + r"(?![\w-])", c, re.I) for h in hosts)]


def _option_words(text):
    return values.text_key(text).split()


def _digits_backed(digits, option, given):
    """A figure backs an option only as the option writes it with its sign ("18%"), written so in what was given: a
    bare figure of a document (a quantity of 18) names no rate."""

    for d in digits:
        m = re.search(r"(?<![\w.])" + re.escape(d) + r"\s?([^\w\s])", option)
        if not m or not re.search(r"(?<![\w.])" + re.escape(d) + r"\s?" + re.escape(m.group(1)), given):
            return False
    return True


def _joined_figure(digits, option):
    """Whether a figure is joined to a name in the option ("29-Karnataka", "Karnataka/29"): a code's part, not a word
    that tells this entry from the others."""

    d = re.escape(digits)
    return bool(re.search(r"[A-Za-z][-/]" + d + r"(?!\d)|(?<!\d)" + d + r"[-/][A-Za-z]", option))


def option_backed(option, given, offered=None):
    """
    Whether `given` (a text) picks out `option` among the entries a list offered (`offered`; None when not read).
    Every word that tells the option from the other entries - each of its words no other entry has - must stand in
    the given text as a whole word: a figure only with its sign ("18%"), and a single letter only beside another of
    the option's words ("grade A", never "item a"). "Cedar" picks "Project Cedar" out of Project Maple and Project
    Cedar; "Karnataka" picks "29-Karnataka" (the joined figure is the code's); "18%" picks "GST 18%" among 5%, 12%
    and 18%. "to" never backs "Transfer to Reserve": "transfer" and "reserve" tell it apart. An option whose every word
    another entry has too is never picked out by words. With the list not read, every word of the option must be
    given. Structure only, never a list of words (review round 10, R10-B2).
    """

    option = " ".join(str(option or "").split())
    if not option or not given:
        return False
    words = _option_words(option)
    mine = set(words)
    if not mine:
        return False
    others = [set(_option_words(o)) for o in offered or ()
              if " ".join(str(o).split()).casefold() != option.casefold()]
    others = [o for o in others if o]
    if not others and stands_in(option, given):
        return True
    need = mine - set().union(*others) if others else set(mine)
    need = {w for w in need if not (w.isdigit() and _joined_figure(w, option))}
    if not need:
        return False
    text = _option_words(given)
    have = set(text)
    for w in need:
        if w not in have:
            return False
        if w.isdigit() and not _digits_backed([w], option, given):
            return False
        if len(w) == 1 and not any(text[i] == w and ((i > 0 and text[i - 1] in mine - {w})
                                                     or (i + 1 < len(text) and text[i + 1] in mine - {w}))
                                   for i in range(len(text))):
            return False
    return True


def _cells(line):
    """A line's cells, when it is a table's row (cells split by | or a tab); else None."""

    for mark in ("|", "\t"):
        if mark in line:
            return [c.strip() for c in line.split(mark)]
    return None


def option_beside_label(option, label, text, offered=None):
    """
    Whether a document states `option` for the field `label`: on the line that names the field (after its label), or
    on the next line when that one holds the label alone, or in a table in the cell beside the label's or under its
    column. A document's other words - a footer's "General terms and conditions" - back nothing (review round 10,
    R10-B2: a bill's footer backed a guessed "General" cost centre).
    """

    want = set(label_words(label))
    if not want or not text:
        return False
    lines = str(text).splitlines()
    for i, line in enumerate(lines):
        cells = _cells(line)
        if cells is not None:
            for j, cell in enumerate(cells):
                if not cell or not want <= set(label_words(cell)):
                    continue
                if j + 1 < len(cells) and option_backed(option, cells[j + 1], offered):
                    return True
                for below in lines[i + 1:]:
                    row = _cells(below)
                    if row is None or len(row) != len(cells):
                        break
                    if j < len(row) and option_backed(option, row[j], offered):
                        return True
            continue
        if not want <= set(label_words(line)):
            continue
        end = _label_end(line, want)
        if end is None:
            continue
        value = _value_cell(line[end:])
        if value and option_backed(option, value, offered):
            return True
        if not value and not line[end:].strip(" \t:-=|"):
            # (The label alone on its line: its value is the next line's first cell.)
            after = next((l for l in lines[i + 1:] if l.strip()), "")
            if after and option_backed(option, _value_cell(after), offered):
                return True
    return False


# Where a value cell ends: a sentence's end (a full stop, ! or ? before a space or the end), a ; or a column break (a
# tab, a |, or two spaces or more).
_CELL_END = re.compile(r"(?<!\d)[.!?](?=\s|$)|[;|\t]| {2,}")


def _label_end(line, want):
    """Where, in `line`, the label's words (label_words) end: the end of the shortest run of the line's tokens that
    holds them all, or None."""

    tokens = [(m.end(), label_words(m.group(0))) for m in re.finditer(r"[A-Za-z0-9/#]+", line)]
    for i in range(len(tokens)):
        got = set()
        for j in range(i, len(tokens)):
            got |= set(tokens[j][1])
            if want <= got:
                return tokens[j][0]
    return None


def _value_cell(text):
    """The value cell that follows a label: its text up to the next sentence or column break, a leading ":", "-" or
    "=" aside. A whole merged line is not one value: "Cost centre: to be decided. General terms apply." gives "to be
    decided" (review round 11)."""

    text = re.sub(r"^[\s:=\-]+", "", str(text or ""))
    m = _CELL_END.search(text)
    return (text[:m.start()] if m else text).strip()


def resolved_names(task_id=None, events=None):
    """The entries this task reached from a source value by its identifier: a select whose record says an identifier
    the documents give chose it (core.act's `resolved_by`), and the records resolve.facts matched. Never an action's
    summary words: a plain pick's summary names the entry it took too (review round 9, R9-B1)."""

    out = [p["taken"] or p["option"] for p in _selects(task_id, events) if p["by_id"]]
    try:
        from decurix_agent.computer import resolve
        out += [str(f["name"]) for f in resolve.facts(task_id) if f.get("status") == "matched" and f.get("name")]
    except Exception:  # noqa: BLE001
        pass
    return list(dict.fromkeys(" ".join(v.split()) for v in out if v and v.strip()))


# ------------------------------------------------------------------ a person's answer about a field, kept

def _kept_answer(label, option, host, offered=None):
    """Whether a person's answer about this field (its label, in this application), kept on the control plane as a
    learned row derived from their judgement, names `option` - checked against that judgement's own text."""

    try:
        from decurix_agent import person_record
        rows = person_record.field_answers()
    except Exception:  # noqa: BLE001 - what cannot be read backs nothing
        return False
    for a in rows:
        if a.get("host") and host and a["host"] != host:
            continue
        if not (label_matches(a.get("label", ""), label) and same_value(a.get("option", ""), option, order="")):
            continue
        # The question the person answered (the control plane's, never the row's) names this field: a row cannot move
        # an answer about one field onto another (review round 11). None given: nothing backed.
        words = set(label_words(label))
        if not words or not words <= set(label_words(a.get("source_asked", ""))):
            continue
        if option_backed(option, a.get("source_text", ""), offered):
            return True
    return False


_kept_now = set()


def keep_field_answers(task_id=None, events=None):
    """
    Keep each of this task's answers that settled a field's pick, on the control plane: a pick into a saved form that
    only the person's reply backs - a reply the control plane confirms as their judgement on this task, to a question
    that named that field. The next task's pick of the same entry in that field of that application is then backed by
    it (_kept_answer), and the person is not asked again. Only at a final report, never inside a check. Returns how
    many were posted. Never raises.
    """

    try:
        from decurix_agent import person_record, unit_answers
        task_id = task_id or task_context.current()
        # The person's answers about a field's unit too (unit_answers.keep): kept the same way, at the same moment.
        posted = unit_answers.keep(task_id)
        said = person_record.answers(task_id)
        if not said:
            return posted
        sources = None
        for p in picks(task_id, events=events):
            value = p["taken"] or p["option"]
            if not p["label"] or p["by_id"]:
                continue
            if sources is None:
                sources = given_sources(task_id)
                without = "\n".join([task_context.current_request() or "", _taught_text(), _org_text(task_id)])
            if option_backed(value, without, p["offered"]) or any(
                    option_beside_label(value, p["label"], f, p["offered"]) for f in sources["files"]):
                continue
            words = set(label_words(p["label"]))
            for a in said:
                if not (words and words <= set(label_words(a["asked"])) and option_backed(value, a["text"], p["offered"])):
                    continue
                mark = (a["id"], p["host"], " ".join(label_words(p["label"])))
                if mark not in _kept_now:
                    _kept_now.add(mark)
                    person_record.keep_field_answer(task_id, a["id"], p["label"], p["host"], value, asked=a["asked"])
                    posted += 1
                break
        return posted
    except Exception:  # noqa: BLE001 - what is kept for later never costs the check
        return 0


def pick_backed(pick, given=None, resolved=None, value=None, labels=()):
    """
    Whether a pick (picks()) is backed: chosen by an identifier, the entry a resolution matched, picked out by the
    free sources (option_backed, with the entries the list offered), stated for its field by a file
    (option_beside_label: the pick's label, or `labels`), or a person's kept answer about that field. `given`:
    given_sources (a text is taken as free sources only).
    """

    value = value or pick.get("taken") or pick.get("option")
    if pick.get("by_id"):
        return True
    if any(same_value(r, value, order="") for r in (resolved or ())):
        return True
    sources = given if isinstance(given, dict) else {"free": given or "", "files": []}
    candidates = [value] + ([pick["option"]] if pick.get("option") and pick["option"] != value else [])
    offered = pick.get("offered")
    for v in candidates:
        if option_backed(v, sources.get("free") or "", offered):
            return True
        for label in [pick.get("label")] + list(labels or ()):
            if label and any(option_beside_label(v, label, f, offered) for f in sources.get("files") or ()):
                return True
    return bool(pick.get("label")) and _kept_answer(pick["label"], value, pick.get("host", ""), offered)


def unbacked_pick_entries(task_id=None, events=None):
    """The picks into the forms this task saved that nothing backs (pick_backed), each once: what the final check
    holds against the task (review round 9, A)."""

    found = picks(task_id, events=events)
    if not found:
        return []
    resolved = resolved_names(task_id, events)
    given = None
    out = []
    for p in found:
        if p["by_id"] or any(same_value(r, p["taken"] or p["option"], order="") for r in resolved):
            continue
        if given is None:
            given = given_sources(task_id)
        if not pick_backed(p, given, resolved):
            if not any(o["label"] == p["label"] and o["option"] == p["option"] for o in out):
                out.append(p)
    return out


def unbacked_picks(task_id=None, events=None):
    """The options of unbacked_pick_entries, as entries taken (long_task.invented_typing's `chosen`)."""

    return list(dict.fromkeys(p["taken"] or p["option"] for p in unbacked_pick_entries(task_id, events)))


def chosen_values(task_id=None):
    """
    The entries this task chose from the application's own lists (gui select: what was asked and the entry taken), and
    the records its identifiers resolved to: values the application holds, so a record may name them even when no
    document writes them that way. Found live (2026-10-26): a customer's ledger, found by its tax number, was named
    otherwise than the bill wrote it; declared with the ledger's name the record was refused, declared with the bill's
    the save was stopped. (Whether such a value is backed is declare's to decide: pick_backed.)
    """

    return list(dict.fromkeys(picked_options(task_id) + resolved_names(task_id)))


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


def _source_units(key, kept, text, task_id):
    """
    A figure declared bare keeps the unit what backs it writes it with (owner decision, 2026-11-08). Found live: a note
    said "18%", the worker declared the record's Rate as "18", the save compared 18 with the unit-less field's 18 and
    the unit question - the very case it exists for - never came. What backs a figure is the record's source for one
    kept "from source", and what was given (given_text: the request, its files, the person's answers) for one kept
    "from seen" (never the application's pages it read, where the form's own bare figure stands). Where every place it writes the figure writes the same unit, the intent is
    the figure as written there ("18%"), changed in `kept`, so the save's comparison with a field whose label states no
    unit reaches the person's question (presave, _undecided). Where it writes the figure with a unit and also bare, or
    with two units, which one is this field's cannot be told: refused, once when one of them is bare (declared bare
    again, that is the worker's choice, made with both shown), and every time otherwise (the source never writes it
    bare). Returns (the "label=written" lines kept with a unit, the refusals).
    """

    united, forms = [], []
    given = None
    for label, f in kept.items():
        value = f.get("intent") or ""
        bare = values.parse_amount(value)
        if f.get("from") not in ("source", "seen") or not isinstance(bare, values.Amount) or bare.unit or bare.currency:
            continue
        if f["from"] == "seen":
            if given is None:
                given = given_text(task_id)
            backing = given
        else:
            backing = text
        places = values.units_beside(value, backing)
        units = {u for u, _ in places}
        if not units or units == {""}:
            continue
        if len(units) == 1:
            f["intent"] = next(w for u, w in places if u)
            united.append(f"{label}={f['intent']}")
            continue
        shown = list(dict.fromkeys(w for _, w in places))[:6]
        if "" in units and not stop_once(task_id, f"unit-{key}-{label}-{value}"):
            continue
        forms.append(f"{label}: '{value}' - written there as " + ", ".join(repr(w) for w in shown))
    return united, forms


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
    # A record this task saved already is not declared again: a second save of it makes a second record (review round
    # 9, B: a task tried again from the start declared and saved its bill twice).
    done = saved_record(load(task_id), key=key)
    text = _source_text(source)
    # The key on a page of the application this task read before (a list, a record): it may be entered already - the
    # task stops once and asks (review round 9, C: a bill already in the books was entered again).
    shown = _key_on_an_app_page(key, source, task_id) if done is None else ""  # (this task's own saved record)
    if shown and stop_once(task_id, f"already-{key}"):
        raise ComputerError(f"not declared - {key} stands on a page of the application this task read ({shown}): it "
                            "may be entered already. Ask the person (HELP) whether it is already in the application "
                            "before entering it; with their answer, declare it again.")
    # The order this source writes its digit dates in, from its own dates that cannot be read the other way (a bill
    # writing 28/09/2026 is day first), else the task's: kept on the record, so the save reads its dates by it.
    from decurix_agent import binding
    source_order = binding.date_order([text]) or task_context.source_date_order()
    picture = is_picture(source)
    elsewhere = chosen = selects = resolved = given = None
    kept, unsourced, partial, unbacked = {}, [], [], []
    # A number, reference or id given as words (wordy_identifiers): refused before anything else is asked of the value -
    # a document that writes the words there, beside the field's label, does not give the code (found live 2026-11-08).
    wordy = wordy_identifiers(fields.items(), task_id)
    if wordy:
        raise ComputerError("not declared - " + wordy_text(wordy))

    def backed(value, label):
        # Whether a value the worker took from an application's list is backed (pick_backed): by an identifier, a
        # resolution, what was given, or a person's kept answer about its field. An option the worker picked itself is
        # no source for itself (review round 9, A and R9-B1).
        nonlocal given
        mine = [p for p in selects if same_value(p["option"], value, order="") or same_value(p["taken"], value, order="")]
        if not mine:
            return None
        if given is None:
            given = given_sources(task_id)
        return any(pick_backed(p, given, resolved, value=value, labels=(label,)) for p in mine)
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
            selects, resolved = _selects(task_id), resolved_names(task_id)
            chosen = list(dict.fromkeys([v for p in selects for v in (p["option"], p["taken"]) if v] + resolved))
        if any(compare_shown(c, value, source_order).same is True for c in chosen):
            # An entry of the application's own list - backed only when it was reached from a source value (review
            # round 9, A: "General" picked for a cost centre no source gives was declared, saved and passed as checked).
            if backed(value, label) is False:
                unbacked.append(f"{label}: {value}")
                continue
            kept[label] = {"intent": value, "from": "chosen"}
            continue
        if backed(value, label) is False:
            # (A value the worker picked from a list, standing only on the pages it read - the list itself - is no
            # more backed than above.)
            unbacked.append(f"{label}: {value}")
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
    if unbacked:
        raise ComputerError("not declared - these values are options you picked from the application's own list, and "
                            "nothing given backs them (not the source, the request, its files or the person's "
                            "answers): "
                            + "; ".join(unbacked) + ". The list offering an option does not say which one this record "
                            "takes: ask the person (HELP) which one, then declare it.")
    if unsourced:
        raise ComputerError("not declared - these values stand neither in the source nor in anything this task read "
                            "or was told: " + "; ".join(unsourced) + ". Declare each value as the DOCUMENT writes it "
                            "(a place as 'Kerala', not the application's option label '32-Kerala'; a party as the "
                            "document names it): the application's own wording of an option or an entry chosen from "
                            "its list is accepted at the save. A value the document does not give at all is not typed.")
    united, unit_forms = _source_units(key, kept, text, task_id)
    if unit_forms:
        raise ComputerError("not declared - a figure is declared bare where what backs it writes it with a unit: "
                            + "; ".join(unit_forms) + ". Declare each as the source writes it for this field (with its "
                            "unit): the save compares it with the field and asks the person when the field's label "
                            "does not say the unit. A figure the source also writes bare may be declared bare again "
                            "only when that is the one this field takes.")
    records = load(task_id)
    if done is not None:
        return _add_to_saved(task_id, records, done, kept)
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
    if united:
        lines.append("(kept with the unit their source writes them in, which the declaration left out: "
                     + ", ".join(united) + ")")
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


def field_place(task_id=None, events=None):
    """(host, form) of the page the task acts on now (unit_answers.place_of of its last recorded action's address):
    where a field's unit, as the person answered it, belongs. ("", "") when nothing recorded names a page."""

    from decurix_agent import unit_answers
    try:
        if events is None:
            from decurix_agent.computer import evidence
            events = evidence._task_events(task_id)
    except Exception:  # noqa: BLE001 - an unreadable recording names no page
        return "", ""
    for event in reversed(events or []):
        url = event.get("url") if isinstance(event, dict) else None
        if url:
            return unit_answers.place_of(url)
    return "", ""


def _one_sided_unit(shown, want):
    """The unit written on one side only of two figures (values.parse_amount), or ""."""

    a, b = values.parse_amount(shown), values.parse_amount(want)
    if not isinstance(a, values.Amount) or not isinstance(b, values.Amount) or bool(a.unit) == bool(b.unit):
        return ""
    return a.unit or b.unit


def _by_the_persons_unit(flabel, fvalue, want, source_order, place):
    """
    (result, unit) for a field compared undecided for a unit on one side only: compared again with the unit the person
    said this field takes (unit_answers.answered: their judgement on this task, else a learned row kept from one) -
    the same figure when they said it takes it, a different one when they said it does not, and (None, unit) when they
    have not said or what they said cannot be read from the control plane (then the save waits and they are asked).
    """

    from decurix_agent import unit_answers
    unit = _one_sided_unit(fvalue, want)
    if not unit:
        return None, ""
    said = unit_answers.answered(flabel, place[0], place[1], unit)
    if said is True:
        return compare_shown(fvalue, want, source_order, flabel, units=(unit,)), unit
    if said is False:
        return values.Result(False, "unit"), unit
    return None, unit


def _undecided(flabel, fvalue, want, why, place=("", ""), unit=""):
    """The line a save waits on when a field cannot be compared - and, for a date whose order is not known, the
    question to the person it waits on (task_context.ask_date_order: both readings, an option each); for a unit on
    one side only, the question whether the field takes it (unit_answers.ask: asked once for that field)."""

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
    if why == "unit" and unit:
        from decurix_agent import unit_answers
        task_id = task_context.current()
        if unit_answers.asked_already(task_id, flabel, place[0], place[1], unit):
            return (f"{flabel} holds {fvalue!r} and the record says {want!r}: the person answered whether this field "
                    f"takes its figures in {unit}, and their answer cannot be read from the control plane now, so "
                    "they cannot be compared - this needs the person; do not retype the field.")
        if unit_answers.ask(task_id, flabel, place[0], place[1], unit, fvalue, want):
            return (f"{flabel} holds {fvalue!r} and the record says {want!r}: a unit ({unit}) is written on one side "
                    "only and the field's label does not state it - this needs the person. The agent asks them itself "
                    "whether this field takes its figures in that unit, as a question of its own, when you next ask "
                    "them anything (HELP: say the save waits on this field's unit) or stop; their answer is kept for "
                    "this field, so it is asked once. Do not retype the field.")
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
    chosen = place = None
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
            unit = ""
            if decided.same is None and decided.why == "unit":
                # A unit on one side only: the person's recorded word about this field's unit decides it, if any.
                if place is None:
                    place = field_place()
                said, unit = _by_the_persons_unit(flabel, fvalue, want, _source_order(rec), place)
                if said is not None and said.same is True:
                    continue
                if said is not None:
                    blocking.append(f"{flabel} holds {fvalue!r} where the record says {want!r}, and the person said "
                                    f"this field does not take its figures in {unit}")
                    continue
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
                blocking.append(_undecided(flabel, fvalue, want, decided.why, place or ("", ""), unit))
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


def note_presave(rec, blocking, task_id=None, undeclared=None):
    """Keep the last pre-save diff on the record (what blocked, how many checks, and the fields this task set on the
    form that the record does not declare - what the final check holds against a saved record)."""

    task_id = task_id or task_context.current()
    records = load(task_id)
    for r in records:
        if r.get("key") == rec.get("key") and r.get("declared_at") == rec.get("declared_at"):
            r["checks"] = int(r.get("checks") or 0) + 1
            r["last_blocked"] = blocking[:12]
            if undeclared is not None:
                r["undeclared"] = [f"{l}={v}" for l, v in undeclared][:12]
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


# Whether presave blocks a save over a field the task set that no declaration covers. ADVISORY until it passes the
# replay gate (CLAUDE.md: a new code check ships advisory); the final check holds it against a saved record now.
UNDECLARED_FIELD_BLOCKS = False


def undeclared_task_fields(rec, form_fields, task_set):
    """[(label, value)] of the form's filled fields that this task set (`task_set`: labels typed or chosen into this
    form) and the record does not declare (review round 9, A)."""

    declared = [l for l, f in (rec.get("fields") or {}).items()]
    out = []
    for label, value in form_fields:
        if not str(value if value is not None else "").strip():
            continue
        if not any(label_matches(t, label) or label_words(t) == label_words(label) for t in task_set):
            continue
        if any(label_matches(d, label) or label_words(d) == label_words(label) for d in declared):
            continue
        out.append((str(label), str(value)))
    return out


def gate_faults(task_id=None, created=None):
    """
    What the final check holds against the task's saved records, decided in code (review round 9): a field this task
    set on a saved record's form that no declaration covers, and more records made by this task's saves (`created`,
    the saves that made a new record) than the declared records it saved - a save outside the ledger, or one record
    saved twice. [] when none, or when the task declared no record.
    """

    found = load(task_id)
    if not found:
        return []
    out = []
    for rec in found:
        if rec.get("saved_at") and rec.get("undeclared"):
            out.append(finding(UNDECLARED, f"record {rec['key']} was saved with " + ", ".join(rec["undeclared"][:6])
                               + " - a value this task set that no declaration backs"))
    saved = sum(1 for r in found if r.get("saved_at"))
    if created is not None and created > saved:
        out.append(finding(UNCOVERED, f"this task's saves made {created} new record(s), and {saved} declared record(s) "
                           "were saved: a record was made that no declaration covers"))
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
