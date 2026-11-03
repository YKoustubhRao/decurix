"""
One comparator for a field's values: whether what a form holds is the value wanted, whatever its form.

Every check that asks "is this the same value?" - the record ledger's save gate and read-back (computer/records.py), the
screen's crossed-values refusal (computer/core.py), a taught run's "never entered" check (skill_task.py) - asks here, so
the three cannot disagree. Found in the review of 2026-11-03 (W0.2, W0.3): the screen's comparator read every digit
date day first (04/13/2026 became 2026-13-04) and compared numbers by their last 10 digits, so two account numbers that
shared those were "the same"; a taught run's comparator went through float(), which loses digits past the 15th.

`same_value(a, b, order)` answers True, False, or None: None when what is known cannot decide it - a digit date whose
day and month could be either way, against a date written so it cannot. A caller that would let something through on
a match passes only on True; a caller that would stop something on a difference treats None as "ask" - it fails
closed both ways.

Dates. ISO (2026-04-03) and dates with a named month are always decided. A digit date p1/p2/y with no order known is
decided only by its own figures: p1 over 12 makes it day first, p2 over 12 month first, p1 == p2 either way; otherwise
it is ambiguous. With an order known ("day": DD/MM, "month": MM/DD), the order says which figure is the month, and a
month over 12 or a day over 31 in that place is not a date at all - 04/13/2026 under "day" is no date, never the 13th
month. Two ambiguous dates are compared figure by figure; an ambiguous date against a decided one is None when one of
its readings is that day, False when neither is.

Numbers are equal only when every digit is (Decimal). Letters or a dash joined to the digits make an identifier, and so
does a leading zero: those are compared as their letters and digits, case folded ("INV-0042" is not "0042"). One
structural exception: a value written with a leading "+" and a 1-3 digit country code, then a space or a dash (the
E.164 shape), equals the same digits without that code.

Everything else is text: case, spaces and punctuation aside, and a short code in brackets at the end of an option's
label ("Karnataka (29)") aside.
"""

import datetime
import re
from decimal import Decimal, InvalidOperation

ORDERS = ("day", "month")

_MONTHS = {m: i for i, m in enumerate(("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov",
                                       "dec"), 1)}

# A digit date: two figures of one or two digits and a year of two or four, with / . or - between (a space would read
# a number grouped by spaces, "12 10 2026", as a date). A time may follow.
_DIGIT_DATE = re.compile(r"(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4}|\d{2})(?:[ T,].*)?")
# Year first: 2026-04-03, 2026/4/3 - always year, month, day.
_YEAR_FIRST = re.compile(r"(\d{4})[/.\-](\d{1,2})[/.\-](\d{1,2})(?:[ T].*)?")
# A named month: 1-Oct-26, 1 October 2026, 3rd Apr 2026.
_DAY_NAMED = re.compile(r"(?i)(\d{1,2})(?:st|nd|rd|th)?[\s/.\-]+([a-z]{3,9})\.?[\s/.\-,]+(\d{4}|\d{2})(?:[ T,].*)?")
# Oct 1, 2026 / October 1 2026.
_NAMED_DAY = re.compile(r"(?i)([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})")

# A number as a value writes it: a currency sign or word before it ("Rs. 5,850.00", "₹78", "INR 400"), a sign or a
# unit after it ("500/-", "18%", "15 kg"). Letters or a dash joined to the digits ("INV-1234", "PO1234", "1234A") make
# it an identifier, never a number: "INV-1234" and "PO-1234" are two documents, not the figure -1234 twice. Found in
# review (2026-11-02): the save gate passed a form holding the wrong document number.
_AS_NUMBER = re.compile(
    r"\s*(?:(?:rs\.?|inr|rupees?|usd|eur|gbp)\s*|[^\w\s.,+-]\s*)?([-+]?)\s*"
    r"(\d(?:[\d,]|\s(?=\d))*(?:\.\d+)?)"
    r"\s*(?:/-|%|[^\w\s.,]|\s[A-Za-z]{1,12}\.?)?\s*", re.I)

# The E.164 shape: "+", a country code of 1-3 digits, a space or a dash, then the national number (six digits at the
# least, so an amount written "+1 500" is not read as a phone).
_COUNTRY_CODE = re.compile(r"\s*\+\d{1,3}[\s-]+(?=(?:\d[\s-]?){6})")


def _order(order):
    return order if order in ORDERS else None


