import hashlib
import json
import re
import time
import uuid
from contextlib import contextmanager
from pathlib import Path

from decurix_agent import config, filelock
from decurix_agent.computer import elements as el
from decurix_agent.computer import privacy


# ============================================================
# The approval gate for on-screen actions
#
# The same rule every engine's prompt already states - stop before paying,
# before sending a message as the human, before anything with real-world
# consequences - enforced on what an action is about to touch, not on what
# the model says it is doing. It is the on-screen counterpart of TheBot's
# internal/safety: asked of the model, and checked here where it cannot be
# argued with.
#
# How a pause and its approval travel, for all three engines alike:
#
#   act() on a Send/Pay-like control
#     -> check() finds no grant: records a *pending* approval, the action
#        does not happen, and the engine is told NEEDS_APPROVAL
#     -> the engine's harness turns that into ask_for_query (the chat's
#        "Needs your approval" card)
#   the human replies
#     -> watchers.command_watcher calls resolve_reply() before handing the
#        reply to the harness: an approval turns the pending entry into a
#        one-shot *grant* for exactly that action; anything else drops it
#   the engine repeats the action
#     -> check() consumes the grant and lets it through
#
# State is a small JSON file rather than module globals because the checker
# and the resolver are different processes: `gui` runs as a shell command
# (Ollama, TheBot), or inside Claude Code's MCP server, while replies arrive
# in the agent itself.
#
# The patterns are narrow on purpose, for the reason internal/safety gives:
# a gate that stops "Save settings" teaches a human to approve without
# reading.
# ============================================================

STATE_DIR = config.DECURIX_DIR / "computer"
STATE_FILE = STATE_DIR / "approval.json"

from decurix_agent.computer import modes

GRANT_SECONDS = 15 * 60

# Controls whose own label says they commit the human to something.
CONSEQUENTIAL_CONTROL = re.compile(
    r"^\s*("
    r"send|send now|send (it|message|email|e-mail|mail|newsletter|invite|invitation|reply|sms|text|money|payment)"
    r"|pay|pay now|make (a )?payment|submit payment|authori[sz]e payment"
    r"|confirm (and pay|payment|purchase|order|transfer|booking)"
    r"|buy|buy now|purchase|place (your |my )?order|complete (order|purchase|payment|checkout)"
    r"|checkout|check out(?=\s*(now)?\s*$)|proceed to (checkout|payment)"
    r"|transfer( money| funds| now)?|donate|withdraw"
    r"|publish|post|post now|tweet|reply all"
    r"|delete (my )?account|close (my )?account|cancel (my )?(subscription|membership|account|order)"
    r"|book now|reserve now|subscribe( now)?"
    r")\b",
    re.IGNORECASE,
)

# Roles that carry out their label when activated.
ACTIVATING_ROLES = {"button", "link", "menuitem", "toggle", "listitem", "icon", "option"}

# Windows and pages where Enter sends what was just typed.
MESSAGING_CONTEXT = re.compile(
    r"(whatsapp|telegram|messenger|slack\.com|\bslack\b|discord|signal|teams\.microsoft|"
    r"mail\.google|gmail|outlook|mail\.yahoo|proton\.me|protonmail|thunderbird|"
    r"\bcompose\b|new message|linkedin\.com/messaging|twitter\.com|x\.com/)",
    re.IGNORECASE,
)

# Channels and external network endpoints
EXTERNAL_CHANNELS = re.compile(
    r"\b(email|mail|gmail|outlook|yahoo|proton|slack|telegram|discord|whatsapp|"
    r"teams|twitter|x\.com|linkedin|facebook|stripe|paypal|checkout)\b",
    re.IGNORECASE,
)

ENTER_KEY = re.compile(r"(^|\+)(return|enter|kp_enter)$", re.IGNORECASE)

# A field Enter searches from, wherever it is. Found live on LinkedIn
# (2026-10-03): Enter in Messaging's "Search messages" box - looking a person
# up, the step before any message is written - was stopped as "sends what was
# typed as a message", every run, and a yes to it sent nothing and found
# nobody. Named for searching at the start of its label, or a search field by
# its own markup; the message box beside it ("Write a message...") is not.
SEARCH_FIELD = re.compile(r"^\s*(search|find|filter|look up)\b", re.IGNORECASE)

# The same words soul's internal/safety.IsApproval accepts, so a reply means
# the same thing to the GUI gate as it does to TheBot's own gate.
APPROVAL_RE = re.compile(
    r"^\s*(yes|yep|yeah|yup|y|ok|okay|sure|approved?|i approve|confirm(ed)?|go( ahead)?|"
    r"go for it|do it|proceed|continue|lgtm|sounds good|fine|allowed?|granted|you can|"
    r"please do|affirmative)\b",
    re.IGNORECASE,
)
REFUSAL_RE = re.compile(
    r"\b(no|nope|don'?t|do not|stop|cancel|abort|wait|hold on|never|not now|deny|denied|reject(ed)?)\b",
    re.IGNORECASE,
)


