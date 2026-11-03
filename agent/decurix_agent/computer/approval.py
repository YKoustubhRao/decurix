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

# Nouns of money that make a "transfer" after them the moving of money ("Bank Transfer", "Wire transfer"), not a kind of
# record ("Stock Transfer", "Material Transfer" stay record types: _ALSO_NOUNS). Payment networks are named as banks
# name them on a button.
_MONEY_NOUNS = set("bank wire fund funds money cash balance payment remittance interbank inter-bank ach sepa swift neft "
                   "rtgs imps upi".split())
_MONEY_TRANSFER = "(?:" + "|".join(sorted(_MONEY_NOUNS)) + ") transfer"

# Controls whose own label says they commit the human to something.
CONSEQUENTIAL_CONTROL = re.compile(
    r"^\s*("
    r"send|send now|send (it|message|email|e-mail|mail|newsletter|invite|invitation|reply|sms|text|money|payment)"
    r"|pay|pay now|make (a )?payment|submit payment|authori[sz]e payment"
    r"|confirm (and pay|payment|purchase|order|transfer|booking|trade|sale)"
    r"|buy|buy now|purchase|place (your |my )?order|complete (order|purchase|payment|checkout)"
    # Money moved, paid out or given back, a holding sold, something held let go of (review 2026-11-03: "Bank
    # Transfer", "Disburse", "Sell", "Release" passed). "Release notes", "New release", "Sell price" are read as names
    # (_only_reads).
    r"|" + _MONEY_TRANSFER + r"|disburse|sell|liquidate|refund|release"
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
    # ...and the one return named: "File GSTR-1", "File return", "File the declaration" (review 2026-11-03: ungated). A
    # form's name is a code with a digit in it; "File returns" (plural) stays the menu of returns it was.
    r"|file (?:the |this |my |your )?(?:return|declaration|claim|[a-z]+-?\d[\w-]*)(?= |$)"
    # Approving binds the person (a payment, a claim, a document's workflow); signing, by hand or electronically, is
    # their signature, and so is signing off on something. "Sign in", "Sign up", "Sign out", "Sign on" are not.
    r"|approve|e-?sign\w*|digitally sign|sign-?off|sign(?! ?-?(?:in|up|out|on|into)\b)"
    r")\b",
    re.IGNORECASE,
)

# More phrases that commit the person, read like CONSEQUENTIAL_CONTROL at any word of a label (consequential). Found in
# review (2026-11-02): "Continue to payment", "Authorize", "Approve payment" passed a gate that read only the label's
# first words.
MORE_CONSEQUENTIAL = re.compile(
    r"^\s*(?:"
    r"(?:proceed|continue|go|move on|next|skip) (?:to|with) (?:the )?(?:checkout|payment|pay|paying|file|filing|submit|submission"
    r"|send|transfer|purchase)"
    r"|authori[sz]e"
    r"|(?:make|submit|confirm|complete|process|release|initiate|execute|approve|schedule|send|finali[sz]e|issue) "
    r"(?:the |this |a |my |your )?(?:(?:" + "|".join(sorted(_MONEY_NOUNS)) + r") )?"
    r"(?:payments?|transfers?|transactions?|remittances?|wires?|purchases?|donations?|trades?|swaps?|refunds?"
    r"|disbursements?|payouts?)"
    r"|remit|wire(?: money| funds)?"
    r"|settle (?:the |this )?(?:invoices?|bills?|payments?|balances?|amounts?|dues|trades?)"
    # An order or a trade put to a market or a shop, carried out or placed (review 2026-11-03: "Execute trade" passed).
    r"|place (?:the |this |an |a )?(?:order|trade|bet)|execute (?:the |this |a |my )?(?:orders?|deals?)|trade now"
    # Money let go of, and an order made with a supplier or a shop (review 2026-11-03: "Release funds", "Submit order"
    # passed).
    r"|release (?:the |this )?(?:funds?|money|escrow|amounts?)"
    r"|submit (?:the |this |your |my )?(?:order|purchase|bid|offer|tender|trade)s?"
    r"|cash out|request (?:a )?(?:payout|withdrawal|refund)"
    r")\b",
    re.IGNORECASE,
)

