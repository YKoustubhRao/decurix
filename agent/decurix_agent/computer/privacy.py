import hashlib
import math
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

# "PIN" names a secret - a card's, an ATM's, a UPI or a login PIN. Only a label that says it is a postal code is not
# one: "PIN code", "Pincode", "Pin-code" (an Indian address's postal code; found in review 2026-11-02, a GST or address
# form's "PIN code" field was taken for a secret and the worker stopped for a person). A bare "PIN" stays a secret
# whatever follows it - "PIN: 482193" is six digits like a postal code, and the digits never tell which it is (recheck
# 2026-11-02: a digit-count carve-out let a six-digit PIN through). A PIN named for a card, an ATM, UPI, a login or a
# bank stays a secret even when "code" follows it.
_QUALIFIED_PIN = r"(?:card|atm|upi|login|debit|credit|bank(?:ing)?|transaction|security|account)[\s_-]*pin|m-?pin"
PIN_WORD = r"(?:" + _QUALIFIED_PIN + r"|pin(?![\s_-]{0,3}code\b))"
# A label that names a postal code in other words ("Postal code (PIN)", "ZIP / PIN").
_POSTAL_LABEL = re.compile(r"(?i)\b(?:postal|post|zip)[\s_-]*code\b|\bzip\b")

# A secret after its name in a request: "password Northwind#2026", "PIN: 4821", "api key=abc", "token is xyz".
REQUEST_SECRET = re.compile(
    r"(?i)\b(?:pass(?:word|code|phrase)|pwd|" + PIN_WORD + r"|otp|one[- ]time (?:code|password)|token|api[ _-]?key|secret(?: key)?)"
    r"\b\s*(is\s+|[:=]\s*)?([\"'\u201c\u2018]?)([^\s\"'\u201d\u2019,;)\]]{3,200})")
_WORD = re.compile(r"[^\s\"'\u201c\u201d\u2018\u2019(),;<>\[\]{}=]+")
_MAX_KEPT = 200


def _secrets_file():
    base = Path(os.environ.get("DECURIX_DIR") or (Path(os.environ.get("HOME", "/home/headless")) / ".decurix"))
    return base / "withheld-secrets"


def _hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def request_secrets(text):
    """The secrets a request's own words give after their names (a password, a PIN, a key): [text]."""

    # Said as one ("password is x", "pin: x", quoted), any word; written straight after the name, only what has a
    # digit or a mark in it. Found live (2026-10-22): "No OTP has come to the client's phone yet" kept "has"
    # as a secret, and every "has" of every later reply and step read "[withheld]".
    found = [m.group(3).rstrip(".!?") for m in REQUEST_SECRET.finditer(text or "")
             if m.group(1) or m.group(2) or not m.group(3).rstrip(".!?").isalpha()]
    found += [m.group(1) for m in SPOKEN_SECRET.finditer(text or "")]
    return [s for s in dict.fromkeys(found) if len(s) >= 3 and s.lower() not in {"for", "the", "and", "from", "field"}]


def remember_secrets(text):
    """Keep the hashes of the secrets `text` gives, for withhold_known to find. Never raises."""

    try:
        hashes = [_hash(s) for s in request_secrets(text)]
    except Exception:  # noqa: BLE001 - remembering is a safety net, never a failure of the task
        return
    _keep_hashes(hashes)


def _keep_hashes(hashes):
    """Add hashes to the secrets file. When it cannot be written they are still withheld in this process (_unsaved):
    a secret not remembered must not be a secret let through. Never raises."""

    if not hashes:
        return
    try:
        path = _secrets_file()
        path.parent.mkdir(parents=True, exist_ok=True)
        kept = [h for h in path.read_text(encoding="utf-8").split() if h] if path.exists() else []
        kept = list(dict.fromkeys(kept + list(hashes)))[-_MAX_KEPT:]
        path.write_text("\n".join(kept) + "\n", encoding="utf-8")
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass
        _known_cache.clear()
    except Exception:  # noqa: BLE001 - remembering is a safety net, never a failure of the task
        _unsaved.update(hashes)


_known_cache = {}
# Hashes this process could not write to the secrets file (_keep_hashes): still withheld here.
_unsaved = set()


class _Unreadable(Exception):
    """The secrets file is there but cannot be read: what it holds cannot be withheld, so nothing is let through."""


