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

# "PIN" names a secret - a card's, an ATM's, a UPI or a login PIN - and an Indian address's postal code (six digits,
# the first 1-9). Only a label that says it is a postal code is surely one: "PIN code", "Pincode", "Pin-code" (found in
# review 2026-11-02, a GST or address form's "PIN code" field was taken for a secret and the worker stopped for a
# person). A PIN named for a card, an ATM, UPI, a login or a bank stays a secret even when "code" follows it.
_QUALIFIED_PIN = (r"(?:card|atm|upi|login|debit|credit|bank(?:ing)?|transaction|security|account)[\s_-]*pin"
                  r"|m-?pin|t-?pin")
PIN_WORD = r"(?:" + _QUALIFIED_PIN + r"|pin(?![\s_-]{0,3}code\b))"
# A label that names a postal code in other words ("Postal code (PIN)", "ZIP / PIN").
_POSTAL_LABEL = re.compile(r"(?i)\b(?:postal|post|zip)[\s_-]*code\b|\bzip\b")

# A PIN and its value: "UPI PIN 4821", "ATM PIN code: 1234", "PIN: 576104", "PIN-560001", "pin '4821'". A PIN is
# digits only (never "Mumbai" in "Pin: Mumbai 400001"), four to eight of them.
PIN_VALUE = re.compile(
    r"(?i)(?<![A-Za-z])(?P<label>" + _QUALIFIED_PIN + r"|pin)(?P<suffix>(?:[\s_-]*(?:code|number|no)\b\.?)?)"
    r"(?:\s*(?:is|was)\s+|\s*[:=#-]?\s*)(?P<q>[\"'\u201c\u2018]?)(?P<value>\d{4,8})(?!\d)(?![A-Za-z])")
# Words that make the PIN beside them a sign-in's or a payment's (recheck 2026-11-03).
_PIN_SECRET_CUE = re.compile(
    r"(?i)\b(?:log\s?in|sign\s?in|user(?:name|\s?id)?|password|passcode|otp|portal|net\s?banking|bank|card|atm|upi"
    r"|secret|credential|enter|type|my|your|his|her|their|our)\b")
# Words that make the PIN beside them an address's postal code.
_PIN_ADDRESS_CUE = re.compile(
    r"(?i)\b(?:address|addr|road|rd|street|st|lane|marg|nagar|colony|sector|layout|city|town|village|district|dist"
    r"|taluk|tehsil|state|india|ship(?:ping)?\s+to|deliver(?:y|ed)?|billing|bill\s+to|postal|post|zip|floor"
    r"|building|bldg|near|opp(?:osite)?|landmark|cross|main|east|west|north|south)\b")
# A place's name right before the label ("Bengaluru PIN 560001", "Udupi, PIN: 576104", "Mumbai - PIN 400001"), as
# against an English word that starts a sentence ("Your PIN", "The PIN", "Enter PIN").
_PLACE_BEFORE = re.compile(r"\b([A-Z][a-z]{2,})\s*[,-]?\s*$")
_NOT_A_PLACE = {"the", "your", "my", "his", "her", "their", "our", "its", "this", "that", "new", "old", "current",
                "default", "temporary", "temp", "enter", "use", "with", "and", "same", "secret", "login", "user",
                "type", "send", "share", "give", "also", "then", "please", "kindly", "card", "bank", "otp",
                "password", "portal", "account", "transaction", "security", "debit", "credit", "upi", "atm", "here",
                "set", "change", "reset", "valid", "wrong", "correct", "only", "is", "was"}


