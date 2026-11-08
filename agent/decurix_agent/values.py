"""
One comparator for a field's values: whether what a form holds is the value wanted, whatever its form.

Every check that asks "is this the same value?" - the record ledger's save gate and read-back (computer/records.py), the
screen's crossed-values refusal (computer/core.py), a taught run's "never entered" check (skill_task.py) - asks here, so
the three cannot disagree. Found in the review of 2026-11-03 (W0.2, W0.3): the screen's comparator read every digit
date day first and compared numbers by their last 10 digits; a taught run's went through float(). Review round 2
(2026-11-04): signs, units and non-Latin letters were dropped, and one date order was applied to both sides.

`compare(a, b, a_order, b_order)` answers True, False or None, with why: None when what is known cannot decide it. A
caller that lets something through on a match passes only on True; a caller that stops something on a difference
treats None as "ask" - it fails closed both ways. `same_value` is its answer alone.

Dates. Each side is read under its own order: the source document's convention and the application's display
convention are different facts (`a_order`, `b_order`: "day" for DD/MM, "month" for MM/DD, or None). ISO (2026-04-03)
and dates with a named month are always decided. A digit date p1/p2/y with no order known is decided only by its own
figures (p1 over 12 day first, p2 over 12 month first, p1 == p2 either way); otherwise it is ambiguous, and an
ambiguous side makes the answer None unless none of its readings can match. Under a known order, a date the order
cannot read (04/13/2026 under "day") is not decided either: None (ask), never False. A time after the date is compared
when both sides give one.

Numbers are a sign, digits and a unit, and all must match: brackets, a Unicode minus, a trailing minus and a minus
before or after a currency sign are negative. Digits compare by value (Decimal, every digit). A unit written beside the
figure with a space ("15 kg", "500 Cr", "18 %") must be the same unit; a currency mark matches its own kind only, and a
figure written with none matches any. Letters or a dash joined to the digits make an identifier, and so does a leading
zero: those are text. Separators that read two ways (1.250,00, 12,5) are not decided. When one side reads as a number
and the other does not, the answer is None - never a match of their punctuation-stripped text. One structural
exception: a value written with a leading "+" and a 1-3 digit country code, a space or a dash, then a national number
of at least seven digits (the E.164 shape), equals the same national number.

Text: NFKC, case folded, whitespace collapsed; every letter, mark and digit of every script kept, punctuation aside;
a short code in brackets at the end of an option's label ("Karnataka (29)") aside.
"""

import datetime
import re
import unicodedata
from collections import namedtuple
from decimal import Decimal, InvalidOperation

ORDERS = ("day", "month")

Result = namedtuple("Result", "same why")
# A date as read: kind "date" (y, m, d) with an optional (h, min, s); "ambiguous" (p1, p2, y); "unreadable": a digit
# date the known order cannot read.
Day = namedtuple("Day", "kind parts time")
# A number as read: sign (-1, 1), its magnitude, a unit written beside it ("" for none), a currency ("" for none).
Amount = namedtuple("Amount", "sign magnitude unit currency")
UNDECIDED = "undecided"  # parse_amount: a figure whose separators read two ways

_MONTHS = {}
for _n, _names in enumerate((("january", "jan"), ("february", "feb"), ("march", "mar"), ("april", "apr"), ("may",),
                             ("june", "jun"), ("july", "jul"), ("august", "aug"), ("september", "sep", "sept"),
                             ("october", "oct"), ("november", "nov"), ("december", "dec")), 1):
    for _name in _names:
        _MONTHS[_name] = _n
_MONTH_NAMES = ("January", "February", "March", "April", "May", "June", "July", "August", "September", "October",
                "November", "December")

_TIME = r"(?:[ T,]+(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?\s*(?:([AaPp])\.?[Mm]\.?)?(?:Z|[+-]\d{2}:?\d{2})?)?"
_DIGIT_DATE = re.compile(r"(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4}|\d{2})" + _TIME)
_YEAR_FIRST = re.compile(r"(\d{4})[/.\-](\d{1,2})[/.\-](\d{1,2})" + _TIME)
_DAY_NAMED = re.compile(r"(?i)(\d{1,2})(?:st|nd|rd|th)?[\s/.\-]+([a-z]{3,9})\.?[\s/.\-,]+(\d{4}|\d{2})" + _TIME)
_NAMED_DAY = re.compile(r"(?i)([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})" + _TIME)