def is_approval(reply):
    """A reply that says yes, and does not also say no."""

    reply = (reply or "").strip()
    return bool(reply) and bool(APPROVAL_RE.match(reply)) and not REFUSAL_RE.search(reply)


# A reply that turns the action down and asks for nothing in its place: "no",
# "don't send it", "stop". "No, send it to Priya instead" is an instruction.
REFUSAL_LEAD = re.compile(r"^\s*(no|nope|don'?t|do not|stop|cancel|abort|never|deny|denied|reject(ed)?)\b", re.IGNORECASE)
REDIRECTS = re.compile(r"\b(instead|rather|but|use|try|change|make it)\b", re.IGNORECASE)


def is_refusal(reply):
    """A reply that says no to the action and does not say to do something else."""

    reply = (reply or "").strip()
    return bool(REFUSAL_LEAD.match(reply)) and not REDIRECTS.search(reply)


def is_external_action(kind, element=None, text=None, keys=None, context=None):
    """
    Check if an on-screen action targets an external communication channel,
    payment provider, or public service (Dual-zone policy).
    """

    context = context or {}
    where = " ".join(filter(None, [context.get("window"), context.get("url"),
                                   element.get("window_title") if element else None]))

    if element is not None:
        name = element.get("name") or ""
        if element.get("role") in ACTIVATING_ROLES:
            if CONSEQUENTIAL_CONTROL.match(name) or EXTERNAL_CHANNELS.search(name):
                return True

    if MESSAGING_CONTEXT.search(where) or EXTERNAL_CHANNELS.search(where):
        if kind == "key" and keys and any(ENTER_KEY.search(k.strip()) for k in keys):
            return True
        if kind == "type" and text and ("\n" in text or "@" in text):
            return True

    return False


# Where a click goes somewhere rather than does something. Found live in the web app:
# picking the "Purchase" app from its menu search (an option in a listbox) was
# stopped as a purchase, so no purchasing workflow could get past the menu.
# - An option is a choice in a list; it commits nothing. The button that
#   follows it ("Confirm", "Pay now") is what acts, and is still gated.
# - A one-word link, menu item or tab inside the site's own navigation bar is a
#   place ("Purchase", "Sales", "Invoicing"). A menu item in an action menu, a
#   button, and any label of more words ("Send & Print", "Pay now") still stop.
NAVIGATION_PATH = re.compile(r"(^|>\s*)navigation\b")

# A link named for a kind of record whose first word only looks like an act: "Purchase Invoice", "Purchase Order",
# "Transfer Request". Not "post": posting an invoice is the act of booking it. Found live in the web app: its sidebar link "Purchase Invoice" was stopped as a purchase,
# and every bill a worker entered began with a person asked to approve opening a list. Only those first words, only
# for links, menu items and tabs, and only a record noun after them - "Send invoice" and "Pay now" still stop.
RECORD_NAME = re.compile(
    r"^\s*(purchase|transfer)\s+(invoices?|orders?|receipts?|requests?|returns?|ledgers?|registers?|reports?|"
    r"analytics|history|list|settings|entr(?:y|ies)|summary|trends|quotations?|requisitions?|analysis|items?|taxes|"
    r"records?|notes?|vouchers?|bills?)"
    # ...and the places such records are looked at: "Purchase Invoice List", "Purchase Order Report". Found live:
    # a worker looking for an earlier copy of a bill before entering it was stopped for a yes at the list of them.
    r"(?:\s+(?:list|lists|report|reports|register|summary|analytics|analysis|history|trends|dashboard|view|"
    r"search|settings))?\s*$",
    re.IGNORECASE,
)


DOCUMENT_NAME = re.compile(
    r"^\s*(purchase|transfer)\s+(invoices?|receipts?|ledgers?|registers?|reports?|quotations?|requisitions?|"
    r"vouchers?)\s*$",
    re.IGNORECASE,
)


def searches(element):
    """Whether Enter in this field runs a search rather than sending what was typed."""

    if not element:
        return False
    handle = element.get("handle") or {}
    if element.get("role") == "searchbox" or handle.get("dom_role") == "searchbox" or handle.get("input_type") == "search":
        return True
    return element.get("role") in ("textbox", "combobox", "entry") and bool(SEARCH_FIELD.match(element.get("name") or ""))