def pin_kind(text, match):
    """What a PIN_VALUE match in text is: "secret" (a card's, a bank's or a sign-in's), "postal" (an address's code)
    or "unsure". Six digits after a bare "PIN" may be either; the words beside it decide, and with none that do it is
    "unsure" - withheld where it is written, never remembered as a secret for everywhere (recheck 2026-11-03: a
    remembered postal code hid every matching invoice total)."""

    label, suffix, value = match.group("label"), match.group("suffix"), match.group("value")
    if label.lower() != "pin":
        return "secret"
    if suffix.strip(" _-.").lower() == "code":
        return "postal"
    if len(value) != 6 or value[0] == "0":
        return "secret"  # an Indian postal code is exactly six digits, the first 1-9
    line_start = max(text.rfind("\n", 0, match.start()) + 1, match.start() - 60)
    line_end = text.find("\n", match.end())
    line_end = len(text) if line_end < 0 else line_end
    line_end = min(line_end, match.end() + 30)
    before, after = text[line_start:match.start()], text[match.end():line_end]
    place = _PLACE_BEFORE.search(before)
    place = place is not None and place.group(1).lower() not in _NOT_A_PLACE
    address = place or _PIN_ADDRESS_CUE.search(before) is not None or _PIN_ADDRESS_CUE.search(after) is not None
    secret = _PIN_SECRET_CUE.search(before) is not None or _PIN_SECRET_CUE.search(after) is not None
    if place or (address and not secret):
        return "postal"
    return "secret" if secret else "unsure"


def request_pins(text):
    """The PINs a request gives that are surely secrets: [value]."""

    return [m.group("value") for m in PIN_VALUE.finditer(text or "") if pin_kind(text, m) == "secret"]


# A secret after its name in a request: "password Northwind#2026", "api key=abc", "token is xyz". (A PIN is
# PIN_VALUE's.) The value runs to a space or a quote: "password: Abc;123xyz" is all secret (review 2026-11-03: the
# part after ";" got through); the punctuation that ends it ("x9y8z7);") is left out.
REQUEST_SECRET = re.compile(
    r"(?i)\b(?P<label>pass(?:word|code|phrase)|pwd|otp|one[- ]time (?:code|password)|token|api[ _-]?key|secret(?: key)?)"
    r"\b\s*(is\s+|[:=]\s*)?([\"'\u201c\u2018]?)([^\s\"'\u201d\u2019]{2,199}?[^\s\"'\u201d\u2019.,;:!?)\]])(?=[.,;:!?)\]]*(?:[\s\"'\u201d\u2019]|$))")
_WORD = re.compile(r"[^\s\"'\u201c\u201d\u2018\u2019(),;<>\[\]{}=]+")
# A whole word between spaces or quotes, checked as well as its parts: a password with a ";" or "," in it is one
# word here and two for _WORD.
_SPACED_WORD = re.compile(r"[^\s\"'\u201c\u201d\u2018\u2019<>]+")
_MAX_KEPT = 200


def _secrets_file():
    base = Path(os.environ.get("DECURIX_DIR") or (Path(os.environ.get("HOME", "/home/headless")) / ".decurix"))
    return base / "withheld-secrets"


def _hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def request_secrets(text):
    """The secrets a request's own words give after their names (a password, a PIN, a key) and the card numbers it
    spells out: [text]."""

    # Said as one ("password is x", "pin: x", quoted), any word; written straight after the name, only what has a
    # digit or a mark in it. Found live (2026-10-22): "No OTP has come to the client's phone yet" kept "has"
    # as a secret, and every "has" of every later reply and step read "[withheld]".
    text = text or ""
    found = [m.group(4).rstrip(".!?") for m in REQUEST_SECRET.finditer(text) if _request_value(m)]
    found += [m.group(1) for m in SPOKEN_SECRET.finditer(text)]
    found += request_pins(text)
    found += [re.sub(r"\D", "", text[a:b]) for a, b in _card_spans(text)]
    return [s for s in dict.fromkeys(found) if len(s) >= 3 and s.lower() not in {"for", "the", "and", "from", "field"}]