def _known():
    path = _secrets_file()
    try:
        stamp = path.stat().st_mtime_ns
    except FileNotFoundError:
        return set(_unsaved)
    except OSError as exc:
        raise _Unreadable(str(exc)) from exc
    if _known_cache.get("stamp") != stamp:
        try:
            _known_cache.update(stamp=stamp, hashes=set(path.read_text(encoding="utf-8").split()))
        except OSError as exc:
            # Found in review (2026-11-02): an unreadable file read as "no secrets", and every one passed through.
            raise _Unreadable(str(exc)) from exc
    return _known_cache.get("hashes", set()) | _unsaved


def known_secret_in(text):
    """Whether text holds a word that is one of the request secrets remembered. True when it cannot be told (fail
    closed: a text that cannot be checked is treated as holding one)."""

    if not text:
        return False
    try:
        known = _known()
        if not known:
            return False
        return any(_hash(w) in known or _hash(w.rstrip(".:!?")) in known for w in _WORD.findall(str(text)))
    except Exception:  # noqa: BLE001 - fail closed
        return True


# What is shown in place of a text that could not be checked for secrets. Withholding fails closed: an error in it
# never lets the text through (review 2026-11-02: every caller caught the error and sent the text as it was).
HELD_BACK = "[held back: this text could not be checked for secrets]"


def withhold_known(text):
    """text with every word that is a remembered request secret replaced by [withheld], and every secret it spells
    out (withhold_spoken_secrets). Never raises: a text that cannot be checked is replaced whole by HELD_BACK."""

    if text is None or text == "":
        return text
    try:
        return _withhold_known(str(text))
    except Exception:  # noqa: BLE001 - fail closed
        return HELD_BACK


def _withhold_known(text):
    text = _withhold_shapes(text)
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


def safe_text(text):
    """withhold_known for anything a caller is about to send off the desktop: a str (or None) always comes back, and
    an error means HELD_BACK, never the text."""

    return withhold_known(text)


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
    r"\b(pass(word|code|phrase)|" + PIN_WORD + r"|otp|one[- ]time (code|password|pin)|verification code"
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

# A card number stands alone: digits glued to letters are a code (an acknowledgement number like "AB1234567890123C"
# holds 13 digits that may pass the checksum). Found live (2026-10-25): a message to a client quoting two such numbers was
# withheld whole as "a card number", and the person asked to approve sending it was shown nothing of it.
CARD_NUMBER = re.compile(r"(?<![\w-])\d(?:[ -]?\d){12,18}(?![\w-])")

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

    name = element.get("name") or ""
    if element.get("role") in TEXT_ENTRY_ROLES and SECRET_LABEL.search(name):
        # A field the page marks as a postal code (autocomplete=postal-code), or whose label names one ("Postal code
        # (PIN)"), is an address's PIN code - when a bare "PIN" is the label's only secret word. Anything else stays
        # a secret field.
        postal = ("postal-code" in (handle.get("autocomplete") or "").lower().split()) or _POSTAL_LABEL.search(name)
        only_bare_pin = (not re.search(r"(?i)\b(?:" + _QUALIFIED_PIN + r")\b", name)
                         and not SECRET_LABEL.search(re.sub(r"(?i)\bpin\b", " ", name)))
        if not (postal and only_bare_pin):
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
    """The kind of credential a piece of text looks like, or None. Fails closed: a text that cannot be checked is
    "text that could not be checked for secrets", never None."""

    if not text:
        return None
    try:
        return _credential_in(str(text))
    except Exception:  # noqa: BLE001 - fail closed
        return "text that could not be checked for secrets"


def _credential_in(text):
    if known_secret_in(text):
        return "password the request gave"

    for label, pattern in CREDENTIALS:
        if pattern.search(text):
            return label

    for label, pattern, group in GENERIC_SECRETS:
        for match in pattern.finditer(text):
            if _is_secret_value(label, match.group(group) if group else match.group(0)):
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
    r"(?i)\b(?:pass(?:word|code|phrase)|secret|token|api[ _-]?key|" + PIN_WORD + r"|otp|cvv2?)\b[^\"'\u201c\u2018\n]{0,20}?"
    r"[\"'\u201c\u2018]([^\"'\u201d\u2019\n]{1,200})[\"'\u201d\u2019]"
)


# A credential written after its name and a colon or equals sign, unquoted: "- password: Lift$2026", "pwd=x",
# "DB_PASSWORD=x", "client_secret: x". Found live (2026-10-21): a worker's report listed the client's portal password
# read from the person's client list. (Not "PIN": that is the PIN rule's, above. Not "tokens": a usage line's count.)
_SECRET_NAME = (r"pass(?:word|code|phrase)|passwd|pwd|api[ _-]?(?:key|secret)|secret(?:[ _-]?key)?|client[ _-]?secret"
                r"|access[ _-]?(?:key|token)|refresh[ _-]?token|auth(?:entication)?[ _-]?token|private[ _-]?key|token")
