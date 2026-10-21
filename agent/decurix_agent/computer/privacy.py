import hashlib
import os
import re
import urllib.parse
from pathlib import Path

from decurix_agent.computer import elements as el


# ============================================================
# Secrets a request gives in plain words
#
# Found live (2026-10-20, round 5 P2): a request said "user clerk, password Northwind#2026"; a refused typing asked
# the person 'I was about to type "clerk", "Northwind#2026", ...' - the password in the chat. And every command the
# worker runs is shown as a step ("Running: gui type e5 ..."). So the secrets a request spells out are remembered at
# the start of its task - as SHA-256 hashes only, never the text - and every line that reaches the chat, a step, a task
# record or a recording has any word that is one of them replaced by [withheld].
# ============================================================

# A secret after its name in a request: "password Northwind#2026", "PIN: 4821", "api key=abc", "token is xyz".
REQUEST_SECRET = re.compile(
    r"(?i)\b(?:pass(?:word|code|phrase)|pwd|pin|otp|one[- ]time (?:code|password)|token|api[ _-]?key|secret(?: key)?)"
    r"\b\s*(?:is\s+|[:=]\s*)?[\"'\u201c\u2018]?([^\s\"'\u201d\u2019,;)\]]{3,200})")
_WORD = re.compile(r"[^\s\"'\u201c\u201d\u2018\u2019(),;<>\[\]{}=]+")
_MAX_KEPT = 200


def _secrets_file():
    base = Path(os.environ.get("DECURIX_DIR") or (Path(os.environ.get("HOME", "/home/headless")) / ".decurix"))
    return base / "withheld-secrets"


def _hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def request_secrets(text):
    """The secrets a request's own words give after their names (a password, a PIN, a key): [text]."""

    found = [m.group(1).rstrip(".!?") for m in REQUEST_SECRET.finditer(text or "")]
    found += [m.group(1) for m in SPOKEN_SECRET.finditer(text or "")]
    return [s for s in dict.fromkeys(found) if len(s) >= 3 and s.lower() not in {"for", "the", "and", "from", "field"}]


def remember_secrets(text):
    """Keep the hashes of the secrets `text` gives, for withhold_known to find. Never raises."""

    try:
        hashes = [_hash(s) for s in request_secrets(text)]
        if not hashes:
            return
        path = _secrets_file()
        path.parent.mkdir(parents=True, exist_ok=True)
        kept = [h for h in path.read_text(encoding="utf-8").split() if h] if path.exists() else []
        kept = list(dict.fromkeys(kept + hashes))[-_MAX_KEPT:]
        path.write_text("\n".join(kept) + "\n", encoding="utf-8")
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
        _known_cache.clear()
    except Exception:  # noqa: BLE001 - remembering is a safety net, never a failure of the task
        pass


_known_cache = {}


def _known():
    path = _secrets_file()
    try:
        stamp = path.stat().st_mtime_ns
    except OSError:
        return set()
    if _known_cache.get("stamp") != stamp:
        try:
            _known_cache.update(stamp=stamp, hashes=set(path.read_text(encoding="utf-8").split()))
        except OSError:
            return set()
    return _known_cache.get("hashes", set())


def known_secret_in(text):
    """Whether text holds a word that is one of the request secrets remembered."""

    known = _known()
    if not known or not text:
        return False
    return any(_hash(w) in known or _hash(w.rstrip(".:!?")) in known for w in _WORD.findall(str(text)))


def withhold_known(text):
    """text with every word that is a remembered request secret replaced by [withheld] (and spoken secrets too)."""

    if not text:
        return text
    text = withhold_spoken_secrets(str(text))
    known = _known()
    if not known:
        return text

    def hide(match):
        word = match.group(0)
        if _hash(word) in known:
            return "[withheld]"
        bare = word.rstrip(".:!?")
        if bare != word and _hash(bare) in known:
            return "[withheld]" + word[len(bare):]
        return word
    return _WORD.sub(hide, text)


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

    if known_secret_in(text):
        return "password the request gave"

    for label, pattern in CREDENTIALS:
        if pattern.search(text):
            return label

    for match in CARD_NUMBER.finditer(text):
        digits = re.sub(r"\D", "", match.group(0))
        if 13 <= len(digits) <= 19 and _luhn(digits):
            return "card number"

    return None


# A credential written out in a sentence: `the password "Ttlh"`, `pin: '1234'`. A task's
# own words often carry one, and a recording's title and description are shown to
# anyone who opens the page and exported with it.
SPOKEN_SECRET = re.compile(
    r"(?i)\b(?:pass(?:word|code|phrase)|secret|token|api[ _-]?key|pin|otp|cvv2?)\b[^\"'\u201c\u2018\n]{0,20}?"
    r"[\"'\u201c\u2018]([^\"'\u201d\u2019\n]{1,200})[\"'\u201d\u2019]"
)


def withhold_spoken_secrets(text):
    """`text` with any credential it spells out replaced by [withheld]."""

    if not text:
        return text

    return SPOKEN_SECRET.sub(lambda m: m.group(0).replace(m.group(1), "[withheld]"), text)


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
