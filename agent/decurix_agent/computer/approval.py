import hashlib
import json
import re
import time
import uuid
from contextlib import contextmanager

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
    r"|checkout|check out(?=\s*(now)?\s*$)"
    # The step that leads straight into a committing act is put to the person like the act: "proceed to checkout"
    # always was, and "proceed to file / pay / submit" is the same step (review 2026-11-02: a test pinned "PROCEED TO
    # FILE" as ungated).
    r"|proceed to (checkout|payment|pay|file|filing|submit|submission|send|transfer)"
    r"|transfer( money| funds| now)?|donate|withdraw"
    r"|publish|post|post now|tweet|reply all"
    r"|delete (my )?account|close (my )?account|cancel (my )?(subscription|membership|account|order)"
    # Deleting a named thing ("Delete user", "Delete invoice", "Delete permanently") is not taken back. A bare
    # "Delete" / "Remove" (a row of a form being filled, a chip) is left to the dialog that confirms it
    # (confirms_permanently), which asks then.
    r"|(delete|erase|purge|destroy)( (this|the|selected|all|my))? [\w-]{2,}"
    r"|book now|reserve now|subscribe( now)?"
    # Filing a return or a declaration with an authority binds the person in law and is not taken back; offsetting a
    # liability spends their balance (2026-10-21: a portal's "file with <signature>" and "offset liability" buttons
    # passed unasked). "File" alone is a menu and "file returns" / "file statement" open the page where filing is done,
    # so only the step that says how it is signed ("file with <method>", "file <form> with <method>") and these.
    r"|file( [\w-]+)? with \w+|file now|e-?file( now)?"
    r"|submit (the )?(return|declaration|application|claim|filing)s?"
    r"|(offset|set[- ]off) (the )?liabilit(y|ies)"
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


READ_SYSTEM = """A person was asked to approve one action an AI worker is about to take (below). Read their reply \
the way a colleague would - in any language, slang, shorthand or emoji - and say what it decides about THAT action, now:
- "yes": they approve it now, with no condition (remarks that set none are fine: "yes - the client has paid; go ahead").
- "if": they approve it only once something is checked or done first ("yes, provided you checked X", "ok after you fix \
Y", "file it if Z"). Only a condition on THIS action counts: what they ask to be done before a later step ("go ahead to \
payment, and check X before you file") is an instruction for that step, and this action is a "yes". So is one that \
can only be met after this action - to show or check what it leads to ("go ahead, show me the next screen before you \
click there"): this action is what brings that about.
- "no": they do not approve it now (not yet, wait, fix something first, never).
- "unclear": the reply neither approves nor refuses it - a question back to the worker, or about something else.
And "wants": what they ask the worker to tell them, show them or do before they decide (a question to answer, a \
report), in their words - empty when they ask nothing of it.
Reply with JSON only: {"decision": "yes" | "if" | "no" | "unclear", "condition": "what must hold or be done first, in \
their words, or empty", "wants": "..."}"""

DECISIONS = ("yes", "if", "no", "unclear")

# The words a reply made of nothing else plainly says yes, or plainly says no, with: such a reply needs no model. Any
# other word - a condition, a question, another language, an emoji - and the reply is the model's to read.
_PLAIN_YES = set("yes yep yeah yup y ok okay k sure approved approve i confirm confirmed go ahead for it do proceed "
                 "continue lgtm sounds good fine allowed granted you can please".split())
_PLAIN_NO = set("no nope don't dont do not stop cancel abort never deny denied reject rejected wait hold on now yet "
                "please sorry".split())
# With no model to ask, a yes that may carry a condition is not taken for a plain one.
_MAY_CONDITION = re.compile(r"\b(if|provided|unless|once|after|before|when|only|but|first|as long as|so long as)\b", re.I)


# The model's readings in this process, by the action and the reply: {(action, text): (when, reading)}. Keyed by the
# action too: "yes, if the totals match" read for one action is not the reading of the same words for another.
_READ = {}
READING_SECONDS = 10 * 60


def _plain(text, words):
    """Whether every word of text is one of words."""

    tokens = re.findall(r"[a-z']+|[^\sa-z'.,!;:-]", text.lower())
    return bool(tokens) and all(t in words for t in tokens)