# The same acts in a few other languages a label may be written in: send, pay, buy (a label is read whole, lower-cased).
# Not a translation table to grow with each new site: the gate's English phrases are the rule, these are the plain
# verbs of the commonest languages so that a page in one of them is not ungated wholesale.
OTHER_LANGUAGES = re.compile(
    r"(?:^|\s)(?:envoyer|payer|acheter|enviar|pagar|comprar|senden|absenden|bezahlen|kaufen|inviare|invia|pagare|"
    r"acquista|verzenden|betalen|kopen)(?:\s|$)|भुगतान|भेजें|खरीदें",
    re.IGNORECASE,
)

# A label as words: "&", "+" and "/" read as "and"; symbols, emoji and arrows dropped ("🔒 Pay now →" is "Pay now").
_JOINERS = re.compile(r"\s*[&+/]\s*")
_NOT_A_WORD = re.compile(r"[^\w\s'-]+")

# How a label that holds a committing word only reads or goes somewhere: a read or navigation word before it ("View
# post", "Track transfer"), or nothing but nouns of looking after it ("Payment history", "Pay slip", "Transfer
# history"); "New Purchase Invoice" names a kind of record. Words that destroy or end something
# are never read so: "Delete history" deletes it.
_READ_BEFORE = set("view show see track print download export preview find search filter browse open review "
                   "read list".split())
_READ_AFTER = set("history list lists report reports details detail summary status settings setting options option "
                  "methods method preferences statement statements slip slips log logs overview dashboard records "
                  "activity analytics info information help faq faqs terms policy policies guide page register "
                  "registers trends analysis calendar".split())
_RECORD_NOUNS = set("invoice invoices order orders receipt receipts request requests return returns ledger ledgers "
                    "entry entries quotation quotations requisition requisitions item items record records note notes "
                    "voucher vouchers bill bills".split())
_CREATE_BEFORE = set("new add create draft edit copy duplicate amend make get fetch".split())
_DESTROYS = set("delete erase purge destroy cancel close withdraw remove".split())
# Words after which a committing word names a thing rather than does it: "Get Items From Purchase Order", "Purchase
# Taxes and Charges for Transfer" (review 2026-11-03: matched at any word, they asked).
_OF_WORDS = set("from for of in on by with per against into under via".split())
# Words that name an attribute of a kind of act - a field's or a column's label, never the act: "Post Date", "Buy
# Price", "Purchase Taxes and Charges", "Transfer Type". ("Amount" is not one: "Pay amount" pays it.)
_ATTRIBUTES = set("date dates time times price prices rate rates type types mode modes category categories code codes "
                  "no number numbers period periods tax taxes charge charges rule rules template templates account "
                  "accounts uom unit units status".split())
# Committing words that are also nouns: after a word that is not one of _LEADS they name a kind of thing ("Stock
# Transfer", "Material Transfer", "Make Purchase Receipt"), not an act. Verbs only (pay, send, buy, sign) stay acts
# wherever they are: "Securely pay", "Yes, send".
_ALSO_NOUNS = set("transfer transfers purchase purchases post posts order orders payment payments booking bookings "
                  "reply replies withdrawal withdrawals donation donations return returns publish release".split())
# ...except where a noun of money comes first: "Fund release" lets money go. ("Bank Transfer" is matched whole, and
# judged, at its first word.)
_MOVES_MONEY = {"release"}
# The few words after which a committing verb names a thing: "Release notes", "Release candidate".
_NAMED_AFTER = {"release": set("notes note candidate candidates channel channels branch branches cycle".split())}
# What before such a word makes it the act: a joiner or a yes ("Approve & transfer", "Yes, post it"), an adverb
# ("Securely transfer", "Transfer now" is at word 0 anyway).
_LEADS = set("and then yes ok okay sure please now confirm also just".split())
# Arranging a drawing's objects: "Send to back", "Send backward".
_ARRANGES = (["to", "back"], ["to", "front"], ["backward"], ["backwards"], ["forward"], ["forwards"])
# What a row or a line of a form being filled is: deleting one changes the form, which its own Save then commits
# (review 2026-11-03: "Delete row" in a document's item table asked a person). A record, a file, a user is not this.
_FORM_PARTS = set("row rows line lines field fields filter filters column columns cell cells".split())


def label_words(name):
    """A control's accessible name as plain words (symbols, emoji and arrows dropped; & + / read as "and")."""

    text = _NOT_A_WORD.sub(" ", _JOINERS.sub(" and ", str(name or ""))).replace("_", " ")
    return " ".join(text.split())


