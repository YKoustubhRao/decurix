"""
Provenance (harness audit, 2026-10-21): a value written or typed is real only when a source holds it - the request,
its input files, a person's message, the system's own pages as the tools read them - or the work's own recorded
computation (its output files). One normaliser decides "the same value" everywhere: dates in any common form, figures
with grouping, currency, units (lakh, crore, thousand), signs and percentages, identifiers kept as written.

Nothing here knows an application, a product or a business rule: values, texts and their forms.
"""

import re
from decimal import Decimal, InvalidOperation

_NUMBER = re.compile(r"(?<![\w.])(\()?(-)?\s?(?:(?:Rs\.?|INR|USD|EUR|GBP|₹|\$|€|£)\s?)?"
                     r"(\d{1,3}(?:,\d{2,3})+(?:\.\d+)?|\d+(?:\.\d+)?)(\))?"
                     r"\s?(lakhs?|lacs?|crores?|cr|bn|billions?|mn|millions?|thousands?|k|m|l)?(?![\w])", re.I)
_SCALES = {"lakh": 10**5, "lakhs": 10**5, "lac": 10**5, "lacs": 10**5, "l": 10**5, "crore": 10**7, "crores": 10**7,
           "cr": 10**7, "k": 10**3, "m": 10**6, "mn": 10**6, "million": 10**6, "bn": 10**9, "billion": 10**9,
           "millions": 10**6, "billions": 10**9, "thousand": 10**3, "thousands": 10**3}


def parse_number(text):
    """(Decimal value, Decimal step) of the first figure in text, or None. Step is how precisely it is written."""

    match = _NUMBER.search(text or "")
    if not match:
        return None
    paren, minus, digits, close, scale = match.groups()
    try:
        value = Decimal(digits.replace(",", ""))
    except InvalidOperation:
        return None
    decimals = len(digits.split(".")[1]) if "." in digits else 0
    factor = Decimal(_SCALES.get((scale or "").lower(), 1))
    step = Decimal(1).scaleb(-decimals) * factor
    value *= factor
    if minus or (paren and close):
        value = -value
    return value, step


def same_figure(a, b):
    """Two written figures agree when they are equal at the coarser of the two precisions."""

    (va, sa), (vb, sb) = a, b
    return abs(va - vb) <= max(sa, sb) / 2 + Decimal("0.005")


_MONTHS = {m: n for n, m in enumerate(("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov",
                                       "dec"), 1)}
_NAMED_DATE = re.compile(r"(?i)^\s*(\d{1,2})(?:st|nd|rd|th)?[\s\-./]+([a-z]{3})[a-z]*\.?[\s\-./,]+(\d{4})\s*$")
_NAMED_DATE_US = re.compile(r"(?i)^\s*([a-z]{3})[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\s*$")
_ISO_DATE = re.compile(r"^\s*(\d{4})-(\d{1,2})-(\d{1,2})(?:[ T][\d:.]+)?\s*$")
_NUM_DATE = re.compile(r"^\s*(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})\s*$")
_MONEY_WORDS = re.compile(r"(?i)\b(?:rs|inr|usd|eur|gbp|rupees?)\b\.?|[₹$€£]")


