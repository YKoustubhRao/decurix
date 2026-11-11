"""
A value that only fills a field - 9999999999, 1234567890, "test", "n/a", name@example.com - is never entered as if it
were real.

Found live (2026-10-20, round 5 P4): a joiner was onboarded in a portal with the phone number 9999999999; the tracker
the request attached had her real one, "+91 90040 55318". The worker had looked at the ID pictures (which turned the
guard against untraced values off) and made the number up; nothing in the report said so. A value that looks like
filler and appears in no source is refused before a form is saved (gui), and is not done if it was typed anyway (the
final check). A real value that happens to look so - it is in a source - is fine.
"""

import csv
import re
from pathlib import Path

from decurix_agent.logging_utils import log

_WORD_FILLER = re.compile(r"(?i)^\W*(?:test(?:ing)?|dummy|sample|placeholder|x{3,}|asdf\w*|qwerty|lorem(?: ipsum.*)?|"
                          r"n\s*/?\s*a|tbd|to be (?:confirmed|decided)|none|null|unknown|abc|xyz)\W*$")
_FILLER_DOMAIN = re.compile(r"(?i)@(?:example|test|sample|dummy|domain|email|mail)\.(?:com|org|net|in|co\.in)\b")


def _digits(value):
    return re.sub(r"\D", "", str(value or ""))


def looks_filler(value):
    """Whether a value only fills a field: a word like test/n.a., an address at example.com, or a number of five or
    more digits that is one digit repeated or a plain run (123456, 9876543210)."""

    text = str(value or "").strip()
    if not text:
        return False
    if _WORD_FILLER.match(text) or _FILLER_DOMAIN.search(text):
        return True
    digits = _digits(text)
    if len(digits) >= 5 and len(digits) >= len(re.sub(r"[\s+\-().]", "", text)):  # (the value is a number)
        if len(set(digits)) == 1:
            return True
        if digits in "01234567890123456789" or digits in "98765432109876543210":
            return True
    return False


def _cells(path):
    """The values of an input table (CSV/TSV, every workbook sheet), or a text file's text, as strings."""

    p = Path(path)
    out = []
    try:
        if p.suffix.lower() in {".xlsx", ".xlsm"}:
            import openpyxl
            book = openpyxl.load_workbook(p, read_only=True, data_only=True)
            try:
                for sheet in book.worksheets:
                    for row in sheet.iter_rows(values_only=True):
                        out += [str(c) for c in row if c is not None]
            finally:
                book.close()
        elif p.suffix.lower() in {".csv", ".tsv"}:
            with open(p, encoding="utf-8-sig", errors="replace", newline="") as handle:
                for row in csv.reader(handle, delimiter="\t" if p.suffix.lower() == ".tsv" else ","):
                    out += row
        elif p.suffix.lower() in {".txt", ".md", ".json", ".eml", ".html", ".htm", ".xml"}:
            out.append(p.read_text(encoding="utf-8", errors="replace"))
    except Exception as exc:  # noqa: BLE001 - an input that cannot be read gives nothing
        log(f"filler: could not read {p.name}: {exc}")
    return out


def headers(paths):
    """{header: [file names]} of the tables among paths (CSV/TSV, each workbook sheet's first row)."""

    found = {}
    for path in paths or ():
        p = Path(path)
        rows = []
        try:
            if p.suffix.lower() in {".xlsx", ".xlsm"}:
                import openpyxl
                book = openpyxl.load_workbook(p, read_only=True, data_only=True)
                try:
                    rows = [next(sheet.iter_rows(values_only=True), ()) for sheet in book.worksheets]
                finally:
                    book.close()
            elif p.suffix.lower() in {".csv", ".tsv"}:
                with open(p, encoding="utf-8-sig", errors="replace", newline="") as handle:
                    rows = [next(csv.reader(handle, delimiter="\t" if p.suffix.lower() == ".tsv" else ","), [])]
        except Exception:  # noqa: BLE001
            continue
        for row in rows:
            for cell in row or ():
                text = " ".join(str(cell or "").split())
                if 2 < len(text) <= 40:
                    found.setdefault(text, [])
                    if p.name not in found[text]:
                        found[text].append(p.name)
    return found


def in_sources(value, texts):
    """Whether a value is in any of the source texts: as written, or - for a number - by its digits."""

    v = " ".join(str(value or "").split()).lower()
    digits = _digits(value)
    for text in texts:
        t = str(text).lower()
        if v and v in " ".join(t.split()):
            return True
        if len(digits) >= 5 and digits in _digits(t):
            return True
    return False


def source_texts(request, paths=()):
    """The request's text and the values of its input files (the ones attached, and `paths`)."""

    from decurix_agent import attachments
    texts = [attachments.text(request or "")]
    for path in list(attachments.files(request or "")) + list(paths or ()):
        texts += _cells(path)
    return texts


def where_it_is(label, paths):
    """Where the real value of a field might be: the input tables' columns that share a word with its label."""

    words = {w for w in re.findall(r"[a-z]{3,}", (label or "").lower())} - {"the", "and", "line", "number", "name"}
    places = []
    for header, files in headers(paths).items():
        if words & set(re.findall(r"[a-z]{3,}", header.lower())):
            places.append(f"{header} is a column of {', '.join(files)}")
    return places[:3]


def invented(fields, request, paths=(), said=()):
    """[(label, value)] of the filled fields whose value looks like filler and is in no source. `said`: what the person
    said in this task, as the control plane confirms it (records._answers_text) - their own words are a source."""

    found = [(label, value) for label, value in fields if looks_filler(value)]
    if not found:
        return []
    texts = source_texts(request, paths) + [str(t) for t in said or () if t]
    return [(label, value) for label, value in found if not in_sources(value, texts)]


def refusal(invented_fields, paths=()):
    """What the operator is told when a form holds invented values."""

    parts = []
    for label, value in invented_fields:
        hint = where_it_is(label, paths)
        parts.append(f"{label}: {value!r}" + (f" ({'; '.join(hint)})" if hint else ""))
    return ("Not pressed: these values look invented - a filler, not a value read from any source: " + "; ".join(parts)
            + ". Never type a value you did not read from a source or the person: find the real one (the request's "
              "input files, what earlier steps made) and give it in the format the field accepts (for a phone: digits "
              "only, without the country code, if that is what the field takes) - or ask the person, or leave an "
              "optional field empty.")