LABELLED_SECRET = re.compile(
    r"(?i)(?<![A-Za-z])(?:" + _SECRET_NAME + r")(?![A-Za-z])\s*[:=]\s*[`\"'“‘]?([^\s`\"'”’,;)\]]{3,200})")

# Generic shapes of a secret written out, beyond the named credentials above (review 2026-11-02: only named secrets
# were withheld). Each is unambiguous on its own or by the word next to it, so a figure, a date, a GSTIN, an invoice
# number or a postal code is never one: (label, pattern, the group that is the secret - 0 for the whole match).
GENERIC_SECRETS = [
    ("private key", re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?"
                               r"(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\Z)"), 0),
    # "Authorization: Bearer x", "Basic dXNlcjpwYXNz" (not "bearer cheque": the value must look like a token).
    ("bearer token", re.compile(r"(?i)\b(?:bearer|basic)\s+([A-Za-z0-9._~+/-]{16,}=*)"), 1),
    # https://user:password@host
    ("password in a web address", re.compile(r"(?i)\b[a-z][a-z0-9+.-]*://[^\s:/@]+:([^\s@/]+)@"), 1),
    # ?access_token=x, &api_key=x, &sig=x
    ("secret in a web address", re.compile(
        r"(?i)[?&#;](?:[\w.-]*[_-])?(?:token|secret|password|passwd|pwd|apikey|api[_-]?key|key|access[_-]?key|signature"
        r"|sig|otp|session(?:[_-]?id)?|auth)=([^&\s#\"'<>]+)"), 1),
    ("password", LABELLED_SECRET, 1),
    # "the password is Lift$2026" (a value with a digit or a mark: "the password is incorrect" is a screen's words).
    ("password said", re.compile(r"(?i)(?<![A-Za-z])(?:pass(?:word|code|phrase)|passwd|pwd|api[ _-]?key|secret[ _-]?key"
                            r"|client[ _-]?secret|access[ _-]?token)\s+(?:is|was)\s+[`\"'“‘]?([^\s`\"'”’,;)\]]{3,200})"), 1),
    # "OTP 529749", "verification code: 4821", "CVV 123".
    ("one-time code", re.compile(r"(?i)(?<![A-Za-z])(?:otp|one[- ]time (?:code|password|passcode)|verification code"
                                 r"|security code|auth(?:entication)? code|2fa code|mfa code|totp)(?![A-Za-z])"
                                 r"[^\d\n]{0,12}?(?<!\d)(\d{4,8})(?!\d)"), 1),
    ("card security code", re.compile(r"(?i)(?<![A-Za-z])(?:cvv2?|cvc2?)(?![A-Za-z])[^\d\n]{0,6}?(?<!\d)(\d{3,4})(?!\d)"),
     1),
    # A long random token next to a word that says what it is: "client key 9f8a7b...", "secret: a1B2...".
    # (Not "key" alone: "the key figures" in a letter is no secret's name.)
    ("key or token", re.compile(r"(?i)(?<![A-Za-z])(?:(?:api|access|secret|client|private|licen[cs]e)[ _-]?key|apikey"
                                r"|secret|token|password|passwd|credentials?|signature)"
                                r"(?![A-Za-z])[^\n]{0,30}?(?<![A-Za-z0-9+/_=.-])([A-Za-z0-9+/_=.-]{20,})"), 1),
]

def _tokenish(value):
    """Whether a value reads as a machine's token rather than a word: a digit, a mark, or a capital after a small
    letter."""

    return bool(re.search(r"[\d+/=_~.-]", value) or re.search(r"[a-z][A-Z]", value))


def _entropy(value):
    counts = {}
    for ch in value:
        counts[ch] = counts.get(ch, 0) + 1
    return -sum(n / len(value) * math.log2(n / len(value)) for n in counts.values())