def fact_value(text, day_first=None, unit=1):
    """A figure as what it stands for: ("date", (y, m, d)), ("number", Decimal), or ("text", folded text)."""

    import datetime
    raw = " ".join(str(text or "").split())
    for pattern, order in ((_NAMED_DATE, "dmy"), (_NAMED_DATE_US, "mdy")):
        m = pattern.match(raw)
        if m:
            parts = dict(zip(order, m.groups()))
            month = _MONTHS.get(parts["m"][:3].lower())
            if month:
                try:
                    return ("date", datetime.date(int(parts["y"]), month, int(parts["d"])).timetuple()[:3])
                except ValueError:
                    pass
    m = _ISO_DATE.match(raw)
    if m:
        try:
            return ("date", datetime.date(int(m.group(1)), int(m.group(2)), int(m.group(3))).timetuple()[:3])
        except ValueError:
            pass
    m = _NUM_DATE.match(raw)
    if m:
        a, b, y = int(m.group(1)), int(m.group(2)), int(m.group(3))
        if a > 12 or (a <= 12 and b <= 12 and day_first is True):
            d, mo = a, b
        elif b > 12 or day_first is False:
            d, mo = b, a
        else:
            return ("text", raw.lower())  # 02/11/2026 with no convention: compared as written
        try:
            return ("date", datetime.date(y, mo, d).timetuple()[:3])
        except ValueError:
            return ("text", raw.lower())
    bare = _MONEY_WORDS.sub(" ", raw).strip().replace("−", "-")
    # A percentage, however written: 11.55%, "11.55 percent", "11.55 per cent", "11.55 pct" (round 5 P5, 2026-10-20:
    # "F4=11.55" in a Variance % column against "(+11.55%)" was reported as not agreeing).
    pct = bool(re.search(r"(?i)%|\bper ?cent\b|\bpct\b", bare))
    if pct:
        bare = re.sub(r"(?i)\s*(?:%|\bper ?cent\b|\bpct\b\.?)", "", bare).strip()
    found = parse_number(bare) if re.fullmatch(r"[+(]{0,2}-?\s?[\d,]+(?:\.\d+)?\)?\s*(?:lakhs?|lacs?|l|crores?|cr|"
                                               r"thousands?|k|mn|bn|millions?|billions?)?\.?", bare, re.I) else None
    if found:
        value, step = found
        applied = 1
        whole = re.sub(r"\D", "", re.split(r"\.(?=\d+\)?\.?$)", bare)[0])
        if unit != 1 and not pct and len(whole) < 5 and not re.search(
                r"(?i)[\d.]\s*(?:lakhs?|lacs?|l|crores?|cr|thousands?|k|mn|bn|millions?|billions?)\.?$", bare):
            # A bare figure in a document that declares its unit ("Figures in Rs lakh"): read in that unit - unless it
            # is written in full (five or more whole digits: "Rs 593,180.00" is rupees, never 593,180 lakh). Found live
            # (round 5 P5 on pc3, 2026-10-21): three customer contributions written alike in the memo and the deck were
            # reported as not agreeing, because the deck's "Figures in Rs lakh" scaled them.
            value, step, applied = value * unit, step * unit, unit
        # The value, and how finely it is written: a rounded figure equals the precise one within its own rounding.
        # (Kept beside it: whether it is written as a percentage, and the declared unit applied to it.)
        return ("number", (value, step), {"pct": pct, "unit": applied})
    return ("text", raw.lower().strip(" .,:;"))


# A document that says in what unit its figures are written: "Figures in Rs lakh", "(Rs crore)", "Amounts in thousands".
_DECLARED_UNIT = re.compile(r"(?i)(?:figures|amounts|values|numbers|all)\s+(?:are\s+)?in\s+(?:rs\.?\s*|₹\s*|inr\s*)?"
                            r"(lakhs?|lacs?|crores?|thousands?|millions?)\b|\((?:rs\.?|₹|inr)\s*(lakhs?|lacs?|crores?|"
                            r"thousands?|millions?)\)")
_UNIT_FACTORS = {"lakh": 10**5, "lac": 10**5, "crore": 10**7, "thousand": 10**3, "million": 10**6}


def declared_unit(text):
    """The factor of the unit a document declares for its figures (100000 for "Figures in Rs lakh"), else 1."""

    m = _DECLARED_UNIT.search(text or "")
    if not m:
        return 1
    word = (m.group(1) or m.group(2)).lower().rstrip("s")
    return Decimal(_UNIT_FACTORS.get(word, 1))


def _readings(fact, percent):
    """The ways a number may be read: as written; for a percentage comparison also without a declared unit (a bare
    11.55 in a "Figures in Rs lakh" deck's % column), and a bare fraction as a percentage (0.1155 = 11.55%)."""

    value, step = fact[1]
    meta = fact[2] if len(fact) > 2 else {}
    out = [(value, step)]
    if percent:
        unit = Decimal(meta.get("unit") or 1)
        if unit != 1:
            out.append((value / unit, step / unit))
        if not meta.get("pct"):
            out += [(v * 100, s * 100) for v, s in list(out) if abs(v) <= Decimal("1.5")]
    return out


