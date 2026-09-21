import re
from datetime import date


# ============================================================
# Volatile text
#
# Ported from openadapt-flow's volatility classifier (openadapt_flow/
# volatility.py), standard library only like the original. A replay checks
# each step against what that step left on the screen when it was recorded,
# and some of that text can never read the same way twice: a clock, "3 min
# ago", a date near the day of the recording, "2 of 9", "Inbox (3)". Asserted
# verbatim it fails a correct replay the moment the minute changes -
# openadapt-flow mined a bare ":01" off an OpenEMR screen, and every later
# replay halted on it. Classified here, it is left out of what a replay
# checks, and out of what an action reports as having changed (the XFCE
# panel clock ticks underneath every other action).
#
# Deliberately asymmetric about dates, as the original is: a date near the
# recording date is chronology and volatile; a date far from it is identity
# data (a date of birth, a contract date) and stays usable.
# ============================================================

# Clock times, including the bare ":01" fragment that slipped past a combined
# pattern upstream.
CLOCK_RE = re.compile(
    r"(?<!\d)\d{1,2}:\d{2}(?::\d{2})?(?!\d)"
    r"|(?<![\w:]):\d{2}(?!\d)"
)

# Dot-separated clocks ("18.38") only where they cannot be a version number.
DOT_CLOCK_RE = re.compile(
    r"(?<![\w.])(?:[01]\d|2[0-3])\.[0-5]\d(?![\d.])"
    r"|(?<![\w.])(?:[01]?\d|2[0-3])\.[0-5]\d\s*(?:am|pm)\b"
    r"|\b(?:at|updated?|update[ds]|time|kl\.?|klo)\s+"
    r"(?:[01]?\d|2[0-3])\.[0-5]\d(?![\d.])",
    re.IGNORECASE,
)

RELATIVE_TIME_RE = re.compile(
    r"(?<!\d)\d+\s*"
    r"(?:sec(?:ond)?s?|min(?:ute)?s?|h(?:ou)?rs?|days?|w(?:ee)?ks?"
    r"|months?|mos?|y(?:ea)?rs?|[smhdw])"
    r"\s*ago(?![a-z])"
    r"|\bjust\s*now\b"
    r"|\bmoments?\s*ago\b",
    re.IGNORECASE,
)

# Volatile only standalone: "Yesterday" as a group header, not "Today's
# Appointments" as a label.
_RELATIVE_DAY_WORDS = frozenset({"today", "yesterday", "tomorrow", "now"})

_COUNT_NOUNS = (
    r"(?:entr(?:y|ies)|results?|records?|rows?|items?|matches"
    r"|messages?|notifications?|unread)"
)
COUNT_RE = re.compile(
    r"(?<!\d)\d[\d,]*\s*(?:to\s*\d[\d,]*\s*)?of\s*\d[\d,]*(?!\d)"
    r"|(?<!\d)\d[\d,]*\s*(?:(?:total|new|unread|more)\s*)?"
    + _COUNT_NOUNS
    + r"(?![a-z\d])"
    r"|\bpage\s*\d+(?!\d)",
    re.IGNORECASE,
)

# A live counter decorating a stable label: "Inbox (2)".
COUNTER_PAREN_RE = re.compile(r"\(\s*\d+\s*\)")

DATE_RE = re.compile(
    r"(?<!\d)\d{4}[-/.]\d{1,2}[-/.]\d{1,2}(?!\d)"
    r"|(?<!\d)\d{1,2}[-/.]\d{1,2}[-/.]\d{2,4}(?!\d)"
)

_MONTH_PAT = (
    r"(?:jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?"
    r"|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?"
    r"|nov(?:ember)?|dec(?:ember)?)"
)
_WEEKDAY_PAT = r"(?:mon|tues?|wed(?:nes)?|thu(?:rs?)?|fri|sat(?:ur)?|sun)(?:day)?"

MONTH_DATE_RE = re.compile(
    rf"\b(?:{_WEEKDAY_PAT}[,\s]*)?"
    rf"(?:(?<!\d)(?P<d1>\d{{1,2}})(?:st|nd|rd|th)?[\s.]*)?"
    rf"(?P<mon>{_MONTH_PAT})\.?"
    rf"(?:\s*(?P<d2>\d{{1,2}})(?:st|nd|rd|th)?)?"
    rf"(?:,?\s*(?P<yr>\d{{4}}))?\b",
    re.IGNORECASE,
)

WEEKDAY_RE = re.compile(rf"\b{_WEEKDAY_PAT}\b|\b(?:am|pm)\b", re.IGNORECASE)

# Within this many days of the recording, a date is chronology.
DATE_WINDOW_DAYS = 366