def reading(reply, action="", plain=True):
    """
    What a reply to an approval decides about that one action, read once for every part of the worker that acts on
    it: {"decision": "yes" | "if" | "no" | "unclear", "condition": what must hold first, or ""}. Only "yes" grants.

    plain=False: whether the reply is a yes to THIS action is itself the question (a reply to another question, held
    against the action the gate stops next - _reply_grants), so code never settles a yes and with no model the answer
    is "unclear"; a plain no is still a no.

    Code decides only a reply that plainly says yes or no (every word of it a plain yes, or a plain no); every other
    reply is the model's reading. Found live (2026-10-23): "Provided you looked in the client's messages and found
    nothing saying that record was cancelled, yes - go ahead" was taken for a yes by a word list (it starts with yes
    and holds no refusal word), the action ran at once, and the condition was never checked; and a person's
    "haan fine" - a yes in their own words - was a no to every word list. With no model to ask, a reply the lists
    cannot settle is "unclear": the action waits and is put to the person again - never guessed either way.
    """

    text = " ".join(str(reply or "").split())
    if not text:
        return {"decision": "no", "condition": ""}
    if plain and _plain(text, _PLAIN_YES) and not REFUSAL_RE.search(text):
        return {"decision": "yes", "condition": ""}
    if _plain(text, _PLAIN_NO) and REFUSAL_RE.search(text):
        return {"decision": "no", "condition": ""}
    # One reply, one reading per action: the parts that act on it in turn (a long request, the gate, the engine) reuse
    # it - never for another action.
    key = (" ".join(str(action or "").split())[:6000], text, bool(plain))
    kept = _READ.get(key)
    if kept and time.time() - kept[0] < READING_SECONDS:
        return dict(kept[1])
    try:
        from decurix_agent import llm_client
        if llm_client.available():
            message, _ = llm_client.chat([{"role": "system", "content": READ_SYSTEM},
                                          {"role": "user", "content": f"The action (as the question they answered put "
                                           f"it): {action[:6000]}\n\nTheir reply: {text[:3000]}"}],
                                         max_tokens=300)
            found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
            said = json.loads(found.group(0)) if found else {}
            decision = str(said.get("decision") or "").strip().lower()
            if decision in DECISIONS:
                wants = " ".join(str(said.get("wants") or "").split())[:600]
                said = {"decision": decision, "condition": " ".join(str(said.get("condition") or "").split())[:600]}
                if wants:
                    # What they ask the worker to tell or show them before they decide: it answers that first.
                    said["wants"] = wants
                if len(_READ) > 64:
                    _READ.clear()
                _READ[key] = (time.time(), said)
                return dict(said)
    except Exception:  # noqa: BLE001 - no reading to be had: what the word lists can settle, else unclear
        pass
    if plain and is_approval(text) and not _MAY_CONDITION.search(text):
        return {"decision": "yes", "condition": ""}
    if REFUSAL_LEAD.match(text):
        return {"decision": "no", "condition": ""}
    return {"decision": "unclear", "condition": ""}


def read_reply(reply, action=""):
    """What a reply to an approval decides about that one action: "yes", "if", "no" or "unclear" (reading)."""

    return reading(reply, action)["decision"]


def says_yes(reply, action=""):
    """Whether a reply to an approval grants that action now - the one reading every caller acts on (reading)."""

    return read_reply(reply, action) == "yes"


CHOICE_SYSTEM = """An AI worker asked a person a question and offered some choices (below, each with what it means). \
Read their reply the way a colleague would - in any language, slang, shorthand or emoji - and say:
- "choice": the name of the one choice the reply makes; "unclear" when it makes none of them, or could be either of two.
- "instruction": everything else the reply asks the worker to do, use, change or keep in mind, in their words - empty \
when there is nothing more.
A reply that makes a choice and adds to it ("retry, but use the second file") makes that choice, with the rest as the \
instruction. A reply that says something is still wrong, missing or to be done never makes a choice that means all is \
well - however it starts ("correct except the total", "the rest is done, 2 still pending").
Reply with JSON only: {"choice": "<name>" | "unclear", "instruction": "..."}"""

# Words that only soften a reply ("retry please", "done, thanks"): left out when a reply is matched to a plain phrase.
_SOFTENERS = {"please", "pls", "plz", "thanks", "thank", "you", "thx", "ji", "sir", "madam", "bhai", "boss"}

# The model's choices in this process: {(reply, question, choices): (when, result)}.
_CHOSEN = {}