def same_fact(a, b, percent=False):
    """
    Whether two figures are the same: the same date, the same number (to the coarser precision), the same text.
    Percentages (`percent`: the figure is one, or either side is written with %) are compared on one scale.
    """

    if a[0] != b[0]:
        return False
    if a[0] == "number":
        percent = percent or any(len(f) > 2 and f[2].get("pct") for f in (a, b))
        for va, sa in _readings(a, percent):
            for vb, sb in _readings(b, percent):
                # One the other rounded is the same figure: within half the coarser one's last place. A variance
                # written as "+5.3 lakh" in one place and "an adverse/favourable variance of 531,169" in another
                # carries its sign in words: the amounts are compared (the signs are figure_sign's).
                tolerance = max(sa, sb) / 2 + Decimal("0.005")
                if abs(va - vb) <= tolerance or abs(abs(va) - abs(vb)) <= tolerance:
                    return True
        return False
    return a[1] == b[1]


# A file name at the end of a value ("1015_E02.eml"), or one token joining letters and digits with _ - / (SO-5102,
# 1015_E02, HH/PO/7745, INV-2026-001): a name or a code, never a figure.
_FILE_END = re.compile(r"[^\s/\\]\.[A-Za-z][A-Za-z0-9]{1,4}$")


def identifier_like(value, day_first=None):
    """
    Whether a value is an identifier (a file name, an order/invoice/PO number, an ID, a code), not a figure. Found live
    (round 5 P6, 2026-10-20): "1015_E02.eml" in two workbooks and "1015_E02" in a memo were reported as "Figures that do
    not agree", and a finished run as not done.
    """

    v = " ".join(str(value or "").split()).strip(" '\"`.,;:()[]")
    if not v:
        return False
    if _FILE_END.search(v) and not re.fullmatch(r"[\d.,]+", v):
        return True
    token = v.lstrip("+-\u2212")
    if " " in token or not (re.search(r"[A-Za-z]", token) and re.search(r"\d", token)):
        return False
    if not re.search(r"[_/-]", token):
        return False
    return fact_value(v, day_first)[0] != "date"


# ------------------------------------------------------------ the facts a text states, and whether a source holds them
#
# What blocks (independent review, 2026-10-21): only what code decides with certainty - an amount, a long identifier
# (by its digits) or an address that no source holds and no single arithmetic step from the figures at hand explains.
# A date ("Pay by 24 Oct 2026" - today plus three) and a token joining letters and digits ("FY27", "2FA", "Q3", "A4")
# are often the work's own, rightly: they are evidence (`doubtful`), never a verdict.

_URL = re.compile(r"(?i)\bhttps?://\S+|\bwww\.\S+|(?:[A-Za-z]:)?(?:[\\/][\w.~ -]+){2,}")
_EMAIL = re.compile(r"[\w.+-]+@[\w-]+(?:\.[\w-]+)+")
_DATE_IN_TEXT = re.compile(
    r"(?i)\b\d{1,2}(?:st|nd|rd|th)?[\s\-./]+(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?[\s\-./,]+\d{4}\b"
    r"|\b(?:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+\d{1,2}(?:st|nd|rd|th)?,?\s+\d{4}\b"
    r"|\b\d{4}-\d{1,2}-\d{1,2}\b|\b\d{1,2}[/.\-]\d{1,2}[/.\-]\d{4}\b")
# A token of letters and digits, maybe joined by - / _ ("SO-5101", "HH/PO/7781", "FY27", "EMP2041").
_CODE = re.compile(r"\b[A-Za-z0-9]+(?:[-/_][A-Za-z0-9]+){0,4}\b")
# A long number written as an identifier: a phone, an account ("+91 98765 43210", "022-2345-6789", "9876543210").
_IDENT = re.compile(r"(?<![\w.,])(?:\+\d{1,3}[ \-]?(?:\(?\d+\)?[ \-]?){1,5}\d|\d{2,6}(?:-\d{2,8}){1,4}|"
                    r"\d{3,6}(?: \d{3,6}){1,3}|\d{7,})(?![\w,]|\.\d)")
_TIME = re.compile(r"(?i)\b\d{1,2}[:.]\d{2}(?:[:.]\d{2})?\s*(?:am|pm)?\b|\b\d{1,2}\s*(?:am|pm)\b")
_PERCENT = re.compile(r"(?i)[-+]?\d[\d,]*(?:\.\d+)?\s*(?:%|per ?cent\b|pct\b)")
_ORDINAL = re.compile(r"(?i)\b\d+(?:st|nd|rd|th)\b")
# Kinds that block when no source holds them; the others are evidence only.
BLOCKING_KINDS = ("email", "ident", "number")


def _is_code(token):
    return len(token) >= 2 and bool(re.search(r"[A-Za-z]", token)) and bool(re.search(r"\d", token))