def _only_reads(words, i, goes_somewhere=True):
    """Whether the committing word at words[i] is used to read or go somewhere, not to commit (_READ_BEFORE...).

    goes_somewhere=False (a button, or any control that is not a link, a tab or a navigation menu's item): only the
    label's structure may say so ("Send to back" arranges a drawing), never the words beside the committing one. Found
    (review 2026-11-03, D-04): "Pay tax", "Send statement", "Instant transfer" on buttons were read as names by the
    vocabularies below and paid or sent unasked. A link goes somewhere, and what it leads to is gated there."""

    verb = words[i].lower()
    if verb in _DESTROYS:
        return False
    before = words[i - 1].lower() if i else ""
    after = [w.lower() for w in words[i + 1:]]
    # A verb of two words is read as one: "Bank Transfer details" is about a transfer, "Sign off date" about signing off.
    if verb in _MONEY_NOUNS and after[:1] == ["transfer"]:
        verb, after = "transfer", after[1:]
    elif verb == "sign" and after[:1] == ["off"]:
        after = after[1:]
    if verb in ("send", "bring") and after in _ARRANGES:
        return True
    if not goes_somewhere:
        return False
    if after[:1] and after[0] in _NAMED_AFTER.get(verb, ()):
        return True
    if before in _READ_BEFORE:
        return True
    # (Only nouns of looking: "Purchase Invoice List" on a button may still be an act - a kind of record after the word
    # is RECORD_NAME's to read, for links and menus.)
    if after and all(w in _READ_AFTER for w in after):
        return True
    if before in _OF_WORDS:
        return True
    if after and all(w in _ATTRIBUTES or w == "and" for w in after) and any(w in _ATTRIBUTES for w in after):
        return True
    if i and verb in _ALSO_NOUNS and before not in _LEADS and not before.endswith("ly") \
            and not (verb in _MOVES_MONEY and before in _MONEY_NOUNS):
        return True
    if verb in _CREATE_BEFORE and len(after) >= 2 and after[-1] in _RECORD_NOUNS:
        # "Make Purchase Receipt", "Make Payment Entry": a new record of that kind is opened, to be saved by its form.
        return True
    return before in _CREATE_BEFORE and bool(after) and all(w in _RECORD_NOUNS for w in after)


def consequential(name, goes_somewhere=False):
    """
    Whether a control's label commits the person to something (CONSEQUENTIAL_CONTROL, MORE_CONSEQUENTIAL), matched as
    whole words at any word of it - "Confirm & Pay", "Securely pay", "🔒 Pay", "Pay now →". Found in review
    (2026-11-02): matched at the label's first word only, every one of those passed unasked. Anything not plainly a
    read is asked: unsure is asked.

    goes_somewhere: the control is a link, a tab or an item of a navigation menu (goes_somewhere), whose label may name
    a place ("Payment history", "View post") - the words around the committing one are read for that (_only_reads).
    On a button they are not: a committing word anywhere in its label asks.
    """

    words = label_words(name).split()
    if _edits_the_form(words):
        return False
    for i in range(len(words)):
        rest = " ".join(words[i:])
        if (CONSEQUENTIAL_CONTROL.match(rest) or MORE_CONSEQUENTIAL.match(rest)) \
                and not _only_reads(words, i, goes_somewhere):
            return True
    return bool(OTHER_LANGUAGES.search(" ".join(str(name or "").split())))


def _edits_the_form(words):
    """Whether a label removes a part of the form being filled ("Delete row", "Remove selected lines"): a change the
    form's own Save commits, never the person's record itself (_FORM_PARTS)."""

    lowered = [w.lower() for w in words]
    if not lowered or lowered[0] not in _DESTROYS:
        return False
    rest = [w for w in lowered[1:] if w not in ("this", "the", "selected", "all", "a", "empty")]
    return bool(rest) and all(w in _FORM_PARTS for w in rest)

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

# A key that presses whatever has the keyboard: Return or Enter on a button, Space on a button or a box.
ACTIVATING_KEY = re.compile(r"(^|\+)(return|enter|kp_enter|space|spacebar)$", re.IGNORECASE)
_MODIFIER_KEYS = {"ctrl", "control", "shift", "alt", "altgr", "super", "win", "cmd", "meta", "hyper"}
# Fields whose keys stay in them: letters, Backspace and the caret keys edit the text or move in it, not the keyboard.
TEXT_ROLES = {"textbox", "document", "combobox", "searchbox", "spinbutton", "password text", "entry", "editbar",
              "terminal"}