# Currency marks, as the old comparator knew them: a sign of the Unicode currency class, or one of these written out.
_CURRENCY_WORDS = {"rs": "inr", "inr": "inr", "rupee": "inr", "rupees": "inr", "usd": "usd", "eur": "eur", "gbp": "gbp"}
_CURRENCY_SIGNS = {"₹": "inr", "$": "usd", "€": "eur", "£": "gbp"}
_DIGITS = r"\d(?:[\d,.]|\s(?=\d))*"
# The E.164 shape: "+", a country code of 1-3 digits, a space or a dash, then the national number (seven digits at the
# least, so a signed amount "+1 500000" is not read as a phone).
_COUNTRY_CODE = re.compile(r"\s*\+\d{1,3}[\s-]+(?=(?:\d[\s-]?){7})")


def _order(order):
    return order if order in ORDERS else None


def _norm(value):
    return " ".join(unicodedata.normalize("NFKC", str(value if value is not None else "")).split())


# ------------------------------------------------------------------ numbers

def _currency(token):
    token = token.strip().rstrip(".").casefold()
    if token in _CURRENCY_WORDS:
        return _CURRENCY_WORDS[token]
    if len(token) == 1 and unicodedata.category(token) == "Sc":
        return _CURRENCY_SIGNS.get(token, token)
    return ""


def _magnitude(digits):
    """Decimal of a figure's digits, None when it is not a figure, UNDECIDED when its separators read two ways."""

    if re.match(r"0\d", digits):
        return None  # a leading zero: an identifier, "0012345" is not 12345
    plain = re.fullmatch(r"\d+(?:\.\d+)?", digits)
    grouped = (re.fullmatch(r"\d{1,3}(?:,\d{3})+(?:\.\d+)?", digits)                 # 1,234,567.89
               or re.fullmatch(r"\d{1,2}(?:,\d{2})+,\d{3}(?:\.\d+)?", digits)         # 1,25,000.00 (lakh)
               or re.fullmatch(r"\d{1,3}(?: \d{3})+(?:\.\d+)?", digits))              # 1 234 567.89
    if plain or grouped:
        try:
            return Decimal(re.sub(r"[, ]", "", digits))
        except InvalidOperation:
            return None
    if re.fullmatch(r"[\d.,\s]+", digits) and re.search(r"[.,]", digits):
        return UNDECIDED  # 1.250,00 / 12,5 / 1.250.000: the separators say two things
    return None


def parse_amount(value):
    """
    An Amount when a value writes one number (sign, magnitude, unit, currency); UNDECIDED when its separators read two
    ways; None when it writes no number - an identifier included (letters or a dash joined to the digits, or a leading
    zero).
    """

    text = _norm(value)
    if not text:
        return None
    sign = 1
    if text.startswith("(") and text.endswith(")"):
        sign, text = -1, text[1:-1].strip()
    if text.endswith("/-"):
        text = text[:-2].strip()  # "500/-": the amount and no more, not a minus
    elif text.endswith(("-", "−")) and len(text) > 1:
        sign, text = -sign, text[:-1].strip()
    currency, unit = "", ""
    m = re.match(r"([+\-−])?\s*((?:rs|inr|rupees?|usd|eur|gbp)\.?(?=[\s\d+\-−])|\p{Sc})?\s*"
                 .replace(r"\p{Sc}", "[₹$€£¥]"), text, re.I)
    if m and (m.group(1) or m.group(2)):
        if m.group(1) in ("-", "−"):
            sign = -sign
        if m.group(2):
            currency = _currency(m.group(2))
        text = text[m.end():]
        n = re.match(r"([+\-−])\s*", text)  # "Rs -500"
        if n:
            if n.group(1) in ("-", "−"):
                sign = -sign
            text = text[n.end():]
    if re.match(r"\.\d", text):
        text = "0" + text  # ".50" is 0.50
    m = re.fullmatch(r"(" + _DIGITS + r")(?:\s*(%)|\s+([^\W\d_]{1,12}\.?)|\s*([₹$€£¥]))?", text)
    if not m:
        return None
    if m.group(2):
        unit = "%"
    elif m.group(3):
        word = m.group(3)
        currency = currency or _currency(word)
        unit = "" if _currency(word) else word.rstrip(".").casefold()
    elif m.group(4):
        currency = currency or _currency(m.group(4))
    magnitude = _magnitude(m.group(1))
    if magnitude is None or magnitude == UNDECIDED:
        return magnitude
    return Amount(sign if magnitude else 1, magnitude, unit, currency)