def _is_ident(token):
    digits = re.sub(r"\D", "", token)
    if len(digits) < 7:
        return False
    # (Space-separated groups are an identifier only in a phone's shape: 10 to 13 digits in all.)
    if " " in token.strip() and not token.strip().startswith("+") and not 10 <= len(digits) <= 13:
        return False
    return True


def _squash(text):
    return re.sub(r"[^a-z0-9]+", "", str(text or "").lower())


def date_readings(written):
    """The (y, m, d) readings of one written date: an ambiguous 02/11/2026 is both 2 Nov and 11 Feb; none for a text
    that is no date."""

    out = set()
    for day_first in (True, False):
        value = fact_value(written, day_first)
        if value[0] == "date":
            out.add(value[1])
    return out


def _dates(text):
    """Every reading of every date a text writes: an ambiguous 02/11/2026 is both 2 Nov and 11 Feb."""

    out = set()
    for m in _DATE_IN_TEXT.finditer(text or ""):
        out |= date_readings(m.group(0))
    return out


def _significant(value):
    """How many whole digits a figure has (1,800.00 and 2100 have 4; 35 and 7.5 have 2 and 1): small counts are not
    looked for, being everywhere."""

    whole = int(abs(Decimal(value)))
    return len(str(whole)) if whole else 0


def facts_in(text):
    """
    [(kind, as written)] of the facts a text states that a source must hold: "email" addresses, "date"s, "code"s of
    letters and digits, "ident"ifiers (a long number however grouped: a phone, an account) and "number"s of 100 or
    more. Web addresses and paths, times, percentages and ordinals are left out (a time or a share is no record's
    value; small counts are everywhere).
    """

    text = _URL.sub(" ", str(text or ""))
    out = []
    for m in _EMAIL.finditer(text):
        out.append(("email", m.group(0)))
    text = _EMAIL.sub(" ", text)
    for m in _DATE_IN_TEXT.finditer(text):
        out.append(("date", m.group(0)))
    text = _DATE_IN_TEXT.sub(" ", text)
    text = _TIME.sub(" ", _PERCENT.sub(" ", _ORDINAL.sub(" ", text)))
    for m in _CODE.finditer(text):
        if _is_code(m.group(0)):
            out.append(("code", m.group(0)))
    text = _CODE.sub(lambda m: " " if _is_code(m.group(0)) else m.group(0), text)
    for m in _IDENT.finditer(text):
        if _is_ident(m.group(0)):
            out.append(("ident", m.group(0).strip()))
    text = _IDENT.sub(lambda m: " " if _is_ident(m.group(0)) else m.group(0), text)
    for m in _NUMBER.finditer(text):
        found = parse_number(m.group(0))
        if found and _significant(found[0]) >= 3:
            out.append(("number", m.group(0).strip().strip("()").strip()))
    return out


def _figures(text):
    """Every figure a text writes, small ones and percentages too (as Decimal amounts): the operands of a step."""

    out = []
    for m in _NUMBER.finditer(_URL.sub(" ", str(text or ""))):
        found = parse_number(m.group(0))
        if found:
            out.append(abs(found[0]))
    return out


IMPLIED = {Decimal(n) for n in (2, 4, 7, 12, 24, 52, 365)}


def derived(value, operands, pool):
    """
    Whether `value` (parse_number's (amount, step)) is ONE arithmetic step from the figures at hand: a sum, a
    difference, a product, a quotient or a percentage of an operand the text itself states (`operands`) and a figure of
    the text or the sources (`pool`, a sorted list). "Total 13,500 for 3 nights" with 4,500 a night in the source is
    held; an amount no single step explains is not. Identity steps (+0, x1, /1, 100%) explain nothing.
    """

    import bisect
    v, step = abs(value[0]), value[1]
    tolerance = max(step, Decimal("0.01")) / 2 + Decimal("0.005")
    # Units every reader knows without a source writing them: a dozen months, a week's days, a year's weeks and days.
    pool = sorted(set(pool) | IMPLIED)
    if not pool:
        return False

    def near(candidate):
        i = bisect.bisect_left(pool, candidate)
        return [pool[j] for j in (i - 1, i, i + 1) if 0 <= j < len(pool)]

    for a in set(operands):
        if a == 0:
            continue
        steps = [(v - a, lambda b: a + b), (a - v, lambda b: a - b), (v + a, lambda b: b - a)]
        if a != 1:
            steps += [(v / a, lambda b: a * b), (a / v if v else None, lambda b: a / b if b else None),
                      (v * a, lambda b: b / a)]
        if a != 100:
            steps.append((v * 100 / a, lambda b: a * b / 100))
        # A figure with a percentage added or taken off ("12,000 with 18% tax is 14,160", "10% off").
        steps += [((v / a - 1) * 100, lambda b: a * (1 + b / 100)), ((1 - v / a) * 100, lambda b: a * (1 - b / 100))]
        for candidate, forward in steps:
            if candidate is None or candidate <= 0:
                continue
            for b in near(candidate):
                if b in (0, 1) or (b == 100 and forward(b) == a):
                    continue
                result = forward(b)
                if result is not None and abs(result - v) <= tolerance:
                    return True
    return False