# Ctrl+ keys that edit a field's text in place: select all, copy, paste, cut, undo, redo, a word or the ends.
_EDIT_CHORDS = set("a c v x z y left right home end backspace delete".split())
_LEAVES_FIELD = {"tab", "iso_left_tab", "escape", "esc", "menu", "return", "enter", "kp_enter"}


def _key_parts(combo):
    return [p for p in re.split(r"\s*\+\s*", str(combo or "").strip().lower()) if p]


def _keeps_focus(combo, in_text, lines=False):
    """Whether this key leaves the keyboard where it was. Unsure is no: outside a text field any key may move it (an
    arrow in a list or a menu, a letter that jumps to an entry, Escape closing a dialog, Return opening one). `lines`:
    the field holds lines of text, where Return starts a new one."""

    parts = _key_parts(combo)
    named = [p for p in parts if p not in _MODIFIER_KEYS]
    if not named:
        return True  # a modifier on its own
    if not in_text or len(named) > 1:
        return False
    key, held = named[0], set(parts) - {named[0]}
    if lines and key in ("return", "enter", "kp_enter") and not held - {"shift"}:
        return True
    if key in _LEAVES_FIELD or re.fullmatch(r"f\d{1,2}", key):
        return False
    if held - {"shift"}:
        return held <= {"ctrl", "control", "shift"} and key in _EDIT_CHORDS
    return True


def press_after_focus_move(keys, focus=None):
    """
    The first key of `keys` that presses whatever has the keyboard (Return, Enter, Space) after an earlier key may have
    moved the keyboard elsewhere - Tab, an arrow, Escape, another Return, or a key chorded with it ("Tab+Return"); None
    when every such press is on the control that had the keyboard at the start. `focus` is that control: in a text field
    letters, the caret keys and ctrl+a stay in it. Found in review (2026-11-03): `gui key Tab Return` pressed the
    control Tab landed on, which nothing had looked at.
    """

    focus = focus or {}
    in_text = focus.get("role") in TEXT_ROLES
    lines = focus.get("role") in ("document", "terminal") or "\n" in str(focus.get("value") or "")
    moved = False
    for combo in keys or ():
        parts = _key_parts(combo)
        if not parts:
            continue
        if ACTIVATING_KEY.search(str(combo).strip()):
            chord = [p for p in parts[:-1] if p not in _MODIFIER_KEYS]
            if moved or chord:
                return str(combo).strip()
        if not _keeps_focus(combo, in_text, lines):
            moved = True
    return None


def _holds_consequential(context, where):
    """What a press whose target cannot be told may land on and commit, in words, or "" when nothing on the screen
    would. Not knowing what the screen holds is taken as holding one."""

    if MESSAGING_CONTEXT.search(where or ""):
        return "in this window Enter sends what was typed as a message"
    controls = context.get("controls")
    if controls is None:
        return "what is on the screen is not known"
    for control in controls:
        if press_effect(control) == "asks" or confirms_permanently(control, context.get("dialogs")):
            label = " ".join(str(control.get("name") or "").split())[:60]
            return f'the screen holds "{label}", which a press there would commit'
    return ""

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
- "no": they do not approve it now (not yet, wait, fix something first, never). A reply that approves with a \
different amount, recipient, account, date or item is "no" for THIS action: put the change they ask for in "wants".
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
    # No reading: a plain yes was settled above, before the model. Found (review 2026-11-03, F-02): a fallback here took
    # any reply that began with a yes word and held no condition word for a yes, so a yes that changed the amount, the
    # payee, the account or the time granted the ORIGINAL action on every desktop whose gate has no model. Anything
    # beyond a plain yes is "unclear" and is asked again; a refusal may still be read as one (that only withholds).
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


def goes_somewhere(element):
    """Whether this control leads to a place rather than acting where it is: a link, a tab, or an item of the site's
    navigation menu (NAVIGATION_PATH). Read from the control's role and container - never from its words."""

    role = (element or {}).get("role") or ""
    if role in ("link", "tab"):
        return True
    return role == "menuitem" and bool(NAVIGATION_PATH.search((element or {}).get("path") or ""))


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


# Roles a click on only focuses or selects for reading (Observation stage, assess): a field, a cell, text.
# Only roles that plainly take the keyboard or a cell selection (review round 2: a row, an image or a pane may open
# or choose something when clicked).
_ONLY_FOCUSES = {"textbox", "searchbox", "document", "cell", "terminal", "editbar"}