def _request_value(m):
    """Whether a REQUEST_SECRET match holds a secret's value. Said as one ("is", ":", quoted), any word but a label;
    written straight after a password's or a code's name, only what has a digit or a mark in it; straight after
    "token", "secret" or "key", only a machine's token (long, letters and digits) - "token number: 7781" and "Ack
    token 112233445566" are a queue's and a reference's numbers (recheck 2026-11-03: "number:" was remembered and
    withheld everywhere). A folder is never one ("pwd /home/user/work")."""

    value = m.group(4).rstrip(".!?:")
    if not value or value.startswith(("/", "~")):
        return False
    if value.isalpha() and value.lower() in {"number", "no", "code", "id", "field", "box", "reset"}:
        return False
    if m.group(2) or m.group(3):
        return True
    if m.group("label").lower().startswith(("token", "api", "secret")):
        return len(value) >= 16 and re.search(r"[A-Za-z]", value) is not None and re.search(r"\d", value) is not None
    return not value.isalpha()


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

    def is_known(word):
        return _hash(word) in known

    def hide(match):
        word = match.group(0)
        if is_known(word):
            return "[withheld]"
        bare = word.rstrip(".:!?,;")
        if bare != word and is_known(bare):
            return "[withheld]" + word[len(bare):]
        return word

    # A remembered card number written with spaces or dashes ("4111 1111 1111 1111") is one number.
    def hide_card(match):
        return "[withheld]" if is_known(re.sub(r"\D", "", match.group(0))) else match.group(0)
    text = CARD_NUMBER.sub(hide_card, text)
    return _WORD.sub(hide, _SPACED_WORD.sub(hide, text))


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
# What a card number is written as: groups of four (Amex 4-6-5) split by spaces or dashes.
_CARD_GROUPED = re.compile(r"^\d{4}([ -])\d{4}\1\d{4}\1\d{1,7}$|^\d{4}([ -])\d{6}\2\d{4,5}$")
# The word that last names the number before it: a card's, or another identifier's (an account, an order, a
# reference), which a long number that passes the card checksum often is (one in ten does).
_CARD_WORD = re.compile(r"(?i)\b(?:card|credit|debit|visa|master\s?card|rupay|amex|maestro|cc)\b")
_NOT_CARD_WORD = re.compile(
    r"(?i)\b(?:a/c|account|acct|order|invoice|inv|ref(?:erence)?|utr|awb|tracking|total|amount|phone|mobile"
    r"|aadhaa?r|uan|policy|folio|ack(?:nowledge?ment)?|irn|txn|transaction|po|bill|voucher|serial|imei)\b")


def _card_spans(text):
    """The (start, end) of every card number text spells out: a number that passes the card checksum and is written
    as one (grouped in fours) or named as one ("card no 4111111111111111"), unless the word before names another
    identifier ("A/c 50100123456789" is an account)."""

    spans = []
    for match in CARD_NUMBER.finditer(text or ""):
        raw = match.group(0)
        digits = re.sub(r"\D", "", raw)
        if not (13 <= len(digits) <= 19 and _luhn(digits)):
            continue
        before = text[max(0, match.start() - 30):match.start()]
        card = list(_CARD_WORD.finditer(before))
        other = list(_NOT_CARD_WORD.finditer(before))
        last_card = card[-1].end() if card else -1
        last_other = other[-1].end() if other else -1
        if last_other > last_card:
            continue
        if last_card >= 0 or _CARD_GROUPED.match(raw):
            spans.append((match.start(), match.end()))
    return spans

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

    for match in PIN_VALUE.finditer(text):
        if pin_kind(text, match) != "postal":
            return "PIN"

    for match in CARD_NUMBER.finditer(text):
        digits = re.sub(r"\D", "", match.group(0))
        if 13 <= len(digits) <= 19 and _luhn(digits):
            return "card number"

    return None


# A credential written out in a sentence: `the password "Ttlh"`, `otp: '1234'`. A task's
# own words often carry one, and a recording's title and description are shown to
# anyone who opens the page and exported with it. (A PIN is PIN_VALUE's: digits only, so "pin 'Mumbai'" is none.)
SPOKEN_SECRET = re.compile(
    r"(?i)\b(?:pass(?:word|code|phrase)|secret|token|api[ _-]?key|otp|cvv2?)\b[^\"'\u201c\u2018\n]{0,20}?"
    r"[\"'\u201c\u2018]([^\"'\u201d\u2019\n]{1,200})[\"'\u201d\u2019]"
)