_DIGIT_RUN = re.compile(r"\+?\d(?:[-. ()]{1,2}\d|\d)*")


def _id_forms(written):
    """The digit strings an identifier as written may stand for: all its digits, and - only where the writing marks a
    prefix - without a country code after "+" (1 to 3 digits) or a trunk "0". Never a cut that the writing does not
    show (review 2026-11-03, E-28: up to three leading digits were stripped from any number)."""

    text = str(written or "").strip()
    digits = re.sub(r"\D", "", text)
    if len(digits) < 7:
        return {digits} if digits else set()
    forms = {digits}
    if text.startswith("+"):
        forms |= {digits[k:] for k in range(1, 4) if len(digits) - k >= 7}
    elif digits.startswith("0") and len(digits) >= 8:
        forms.add(digits[1:])
    return forms


class Sources:
    """
    What the sources of a piece of work hold, put in one form: the request, its input files and messages, the system's
    own pages as the tools read them, and the work's recorded computation (its output files). A fact is held when one
    of them states it - by meaning: a date in any form, a figure at its written precision (grouping, currency, lakh),
    an identifier by its digits (a phone with spaces, a country prefix), a code or an address by its letters and
    digits - or, for a figure, when one arithmetic step from the figures at hand gives it (`derived`).
    """

    def __init__(self, texts=()):
        texts = [str(t) for t in texts if t]
        joined = "\n".join(texts)
        self.flat = " ".join(joined.lower().split())
        self.squashed = _squash(joined)
        # Each identifier a source writes, as its digits: one run of digits with at most two of " -.()" between them,
        # never digits joined across cells or words (review 2026-11-03, E-28: "Qty 12 Rate 3456 Total 789" held
        # "2345678"), in every form its prefix allows (_id_forms).
        self.idents = set()
        for m in _DIGIT_RUN.finditer(joined):
            self.idents |= _id_forms(m.group(0))
        self.emails = {e.lower() for e in _EMAIL.findall(joined)}
        self.dates = _dates(joined)
        self.numbers = []
        for m in _NUMBER.finditer(joined):
            found = parse_number(m.group(0))
            if found:
                self.numbers.append(found)
        self.pool = sorted({abs(n[0]) for n in self.numbers})

    def add_date(self, date):
        """Hold a date known without a text (the day the work is done)."""

        self.dates.add(tuple(date.timetuple()[:3]))

    def _held_digits(self, written):
        """Whether a source writes this identifier: the same digits, a whole run of them, with or without a prefix
        that is written as one (a "+" country code, a trunk "0") - never a part of a longer number."""

        return bool(_id_forms(written) & self.idents)

    def holds(self, kind, written):
        """Whether a source states this fact (kind and text as facts_in gives them)."""

        if kind == "email":
            return written.lower() in self.emails or written.lower() in self.flat
        if kind == "date":
            return bool(date_readings(written) & self.dates) or " ".join(written.lower().split()) in self.flat
        if kind == "code":
            return bool(_squash(written)) and _squash(written) in self.squashed
        if kind == "ident":
            return self._held_digits(written) or self.holds("number", written)
        if kind == "number":
            found = parse_number(written)
            if not found:
                return True
            # (By amount: "(Rs 35,960)" in brackets reads as negative; a sign is the cross-check's, not provenance's.)
            if any(same_figure((abs(found[0]), found[1]), (abs(n[0]), n[1])) for n in self.numbers):
                return True
            digits = re.sub(r"\D", "", written)
            return len(digits) >= 7 and self._held_digits(written)
        return True

    def _missing(self, text, operands=()):
        facts = facts_in(text)
        operands = _figures(text) + list(operands)
        pool = sorted(set(self.pool) | set(operands))
        out = []
        for kind, written in facts:
            if self.holds(kind, written):
                continue
            if kind in ("number", "ident"):
                value = parse_number(written)
                if value and derived(value, operands, pool):
                    continue
            out.append((kind, written))
        return out

    def unsourced(self, text, operands=()):
        """
        The facts a text states that no source holds and no single arithmetic step explains, as written, each once -
        only the kinds that block (BLOCKING_KINDS: an address, an identifier, an amount). "Billed 44,100.00, accepted
        35,280.00, under dispute 8,820.00" and "Total 13,500 for 3 nights" are the text's own arithmetic, and held.
        operands: more figures the step may start from - for a value typed into a form, the other values typed with it.
        """

        return list(dict.fromkeys(w for kind, w in self._missing(text, operands) if kind in BLOCKING_KINDS))

    def doubtful(self, text):
        """The dates and letter-and-digit codes a text states that no source holds: evidence for the judge, never a
        verdict ("Pay by 24 Oct 2026", "FY27" are often the work's own, rightly)."""

        return list(dict.fromkeys(w for kind, w in self._missing(text) if kind not in BLOCKING_KINDS))


