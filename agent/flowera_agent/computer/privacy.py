import re
import urllib.parse

from flowera_agent.computer import elements as el


# ============================================================
# What a recording never keeps
#
# openadapt-capture's rule, applied at the source: a password or secure
# field's value is not retained, and every other captured value is reported
# exactly or withheld with the reason - never rewritten. "withheld: password
# field" is honest; "p******" would be a string the screen never showed, and
# the next reader would take it for evidence.
#
# Applied before anything is written, to every copy: the recording on the
# desktop's disk, the chat step and task record built from it, and the words
# handed back to the engine. Frames get the same treatment by region - a
# secret field's box is painted over in what is saved (screen.Frame.masked),
# while an engine's own live screenshot is left alone.
#
# What it does not cover, as openadapt-flow's secrets contract does not
# either: a secret an application copies somewhere else on the screen, and a
# secret an engine wrote into its own command line (`gui type e5 "hunter2"`),
# which the harness's step view shows before this layer ever sees it.
# ============================================================

# openadapt-capture's cap: a longer value is omitted, not cut, so nothing
# downstream mistakes half a string for the whole one.
MAX_TEXT = 512

# Roles whose value is secret by construction: openadapt-capture's list, which
# includes what AT-SPI calls a GTK password entry.
SECRET_ROLES = {
    "password", "password text", "passwordtext", "passwordbox",
    "securetextfield", "axsecuretextfield", "secureedit",
}

# An HTML field's own declaration that it holds a secret.
SECRET_AUTOCOMPLETE = {
    "current-password", "new-password", "one-time-code",
    "cc-number", "cc-csc", "cc-exp", "cc-exp-month", "cc-exp-year",
}

# A field labelled for a secret, when the toolkit does not say so itself.
SECRET_LABEL = re.compile(
    r"\b(pass(word|code|phrase)|pin|otp|one[- ]time (code|password|pin)|verification code"
    r"|security code|2fa|mfa|totp|cvv2?|cvc2?|csc|card number|credit card"
    r"|api[ _-]?key|secret( key)?|access token|auth(entication)? token|private key)\b",
    re.IGNORECASE,
)

TEXT_ENTRY_ROLES = {"textbox", "combobox", "spinbutton"}

# Text that is a credential whatever field it went into.
CREDENTIALS = [
    ("private key", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----")),
    ("cloud access key", re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),
    ("GitHub token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{22,}")),
    ("API key", re.compile(r"\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}")),
    ("Slack token", re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}")),
    ("Google API key", re.compile(r"\bAIza[0-9A-Za-z_-]{35}\b")),
    ("JSON web token", re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}")),
]

CARD_NUMBER = re.compile(r"(?<![\d-])\d(?:[ -]?\d){12,18}(?![\d-])")

# Query parameters whose values are credentials more often than not.
SECRET_PARAM = re.compile(r"pass|pwd|secret|token|key|auth|session|sig|otp|code|credential", re.IGNORECASE)


def _collapse(role):
    return " ".join((role or "").replace("_", " ").lower().split())


def secret_field(element):
    """Why an element's value must not be kept, or None."""

    if not element:
        return None

    # A recorded element carries the reason it was already withheld for.
    if element.get("secret"):
        return element["secret"] if isinstance(element["secret"], str) else "secret field"

    handle = element.get("handle") or {}

    for role in (handle.get("raw_role"), handle.get("dom_role"), element.get("role")):
        collapsed = _collapse(role)
        if collapsed in SECRET_ROLES or collapsed.replace(" ", "") in SECRET_ROLES:
            return "password field"

    if (handle.get("input_type") or "").lower() == "password":
        return "password field"

    if set((handle.get("autocomplete") or "").lower().split()) & SECRET_AUTOCOMPLETE:
        return "field for a password, code or card number"

    if element.get("role") in TEXT_ENTRY_ROLES and SECRET_LABEL.search(element.get("name") or ""):
        return "field labelled for a secret"

    return None


def _luhn(digits):
    total = 0
    for index, digit in enumerate(reversed(digits)):
        value = int(digit)
        if index % 2:
            value = value * 2 - 9 if value > 4 else value * 2
        total += value
    return total % 10 == 0


def credential_in(text):
    """The kind of credential a piece of text looks like, or None."""

    if not text:
        return None

    for label, pattern in CREDENTIALS:
        if pattern.search(text):
            return label

    for match in CARD_NUMBER.finditer(text):
        digits = re.sub(r"\D", "", match.group(0))
        if 13 <= len(digits) <= 19 and _luhn(digits):
            return "card number"

    return None


def withhold_text(element, text):
    """
    What of typed text may be kept: (text, None), or (None, why not) when it
    went into a secret field or is itself a credential.
    """

    if text is None:
        return None, None

    reason = secret_field(element)
    if reason:
        return None, reason

    credential = credential_in(text)
    if credential:
        return None, f"looks like a {credential}"

    return text, None


def shown_text(text, withheld, limit=60):
    """Typed text as a line a person or an engine reads it."""

    if withheld:
        return f"[text withheld: {withheld}]"
    return el._quote(text or "", limit)


PUBLIC_FIELDS = ("ref", "role", "name", "value", "states", "box", "path", "source", "window_title")


def public_element(element):
    """An element as a recording keeps it: no backend handle, no secret value."""

    if element is None:
        return None

    public = {key: element.get(key) for key in PUBLIC_FIELDS}
    withheld = {}

    reason = secret_field(element)
    if reason:
        public["secret"] = reason
        if public.get("value"):
            public["value"] = None
            withheld["value"] = reason

    for key in ("name", "value", "path", "window_title"):
        value = public.get(key)
        if isinstance(value, str) and len(value) > MAX_TEXT:
            public[key] = None
            withheld[key] = f"longer than {MAX_TEXT} characters"

    if withheld:
        public["withheld"] = withheld

    return public


def secret_boxes(elements):
    """The on-screen boxes of every secret field, to paint over in saved frames."""

    return [e["box"] for e in elements or () if e.get("box") and secret_field(e)]


def kept_text(text):
    """A free-text field (a title, a label) exactly, or None past MAX_TEXT."""

    if isinstance(text, str) and len(text) > MAX_TEXT:
        return None
    return text


def reduce_url(url):
    """
    A URL reduced by structure, as openadapt-flow reduces it: credentials in
    the authority and the values of secret-named parameters are dropped,
    everything else - origin, path, every parameter name - is kept as it was.
    """

    if not url:
        return url

    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return None

    if not parts.query and not parts.fragment and "@" not in parts.netloc:
        return kept_text(url)

    netloc = parts.netloc.rsplit("@", 1)[-1]
    query = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    query = [(name, "" if SECRET_PARAM.search(name) else value) for name, value in query]
    fragment = "" if SECRET_PARAM.search(parts.fragment) else parts.fragment

    return kept_text(urllib.parse.urlunsplit(
        (parts.scheme, netloc, parts.path, urllib.parse.urlencode(query), fragment)
    ))