# A credential written after its name and a colon or equals sign, unquoted: "- password: Lift$2026", "pwd=x",
# "DB_PASSWORD=x", "client_secret: x". Found live (2026-10-21): a worker's report listed the client's portal password
# read from the person's client list. (Not "PIN": that is the PIN rule's, above. Not "tokens": a usage line's count.)
_SECRET_NAME = (r"pass(?:word|code|phrase)|passwd|pwd|api[ _-]?(?:key|secret)|secret(?:[ _-]?key)?|client[ _-]?secret"
                r"|access[ _-]?(?:key|token)|refresh[ _-]?token|auth(?:entication)?[ _-]?token|private[ _-]?key|token")
# The value runs to a space or a quote ("password: Abc;123xyz" is all secret; review 2026-11-03 found the part after
# ";" let through); the punctuation that ends it is left out.
LABELLED_SECRET = re.compile(
    r"(?i)(?<![A-Za-z])(?:" + _SECRET_NAME + r")(?![A-Za-z])\s*[:=]\s*[`\"'“‘]?([^\s`\"'”’]{2,199}?[^\s`\"'”’.,;:!?)\]])(?=[.,;:!?)\]]*(?:[\s`\"'”’]|$))")

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
    # "the password is Lift$2026", "Password Northwind2026" (a value with a digit or a mark: "the password is
    # incorrect" and "password reset" are a screen's words; review 2026-11-03 found the form with no "is" let through).
    ("password said", re.compile(r"(?i)(?<![A-Za-z])(?:pass(?:word|code|phrase)|passwd|pwd|api[ _-]?key|secret[ _-]?key"
                            r"|client[ _-]?secret|access[ _-]?token)\s+(?:(?:is|was)\s+)?[`\"'“‘]?"
                            r"([^\s`\"'”’]{2,199}?[^\s`\"'”’.,;:!?)\]])(?=[.,;:!?)\]]*(?:[\s`\"'”’]|$))"), 1),
    # "OTP 529749", "verification code: 4821", "CVV 123".
    ("one-time code", re.compile(r"(?i)(?<![A-Za-z])(?:otp|one[- ]time (?:code|password|passcode)|verification code"
                                 r"|security code|auth(?:entication)? code|2fa code|mfa code|totp"
                                 r"|log[- ]?in code|sign[- ]?in code|access code)(?![A-Za-z])"
                                 r"[^\d\n]{0,12}?(?<!\d)(\d{4,8})(?!\d)"), 1),
    ("card security code", re.compile(r"(?i)(?<![A-Za-z])(?:cvv2?|cvc2?)(?![A-Za-z])[^\d\n]{0,6}?(?<!\d)(\d{3,4})(?!\d)"),
     1),
    # A long random token next to a word that says what it is: "client key 9f8a7b...", "secret: a1B2...".
    # (Not "key" alone: "the key figures" in a letter is no secret's name. Not past another identifier's name: in
    # "e-invoice token generated, IRN 6f2b..." the hash is the IRN's - review 2026-11-03.)
    ("key or token", re.compile(r"(?i)(?<![A-Za-z])(?:(?:api|access|secret|client|private|licen[cs]e)[ _-]?key|apikey"
                                r"|secret|token|password|passwd|credentials?|signature)"
                                r"(?![A-Za-z])(?:(?!\b(?:irn|hash|ack|utr|gstin|ref|reference|id|no|number|txn)\b)[^\n])"
                                r"{0,30}?(?<![A-Za-z0-9+/_=.-])([A-Za-z0-9+/_=.-]{20,})"), 1),
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
        if bare.startswith(("/", "~", "./", "../")):
            return False  # "pwd /home/user/work" is the shell's command and a folder
        return re.search(r"[\d!@#$%^&*+=~|\\/<>]", bare) is not None or re.search(r"[a-z][A-Z]", bare) is not None
    if label == "bearer token":
        return len(value) >= 16 and _tokenish(value)
    if label == "secret in a web address":
        return len(value) >= 8 and _tokenish(value)
    if label == "key or token":
        return (re.search(r"[A-Za-z]", value) is not None and re.search(r"\d", value) is not None
                and "@" not in value and _entropy(value) >= 3.5)
    return True  # a password after its name