def own_work_only(text, given, work_files):
    """
    [(value, [file names])]: the facts a text states that the given sources (`given`: the request, its inputs and
    messages, the system's pages - no output of the work) do not hold, but that only the work's own files explain
    (`work_files`). The work folder holding a value is circular evidence: the judge is told "explained only by your own
    file X" (independent review, 2026-10-21, #11).
    """

    from pathlib import Path
    out = []
    per_file = {}
    for kind, written in given._missing(text):
        names = []
        for path in work_files or ():
            if path not in per_file:
                per_file[path] = Sources([file_text(path)])
            if per_file[path].holds(kind, written):
                names.append(Path(str(path)).name)
        if names:
            out.append((written, names))
    return out


_FILE_TEXT = {}
_SOURCE_KINDS = {".xlsx", ".xlsm", ".csv", ".tsv", ".txt", ".md", ".json", ".eml", ".html", ".htm", ".xml", ".docx",
                 ".pptx", ".pdf", ".odt", ".ods"}
MAX_SOURCE_FILES = 400


def file_text(path):
    """A file's whole text as the tools read it (every cell of a table, an email decoded), cached by its change time."""

    from pathlib import Path
    p = Path(str(path))
    try:
        key = (str(p), p.stat().st_mtime_ns)
    except OSError:
        return ""
    if key not in _FILE_TEXT:
        text = ""
        try:
            suffix = p.suffix.lower()
            if suffix in {".xlsx", ".xlsm", ".csv", ".tsv"}:
                from decurix_agent import filler
                text = "\n".join(filler._cells(p))
            elif suffix == ".eml":
                from decurix_agent import stage_facts
                text = "\n".join(m["text"] + "\n" + m["sender"] for m in stage_facts.read_messages([p]).values())
            elif suffix in {".txt", ".md", ".json", ".html", ".htm", ".xml"}:
                text = p.read_text(encoding="utf-8", errors="replace")
            else:
                from decurix_agent.tools import office
                text = office.read_document(str(p)).get("text") or ""
        except Exception:  # noqa: BLE001 - a file that cannot be read holds nothing here
            text = ""
        _FILE_TEXT[key] = text
    return _FILE_TEXT[key]


def gather(request, paths=(), roots=(), notes=(), today=True):
    """
    The Sources of a piece of work: the request's text, `paths` (its input files, messages and outputs), every readable
    file under `roots` (the work folder: the inputs and the recorded work), `notes` (what the tools read off the
    system's own pages) and, with `today`, the day the work is done.
    """

    import datetime
    from pathlib import Path
    from decurix_agent import attachments
    texts = [attachments.text(request or "")]
    files = [str(p) for p in paths or ()]
    for root in roots or ():
        try:
            from decurix_agent import produced
            files += [str(p) for p in produced._walk(root)]
        except Exception:  # noqa: BLE001
            continue
    for path in list(dict.fromkeys(files))[:MAX_SOURCE_FILES]:
        if Path(path).suffix.lower() in _SOURCE_KINDS:
            texts.append(file_text(path))
    texts += [str(n) for n in notes or ()]
    found = Sources(texts)
    if today:
        found.add_date(datetime.date.today())
    return found