def _is_secret_value(label, value):
    """Whether what a GENERIC_SECRETS pattern caught is a secret's value (and not already withheld)."""

    value = value or ""
    if not value or value.startswith("[withheld") or value.startswith("[held back"):
        return False
    if label in ("private key", "password in a web address", "one-time code", "card security code"):
        return True
    if label == "password said":
        bare = value.rstrip(".!?:")
        return re.search(r"[\d!@#$%^&*+=~|\\/<>]", bare) is not None or re.search(r"[a-z][A-Z]", bare) is not None
    if label == "bearer token":
        return len(value) >= 16 and _tokenish(value)
    if label == "secret in a web address":
        return len(value) >= 8 and _tokenish(value)
    if label == "key or token":
        return (re.search(r"[A-Za-z]", value) is not None and re.search(r"\d", value) is not None
                and "@" not in value and _entropy(value) >= 3.5)
    return True  # a password after its name


def _withhold_shapes(text):
    """text with every credential it spells out, by name or by shape, replaced by [withheld]. Raises on an error (the
    callers fail closed)."""

    for label, pattern in CREDENTIALS:
        if label != "private key":  # (the whole block goes, below - not its first line only)
            text = pattern.sub("[withheld]", text)
    for label, pattern, group in GENERIC_SECRETS:
        def hide(m, label=label, group=group):
            value = m.group(group) if group else m.group(0)
            if not _is_secret_value(label, value):
                return m.group(0)
            if not group:
                return "[withheld: " + label + "]"
            start, end = m.start(group) - m.start(0), m.end(group) - m.start(0)
            return m.group(0)[:start] + "[withheld]" + m.group(0)[end:]
        text = pattern.sub(hide, text)
    return SPOKEN_SECRET.sub(lambda m: m.group(0).replace(m.group(1), "[withheld]"), text)


def withhold_spoken_secrets(text):
    """`text` with any credential it spells out - after its name, quoted, or in a secret's own shape - replaced by
    [withheld]. Fails closed: an error gives HELD_BACK, never the text."""

    if not text:
        return text
    try:
        return _withhold_shapes(str(text))
    except Exception:  # noqa: BLE001 - fail closed
        return HELD_BACK


def remember_value(value):
    """Keep a secret typed into a password or code field (its hash), so withhold_known keeps it out of replies too.
    Never raises."""

    value = (value or "").strip()
    if len(value) < 3:
        return
    _keep_hashes([_hash(value)])


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


# ============================================================
# What file may leave the desktop
#
# An email's attachment (tools/outbox.py) and a handoff's file (tools/handoff.py) are copied off this desktop. Found in
# review (2026-11-02): either took any path, so ~/.ssh/id_rsa or ~/.claude/.credentials.json could be attached to a
# draft or carried to another worker. A file leaves only from the work folder (the request's attachments are in its
# inbox), the shared folder, or the browser's Downloads - never from a hidden (dot) folder, and never the worker's own
# state (~/.decurix). Fails closed: a path that cannot be checked does not leave.
# ============================================================

def _send_roots():
    from decurix_agent import config, shared

    home = Path(os.path.expanduser("~"))
    # (~/work as well as WORK_DIR: a script proven in a sandbox has its HOME there, and its work folder is the copy.)
    roots = [config.WORK_DIR, home / "work", home / "Downloads"]
    if shared.folder():
        roots += [shared.mount_path(), shared.link_path()]
    return [Path(os.path.realpath(str(r))) for r in roots if r]


def _under(path, root):
    try:
        return Path(path).relative_to(root)
    except ValueError:
        return None


def file_may_leave(path, base=None):
    """(the file's absolute path, None) when it may be sent off this desktop, or (None, why not) - see above. A
    relative path is taken in `base` (default: the work folder)."""

    try:
        from decurix_agent import config

        raw = os.path.expanduser(str(path or "").strip())
        if not raw:
            return None, "no file was named"
        given = Path(raw)
        if not given.is_absolute():
            given = Path(base or config.WORK_DIR) / given
        given = Path(os.path.abspath(str(given)))
        real = Path(os.path.realpath(str(given)))
        state = Path(os.path.realpath(str(config.DECURIX_DIR)))
        if real == state or _under(real, state) is not None:
            return None, "it is the worker's own state, which never leaves the desktop"
        roots = _send_roots()
        for candidate in (given, real):
            for root in roots:
                inside = _under(candidate, root)
                if inside is not None and any(part.startswith(".") for part in inside.parts):
                    return None, "it is in a hidden (dot) folder, which never leaves the desktop"
        if not any(_under(real, root) is not None for root in roots):
            return None, ("it is outside the work folder - only a file of the work (under ~/work, the shared folder "
                          "or ~/Downloads) can be sent")
        return given, None
    except Exception:  # noqa: BLE001 - fail closed
        return None, "it could not be checked, so it is not sent"
