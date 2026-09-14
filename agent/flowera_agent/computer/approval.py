import fcntl
import hashlib
import json
import re
import time
import uuid
from contextlib import contextmanager
from pathlib import Path

from flowera_agent.computer import elements as el


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
#     -> the engine's harness turns that into ask_for_help (the chat's
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

STATE_DIR = Path.home() / ".flowera" / "computer"
STATE_FILE = STATE_DIR / "approval.json"

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

ENTER_KEY = re.compile(r"(^|\+)(return|enter|kp_enter)$", re.IGNORECASE)

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


def assess(kind, element, text=None, keys=None, context=None):
    """
    Why this action needs a human's approval first, or None.

    `context` is {"window": active window title, "url": active tab URL}.
    """

    context = context or {}
    where = " ".join(filter(None, [context.get("window"), context.get("url"),
                                   element.get("window_title") if element else None]))

    if kind in ("click", "double_click") and element is not None:
        name = element.get("name") or ""
        if element["role"] in ACTIVATING_ROLES and CONSEQUENTIAL_CONTROL.match(name):
            label = " ".join(name.split())[:60]
            return f'"{label}" sends, pays, buys or publishes something on your behalf'

    if kind == "key" and keys and any(ENTER_KEY.search(k.strip()) for k in keys):
        if MESSAGING_CONTEXT.search(where):
            return "pressing Enter here sends what was typed as a message"

    if kind == "type" and text and "\n" in text and MESSAGING_CONTEXT.search(where):
        return "typing a new line here sends what was typed as a message"

    return None


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


@contextmanager
def _locked():
    STATE_DIR.mkdir(parents=True, exist_ok=True)

    with open(STATE_DIR / "approval.lock", "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)

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


def describe_action(kind, element, text=None, keys=None, point=None):
    target = el.describe(element) if element else (f"the point {point[0]},{point[1]}" if point else "the screen")
    window = (element or {}).get("window_title")
    where = f' in "{window}"' if window else ""

    if kind == "type":
        return f"type {el._quote(text or '', 60)} into {target}{where}"
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


def request_message(request):
    """The words an engine and a human see for a pending approval."""

    return (
        f"Approval needed before I {request['action']}: {request['reason']}. "
        "Nothing was clicked or sent. Reply yes to let me do it, or tell me what to do instead."
    )