def _withhold_pins(text):
    """text with every PIN it gives withheld where it is written - a card's, a bank's, a sign-in's, and six digits after
    a bare "PIN" that nothing marks as an address's - and every postal code kept ("Bengaluru PIN 560001")."""

    def hide(m):
        if pin_kind(text, m) == "postal":
            return m.group(0)
        start = m.start("value") - m.start(0)
        return m.group(0)[:start] + "[withheld]"
    return PIN_VALUE.sub(hide, text)


def _withhold_cards(text):
    """text with every card number it spells out (_card_spans) withheld."""

    for start, end in reversed(_card_spans(text)):
        text = text[:start] + "[withheld]" + text[end:]
    return text


def _withhold_shapes(text):
    """text with every credential it spells out, by name or by shape, replaced by [withheld]. Raises on an error (the
    callers fail closed)."""

    text = _withhold_cards(_withhold_pins(text))
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

    # A credential in an ordinary field's value, a control's name or a window's title is kept by no recording (review
    # 2026-11-03: only a secret *field* was withheld, so a token pasted into a plain text box was stored).
    for key in ("value", "name", "window_title"):
        value = public.get(key)
        if isinstance(value, str) and value:
            credential = credential_in(value)
            if credential:
                public[key] = None
                withheld[key] = f"looks like a {credential}"

    if withheld:
        public["withheld"] = withheld

    return public


# ============================================================
# The last pass inside every writer
#
# Found in review (2026-11-03): privacy was applied by hand at each place that wrote, and every place that forgot leaked
# - a task's recording titled with the raw request, a demonstration step's summary, a playbook's stored request. So each
# writer (recording's events and meta, evidence notes, progress lines, the playbook store) ends with scrub(): one pass
# over every string it is about to write. The helpers above stay as the first pass, for the better wording ("withheld:
# password field"); scrub only makes forgetting one impossible.
# ============================================================

# How deep scrub follows nested dicts and lists. Nothing a writer stores is near this; past it a value is held back.
_SCRUB_DEPTH = 40


def _scrub_text(text):
    out = withhold_known(text)
    if out == HELD_BACK:
        return out
    kind = credential_in(out)
    # A long number that passes the card checksum is withheld by withhold_known only where it reads as a card (by the
    # words before it): credential_in's context-free card test is for a value typed into a field (withhold_text), and
    # here it would wipe whole notes for an account or invoice number. Every other kind it finds is withheld whole.
    if kind and kind != "card number":
        return HELD_BACK if kind == "text that could not be checked for secrets" else f"[withheld: looks like a {kind}]"
    return out


def _element_shaped(value):
    return "role" in value and ("value" in value or "handle" in value)


def _scrub(value, depth):
    if depth > _SCRUB_DEPTH:
        return HELD_BACK
    if isinstance(value, str):
        return _scrub_text(value)
    if isinstance(value, dict):
        out = {key: _scrub(item, depth + 1) for key, item in value.items()}
        if _element_shaped(value):
            reason = secret_field(value)
            if reason:
                if out.get("value"):
                    out["value"] = None
                out.setdefault("secret", reason)
        return out
    if isinstance(value, (list, tuple)):
        return [_scrub(item, depth + 1) for item in value]
    if value is None or isinstance(value, (bool, int, float)):
        return value
    return _scrub_text(str(value))


def scrub(obj):
    """`obj` (a string, or dicts and lists of them) with every string through withhold_known and credential_in, and the
    value of every element-shaped dict that is a secret field blanked. Returns a new object; never raises. On an error
    the result is a placeholder - HELD_BACK, or {"held_back": HELD_BACK} for a dict, [] for a list - never `obj`."""

    try:
        return _scrub(obj, 0)
    except Exception:  # noqa: BLE001 - fail closed
        if isinstance(obj, dict):
            return {"held_back": HELD_BACK}
        if isinstance(obj, (list, tuple)):
            return []
        return HELD_BACK


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