def assess(kind, element, text=None, keys=None, context=None):
    """
    Why this action needs a human's approval first, or None.

    `context` is {"window": active window title, "url": active tab URL,
    "focused": the field that has the keyboard, when the caller knows it,
    "controls": the controls on the screen, given for keys that move the
    keyboard before they press (press_after_focus_move); "dialogs": the
    dialogs open}.
    """

    stage = modes.get_stage()

    # Stage 1: Observation mode stops mutating actions to observe without external side-effects
    if stage == modes.STAGE_OBSERVATION:
        # The stage promises no external change (modes.py): only looking, scrolling and a click that only puts the
        # keyboard or the selection somewhere go ahead. Found in review (2026-11-03, D-28): only presses on activating
        # roles and Enter were stopped, so typing, Ctrl+S, a select, a drag and a click at a point went through.
        if kind in ("click", "double_click") and element is not None \
                and str(element.get("role") or "").lower() in _ONLY_FOCUSES:
            return None
        if kind not in ("scroll", "right_click"):
            return "worker is in Stage 1 Observation (shadowing) mode - an action that may change something is paused"

    context = context or {}
    where = " ".join(filter(None, [context.get("window"), context.get("url"),
                                   element.get("window_title") if element else None]))

    if kind in ("click", "double_click") and element is not None:
        name = element.get("name") or ""
        if press_effect(element) == "asks":
            label = " ".join(name.split())[:60]
            return f'"{label}" sends, pays, buys or publishes something on your behalf'

    if kind in ("click", "double_click") and element is not None:
        asked = confirms_permanently(element, context.get("dialogs"))
        if asked:
            return f'this answers "{asked}" - it cannot be taken back'

    if kind == "key" and keys:
        # Return or Space after Tab (or any key that may move the keyboard) presses a control nothing has looked at:
        # which one cannot be told beforehand, so it is asked wherever a press could commit something.
        pressed = press_after_focus_move(keys, element or context.get("focused"))
        held = _holds_consequential(context, where) if pressed else ""
        if held:
            return (f"{' '.join(str(k).strip() for k in keys)} moves the keyboard and then presses {pressed}, and where "
                    f"that press lands cannot be told beforehand - {held} (pressed alone, after the keys that move the "
                    f"keyboard, {pressed} is checked against the control it lands on)")

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


def press_effect(element):
    """
    What a press on this control does, as one decision for every part that asks: "asks" - it commits the person to
    something (consequential), and the gate puts it to them first; "commits" - it keeps, sends or ends a record
    (COMMIT_CONTROL at the label's first word, or a consequential label on a role the gate does not ask for), and the
    pre-save checks run before it; None - it only goes somewhere or changes nothing kept.
    """

    role = (element or {}).get("role") or ""
    if not element or role not in PRESS_ROLES or _navigates(element):
        return None
    name = element.get("name") or ""
    if _edits_the_form(label_words(name).split()):
        return None
    if consequential(name, goes_somewhere(element)):
        return "asks" if role in ACTIVATING_ROLES else "commits"
    return "commits" if COMMIT_CONTROL.match(label_words(name)) else None


def commits(element):
    """Whether a press on this control commits something (press_effect: a control the gate stops, or one whose label
    says it keeps, sends or ends a record), and does not only go somewhere (a navigation link, a list's option)."""

    return press_effect(element) is not None


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
    return _names_host(request, host) or _names_host(_taught_text(), host)


def _names_host(text, host):
    """Whether `text` names `host` itself or a domain it is under, on label boundaries: "example.com" names
    example.com and www.example.com, never ample.com or example.com.evil.test. Found in review (2026-11-03, E-47): a
    substring test let a password go unasked to a host whose name was part of a named one."""

    for named in re.findall(r"(?<![\w.-])((?:[a-z0-9-]+\.)+[a-z]{2,})(?![\w-])", text or ""):
        if host == named or host.endswith("." + named):
            return True
    return False


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


SECRET_MARK = "\x00secret"


def sent_digest(fields):
    """
    One digest of every filled field a press sends ([[label, value, secret?], ...]), in order: the amount in the
    tenth field counts as much as the first. A secret field's value never enters it - only that the field is filled
    (SECRET_MARK). Found in review (2026-11-02): the fingerprint held the first three fields shown to the person, so a
    yes stood for the same form with another amount in its fourth.
    """

    parts = []
    for item in fields or ():
        label, value = (list(item) + ["", ""])[:2]
        secret = len(item) > 2 and bool(item[2])
        parts.append(" ".join(str(label or "").split()).lower() + "\x1e"
                     + (SECRET_MARK if secret else " ".join(str(value or "").split())))
    return hashlib.sha256("\x1f".join(parts).encode("utf-8")).hexdigest()[:24] if parts else ""