def parse_number(value):
    """The number a value writes (commas, currency signs and spaces aside, its sign kept), or None - None also for an
    identifier written in digits, a figure whose separators read two ways, and one written with a unit."""

    found = parse_amount(value)
    if not isinstance(found, Amount) or found.unit:
        return None
    return found.sign * found.magnitude


def _same_amount(a, b, field_units=()):
    if a.magnitude != b.magnitude or a.sign != b.sign:
        return Result(False, "number")
    if a.currency and b.currency and a.currency != b.currency:
        return Result(False, "currency")
    if a.unit != b.unit:
        if a.unit and b.unit:
            return Result(False, "unit")
        if (a.unit or b.unit) in field_units:
            return Result(True, "number")  # the field states the unit the bare figure is written in
        return Result(None, "unit")
    return Result(True, "number")


def field_units(label):
    """
    The units a form field's label (or the text shown beside it) states for its figure: "%" and each word, case
    folded - "GST Rate (%)" states "%", "Weight (kg)" states "kg". Words given as alternatives ("Dr/Cr", "kg / lb")
    state none of them: such a field does not say which. A figure written bare in that field carries a unit the label
    states, so "18%" and the "18" of a field labelled "Rate (%)" are one figure (review round 3, R5); without such a
    label a unit on one side only stays undecided - "15 kg" against the "15" of a field labelled "Weight (lb)" is not
    a pass. A label that names more than one unit names none: words joined by "/", "or" or a comma are alternatives
    ("Weight in kg or lb", "Weight (kg, lb)"), and so is a "%" beside a bracketed word - such a field does not say
    which (review round 4).
    """

    text = _norm(label).casefold()
    # Units named in brackets, and "%", counted across the whole label: two or more and it names none ("Weight (kg)
    # or volume (l)", "Rate % (kg)" - review round 5, N5).
    if len(re.findall(r"\([^()]*[^\W\d_%][^()]*\)|%", text)) >= 2:
        return frozenset()
    text = re.sub(r"[^\W_]+(?:\s*(?:/|,|\bor\b)\s*[^\W_]+)+", " ", text)
    return frozenset(re.findall(r"%|[^\W\d_]+", text))


# A figure a text writes as a token of its own: not inside a longer number, a date, a time, a code or a word.
_FIGURE_IN_TEXT = re.compile(r"(?<![\w.,/\-])(?<!\d:)\d[\d,]*(?:\.\d+)?(?![\w/\-]|[.,]\d)")
# What opens a value of its own just before a figure, on its line: the line's start, a table's cell, a label's ":"/"=".
_VALUE_OPENS = re.compile(r"(?:^|[|\t:=]| {2})[ \t]*$")
# One word right after a figure that closes the value: the end of the line or of a table's cell, or a comma/semicolon.
_UNIT_WORD = re.compile(r"[ \t]+[^\W\d_]{1,12}\.?(?=[ \t]*(?:[\r\n|,;]|$)|\t| {2})")