def _bare(text):
    """A reply as matched to a plain phrase: case, punctuation and softening words aside."""

    words = re.findall(r"[a-z0-9']+", str(text or "").lower())
    while words and words[-1] in _SOFTENERS:
        words.pop()
    while words and words[0] in _SOFTENERS:
        words.pop(0)
    return " ".join(words)


def read_choice(reply, question, choices, plain=None):
    """
    Which of `choices` ({name: what it means}) a reply to `question` makes, and what else it asks:
    {"choice": a name or "unclear", "instruction": the rest of their words, or ""}.

    The one reading of a reply to a question with set answers ("reply retry, skip or stop", "reply done if it is
    done"). Code decides only a reply that is, whole, one of a choice's plain phrases (plain: {name: [phrases]}; case,
    punctuation and please/thanks aside); every other reply is read once by the model, and what it adds to the choice
    is kept as the instruction - never dropped. With no model, such a reply is "unclear": asked again, never guessed.
    Found (audit, 2026-10-24): word lists anchored at a reply's first word read "retry but use the second file" as a
    plain retry, "correct except the total, fix that" as done, "no, use option 2" as stop.
    """

    text = " ".join(str(reply or "").split())
    if not text:
        return {"choice": "unclear", "instruction": ""}
    bare = _bare(text)
    for name, phrases in (plain or {}).items():
        if bare and bare in {_bare(p) for p in phrases}:
            return {"choice": name, "instruction": ""}
    key = (text, " ".join(str(question or "").split())[:400], tuple(sorted(choices)))
    kept = _CHOSEN.get(key)
    if kept and time.time() - kept[0] < READING_SECONDS:
        return dict(kept[1])
    try:
        from decurix_agent import llm_client
        if llm_client.available():
            offered = "\n".join(f"- {name}: {meaning}" for name, meaning in choices.items())
            message, _ = llm_client.chat([{"role": "system", "content": CHOICE_SYSTEM},
                                          {"role": "user", "content": f"The question: {str(question or '')[:2000]}\n\n"
                                           f"The choices:\n{offered}\n\nTheir reply: {text[:3000]}"}],
                                         max_tokens=400)
            found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
            said = json.loads(found.group(0)) if found else {}
            choice = str(said.get("choice") or "").strip()
            if choice in choices or choice == "unclear":
                result = {"choice": choice, "instruction": " ".join(str(said.get("instruction") or "").split())[:1000]}
                if len(_CHOSEN) > 64:
                    _CHOSEN.clear()
                _CHOSEN[key] = (time.time(), result)
                return dict(result)
    except Exception:  # noqa: BLE001 - no reading to be had: unclear, asked again
        pass
    return {"choice": "unclear", "instruction": ""}


# A reply that turns the action down and asks for nothing in its place: "no",
# "don't send it", "stop". "No, send it to Priya instead" is an instruction.
REFUSAL_LEAD = re.compile(r"^\s*(no|nope|don'?t|do not|stop|cancel|abort|never|deny|denied|reject(ed)?)\b", re.IGNORECASE)
REDIRECTS = re.compile(r"\b(instead|rather|but|use|try|change|make it)\b", re.IGNORECASE)


def is_refusal(reply):
    """A reply that says no to the action and does not say to do something else."""

    reply = (reply or "").strip()
    return bool(REFUSAL_LEAD.match(reply)) and not REDIRECTS.search(reply)


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
        asked = confirms_permanently(element, context.get("dialogs"))
        if asked:
            return f'this answers "{asked}" - it cannot be taken back'

    if kind == "key" and keys and any(ENTER_KEY.search(k.strip()) for k in keys):
        # Where the keyboard is decides what Enter does: in a search box it
        # searches. Unknown, it is taken to send - the cautious reading.
        if MESSAGING_CONTEXT.search(where) and not searches(element or context.get("focused")):
            return "pressing Enter here sends what was typed as a message"

    if kind == "type" and text and "\n" in text and MESSAGING_CONTEXT.search(where):
        return "typing a new line here sends what was typed as a message"

    if kind == "type" and text and element is not None and privacy.secret_field(element):
        host = _host(context.get("url"))
        if host and not site_known(host):
            return (f"this types a password or code into {host}, a site that neither the request nor anything a person "
                    "taught on this desktop names - make sure it is the right site before a secret goes there")

    return None