def parse_number(value):
    """The number a value writes (commas, currency signs and spaces aside), or None - None also for an identifier
    written in digits: letters joined to them, or a leading zero ("0012345" is not 12345)."""

    m = _AS_NUMBER.fullmatch(str(value if value is not None else ""))
    if not m:
        return None
    digits = re.sub(r"[,\s]", "", m.group(2))
    if re.match(r"0\d", digits):
        return None
    try:
        return Decimal(m.group(1) + digits)
    except InvalidOperation:
        return None


def _day(y, m, d):
    try:
        return datetime.date(y, m, d).timetuple()[:3]
    except ValueError:
        return None


def _year(text):
    y = int(text)
    return y + 2000 if len(text) == 2 else y


def parse_date(value, order=None):
    """
    The day a value names: ("date", (y, m, d)) when decided; ("ambiguous", (p1, p2, y), "p/p/y") for a digit date
    whose day and month could be either way and no order says which; None when it is not a date (a month over 12, or
    a day over 31, where the order puts them is not a date).
    """

    order = _order(order)
    if isinstance(value, datetime.datetime):
        return ("date", value.date().timetuple()[:3])
    if isinstance(value, datetime.date):
        return ("date", value.timetuple()[:3])
    text = " ".join(str(value if value is not None else "").split())
    if not text:
        return None
    m = _YEAR_FIRST.fullmatch(text)
    if m:
        day = _day(int(m.group(1)), int(m.group(2)), int(m.group(3)))
        return ("date", day) if day else None
    m = _DIGIT_DATE.fullmatch(text)
    if m:
        p1, p2, y = int(m.group(1)), int(m.group(2)), _year(m.group(3))
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
            return ("ambiguous", (p1, p2, y), "p/p/y")
        return ("date", day) if day else None
    for pattern, (di, mi, yi) in ((_DAY_NAMED, (1, 2, 3)), (_NAMED_DAY, (2, 1, 3))):
        m = pattern.fullmatch(text)
        if m:
            month = _MONTHS.get(m.group(mi).lower()[:3])
            if not month:
                continue
            day = _day(_year(m.group(yi)), month, int(m.group(di)))
            return ("date", day) if day else None
    return None


def _readings(ambiguous):
    p1, p2, y = ambiguous[1]
    return {r for r in (_day(y, p2, p1), _day(y, p1, p2)) if r}


def _same_date(da, db):
    if da[0] == "date" and db[0] == "date":
        return da[1] == db[1]
    if da[0] == "ambiguous" and db[0] == "ambiguous":
        return da[1] == db[1]
    ambiguous, decided = (da, db) if da[0] == "ambiguous" else (db, da)
    return None if decided[1] in _readings(ambiguous) else False


def text_key(value):
    """A value as text, case, spaces and punctuation aside. A short code in brackets at the end - "Karnataka (29)" -
    names the same thing as the bare name: an option's label often carries one (found live 2026-10-26, a right form
    was stopped once over it)."""

    return re.sub(r"[^a-z0-9]", "", re.sub(r"\s*\([^()]{1,12}\)\s*$", "", str(value or "")).lower())


def _national(value):
    """What follows a leading country code written in the E.164 shape, or None when the value has none."""

    if not isinstance(value, str):
        return None
    m = _COUNTRY_CODE.match(value)
    return value[m.end():] if m else None


def same_value(a, b, order=None):
    """
    Whether two values of one field are the same whatever their form: True, False, or None when what is known cannot
    decide it (an ambiguous digit date against a decided one that is one of its readings). `order` is the date order
    known for the task ("day", "month" or None).
    """

    order = _order(order)
    da, db = parse_date(a, order), parse_date(b, order)
    if da or db:
        return _same_date(da, db) if da and db else False
    ca, cb = _national(a), _national(b)
    if (ca is None) != (cb is None):
        return same_value(a if ca is None else ca, b if cb is None else cb, order)
    na, nb = parse_number(a), parse_number(b)
    if na is not None and nb is not None:
        return na == nb
    ka, kb = text_key(a), text_key(b)
    return bool(ka) and ka == kb


def as_iso(value, order=None):
    """The day a value names as YYYY-MM-DD when it is decided, else None."""

    found = parse_date(value, order)
    if not found or found[0] != "date":
        return None
    y, m, d = found[1]
    return f"{y:04d}-{m:02d}-{d:02d}"
