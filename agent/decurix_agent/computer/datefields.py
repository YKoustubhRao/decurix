"""
Native date and time inputs (<input type=date|datetime-local|time|month|week>) in a web page.

Such a field shows its value in the browser's own order (mm/dd/yyyy here) split into Month/Day/Year
segments, but holds it in one fixed format. Typed key by key, "2026-10-06" lands in the wrong segments;
so a value is put in the way the field's own picker does it - through the input's value - and only once
it is certain which date was meant. 06/10/2026 is 6 October or 10 June: such a date is refused and the
engine is asked for YYYY-MM-DD, never guessed (the same rule TheBot's grounding keeps for typed dates).
"""

import datetime as _dt
import re

from decurix_agent.computer.errors import ComputerError

# What each kind of input holds, whatever it shows.
FORMATS = {"date": "YYYY-MM-DD", "datetime-local": "YYYY-MM-DDTHH:MM", "time": "HH:MM", "month": "YYYY-MM", "week": "YYYY-Www"}

_MONTH_NAMES = [("jan", "january"), ("feb", "february"), ("mar", "march"), ("apr", "april"), ("may",), ("jun", "june"),
                ("jul", "july"), ("aug", "august"), ("sep", "sept", "september"), ("oct", "october"), ("nov", "november"),
                ("dec", "december")]
MONTHS = {name: n for n, names in enumerate(_MONTH_NAMES, 1) for name in names}
FULL = ["", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November",
        "December"]


def _ymd(raw, y, m, d):
    try:
        return _dt.date(int(y), int(m), int(d)).isoformat()
    except ValueError:
        raise ComputerError(f"{raw!r} is not a real date") from None


def _clock(raw, rest):
    rest = rest.strip()
    if not rest:
        return None
    mt = re.fullmatch(r"(\d{1,2})(?::(\d{2}))?(?::\d{2})?\s*(am|pm|a\.m\.|p\.m\.)?", rest)
    if not mt:
        raise ComputerError(f"{raw!r}: the time part is not a time (use HH:MM)")
    h, mi, half = int(mt.group(1)), int(mt.group(2) or 0), (mt.group(3) or "")[:1]
    if half == "p" and h < 12:
        h += 12
    if half == "a" and h == 12:
        h = 0
    if h > 23 or mi > 59:
        raise ComputerError(f"{raw!r} is not a real time")
    return f"{h:02d}:{mi:02d}"


def _date(raw, part):
    mt = re.fullmatch(r"(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})", part)
    if mt:
        return _ymd(raw, *mt.groups())
    mt = re.fullmatch(r"(\d{1,2})(?:st|nd|rd|th)?[-/. ]+([a-z]+)\.?[-/. ]+(\d{4})", part)      # 6 Oct 2026, 06-Oct-2026
    if mt and mt.group(2) in MONTHS:
        return _ymd(raw, mt.group(3), MONTHS[mt.group(2)], mt.group(1))
    mt = re.fullmatch(r"([a-z]+)\.?\s+(\d{1,2})(?:st|nd|rd|th)?\s+(\d{4})", part)               # October 6 2026
    if mt and mt.group(1) in MONTHS:
        return _ymd(raw, mt.group(3), MONTHS[mt.group(1)], mt.group(2))
    mt = re.fullmatch(r"(\d{1,2})[-/.](\d{1,2})[-/.](\d{4})", part)
    if mt:
        a, b, y = int(mt.group(1)), int(mt.group(2)), mt.group(3)
        if a > 12 >= b:
            return _ymd(raw, y, b, a)
        if b > 12 >= a or a == b:
            return _ymd(raw, y, a, b)
        raise ComputerError(f"{raw!r} could be day/month or month/day: type the date as YYYY-MM-DD "
                            f"({y}-{b:02d}-{a:02d} for {a} {FULL[b]}, {y}-{a:02d}-{b:02d} for {b} {FULL[a]})")
    return None


def value_for(kind, text):
    """The value a `kind` input takes for `text`, in FORMATS[kind]; ComputerError when unsure or not a date."""

    raw = (text or "").strip()
    if not raw:
        return ""
    t = re.sub(r"\s+", " ", raw.lower().replace(",", " ")).strip()
    if kind == "time":
        return _clock(raw, t)
    if kind == "month":
        mt = re.fullmatch(r"(\d{4})-(\d{1,2})", t)
        if mt:
            return f"{int(mt.group(1)):04d}-{int(mt.group(2)):02d}"
        mt = re.fullmatch(r"([a-z]+)\.? (\d{4})", t)
        if mt and mt.group(1) in MONTHS:
            return f"{mt.group(2)}-{MONTHS[mt.group(1)]:02d}"
        raise ComputerError(f"{raw!r} is not a month: type it as YYYY-MM")
    if kind == "week":
        mt = re.fullmatch(r"(\d{4})-?w(\d{1,2})", t)
        if mt:
            return f"{mt.group(1)}-W{int(mt.group(2)):02d}"
        raise ComputerError(f"{raw!r} is not a week: type it as YYYY-Www")
    date_part, time_part = t, ""
    mt = re.fullmatch(r"(.+?)(?:t| )(\d{1,2}:\d{2}.*)", t)
    if mt:
        date_part, time_part = mt.group(1).strip(), mt.group(2)
    value = _date(raw, date_part)
    if value is None:
        raise ComputerError(f"{raw!r} is not a date this field can take: type it as YYYY-MM-DD")
    if kind == "datetime-local":
        hhmm = _clock(raw, time_part)
        if not hhmm:
            raise ComputerError(f"{raw!r}: this field takes a date and a time - type it as YYYY-MM-DD HH:MM")
        return f"{value}T{hhmm}"
    if time_part:
        raise ComputerError(f"{raw!r}: this field takes a date only")
    return value


SET_VALUE = """function(v) {
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    set.call(this, v);
    this.dispatchEvent(new Event('input', {bubbles: true}));
    this.dispatchEvent(new Event('change', {bubbles: true}));
    return this.value;
}"""


def put(page, object_id, kind, text):
    """Put `text` into the date/time input `object_id` through its value, as its picker does; check it held."""

    value = value_for(kind, text)
    held = page.call("Runtime.callFunctionOn", {"objectId": object_id, "returnByValue": True, "functionDeclaration": SET_VALUE,
                                               "arguments": [{"value": value}]}).get("result", {}).get("value")
    if held != value:
        raise ComputerError(f"the {kind} field did not take {value!r} (it holds {held!r}) - the page may not allow that "
                            f"value (outside the field's earliest or latest date)")