def units_beside(value, text):
    """
    For each place `text` writes the figure `value` writes (by value, however grouped), the unit parse_amount reads
    beside it there and how it is written there, as (unit, written): ("%", "18%"), and ("", "18") for a place that
    writes it bare. [] when `value` is no plain figure or the text never writes it. A percent sign is a unit wherever
    it stands ("GST @ 18%", "18 %"); a word only where the figure and the word make a whole value of their own - a
    table's cell, or a label's value to the end of its line ("Weight: 15 kg", "| 18 Nos |") - never a word a sentence
    runs on with ("came to 18 today"). Structure only, never a list of units (2026-11-08: a note's "18%" declared as
    a bare 18 settled a unit-less field's unit silently - computer/records.py declare).
    """

    want = parse_amount(value)
    if not isinstance(want, Amount):
        return []
    text = unicodedata.normalize("NFKC", str(text or ""))
    out = []
    for m in _FIGURE_IN_TEXT.finditer(text):
        got = parse_amount(m.group(0))
        if not isinstance(got, Amount) or got.magnitude != want.magnitude:
            continue
        rest = text[m.end():]
        written = m.group(0)
        pct = re.match(r"[ \t]?%", rest)
        if pct:
            written += pct.group(0)
        elif _VALUE_OPENS.search(text[text.rfind("\n", 0, m.start()) + 1:m.start()]):
            word = _UNIT_WORD.match(rest)
            if word and isinstance(parse_amount(written + word.group(0)), Amount):
                written += word.group(0)
        found = parse_amount(written)
        out.append((found.unit if isinstance(found, Amount) else "", " ".join(written.split())))
    return out


# ------------------------------------------------------------------ dates

def _day(y, m, d):
    try:
        return datetime.date(y, m, d).timetuple()[:3]
    except ValueError:
        return None


def _year(text):
    y = int(text)
    if len(text) != 2:
        return y
    this = datetime.date.today().year
    return 2000 + y if 2000 + y <= this + 20 else 1900 + y  # (31/12/99 is 1999)


def _time(m, first):
    """(h, min, s) a match's time groups give, or None."""

    if not m.group(first):
        return None
    h, mi, s = int(m.group(first)), int(m.group(first + 1)), int(m.group(first + 2) or 0)
    ampm = (m.group(first + 3) or "").lower()
    if ampm == "p" and h < 12:
        h += 12
    if ampm == "a" and h == 12:
        h = 0
    return (h, mi, s) if h < 24 and mi < 60 and s < 61 else None


def parse_date(value, order=None):
    """
    The day a value names, as a Day: kind "date" when decided; "ambiguous" for a digit date whose day and month could be
    either way when no order says which; "unreadable" for a digit date the known order cannot read. None when the value
    is not a date at all.
    """

    order = _order(order)
    if isinstance(value, datetime.datetime):
        return Day("date", value.date().timetuple()[:3], (value.hour, value.minute, value.second))
    if isinstance(value, datetime.date):
        return Day("date", value.timetuple()[:3], None)
    text = _norm(value)
    if not text:
        return None
    m = _YEAR_FIRST.fullmatch(text)
    if m:
        day = _day(int(m.group(1)), int(m.group(2)), int(m.group(3)))
        return Day("date", day, _time(m, 4)) if day else None  # (2026-13-04 is no date)
    m = _DIGIT_DATE.fullmatch(text)
    if m:
        p1, p2, y = int(m.group(1)), int(m.group(2)), _year(m.group(3))
        time = _time(m, 4)
        if order == "day":
            day = _day(y, p2, p1)
        elif order == "month":
            day = _day(y, p1, p2)
        elif p1 > 12:
            day = _day(y, p2, p1)
        elif p2 > 12 or p1 == p2:
            day = _day(y, p1, p2)
        else:
            if not (1 <= p1 <= 12 and 1 <= p2 <= 12):
                return None
            return Day("ambiguous", (p1, p2, y), time)
        if day:
            return Day("date", day, time)
        # Under a known order, figures it cannot read are not shown to differ: the order may not be this one's.
        return Day("unreadable", text, None) if order and _readings_any(p1, p2, y) else None
    for pattern, (di, mi, yi) in ((_DAY_NAMED, (1, 2, 3)), (_NAMED_DAY, (2, 1, 3))):
        m = pattern.fullmatch(text)
        if m:
            month = _MONTHS.get(m.group(mi).lower())
            if not month:
                continue
            day = _day(_year(m.group(yi)), month, int(m.group(di)))
            return Day("date", day, _time(m, 4)) if day else None
    return None