def _fingerprint(kind, element, text, keys, point, context=None, about="", sends=""):
    """
    What one approval is for: the action, the control, and what it acts on - the window, the page's address (the
    document, the chat, the record), the filled fields beside it as shown (the recipient, the amount, the message) and
    every field the press sends (`sends`, sent_digest). A yes to "Send" in one chat, or to a form holding one amount,
    is no yes once the page or the fields are another's (review 2026-11-02: the fingerprint was the label and the
    window title only).
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
        str(sends or ""),
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


def _local_note_reply(text, asked=""):
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


def check(kind, element, text=None, keys=None, point=None, context=None, about="", sends=None, require=None):
    """
    Decide whether an action may run now. Consumes a matching grant. `about`
    says what the action would act on (the filled fields beside it), shown to
    the person with the request. `sends`: every filled field the press sends
    ([[label, value, secret?]], or a function giving them, read only when the
    action is gated) - bound into the grant whole (sent_digest). `require`: a
    reason the caller has to put this action to the person when assess finds
    none (an Enter that presses a form's Pay, a replay saving today's values).
    """

    reason = assess(kind, element, text=text, keys=keys, context=context) or require

    if reason is None:
        return Decision(True)

    if callable(sends):
        try:
            sends = sends()
        except Exception as exc:  # noqa: BLE001 - what it sends cannot be read: no grant can be bound to it
            sends = [["(the fields could not be read)", f"{type(exc).__name__} {time.time()}"]]
    fingerprint = _fingerprint(kind, element, text, keys, point, context=context, about=about,
                               sends=sent_digest(sends))
    description = describe_action(kind, element, text=text, keys=keys, point=point)
    # What the gate makes this action's fingerprint and words from, both from the same description of it.
    material = {"kind": kind, "element": element, "text": text, "keys": list(keys or []) or None,
                "point": list(point) if point else None, "context": _for_gate(context), "about": about or "",
                "sends": sent_digest(sends)}

    return _decide(fingerprint, description, reason, element, about, material=material)


GATE_CONTROLS = 150


def _for_gate(context):
    """context as the gate gets it: a busy page's list of controls cut short (the action is already held here; the
    list only lets the gate find its own reason, and the caller's stands when it finds none)."""

    context = dict(context or {})
    controls = context.get("controls")
    if isinstance(controls, list):
        context["controls"] = [{"role": str((c or {}).get("role") or "")[:40], "name": str((c or {}).get("name") or "")[:120],
                                "path": str((c or {}).get("path") or "")[:200]}
                               for c in controls[:GATE_CONTROLS] if isinstance(c, dict)]
    return context


def from_material(material):
    """
    (fingerprint, description, element, about) of an action described by `material` (check's, or TheBot's paused
    command). Run by the gate: both come from the same description, so one action's grant cannot be asked for in
    another action's words (round-3 recheck).
    """

    kind = str(material.get("kind") or "")
    if kind == "command":
        command = " ".join(str(material.get("command") or "").split())
        request = " ".join(str(material.get("request") or "").split())
        workdir = str(material.get("workdir") or "")
        fingerprint = hashlib.sha256("".join(["command", command, request, workdir]).encode("utf-8")).hexdigest()[:24]
        if command:
            description = f"run the command `{command[:600]}`" + (f" in {workdir}" if workdir else "")
        else:
            description = f"start this request: {request[:600]}"
        return fingerprint, description, None, ""
    element = material.get("element") if isinstance(material.get("element"), dict) else None
    text = material.get("text")
    text = None if text is None else str(text)
    keys = [str(k) for k in (material.get("keys") or [])] or None
    point = material.get("point")
    point = (point[0], point[1]) if isinstance(point, (list, tuple)) and len(point) >= 2 else None
    context = material.get("context") if isinstance(material.get("context"), dict) else {}
    about = str(material.get("about") or "")
    fingerprint = _fingerprint(kind, element, text, keys, point, context=context, about=about,
                               sends=str(material.get("sends") or ""))
    return fingerprint, describe_action(kind, element, text=text, keys=keys, point=point), element, about