# A control whose own label says a press keeps, sends or ends something: it saves, submits, posts, files, books, pays,
# sends, deletes, places, confirms or creates. Wider than CONSEQUENTIAL_CONTROL on purpose: this does not ask the
# person, it says the press commits a record - the pre-save checks run before it (core._commit_check), and healing
# never re-targets a step onto one under another name (healing.decide).
COMMIT_CONTROL = re.compile(
    # ("Add", "Apply", "Remove" are left out: an "Add row", an "Apply filter", a "Remove" chip are steps of filling a
    # form; the form's own submit control is the commit, and a <form>'s is found by its markup whatever its label.)
    r"^\s*(?:save|submit|post|file|e-?file|book|pay|send|delete|place|confirm|create|register|onboard|"
    r"finish|complete|accept|approve|update|publish|finali[sz]e|transfer|cancel (?:the |this )?"
    r"(?:order|invoice|document|booking|payment|entry))\b",
    re.IGNORECASE,
)
PRESS_ROLES = ACTIVATING_ROLES | {"menuitem", "tab"}


def commits(element):
    """Whether a press on this control commits something (COMMIT_CONTROL, or a control the gate stops), and does not
    only go somewhere (a navigation link, a list's option)."""

    if not element or (element.get("role") or "") not in PRESS_ROLES or _navigates(element):
        return False
    name = element.get("name") or ""
    return bool(COMMIT_CONTROL.match(name) or CONSEQUENTIAL_CONTROL.match(name))


# Where a person's secrets may be typed without asking (2026-10-21): a taught task's replay stopped
# because its portal was not open, and the engine opened the real government site of that name from its own memory and
# typed the client's username and password from the person's client list into it. A password or code goes, unasked,
# only to a site the request names or a person used in a recording or a taught skill on this desktop; anywhere else
# the person says yes first (a phishing page and a remembered address look the same to an engine).
_KNOWN_CACHE = {"at": 0.0, "text": ""}
KNOWN_SECONDS = 30


def _host(url):
    import urllib.parse

    try:
        return (urllib.parse.urlparse(url or "").hostname or "").lower()
    except ValueError:
        return ""


def _taught_text():
    """The skills and the people's recordings on this desktop, as one lower-case text (cached a little)."""

    now = time.time()
    if now - _KNOWN_CACHE["at"] < KNOWN_SECONDS:
        return _KNOWN_CACHE["text"]
    parts = []
    roots = [config.DECURIX_DIR / "skills", config.DECURIX_DIR / "recordings"]
    for root in roots:
        try:
            files = list(root.glob("*.json")) + list(root.glob("demo-*/events.jsonl"))
        except OSError:
            continue
        for path in files[:400]:
            try:
                with open(path, encoding="utf-8", errors="replace") as handle:
                    parts.append(handle.read(400_000))
            except OSError:
                continue
    _KNOWN_CACHE.update(at=now, text="\n".join(parts).lower())
    return _KNOWN_CACHE["text"]


def site_known(host):
    """Whether this host is named by the current request or by what a person taught or recorded on this desktop."""

    host = (host or "").lower()
    if not host:
        return True
    try:
        from decurix_agent import task_context
        request = (task_context.current_request() or "").lower()
    except Exception:  # noqa: BLE001 - no request to read is no request naming it
        request = ""
    return host in request or host in _taught_text()


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


def confirms_permanently(element, open_dialogs=None):
    """
    The question a yes-button answers when its dialog asks to do something that cannot be taken back, or None. The
    dialog is the one the button's path names, else one open on the screen (`open_dialogs`, their names). Found live
    (2026-10-31): an application's "Permanently delete <record>?" was answered Yes unasked - its buttons' paths did not
    name the dialog they sit in.
    """

    if (element.get("role") or "") not in ACTIVATING_ROLES or not CONFIRM_ANSWER.match(element.get("name") or ""):
        return None
    dialogs = DIALOG_IN_PATH.findall(element.get("path") or "") or [d for d in (open_dialogs or []) if d.strip()]
    asked = next((d.strip() for d in reversed(dialogs) if PERMANENT_QUESTION.search(d)), "")
    return asked or None