def _readings_any(p1, p2, y):
    return bool(_day(y, p2, p1) or _day(y, p1, p2))


def readings(value):
    """{"day": (y, m, d), "month": (y, m, d)}: how an ambiguous digit date reads under each order (only those valid)."""

    found = parse_date(value)
    if not found or found.kind != "ambiguous":
        return {}
    p1, p2, y = found.parts
    out = {"day": _day(y, p2, p1), "month": _day(y, p1, p2)}
    return {k: v for k, v in out.items() if v}


def spoken(day):
    """(y, m, d) as words: 3 April 2026."""

    y, m, d = day
    return f"{d} {_MONTH_NAMES[m - 1]} {y}"


def _same_date(da, db):
    if "unreadable" in (da.kind, db.kind):
        return Result(None, "date_order:" + ("a" if da.kind == "unreadable" else "b"))
    if da.kind == "date" and db.kind == "date":
        if da.parts != db.parts:
            return Result(False, "date")
        if da.time and db.time and da.time != db.time:
            return Result(False, "time")
        return Result(True, "date")
    reads_a = _readings_of(da)
    reads_b = _readings_of(db)
    if not reads_a & reads_b:
        return Result(False, "date")
    return Result(None, "date_order:" + ("a" if da.kind == "ambiguous" else "b"))


def _readings_of(day):
    if day.kind == "date":
        return {day.parts}
    p1, p2, y = day.parts
    return {r for r in (_day(y, p2, p1), _day(y, p1, p2)) if r}


# ------------------------------------------------------------------ text

def text_key(value):
    """A value as text: NFKC, case folded, every letter, mark and digit of every script; punctuation is a gap like a
    space, and gaps collapse to one - "Pvt.Ltd." and "Pvt. Ltd.", "Saint-Gobain" and "Saint Gobain" are one name. A short code in brackets at the end - "Karnataka (29)" - names the same thing as the bare name:
    an option's label often carries one (found live 2026-10-26, a right form was stopped once over it)."""

    kept = "".join(ch if unicodedata.category(ch)[0] in "LMN" else " " for ch in _text_of(value))
    return " ".join(kept.split())


def _text_of(value):
    return re.sub(r"\s*\([^()]{1,12}\)\s*$", "", _norm(value)).casefold()


def _compare_key(value):
    """What two texts are compared by: their letters, marks and digits in order, gaps aside - except a gap between
    two digits, punctuation or space, which is kept as one gap ("12-345" is not "123-45", "PO 12/3" is not "PO 1/23",
    "INV-2024-0042" is "INV 2024 0042"): digits grouped differently are a different code or address (review round 4).
    A value of digits and spaces only is compared by its digits before this (_spaced_digits), so an account written
    "1234 5678" is still "12345678"; inside text the grouping counts, which can only make a save wait."""

    out, gap = [], False
    for ch in _text_of(value):
        if unicodedata.category(ch)[0] in "LMN":
            if gap and out and out[-1].isdigit() and ch.isdigit():
                out.append("|")
            out.append(ch)
            gap = False
        else:
            gap = True
    return "".join(out)


def _spaced_digits(value):
    """The digits of a value that is only digits and spaces (no sign, no separator), else ""."""

    text = _norm(value)
    return _ascii_digits(text.replace(" ", "")) if re.fullmatch(r"\d+(?: \d+)*", text) else ""


def _ascii_digits(text):
    """Digits of any script as ASCII ("१२३" is "123")."""

    return "".join(str(unicodedata.digit(ch)) if ch.isdigit() else ch for ch in text)


def _national(value):
    """What follows a leading country code written in the E.164 shape, or None when the value has none."""

    if not isinstance(value, str):
        return None
    m = _COUNTRY_CODE.match(value)
    return value[m.end():] if m else None


# ------------------------------------------------------------------ the comparator