def _navigates(element):
    role = element.get("role")
    if role == "option":
        return True
    name = (element.get("name") or "").strip()
    if role == "button":
        # A button whose whole label is a kind of document opens that kind
        # of document. Found live: a page's shortcut button to its list of
        # purchase invoices stopped a task for a yes. Only nouns nobody can
        # buy or move - "Purchase items" and "Purchase order" on a button
        # can be acts, and still stop.
        return bool(DOCUMENT_NAME.match(name))
    if role not in ("link", "menuitem", "tab"):
        return False
    if RECORD_NAME.match(name):
        return True
    return len(name.split()) == 1 and bool(NAVIGATION_PATH.search(element.get("path") or ""))


def assess(kind, element, text=None, keys=None, context=None):
    """
    Why this action needs a human's approval first, or None.

    `context` is {"window": active window title, "url": active tab URL,
    "focused": the field that has the keyboard, when the caller knows it}.
    """

    stage = modes.get_stage()

    # Stage 1: Observation mode stops mutating actions to observe without external side-effects
    if stage == modes.STAGE_OBSERVATION:
        if kind in ("click", "double_click") and element is not None and element.get("role") in ACTIVATING_ROLES:
            return "worker is in Stage 1 Observation (shadowing) mode - mutating action paused"
        if kind == "key" and keys and any(ENTER_KEY.search(k.strip()) for k in keys):
            return "worker is in Stage 1 Observation (shadowing) mode - key submission paused"

    context = context or {}
    where = " ".join(filter(None, [context.get("window"), context.get("url"),
                                   element.get("window_title") if element else None]))

    if kind in ("click", "double_click") and element is not None:
        name = element.get("name") or ""
        if element["role"] in ACTIVATING_ROLES and CONSEQUENTIAL_CONTROL.match(name) and not _navigates(element):
            label = " ".join(name.split())[:60]
            return f'"{label}" sends, pays, buys or publishes something on your behalf'

    if kind in ("click", "double_click") and element is not None:
        asked = confirms_permanently(element)
        if asked:
            return f'this answers "{asked}" - it cannot be taken back'

    if kind == "key" and keys and any(ENTER_KEY.search(k.strip()) for k in keys):
        # Where the keyboard is decides what Enter does: in a search box it
        # searches. Unknown, it is taken to send - the cautious reading.
        if MESSAGING_CONTEXT.search(where) and not searches(element or context.get("focused")):
            return "pressing Enter here sends what was typed as a message"

    if kind == "type" and text and "\n" in text and MESSAGING_CONTEXT.search(where):
        return "typing a new line here sends what was typed as a message"

    return None


# A dialog's plain yes, and what such a dialog asks when the act cannot be
# undone. Found live (2026-10-13): a worker pressed Ctrl+S on a draft its
# replay had just saved, and the application - where a saved draft's next
# action is to submit it to the books - asked "Confirm: Permanently Submit
# PINV-26-00021?". The worker answered No itself; nothing would have stopped
# a Yes, since the gate looks at a control's own label and this one says
# only "Yes". The dialog's question is the label that matters: it is the
# application saying, in its own words, that this is the consequential step.
CONFIRM_ANSWER = re.compile(r"^\s*(yes|ok|okay|confirm|continue|proceed|i'?m sure|i am sure|yes,? [\w ]{1,30})\s*[.!]?\s*$",
                            re.IGNORECASE)
PERMANENT_QUESTION = re.compile(
    r"\b(permanent(ly)?|irreversibl[ey]|cannot be (undone|reversed)|can'?t be (undone|reversed)|"
    r"submit|send|pay|publish|post|transfer|delete|cancel (this|the) (order|invoice|payment|document|booking))\b",
    re.IGNORECASE)
DIALOG_IN_PATH = re.compile(r"\b(?:alertdialog|dialog) \"([^\"]*)\"", re.IGNORECASE)


def confirms_permanently(element):
    """The question a yes-button answers when its dialog asks to do something that cannot be taken back, or None."""

    if (element.get("role") or "") not in ACTIVATING_ROLES or not CONFIRM_ANSWER.match(element.get("name") or ""):
        return None
    dialogs = DIALOG_IN_PATH.findall(element.get("path") or "")
    asked = dialogs[-1].strip() if dialogs else ""
    return asked if asked and PERMANENT_QUESTION.search(asked) else None


def _fingerprint(kind, element, text, keys, point):
    parts = [
        kind,
        (element or {}).get("role", ""),
        " ".join(((element or {}).get("name") or "").split()).lower(),
        (element or {}).get("window_title", ""),
        text or "",
        "+".join(keys or []),
    ]
    if element is None and point:
        parts.append(f"{point[0]},{point[1]}")
    return hashlib.sha256("\x1f".join(parts).encode("utf-8")).hexdigest()[:24]


# How long anything waits for the approval store. Every holder keeps it for a
# file read and write, so a wait this long means a holder is stuck (a `gui`
# process frozen mid-write); the agent's command watcher settles every reply
# through here and must never wait on that forever.
LOCK_TIMEOUT_SECONDS = 10