def _fingerprint(kind, element, text, keys, point, context=None, about=""):
    """
    What one approval is for: the action, the control, and what it acts on - the window, the page's address (the
    document, the chat, the record) and the filled fields beside it (the recipient, the amount, the message). A yes
    to "Send" in one chat, or to a form holding one amount, is no yes once the page or the fields are another's
    (review 2026-11-02: the fingerprint was the label and the window title only).
    """

    context = context or {}
    parts = [
        kind,
        (element or {}).get("role", ""),
        " ".join(((element or {}).get("name") or "").split()).lower(),
        (element or {}).get("window_title", ""),
        text or "",
        "+".join(keys or []),
        str(context.get("url") or ""),
        " ".join(str(about or "").split()),
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
    def __init__(self, allowed, reason=None, approved=False, request=None, by_reply=""):
        self.allowed = allowed
        self.reason = reason
        self.approved = approved
        self.request = request
        self.by_reply = by_reply  # the person's reply that was this action's yes, when one was


# How long the person's latest reply can stand as the yes for the action the gate stops next.
REPLY_GRANT_SECONDS = 900


def note_reply(text, asked=""):
    """
    The person's latest reply in this task and the question it answered, kept for check(): when that question named
    the very control the gate stops next, and the reply - read against the question and that action - approves that
    action, it is that action's yes: one reply, one action. Found live (2026-10-26): "Yes, file the return" to the
    figures question, then the gate asked for the file click, then "file it", then the gate asked again.
    """

    text = " ".join(str(text or "").split())
    if not text:
        return
    with _locked() as state:
        state["last_reply"] = {"text": text[:2000], "asked": " ".join(str(asked or "").split())[:2000],
                               "at": time.time(), "used": False}


def _words(text):
    return re.findall(r"[a-z0-9]+", str(text or "").lower())


def names_control(question, element):
    """
    Whether a question names this control by its whole label, as a phrase ("click FILE WITH EVC", "send the
    message"). The control's label is what the gate stops; a question that does not name it was about something else.
    A share of words is not enough (review 2026-11-02: a reply that shared one four-letter word with an action - its
    own reason text gave "your", "something" - stood as that action's yes).
    """

    label = _words((element or {}).get("name"))
    asked = _words(question)
    if not label or len(label) > len(asked):
        return False
    return any(asked[i:i + len(label)] == label for i in range(len(asked) - len(label) + 1))


def _reply_bound(prior, element):
    """Whether the person's latest reply could be this action's yes: fresh, unused, and answering a question that
    named this very control (names_control). Its reading decides the rest (_reply_grants)."""

    if not prior or prior.get("used") or time.time() - float(prior.get("at") or 0) > REPLY_GRANT_SECONDS:
        return False
    return bool(prior.get("text")) and names_control(prior.get("asked") or "", element)


def _reply_grants(prior, description, element, reason, about=""):
    """
    Whether the person's latest reply is the yes for this action: bound to it (_reply_bound), and read by the model -
    against the question they answered and this exact action - as a yes to this action now (reading, plain=False).
    A plain "yes" is not settled by code here: the question was not the gate's, so whether it was about this action is
    a judgement; with no model there is none, and the gate asks. "if" and "unclear" never grant.
    """

    if not _reply_bound(prior, element):
        return False
    action = (f"The question they answered: {prior.get('asked')}\n\nThe one action now about to happen, held until "
              f"they approve it: {description} - {reason}." + (f" {about}" if about else "")
              + "\nSay \"yes\" only if their reply approves exactly this action, now.")
    return reading(prior.get("text") or "", action=action, plain=False)["decision"] == "yes"


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


def check(kind, element, text=None, keys=None, point=None, context=None, about=""):
    """
    Decide whether an action may run now. Consumes a matching grant. `about`
    says what the action would act on (the filled fields beside it), shown to
    the person with the request.
    """

    reason = assess(kind, element, text=text, keys=keys, context=context)

    if reason is None:
        return Decision(True)

    fingerprint = _fingerprint(kind, element, text, keys, point, context=context, about=about)
    description = describe_action(kind, element, text=text, keys=keys, point=point)

    with _locked() as state:
        for grant in state["grants"]:
            if grant["fingerprint"] == fingerprint:
                state["grants"].remove(grant)
                return Decision(True, reason=reason, approved=True)
        prior = dict(state.get("last_reply") or {})

    # Read outside the lock (a model's reading takes seconds, and every gui action waits on this store).
    if _reply_grants(prior, description, element, reason, about=about):
        with _locked() as state:
            now = state.get("last_reply") or {}
            # The very reply read, still unused: two actions racing for one yes get one of them.
            if now.get("at") == prior.get("at") and now.get("text") == prior.get("text") and not now.get("used"):
                now["used"] = True
                state["last_reply"] = now
                granted = True
            else:
                granted = False
        if granted:
            try:
                from decurix_agent import progress
                progress.emit(f"Taking what you said as the yes for this one action - \"{prior['text'][:160]}\" - "
                              f"and going on to {description[:120]}")
            except Exception:  # noqa: BLE001 - progress is decoration
                pass
            return Decision(True, reason=reason, approved=True, by_reply=prior["text"])

    with _locked() as state:
        request = {
            "id": uuid.uuid4().hex[:12],
            "fingerprint": fingerprint,
            "reason": reason,
            "action": description,
            "about": about or "",
            "created": time.time(),
        }
        state["pending"] = request

    return Decision(False, reason=reason, request=request)


def pending():
    """The approval an engine is currently stopped on, or None."""

    with _locked() as state:
        return state["pending"]


def resolve_reply(reply, asked=""):
    """
    Settle the pending approval with the human's reply.

    Returns "granted", "declined", or None when nothing was pending. The reading is kept (last_reading) for the
    engine the reply goes on to, so the gate and the engine never read one reply two ways. asked: the question the
    person was shown (messaging keeps it); when it is the one about this action, the reply is read against all of it.
    Found live (2026-10-24): 'yes go. "liability offset successfully" is enough. file sizes irrelevant' answered the
    points the question listed; read against the bare action it was "if" two times in three and the filing was asked
    for again - read against the question, "yes" three in three.
    """

    with _locked() as state:
        request = state["pending"]
    if request is None:
        return None
    with _locked() as state:
        if state.get("last_reply"):
            state["last_reply"]["used"] = True  # (a reply that settles a pending approval grants nothing else)
    shown = " ".join(str(asked or "").split())
    if not asked_about(request, shown):
        # The reply answered another question (an older card, a question of the engine's own that did not put this
        # action to them): it settles nothing here. The pending action is withdrawn, so the engine's repeat of it is
        # put to the person in the gate's own words - a yes is only ever the yes of the action it was asked for.
        with _locked() as state:
            if state["pending"] is not None and state["pending"].get("id") == request.get("id"):
                state["pending"] = None
        return None
    # Read outside the lock: a model's reading takes seconds, and every gui action waits on this store.
    said = reading(reply, shown)

    with _locked() as state:
        if state["pending"] is None or state["pending"].get("id") != request.get("id"):
            return None
        state["pending"] = None
        state["reading"] = dict(said, reply=" ".join(str(reply or "").split()), at=time.time())

        if said["decision"] != "yes":
            return "declined"

        state["grants"].append({
            "fingerprint": request["fingerprint"],
            "action": request["action"],
            "expires": time.time() + GRANT_SECONDS,
        })

        return "granted"


def asked_about(request, question):
    """
    Whether the question a person answered put this pending action to them: it holds the gate's line for the action
    (describe_action, as request_text words it - soul quotes it, and its review question cuts it at 200 characters).
    Every harness asks with request_text or text that quotes it; a question without it was another one. What the
    action acts on (the filled fields) is bound by the grant's fingerprint, not here.
    """

    question = " ".join(str(question or "").split())
    action = " ".join(str((request or {}).get("action") or "").split())
    return bool(question and action) and action[:120] in question


def last_reading(reply):
    """The reading resolve_reply made of this very reply (decision and condition), or None."""

    text = " ".join(str(reply or "").split())
    try:
        with _locked() as state:
            kept = state.get("reading")
    except (OSError, TimeoutError):
        return None
    if kept and kept.get("reply") == text and time.time() - kept.get("at", 0) < READING_SECONDS:
        said = {"decision": kept.get("decision"), "condition": kept.get("condition") or ""}
        if kept.get("wants"):
            said["wants"] = kept["wants"]
        return said
    return None


def has_grant():
    with _locked() as state:
        return bool(state["grants"])


def reset():
    """Forget pending approvals and grants - a new task starts clean."""

    with _locked() as state:
        state["pending"] = None
        state["grants"] = []
        state["last_reply"] = None


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

    about = f" {request['about'].rstrip('.')}." if request.get("about") else ""
    return (
        f"Approval needed before I {request['action']}: {request['reason']}.{about} "
        "Nothing was clicked or sent. Reply yes to let me do it, or tell me what to do instead."
    )