def compare(a, b, a_order=None, b_order=None, units=()):
    """
    Whether two values of one field are the same whatever their form: Result(same, why), same True, False, or None when
    what is known cannot decide it. `a_order` / `b_order`: the date order each side is written in ("day", "month" or
    None) - the source document's and the application's are different facts. `why` says what decided or what is
    missing: "date_order:a" / "date_order:b" is the side whose date order would decide it. `units`: what the field
    states its figure is in (field_units of its label) - a figure written with one of them matches the same figure
    written bare.
    """

    a_order, b_order = _order(a_order), _order(b_order)
    da, db = parse_date(a, a_order), parse_date(b, b_order)
    if da or db:
        if da and db:
            return _same_date(da, db)
        other = b if da else a
        return Result(None, "kinds") if re.search(r"\d", _norm(other)) else Result(False, "kinds")
    ca, cb = _national(a), _national(b)
    if (ca is None) != (cb is None):
        national, other = (ca, _norm(b)) if ca is not None else (cb, _norm(a))
        digits, theirs = (_ascii_digits(re.sub(r"[\s-]", "", national)), _ascii_digits(re.sub(r"[\s-]", "", other)))
        if digits.isdigit() and theirs.isdigit():
            return Result(digits == theirs, "phone")
        return Result(None, "kinds")
    # Digits grouped by spaces as no number is (a phone, an account: "98765 43210") are those digits.
    sa, sb = _spaced_digits(a), _spaced_digits(b)
    if sa and sb:
        return Result(sa == sb, "digits")
    na, nb = parse_amount(a), parse_amount(b)
    if isinstance(na, Amount) and isinstance(nb, Amount):
        return _same_amount(na, nb, frozenset(units or ()))
    if UNDECIDED in (na, nb):
        return Result(None, "number_format")
    if isinstance(na, Amount) or isinstance(nb, Amount):
        return Result(None, "kinds")
    # Two texts are one when their letters and digits agree in order, gaps aside: a gap is where punctuation or a
    # space stood, and apps write the same name both ways ("ACME-LTD" / "acme ltd", "INV-0042" / "inv0042") - but
    # never a gap between two digits (_compare_key).
    ka, kb = _compare_key(a), _compare_key(b)
    if ka and ka != kb and ka.replace("|", "") == kb.replace("|", ""):
        # The same letters and digits, grouped otherwise ("HSN 8471 30" / "HSN 847130"): it may be one code an
        # application regrouped, or two - undecided, so the save asks rather than sends the worker to retype it (review
        # round 5, N4).
        return Result(None, "digit_groups")
    return Result(bool(ka) and ka == kb, "text")


def range_count(before, left, stated):
    """
    Whether "<left> = <stated>", written right after `before`, is a range of numbered things and how many it holds,
    not a subtraction: "Rows 2 - 10 = 9." `left` is two whole numbers joined by a dash, the first below the second;
    `stated` is a whole number equal to how many the range holds, both ends counted (10 - 2 + 1); and the clause it
    stands in names what is numbered - a word stands right before the first number, with no full stop, colon,
    semicolon or line break between them. Decided by that structure alone, never by which word it is. A subtraction of
    a larger figure from a smaller one is never claimed to come to that count, so a wrong sum is still a wrong sum: a
    reversed difference ("Balance 200 - 350 = 150") is not the range's count (151), and stays a sum.
    """

    m = re.fullmatch(r"\s*(\d+)\s*[-–]\s*(\d+)\s*", str(left or ""))
    count = re.fullmatch(r"\s*(\d+)\s*", str(stated or ""))
    if not m or not count or re.match(r"0\d", m.group(1)) or re.match(r"0\d", m.group(2)):
        return False
    low, high = int(m.group(1)), int(m.group(2))
    if low >= high or int(count.group(1)) != high - low + 1:
        return False
    clause = re.split(r"[.;:!?\n]", str(before or ""))[-1]
    return bool(re.search(r"[^\W\d_]\s+$", clause))


def same_value(a, b, order=None, a_order=None, b_order=None):
    """compare(...).same. `order` is both sides' when given (a value compared with one written the same way)."""

    if order is not None:
        a_order = b_order = order
    return compare(a, b, a_order, b_order).same


def as_iso(value, order=None):
    """The day a value names as YYYY-MM-DD when it is decided, else None."""

    found = parse_date(value, order)
    if not found or found.kind != "date":
        return None
    y, m, d = found.parts
    return f"{y:04d}-{m:02d}-{d:02d}"