def reason_from_material(material):
    """The gate's own reason for holding an action, when it finds one; None leaves the caller's."""

    if str(material.get("kind") or "") == "command":
        return None
    try:
        element = material.get("element") if isinstance(material.get("element"), dict) else None
        context = material.get("context") if isinstance(material.get("context"), dict) else None
        return assess(str(material.get("kind") or ""), element, text=material.get("text"),
                      keys=material.get("keys"), context=context)
    except Exception:  # noqa: BLE001 - no reason of the gate's own: the caller's stands
        return None


def _decide_local(fingerprint, description, reason, element, about):
    """The decision on a gated action, in this process's own store: a grant for exactly it, or the latest reply as its
    yes, or a pending approval. Run by the approval gate (gate_server.py) for every desktop that has one."""

    with _locked() as state:
        for grant in state["grants"]:
            if grant["fingerprint"] == fingerprint:
                state["grants"].remove(grant)
                return Decision(True, reason=reason, approved=True)
        prior = dict(state.get("last_reply") or {})
        # The action already asked about keeps its request and the words it was asked in: a later call cannot reword it.
        kept = state.get("pending")
        if kept and kept.get("fingerprint") == fingerprint:
            return Decision(False, reason=kept.get("reason"), request=dict(kept))

    # Read outside the lock (a model's reading takes seconds, and every gui action waits on this store). In the gate
    # a reply is only ever the yes of the request it answered, worded by the gate (gate_server.settle): a person's reply
    # to a question the worker wrote never stands as the yes for the next action.
    if not IN_GATE and _reply_grants(prior, description, element, reason, about=about):
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


def _local_take_grant(fingerprint):
    """Remove the grant for exactly this action, if there is one (TheBot took the yes itself)."""

    with _locked() as state:
        state["grants"] = [g for g in state["grants"] if g.get("fingerprint") != fingerprint]


def _local_pending():
    """The approval an engine is currently stopped on, or None."""

    with _locked() as state:
        return state["pending"]


def _local_resolve_reply(reply, asked=""):
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


def _local_last_reading(reply):
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


def _local_has_grant():
    with _locked() as state:
        return bool(state["grants"])


def _local_reset():
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


# ============================================================
# The approval gate as a process of its own
#
# On a desktop with a gate (gate_server.py, run as another Linux user by the container's entrypoint) this process holds
# no state that makes a yes: grants, the pending approval and the person's latest reply live in the gate, and only a
# reply the control plane signed with the desktop's gate key (internal/desktop/gatereply.go) can change them. The
# worker's shell runs as the same user as this agent, so anything kept here, in a file under ~/.decurix or in the
# environment, is the worker's to write. What the worker can do through the gate's socket is ask: check an action
# (which consumes a grant that exists), see what is pending, withdraw grants. It cannot grant.
#
# A desktop without a gate (made before it existed, or a test) keeps the store above, in this process: no boundary,
# said so in docs/gate-boundary.md.
# ============================================================

import os
import socket

# Fixed paths, not settings: a path the worker's environment could change is a way to point this at nothing.
GATE_DIR = "/run/decurix-gate"
GATE_SOCKET = GATE_DIR + "/gate.sock"
# How long a call to the gate may take: a check can wait on a model's reading of the person's reply.
GATE_CALL_SECONDS = 90
# How long the agent waits for the gate to settle a reply that reached it as a request.
GATE_REPLY_WAIT_SECONDS = 45
# True only inside the gate's own process (gate_server.py sets it): not a setting, and not in the environment, so
# nothing the worker can set turns a `gui` command onto a store the worker can write.
IN_GATE = False


# Set by the control plane on every desktop it makes (desktop.GateRequiredEnv). It can only turn the gate on: without
# it, the directory above still decides. Not a secret.
GATE_REQUIRED_ENV = "DECURIX_GATE_REQUIRED"


def gate_required():
    """Whether the control plane said this desktop must have a gate."""

    return os.environ.get(GATE_REQUIRED_ENV, "").strip() == "1"


def remote():
    """
    Whether this process asks a gate for its decisions: the desktop has one (its directory exists - the entrypoint
    makes it, as the gate's user, and the worker cannot remove it) or the control plane said it must, and this is not
    the gate itself. A gate that is down, or was never started, is then a gate that answers nothing, and nothing is
    allowed; it is never a reason to fall back to a store the worker can write.
    """

    return not IN_GATE and (os.path.isdir(GATE_DIR) or gate_required())


