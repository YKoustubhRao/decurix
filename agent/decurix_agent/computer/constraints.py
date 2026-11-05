import re


# ============================================================
# Parameter constraints: what a value has to look like
#
# AgentRR generates a check function for each recorded experience: the state
# it needs before it starts and the constraints on the parameters it is given.
# A replay that types whatever it is handed into a field that only takes an
# email address either fails half way (a form's own validation) or, worse,
# succeeds at something else. This works out, when a recording is compiled, what
# a typed field takes - and checks a value against it before the first step.
#
# One example proves nothing about a type: a "Customer" field typed "1042" once
# is not therefore a number field. So a constraint is *enforced* only when the
# field itself says so - a numeric widget, or a label that names the kind of
# thing ("Work email") together with an example that is one. When only one of
# those holds, the type is kept as a hint for a person filling the value in, and
# nothing is refused on it. Only kinds that are the same everywhere are enforced
# (an email address, a web address, a number); a date or a phone number is
# written a dozen ways and is only ever a hint.
# ============================================================

EMAIL = "email"
URL = "url"
NUMBER = "number"
DATE = "date"
PHONE = "phone"

ENFORCED = {EMAIL, URL, NUMBER}

LABELS = [
    (EMAIL, re.compile(r"\be-?mail\b", re.IGNORECASE)),
    (URL, re.compile(r"\b(url|website|web ?site|web address|homepage)\b", re.IGNORECASE)),
    (NUMBER, re.compile(r"\b(amount|quantity|qty|price|total|count|number of|percent(age)?|rate|balance)\b", re.IGNORECASE)),
    (DATE, re.compile(r"\b(date|birthday|dob|deadline)\b", re.IGNORECASE)),
    (PHONE, re.compile(r"\b(phone|mobile|telephone|fax)\b", re.IGNORECASE)),
]

EMAIL_SHAPE = re.compile(r"^[^@\s]+@[^@\s]+\.[^@\s]{2,}$")
URL_SHAPE = re.compile(r"^(https?://)?[\w-]+(\.[\w-]+)+(:\d+)?(/\S*)?$", re.IGNORECASE)
# Grouped the Western way (1,250,000) or the Indian way (12,50,000 and 12,34,56,789), or not grouped at all.
NUMBER_SHAPE = re.compile(r"^[+-]?(\d{1,3}(,\d{3})+|\d{1,2}(,\d{2})*,\d{3}|\d+)(\.\d+)?%?$")
DATE_SHAPE = re.compile(r"^(\d{1,4}[-/.]\d{1,2}[-/.]\d{1,4}|\d{1,2} [A-Za-z]{3,9},? \d{2,4}|[A-Za-z]{3,9} \d{1,2},? \d{2,4})$")
PHONE_SHAPE = re.compile(r"^\+?[\d\s().-]{7,20}$")

SHAPES = {EMAIL: EMAIL_SHAPE, URL: URL_SHAPE, NUMBER: NUMBER_SHAPE, DATE: DATE_SHAPE, PHONE: PHONE_SHAPE}

DESCRIPTIONS = {
    EMAIL: "an email address",
    URL: "a web address",
    NUMBER: "a number",
    DATE: "a date",
    PHONE: "a phone number",
}


def _plain(value):
    """A value as its shape is judged: no currency symbol, no stray space."""

    return re.sub(r"^[$€£¥₹]\s*|\s*[$€£¥₹]$", "", (value or "").strip())


def looks_like(kind, value):
    return bool(SHAPES[kind].match(_plain(value) if kind == NUMBER else (value or "").strip()))


def _by_example(example):
    for kind in (EMAIL, NUMBER, URL, DATE, PHONE):
        if looks_like(kind, example):
            return kind
    return None


def _by_label(label):
    for kind, pattern in LABELS:
        if pattern.search(label or ""):
            return kind
    return None


def infer(label, role, example):
    """
    What a field takes: {"type", "strict"} - `strict` when it is checked
    before a run, not merely suggested - or None when nothing is known.
    """

    if role == "spinbutton":
        return {"type": NUMBER, "strict": True}

    named, shown = _by_label(label), _by_example(example)

    if named and named == shown:
        return {"type": named, "strict": named in ENFORCED}

    kind = named or shown
    return {"type": kind, "strict": False} if kind else None


def problem(constraint, value):
    """Why `value` is not what a constrained field takes, or None."""

    if not constraint or not constraint.get("strict") or not (value or "").strip():
        return None

    kind = constraint["type"]

    if looks_like(kind, value):
        return None

    return f"{value!r} is not {DESCRIPTIONS[kind]}"


def violations(params, values, split=None):
    """
    [(param, why)] for every constrained parameter the given values break. A
    list parameter's value is judged item by item when `split` (the rule that
    separates its items, skills.split_items) is given: "1; 4; 10" is three
    numbers, not one value that is not a number.
    """

    found = []

    for param in params:
        value = (values or {}).get(param["name"])

        if param.get("list") and split is not None and value:
            for n, item in enumerate(split(value), 1):
                why = problem(param.get("constraint"), item)
                if why:
                    found.append((param, f"item {n} ({why})"))
                    break
            continue

        why = problem(param.get("constraint"), value)
        if why:
            found.append((param, why))

    return found