@contextmanager
def _locked():
    STATE_DIR.mkdir(parents=True, exist_ok=True)

    with open(STATE_DIR / "approval.lock", "w") as lock:
        deadline = time.monotonic() + LOCK_TIMEOUT_SECONDS
        while not filelock.try_exclusive(lock):
            if time.monotonic() >= deadline:
                raise TimeoutError("the approval store is held by something that is not letting go")
            time.sleep(0.05)

        try:
            state = json.loads(STATE_FILE.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            state = {}

        state.setdefault("pending", None)
        state["grants"] = [g for g in state.get("grants", []) if g.get("expires", 0) > time.time()]

        yield state

        tmp = STATE_FILE.with_suffix(".tmp")
        tmp.write_text(json.dumps(state), encoding="utf-8")
        tmp.replace(STATE_FILE)


class Decision:
    def __init__(self, allowed, reason=None, approved=False, request=None):
        self.allowed = allowed
        self.reason = reason
        self.approved = approved
        self.request = request


def describe_action(kind, element, text=None, keys=None, point=None, withheld=None):
    """
    One line for an action. Typed text that privacy.py does not keep - it
    went into a password field, or is itself a credential - is named as
    withheld, never quoted: this line is the chat step, the task record and
    the approval request alike. `withheld` is the reason, when the caller
    already knows the text went into a secret field this cannot see.
    """

    target = el.describe(element) if element else (f"the point {point[0]},{point[1]}" if point else "the screen")
    window = (element or {}).get("window_title")
    where = f' in "{window}"' if window else ""

    if kind == "type":
        if withheld is None:
            _, withheld = privacy.withhold_text(element, text)
        return f"type {privacy.shown_text(text, withheld)} into {target}{where}"
    if kind == "key":
        return f"press {' '.join(keys or [])} in {target}{where}"
    return f"{kind.replace('_', ' ')} {target}{where}"


def check(kind, element, text=None, keys=None, point=None, context=None):
    """
    Decide whether an action may run now. Consumes a matching grant.
    """

    reason = assess(kind, element, text=text, keys=keys, context=context)

    if reason is None:
        return Decision(True)

    fingerprint = _fingerprint(kind, element, text, keys, point)
    description = describe_action(kind, element, text=text, keys=keys, point=point)

    with _locked() as state:
        for grant in state["grants"]:
            if grant["fingerprint"] == fingerprint:
                state["grants"].remove(grant)
                return Decision(True, reason=reason, approved=True)

        request = {
            "id": uuid.uuid4().hex[:12],
            "fingerprint": fingerprint,
            "reason": reason,
            "action": description,
            "created": time.time(),
        }
        state["pending"] = request

    return Decision(False, reason=reason, request=request)


def pending():
    """The approval an engine is currently stopped on, or None."""

    with _locked() as state:
        return state["pending"]


def resolve_reply(reply):
    """
    Settle the pending approval with the human's reply.

    Returns "granted", "declined", or None when nothing was pending.
    """

    with _locked() as state:
        request = state["pending"]

        if request is None:
            return None

        state["pending"] = None

        if not is_approval(reply):
            return "declined"

        state["grants"].append({
            "fingerprint": request["fingerprint"],
            "action": request["action"],
            "expires": time.time() + GRANT_SECONDS,
        })

        return "granted"


def has_grant():
    with _locked() as state:
        return bool(state["grants"])


def reset():
    """Forget pending approvals and grants - a new task starts clean."""

    with _locked() as state:
        state["pending"] = None
        state["grants"] = []


# What `gui` exits with when the gate stopped it (computer/cli.py).
EXIT_NEEDS_APPROVAL = 3

MARKER = "NEEDS_APPROVAL:"


def blocked_command(exit_code, output):
    """
    If a shell command was a `gui` action the gate stopped, the words to put
    in front of the human; otherwise None.

    For the engines that reach the screen through shell commands (the Ollama
    loop, TheBot's operator): their harness reads this off the command's
    result and pauses the task itself, rather than trusting a 3B model to
    notice the pause and ask.
    """

    # Any line, not just the first: a small model chains several gui
    # commands in one COMMAND, and the shell's exit code is the last one's.
    # The stopped action still did not happen, but the task must pause on it.
    for line in (output or "").splitlines():
        line = line.strip()
        if line.startswith(MARKER):
            if exit_code == EXIT_NEEDS_APPROVAL or pending() is not None:
                return line[len(MARKER):].strip()

    return None


def request_text(request):
    """The words an engine and a human see for a pending approval."""

    return (
        f"Approval needed before I {request['action']}: {request['reason']}. "
        "Nothing was clicked or sent. Reply yes to let me do it, or tell me what to do instead."
    )