MIN_SQUASHED_LEN = 4
MIN_ALPHA_CHARS = 3
MIN_DISTINCT_CHARS = 3

_MONTH_NUMBERS = {
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
    "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}


def _squash(text):
    return "".join(text.lower().split())


def _candidate_dates(fragment):
    """Every calendar date a numeric fragment could mean (D/M/Y and M/D/Y)."""

    numbers = [int(n) for n in re.findall(r"\d+", fragment)]
    if len(numbers) != 3:
        return []

    if numbers[0] >= 1000:
        y, a, b = numbers
        orders = [(y, a, b), (y, b, a)]
    else:
        a, b, y = numbers
        if y < 100:
            y += 2000
        orders = [(y, b, a), (y, a, b)]

    dates = []
    for y, m, d in orders:
        try:
            dates.append(date(y, m, d))
        except ValueError:
            continue
    return dates


def _month_fragments(text):
    """Month-name dates with a day or a year attached - "May I help" is not one."""

    return [
        m for m in MONTH_DATE_RE.finditer(text)
        if m.group("d1") or m.group("d2") or m.group("yr")
    ]


def _month_fragment_near(match, reference):
    month = _MONTH_NUMBERS[match.group("mon").lower()[:3]]
    day = match.group("d1") or match.group("d2")
    year = match.group("yr")

    # A month and day with no year comes round every year: chronology.
    if year is None:
        return True

    try:
        if day is not None:
            nearest = abs((date(int(year), month, int(day)) - reference).days)
        else:
            first, last = date(int(year), month, 1), date(int(year), month, 28)
            if first <= reference <= last:
                nearest = 0
            else:
                nearest = min(abs((first - reference).days), abs((last - reference).days))
    except ValueError:
        return True

    return nearest <= DATE_WINDOW_DAYS


def has_date(text):
    return bool(DATE_RE.search(text)) or bool(_month_fragments(text))


def dates_near(text, reference):
    """
    None when text has no date; True when any date in it is near `reference`
    (or cannot be read - unreadable counts as volatile); False when all are far.
    """

    found = False

    for match in DATE_RE.finditer(text):
        found = True
        candidates = _candidate_dates(match.group(0))
        if not candidates:
            return True
        if min(abs((d - reference).days) for d in candidates) <= DATE_WINDOW_DAYS:
            return True

    for match in _month_fragments(text):
        found = True
        if _month_fragment_near(match, reference):
            return True

    return False if found else None


def _residual(squashed):
    if sum(1 for c in squashed if c.isalpha()) < MIN_ALPHA_CHARS:
        return "digit_dominated"
    if len(set(squashed)) < MIN_DISTINCT_CHARS:
        return "low_entropy"
    return None


def classify(text, reference=None):
    """
    Why text would make a poor thing to check on a replay, or None when it is
    stable. Reasons: too_short, clock_time, relative_time, date (no reference
    date to judge it by), near_date, count, counter, digit_dominated,
    low_entropy.
    """

    text = text or ""
    squashed = _squash(text)

    if len(squashed) < MIN_SQUASHED_LEN:
        return "too_short"
    if CLOCK_RE.search(text) or DOT_CLOCK_RE.search(text):
        return "clock_time"
    if RELATIVE_TIME_RE.search(text) or squashed.strip(".,:;!?") in _RELATIVE_DAY_WORDS:
        return "relative_time"
    if has_date(text):
        if reference is None:
            return "date"
        if dates_near(text, reference):
            return "near_date"
    if COUNT_RE.search(text):
        return "count"
    if COUNTER_PAREN_RE.search(text):
        stripped = COUNTER_PAREN_RE.sub(" ", text).strip()
        if classify(stripped, reference) == _residual(squashed):
            return "counter"
    return _residual(squashed)


def without_counters(text):
    """ "Inbox (3) - Mail" -> "Inbox - Mail": the label a counter decorates."""

    return " ".join(COUNTER_PAREN_RE.sub(" ", text or "").split())


def only_a_moment(text):
    """
    Whether text is nothing but a time or a date ("10:42", "Mon 14 Sep,
    10:42") - what a clock shows, so a change in it is not something an
    action did.
    """

    if not text or not text.strip():
        return False

    if not (CLOCK_RE.search(text) or DOT_CLOCK_RE.search(text) or has_date(text)
            or RELATIVE_TIME_RE.search(text)):
        return False

    rest = text
    for pattern in (CLOCK_RE, DOT_CLOCK_RE, DATE_RE, MONTH_DATE_RE, RELATIVE_TIME_RE, WEEKDAY_RE):
        rest = pattern.sub(" ", rest)

    return not re.search(r"[A-Za-z0-9]", rest)