def gate_call(op, timeout=GATE_CALL_SECONDS, **args):
    """One call to the gate over its socket: {"op": ..., **args} -> the reply's JSON. Raises OSError/ValueError when
    the gate cannot be reached or answers nonsense; a caller that decides anything fails closed on that."""

    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(timeout)
        conn.connect(GATE_SOCKET)
        conn.sendall((json.dumps(dict(args, op=op)) + "\n").encode("utf-8"))
        data = b""
        while not data.endswith(b"\n"):
            chunk = conn.recv(65536)
            if not chunk:
                break
            data += chunk
    reply = json.loads(data.decode("utf-8"))
    if not isinstance(reply, dict) or reply.get("error"):
        raise ValueError(str((reply or {}).get("error") if isinstance(reply, dict) else "bad reply"))
    return reply


def _decide(fingerprint, description, reason, element, about, material=None):
    if not remote():
        return _decide_local(fingerprint, description, reason, element, about)
    try:
        if material is None:
            raise ValueError("no description of the action to give the gate")
        said = gate_call("check", material=json.loads(json.dumps(material, default=str)), reason=reason)
        return Decision(bool(said["allowed"]), reason=reason, approved=bool(said.get("approved")),
                        request=said.get("request"), by_reply=said.get("by_reply") or "")
    except (OSError, ValueError, KeyError) as exc:
        # The gate did not answer: nothing is allowed, and no pending request exists for a reply to settle.
        request = {"id": uuid.uuid4().hex[:12], "fingerprint": fingerprint, "reason": reason,
                   "action": description, "about": about or "", "created": time.time()}
        try:
            from decurix_agent.logging_utils import log
            log(f"The approval gate did not answer ({type(exc).__name__}: {exc}); {description[:80]} is not allowed.")
        except Exception:  # noqa: BLE001
            pass
        return Decision(False, reason=reason, request=request)


def pending():
    """The approval an engine is currently stopped on, or None."""

    if not remote():
        return _local_pending()
    try:
        return gate_call("pending").get("pending")
    except (OSError, ValueError):
        return None


def has_grant():
    if not remote():
        return _local_has_grant()
    try:
        return bool(gate_call("has_grant").get("grant"))
    except (OSError, ValueError):
        return False


def reset():
    """Forget pending approvals and grants - a new task starts clean."""

    if not remote():
        return _local_reset()
    try:
        gate_call("reset")
    except (OSError, ValueError):
        pass


def note_reply(text, asked=""):
    """With a gate, the person's reply was already given to it, signed; a copy from here would be the worker's word."""

    if not remote():
        return _local_note_reply(text, asked)


def resolve_reply(reply, asked=""):
    """
    "granted", "declined" or None. With a gate this asks it what it did with this very reply (it settled the signed
    copy the control plane sent); a reply that reached this agent without that copy settles nothing.
    """

    if not remote():
        return _local_resolve_reply(reply, asked)
    try:
        return gate_call("outcome", timeout=GATE_REPLY_WAIT_SECONDS + 10, reply=" ".join(str(reply or "").split()),
                         wait=GATE_REPLY_WAIT_SECONDS).get("outcome")
    except (OSError, ValueError):
        return None


def gate_reading(reply, wait=20.0):
    """The gate's reading of a person's reply it settled ({"decision", "condition"}), waiting up to `wait` seconds for
    the gate to finish reading it; None when the gate read no signed copy of it (or there is no gate)."""

    if not remote():
        return None
    deadline = time.monotonic() + max(0.0, float(wait))
    while True:
        said = last_reading(reply)
        if said or time.monotonic() >= deadline:
            return said or None
        time.sleep(0.5)


def signed_question():
    """The gate's pending request with its own words and signature ({"id", "question", "sig"}), or None: sent with an
    approval so the control plane shows the person the gate's words and binds their reply to that request."""

    if not remote():
        return None
    try:
        request = gate_call("pending").get("pending")
    except (OSError, ValueError):
        return None
    if not isinstance(request, dict) or not request.get("sig"):
        return None
    return {"id": request.get("id"), "question": request.get("question"), "sig": request.get("sig")}


def last_reading(reply):
    """The reading made of this very reply (decision and condition), or None."""

    if not remote():
        return _local_last_reading(reply)
    try:
        return gate_call("last_reading", reply=" ".join(str(reply or "").split())).get("reading")
    except (OSError, ValueError):
        return None
