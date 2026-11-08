"""
Choosing an entry in a list: select's machinery (docs/computer-use.md, "Select").

Split out of core.py (follow-up O-10): core observes, acts and verifies; this module holds how `select` finds and takes
an entry - a native <select>, a datalist, a custom dropdown or a suggesting field searched by the value's words or by
an identifier the documents give (_select_native_by_id, _select_datalist, _select_custom, _select_in_list), radio
choices, what a list offers (_offered, _offers_nothing), and "nothing else in its place": a search that found nothing
is remembered for its field (FAILED_SEARCH_FILE) and an entry standing in for what was asked is refused there until a
person answers (forget_failed_search, routed through the screen broker as core.forget_failed_search). What the last
select took and the resolution facts it wrote are kept here (_selected, _resolved) and read by core.act.

core imports this module and re-exports what other modules and tests read through it. This module never imports core:
the look at the screen and the task now are core's, handed over at its import (use_core).
"""

import json
import re
import time

from decurix_agent import config
from decurix_agent.computer import cdp, identity, privacy, screen_call, xinput
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


# ============================================================
# What this module calls back in core
#
# The look at the screen (observe) and the task now (_task_now) live in core, which imports this module; so core hands them over at its import (use_core) instead of being
# imported here. Each is looked up in core when called: a test that patches core.observe or core._task_now patches what this module
# reads.
# ============================================================

_CORE = {}


def use_core(**calls):
    """Take the calls this module makes back into core (observe, _task_now) from core at its import."""

    _CORE.update(calls)


def observe(*args, **kwargs):
    """The screen as it is now (core.observe)."""

    return _CORE["observe"](*args, **kwargs)


def _task_now():
    """The task the worker is on now (core._task_now)."""

    return _CORE["_task_now"]()


# Page elements typing can never go into.
NOT_A_FIELD = {"row", "button", "link", "heading", "image", "img", "tab", "menuitem", "option",
               "checkbox", "radio", "listitem", "list", "table", "navigation"}

# What typing does go into, for naming the fields a refusal points at.
TYPING_ROLES = {"textbox", "document", "combobox", "searchbox", "spinbutton", "password text", "entry", "editbar"}


# How long a custom dropdown has to show its options once clicked, and once typed into.
CUSTOM_OPTIONS_SECONDS = 6.0
# The least a list is given to answer a search before two equal readings of it count as its answer.
MIN_LIST_ANSWER = 2.0
CUSTOM_OPEN_SECONDS = 1.5
# How many of the screen's own looks a list is given to answer on a slow host, on top of MIN_LIST_ANSWER.
SLOW_LOOKS = 3


def list_answer_wait(look_took):
    """
    The least a list is given to answer a search before two equal readings count as its answer: MIN_LIST_ANSWER, or
    SLOW_LOOKS times what one look at the screen took, whichever is longer. Found live (2026-11-08, a host at load 24,
    a look ~8 s): two readings of a list that had not yet answered read the same, every search - "Developer
    Workstation" among them - was taken to have found nothing, and a replay stood on a bill's line it could have entered.
    """

    return max(MIN_LIST_ANSWER, SLOW_LOOKS * max(0.0, look_took))

# What a dropdown's list shows: its options, and in many kits (the web app) plain links.
_SUGGESTION_ROLES = ("option", "menuitem", "listitem", "link")

# Entries a list offers besides the choices: they make or search, never pick.
# (A line of that kind often opens with an icon: a character that is no letter.)
_NOT_A_CHOICE = re.compile(r"^[\W_]*(create|add|search more|advanced search|new|filter(?:ed)? by|no (?:results|matches))\b", re.I)


def _offers_nothing(before, after, field=None):
    """Whether typing into a suggesting field left a list whose new entries only create or search - none to pick."""

    known = {e.get("path") for e in (before or {}).get("elements") or []}
    offered = [e for e in (after or {}).get("elements") or []
               if e.get("path") not in known and e.get("role") in _SUGGESTION_ROLES and e.get("name")]
    if offered and all(_NOT_A_CHOICE.match(e["name"]) for e in offered):
        return True
    # A list that was open before the typing shows nothing "new": judge what it holds now. Found live - the
    # field had been clicked first, so its create and search lines were already on the page, and the typing
    # that emptied the list of every real entry was not seen to have left none.
    shown = _list_under(field, after)
    return bool(shown) and all(_NOT_A_CHOICE.match(e["name"]) for e in shown)


# How far below a field its list of suggestions may reach.
LIST_REACH = 420


def _list_under(field, observation):
    """
    The options of the list a field has open: those drawn right under it (or
    over it), in its own column. A page holds other options too - every
    <select> on a form has its own, wherever it is - and found live they made
    an emptied list of suggestions look as if it still offered something.
    """

    box = (field or {}).get("box")
    if not box:
        return []
    left, top, right, bottom = box[0], box[1], box[0] + box[2], box[1] + box[3]
    out = []
    for e in (observation or {}).get("elements") or []:
        b = e.get("box")
        if e.get("role") != "option" or not e.get("name") or not b or "offscreen" in (e.get("states") or []):
            continue
        if b[0] < right and b[0] + b[2] > left and bottom - 12 <= b[1] <= bottom + LIST_REACH:
            out.append(e)
    if out:
        return out
    # No room below: a list drawn over its field ends at the field's top.
    return [e for e in (observation or {}).get("elements") or []
            if e.get("role") == "option" and e.get("name") and e.get("box")
            and "offscreen" not in (e.get("states") or [])
            and e["box"][0] < right and e["box"][0] + e["box"][2] > left
            and top - LIST_REACH <= e["box"][1] and e["box"][1] + e["box"][3] <= top + 12
            and top - (e["box"][1] + e["box"][3]) <= 160]


# How many of a suggesting field's entries are named after typing into it.
OFFERED_SHOWN = 8


def _offered(after, field=None):
    """The entries a suggesting field's list shows now that can be picked - not its "create" or "search" lines."""

    # Options only: a page's links and menu items are there whatever is typed.
    return [e for e in _list_under(field, after) if not _NOT_A_CHOICE.match(e["name"])]


def _words(text):
    return set(re.findall(r"\w+", (text or "").lower()))


def _pick(candidates, option):
    """
    The one candidate labelled `option` - the same name (identity.same: case, spaces and punctuation aside) - or None.
    An entry that only starts with it or holds its words is another entry ("Ravi Kumar Jain" for "Ravi Kumar"): it is
    left in the list the search offered, which is put to the engine, never clicked. Found in review (2026-11-03,
    D-11): such an entry was clicked and reported as the one asked for.
    """

    hits = [e for e in candidates if identity.same(e.get("name"), option)]
    if len(hits) > 1:
        raise ComputerError(f"several options are named {option!r} - pick one yourself by its ref")
    return hits[0] if hits else None


_PLAIN_NUMBER = re.compile(r"[+-]?\d[\d,]*(?:\.\d+)?")


_DECORATED_NUMBER = re.compile(r"(?:[^\w\s]{1,3}\s*|[A-Za-z]{1,3}\.?\s+|[A-Za-z]{1,3}\.)?[-+]?[\d.,]+(?:\s*[^\w\s]{1,3}|\s+[A-Za-z]{1,4})?")


def _decorated_number(text, held):
    """Whether `text` is a number written with something a number field does not take, for a field that held a plain one."""

    text, held = (text or "").strip(), str(held or "").strip()
    if not held or not _PLAIN_NUMBER.fullmatch(held) or _PLAIN_NUMBER.fullmatch(text):
        return False
    # A decoration is a sign or a unit beside the number ("Rs 3,306", "3306 INR", "15 kg"), never a code whose letters
    # are joined to its digits ("INV3306", "ABC-3306"): found live (2026-10-26), a bill number typed into a field that
    # had held the form's default "1" drew "a number field takes digits only", and the worker asked the person.
    return bool(_DECORATED_NUMBER.fullmatch(text))


def _stem(word):
    """A word as compared across a singular and a plural: "Chairs" and "Chair" are one."""

    word = word.lower()
    return word[:-1] if len(word) > 3 and word.endswith("s") and not word.endswith("ss") else word


def _singular(word):
    """A plural word in the singular, as typed into a search ("Mugs" -> "Mug", "T-shirts" -> "T-shirt"); else as it is."""

    return word[:-1] if _stem(word) != word.lower() else word


def _own_name(label):
    """
    The words of an entry's own name. A list often shows an entry as its name
    and then its description, which begins with the name again ("Leased Line
    Leased Line, Services"): the part said twice is the name.
    """

    tokens = [_stem(t) for t in re.findall(r"\w+", label or "")]
    for size in range(len(tokens) // 2, 0, -1):
        if tokens[:size] == tokens[size:2 * size]:
            return tokens[:size]
    return tokens


def _own_label(label):
    """An entry's own name as the list writes it ("USB-C Monitor" of "USB-C Monitor USB-C Monitor, Products")."""

    spans = [m.span() for m in re.finditer(r"\w+", label or "")]
    size = len(_own_name(label))
    if not spans or size >= len(spans):
        return (label or "").strip()
    return label[:spans[size - 1][1]].strip()


def _named_within(label, text):
    """
    Whether an entry's whole name lies within `text`: every word of the name
    is a word of the text, and the name has at least two. A document describes
    a thing in more words than the application's record of it has - "Premium
    Lined Notebook 200 Pages (Pack of 5)" for a record named "Lined Notebook"
    - so the record is found inside the description, never the other way
    round. One shared word proves nothing and is not enough.
    """

    name = set(_own_name(label))
    return len(name) >= 2 and name <= {_stem(w) for w in re.findall(r"\w+", text or "")}


# ============================================================
# Nothing else in its place
#
# When a list has no entry for what was asked, the only honest moves are the
# same thing under the list's own name, or a question. Found live: a document
# named a company the application did not have; the search said so and listed
# what there was; the engine then selected another company from that list and
# saved the record under it. Its final check caught it - after the record
# existed. So a search that found nothing is remembered for its field, and an
# entry that shares no word with what was asked is refused there until a
# person has answered (watchers calls forget_failed_search on a reply).
# ============================================================

FAILED_SEARCH_FILE = config.DECURIX_DIR / "computer" / "failed-search.json"
FAILED_SEARCH_SECONDS = 1800
# Words that say what kind of body or thing something is, not which one.
GENERIC_WORDS = {"ltd", "limited", "pvt", "private", "inc", "llp", "llc", "co", "company", "corp", "corporation", "plc",
                 "gmbh", "the", "and", "of", "for", "with", "new", "group", "services", "service", "products", "product"}


def _telling_words(text):
    return {w for w in (_stem(t) for t in re.findall(r"\w+", text or "")) if len(w) >= 3 and w not in GENERIC_WORDS}


def _note_failed_search(element, wanted):
    try:
        FAILED_SEARCH_FILE.parent.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(FAILED_SEARCH_FILE, {"at": time.time(), "task": _task_now(),
                                                               "field": element.get("name") or "",
                                                               "role": element.get("role") or "", "wanted": wanted})
    except OSError:
        pass


@screen_call.routed("core.forget_failed_search")
def forget_failed_search():
    """A person has answered, or the search has since found its entry: what failed before no longer binds."""

    try:
        FAILED_SEARCH_FILE.unlink()
    except OSError:
        pass


def _in_place_of(element, entry_name):
    """
    What was searched for in `element` (a field) and not found, when
    `entry_name` shares no telling word with it - i.e. the entry would stand in
    for something it is not; else None.
    """

    try:
        kept = json.loads(FAILED_SEARCH_FILE.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if time.time() - float(kept.get("at") or 0) > FAILED_SEARCH_SECONDS:
        return None
    # Of the task it happened in only. Found live: the next task could not select its own, correct record,
    # because the task before it had searched that field for something else and found nothing.
    if kept.get("task") != _task_now():
        return None
    if element is not None and (kept.get("field"), kept.get("role")) != (element.get("name") or "", element.get("role") or ""):
        return None
    wanted = kept.get("wanted") or ""
    if _telling_words(wanted) & _telling_words(" ".join(_own_name(entry_name)) or entry_name):
        return None
    if _person_said(wanted, entry_name):
        return None
    if _in_documents(entry_name):
        return None
    return wanted


_DOCUMENT_WORDS = {}


def _in_documents(entry_name):
    """
    Whether one of the documents this task's request brought names `entry_name`
    (every telling word of its own name is in the document). Such an entry is
    this run's value, not something in its place. Found live (2026-10-13): a
    worker read a remittance's addressee ("To: Example Inc - Accounts
    Receivable") as the paying customer, searched for it, found nothing - and
    the guard then refused the payer the remittance itself names, so the
    worker could not correct its own misreading and asked a person.
    """

    words = _telling_words(" ".join(_own_name(entry_name)) or entry_name)
    if not words:
        return False
    try:
        from decurix_agent import task_context
        from decurix_agent.tools import office
        paths = task_context.current_files()[:4]
    except Exception:
        return False
    from decurix_agent.computer import broker
    for path in paths:
        if path not in _DOCUMENT_WORDS:
            try:
                # Broker mode: read from what the caller handed over (gui_client.task_documents), never by its name.
                _DOCUMENT_WORDS[path] = _telling_words(office.read_document(broker.local_copy(path)).get("text") or "")
            except Exception:
                _DOCUMENT_WORDS[path] = set()
        if words <= _DOCUMENT_WORDS[path]:
            return True
    return False


def _typed_in_place_of(element, text):
    """What was searched for in a field that looks records up and not found, when `text` typed into it now is another thing; else None."""

    if element is None or not text or element.get("role") not in _SEARCHED_ROLES:
        return None
    return _in_place_of(element, text)


def _person_said(wanted, entry_name):
    """
    Whether the person's own words for this task name `entry_name` for what
    was searched for: one sentence of theirs that holds both. Found live: a
    person taught "a van, a truck and a lorry are all the kind Goods Vehicle";
    the document said lorry, the list had no lorry, and the entry they had named
    for it was refused as "another thing in its place" - so the worker asked
    them what they had already said. A list's entry nobody named for it is
    still refused.
    """

    try:
        from decurix_agent import guidance
        said = guidance.persons_words()
    except Exception:
        return False

    entry = " ".join(re.findall(r"\w+", (entry_name or "").lower()))
    own = " ".join(_own_name(entry_name))
    # The words that are the searched value's own, not the entry's: the sentence must tie one of them to the entry.
    telling = _telling_words(wanted) - _telling_words(entry_name)
    if not said or not entry or not telling:
        return False

    for sentence in re.split(r"[.;!?\n]+|\",\s*\"", said):
        words = re.findall(r"\w+", sentence.lower())
        plain, stemmed = " " + " ".join(words) + " ", " " + " ".join(_stem(w) for w in words) + " "
        if (f" {entry} " in plain or (own and f" {own} " in stemmed)) and telling & set(stemmed.split()):
            return True
    return False


SUBSTITUTE = ("{field} has no entry for {wanted!r} - the search for it found none - and {entry!r} shares no word with "
              "it: that is another thing, not this one under another name, and nothing was chosen. Do not put a "
              "different record in its place. If what is asked for is not in the list, stop and ask the person (HELP), "
              "saying what the document names and what the list offers")


# What select chose, when it was not an entry spelt as it was asked for: act() says so in its result.
# What the last select took: the entry's name, why, whether an identifier the documents give chose it (`by_id`: set
# only where resolve matched one - the structured fact the ledger's pick check reads, never the summary's words), and
# the entries the list offered (`offered`, when they were read; None when not).
_selected = {"name": None, "why": "", "by_id": False, "offered": None}
# The most entries an action's record keeps of what a list offered; a longer list is kept as not read (None), so a
# check that needs the whole list never takes a part of it for all of it.
MAX_OFFERED_KEPT = 200

# How many searches a suggesting field is given before select says what it offered.
MAX_SEARCHES = 10
# A value's head searched as runs of its words only up to this many words; beyond it, by its distinctive words.
MAX_RUN_WORDS = 6
MAX_WORD_SEARCHES = 3
# Words that name nothing on their own.
FILLER_WORDS = {"about", "after", "their", "there", "these", "those", "which", "where", "while", "under", "other",
                "between", "within", "without", "through", "including", "against", "during", "before"}


def _searches(option):
    """
    What to type into a searching dropdown, most specific first: the value,
    what comes before its first bracket or comma, its first two words, its
    first word. A list searches for the typed text as one piece, so a variant
    written in another order ("Desk (White, Aluminium)" for the web app's "Desk
    (Aluminium, White)") finds nothing until the search is shorter; the pick is
    still judged against the whole value (_pick).
    """

    words = option.split()
    head = re.split(r"[(,]", option)[0].strip()
    tries = [option.strip(), head]
    # Runs of a short head's words from inside it, longest first: a bill writes "Dedicated Internet Leased Line (ILL)
    # 1 Gbps" where the application's item is "Internet Leased Line" - found live, every search began with "Dedicated" and
    # the item was never offered, so the engine asked a person for something the list had.
    hw = head.split()
    if len(hw) <= MAX_RUN_WORDS:
        for n in range(len(hw) - 1, 1, -1):
            for i in range(1, len(hw) - n + 1):
                run = " ".join(hw[i:i + n])
                tries.append(run)
                # ...and in the singular right after it: a list matches text as a piece, so an order's "coffee
                # mugs" never finds the record "Coffee Mug" (found live, 2026-10-13).
                if _singular(hw[i + n - 1]) != hw[i + n - 1]:
                    tries.append(" ".join(hw[i:i + n - 1] + [_singular(hw[i + n - 1])]))
    # Then its most distinctive words alone, longest first. A long line holds the system's short name somewhere in
    # the middle, and a list searches for a piece of text, not for words: found live, "Professional Fees for the
    # Statutory Audit & Compliance Filing Q2" never found "Statutory Audit & Compliance" (the "&" breaks
    # every run of words), and the engine, offered nothing, clicked an unrelated entry of the open list.
    # A word keeps the punctuation inside it ("T-shirts"): stripped to "Tshirt" it matched nothing (found live).
    bare = lambda w: re.sub(r"^\W+|\W+$", "", w)
    distinct = sorted({w for w in (bare(w) for w in hw) if len(re.sub(r"\W", "", w)) >= 5 and w.lower() not in FILLER_WORDS},
                      key=lambda w: (-len(w), hw.index(next(x for x in hw if bare(x) == w))))
    # Searched in the singular: a list matches the typed text as a piece, and a document's "Monitors" is not in a
    # record named "Monitor" (found live: the plural found nothing, and the record was one letter away).
    tries += [_singular(w) for w in distinct[:MAX_WORD_SEARCHES]]
    tries += [" ".join(words[:2]), words[0] if words else ""]
    out = []
    for t in tries:
        if len(t) >= 3 and t not in out:
            out.append(t)
    return out[:MAX_SEARCHES]


_SEARCHED_ROLES = ("combobox", "textbox", "searchbox")
EDITOR_SECONDS = 3.0


def _editor_opened_by(element):
    """The typable field of the page that has the keyboard after `element` (not a field itself) was pressed, or None."""

    deadline = time.monotonic() + EDITOR_SECONDS
    while True:
        for e in observe()["elements"]:
            if (e.get("source") == "cdp" and e.get("role") in _SEARCHED_ROLES and e.get("path") != element.get("path")
                    and "focused" in (e.get("states") or [])):
                return e
        if time.monotonic() >= deadline:
            return None
        time.sleep(0.3)


def _choose_radio(element, option):
    """
    Choose `option` among the radio buttons of the group `element` is in: the one named it, else the one whose name
    it starts with, else the one radio whose whole name lies within it ("Contractor (12 months)" -> Contractor) - never
    one of two.
    """

    group = (element.get("path") or "").rsplit(" > ", 1)[0]
    radios = [e for e in observe(remember=False)["elements"]
              if e.get("role") == "radio" and (e.get("path") or "").rsplit(" > ", 1)[0] == group]
    want = " ".join(option.lower().split())
    words = lambda s: " " + re.sub(r"[^a-z0-9]+", " ", s.lower()).strip() + " "
    hit = ([e for e in radios if " ".join((e.get("name") or "").lower().split()) == want]
           or [e for e in radios if (e.get("name") or "").lower().startswith(want)]
           or [e for e in radios if words(e.get("name") or "").strip() and words(e.get("name") or "") in words(option)])
    if len(hit) != 1:
        offered = ", ".join(el._quote(e.get("name") or "") for e in radios) or "none"
        raise ComputerError(f"no single radio button in that group is {el._quote(option)} - it offers {offered}")
    if element.get("source") == "cdp":
        cdp.click(hit[0])
        return "cdp"
    x, y = el.center(hit[0])
    xinput.click(x, y)
    return "xdotool"


# The resolution facts the select being made wrote (resolve.py): act() puts them in its summary. One line each.
_resolved = []
# Fields in which a search is sent with Enter when typing alone showed nothing (a page that answers a query once sent);
# never a combobox, whose Enter takes the highlighted entry.
_QUERY_ROLES = ("textbox", "searchbox")
# How many name searches a list page is sent after the identifier found nothing: each is a page load.
MAX_SENT_NAME_SEARCHES = 4


def _keep_fact(track, ident, line):
    """One resolution fact: kept for the gate (evidence note), for the summary (_resolved) and against a second one."""

    from decurix_agent.computer import resolve

    track["facts"][ident] = line
    resolve.note(line)
    _resolved.append(line)


def _wait_page_change(before, seconds=None):
    """Whether the page's elements changed within `seconds` of a query sent (paths, as `before` has them)."""

    deadline = time.monotonic() + (CUSTOM_OPTIONS_SECONDS if seconds is None else seconds)
    while True:
        if {e.get("path") for e in observe()["elements"]} != before:
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.3)


def _row_entries(now):
    """The entries of a page's table rows (a link beside cells): what a list page offers once a query was sent to it."""

    from decurix_agent.computer import resolve

    cells = [e for e in now if e.get("role") in ("cell", "gridcell", "rowheader") and e.get("box")]
    return [e for e in now if e.get("source") == "cdp" and e.get("box") and e.get("role") in _SUGGESTION_ROLES
            and not _NOT_A_CHOICE.match(e.get("name") or "") and any(resolve.same_row(e, c) for c in cells)]


def _matched_by_identifier(field, elements, ids, track):
    """
    Choose, among `elements`, the one record holding one of `ids` (resolve.matches): True when it was chosen - or
    found, when its row has nothing to press - and its fact kept. Two records holding one identifier are not chosen
    between: the ambiguity is kept and raised. False when none holds any. A field's own value never counts.
    """

    from decurix_agent.computer import resolve

    pool = [e for e in elements if e.get("source") == "cdp" and e.get("role") not in TYPING_ROLES
            and e.get("path") != field.get("path")]
    for ident in ids:
        if track["facts"].get(ident):
            continue
        found = resolve.matches(pool, ident)
        if not found:
            continue
        if len(found) > 1:
            _keep_fact(track, ident, resolve.ambiguous_line(ident, [name for _, name in found]))
            raise ComputerError(f"{el.describe(field)}: {len(found)} records hold {ident} - "
                                + ", ".join(el._quote(name, 60) for _, name in found)
                                + ". One identifier, two records: none was chosen. Use the one the documents or the "
                                "person name; when neither does, ask the person which (HELP) and tell them the "
                                "application holds two.")
        entry, name = found[0]
        if entry is not None:
            cdp.click(entry)
        _selected.update(name=name, why=f"matched by identifier {ident}", by_id=True)
        _keep_fact(track, ident, resolve.matched_line(ident, name))
        forget_failed_search()
        return True
    return False


def _select_datalist(element, option, ids=None):
    """
    A field with a <datalist>: the browser draws its list outside the page, so nothing "appears" to search. Its entries
    are read through the page instead; the one that is `option` (or the one entry holding one of `ids`, or the one
    entry whose own name lies within the value) is typed exactly, and the field keeps it. "cdp" when done; None when
    the field has no datalist. Found live (2026-10-26): searched for "Sales@18%" in such a field, the shorter searches
    left "Sales@18" in it and the application refused the save.
    """

    from decurix_agent.computer import resolve

    try:
        entries = cdp.datalist_options(element)
    except (ComputerError, KeyError, TypeError):
        return None
    if entries is None:
        return None
    want = " ".join(option.split()).casefold()
    pick, by_id = None, False
    _selected["offered"] = list(entries)
    for ident in ids or []:
        holders = list(dict.fromkeys(e for e in entries if resolve.holds(e, ident)))
        if len(holders) == 1:
            pick, by_id = holders[0], True
            line = resolve.matched_line(ident, pick)
            resolve.note(line)
            _resolved.append(line)
            break
        if len(holders) > 1:
            line = resolve.ambiguous_line(ident, holders)
            resolve.note(line)
            _resolved.append(line)
            raise ComputerError(f"{el.describe(element)}: {len(holders)} entries hold {ident} - " + ", ".join(el._quote(h, 60) for h in holders))
    if pick is None and ids:
        # The list shows names only; the identifier was resolved on the application's list page (a resolution fact
        # names the record): that record's entry is the one. Found live (2026-10-26): the worker asked the picker for
        # a group it had read off a row and was offered the real ledger.
        from decurix_agent import task_context
        for ident in ids:
            found = resolve.resolution(task_context.current(), ident)
            if found.get("status") == "matched" and found.get("name"):
                match = [e for e in entries if " ".join(e.split()).casefold() == " ".join(found["name"].split()).casefold()]
                if match:
                    pick, by_id = match[0], True
                    _resolved.append(found["line"])
                    break
    if pick is None:
        exact = [e for e in entries if " ".join(e.split()).casefold() == want]
        pick = exact[0] if exact else None
    if pick is None:
        within = [e for e in entries if _named_within(e, option)]
        if len(within) == 1:
            pick = within[0]
    if pick is None:
        offered = ", ".join(el._quote(e, 50) for e in entries[:OFFERED_SHOWN]) + (f" (+{len(entries) - OFFERED_SHOWN} more)" if len(entries) > OFFERED_SHOWN else "")
        how = (f" The list shows names, not identifiers: resolve {', '.join(ids)} on the application's own list page "
               f"first (gui select <its Find field> \"name\" --id {ids[0]}), then choose the record it names here." if ids else
               " Choose one of them by its exact name if it is the same thing; if none is, ask.")
        raise ComputerError(f"{el.describe(element)} offers no entry that is {el._quote(option, 60)}. Its entries: "
                            f"{offered or 'none'}.{how}")
    cdp.type_text(element, pick)
    _selected.update(name=pick, why="an entry of the field's own list" if not _resolved else _resolved[-1], by_id=by_id)
    return "cdp"


def _select_native_by_id(element, option, ids):
    """
    In a native <select>, choose the one option whose label holds one of `ids`: "cdp" when chosen. None when the
    element is no <select> (the drawn list is searched instead) or no label holds any of them (the choice by name
    follows; the whole list was read, so the fact says none holds it). Two labels holding one identifier are not
    chosen between.
    """

    from decurix_agent.computer import resolve

    try:
        labels = cdp.native_options(element)
    except (ComputerError, KeyError, TypeError):
        return None
    if not labels:
        return None
    _selected["offered"] = list(labels)
    for ident in ids:
        holders = [label for label in labels if resolve.holds(label, ident) and not resolve._NOT_A_RECORD.search(label)]
        names = list(dict.fromkeys(" ".join(label.split()) for label in holders))
        if not names:
            continue
        if len(names) > 1:
            line = resolve.ambiguous_line(ident, names)
            resolve.note(line)
            _resolved.append(line)
            raise ComputerError(f"{el.describe(element)}: {len(names)} options hold {ident} - "
                                + ", ".join(el._quote(n, 60) for n in names)
                                + ". One identifier, two records: none was chosen. Use the one the documents or the "
                                "person name; when neither does, ask the person which (HELP).")
        cdp.select_option(element, holders[0])
        _selected.update(name=names[0], why=f"matched by identifier {ident}", by_id=True)
        line = resolve.matched_line(ident, names[0])
        resolve.note(line)
        _resolved.append(line)
        return "cdp"
    for ident in ids:
        line = resolve.none_line(ident, [ident], [option], labels)
        resolve.note(line)
        _resolved.append(line)
    return None


def _select_custom(element, option, ids=None):
    """
    Pick `option` in a dropdown a page drew itself (_select_in_list). With `ids` - the identifiers a document gives
    the record (a tax number, an account number, an email) - the list is searched by them first and an entry whose
    text holds one is the record whatever its name; what each identifier resolved to is kept as one fact
    (resolve.py: matched / no record holds it / ambiguous / unanswered), for the engine's summary and for the save
    gate (_unsearched_record). Found live (2026-10-26, twice): searched by the document's name alone, an existing
    party was made again.
    """

    ids = list(dict.fromkeys(" ".join(str(i).split()) for i in (ids or []) if str(i).strip()))
    _resolved.clear()
    if not ids:
        return _select_in_list(element, option, [], None)
    from decurix_agent.computer import resolve

    track = {"searched": {i: [] for i in ids}, "name_searches": [], "offered": [], "answered": False, "facts": {}}

    def close():
        for ident in ids:
            if track["facts"].get(ident) or not track["searched"][ident]:
                continue  # resolved already, or never searched for: nothing to say about it
            if track["answered"]:
                line = resolve.none_line(ident, track["searched"][ident], track["name_searches"], track["offered"])
            else:
                line = resolve.unanswered_line(ident, track["searched"][ident], el.describe(element))
            _keep_fact(track, ident, line)

    try:
        _select_in_list(element, option, ids, track)
    except ComputerError as exc:
        close()
        if _resolved:
            raise ComputerError(f"{exc}\n" + "\n".join(_resolved)) from exc
        raise
    close()


def _select_in_list(element, option, ids, track):
    """
    Pick `option` in a dropdown a page drew itself (no <select>: OrangeHRM's
    "-- Select --", the web app's customer and product fields, most component kits)
    the way a person does: open it - typing the value, then shorter searches,
    when it only suggests once typed into - then click the matching entry
    among what appeared. Only what appeared counts, so a link elsewhere on the
    page with the same words is never taken; "Create ..." and "Search
    More..." are never choices; two entries that fit equally are not guessed
    between. Live, typing a product and clicking the next field left the web app's
    line with no product at all.
    """

    def refresh(now):
        nonlocal element
        if not element.get("path"):
            return
        fresh = next((e for e in now if e.get("path") == element["path"]
                      and e.get("role") == element.get("role") and e.get("name") == element.get("name")), None)
        if fresh is not None:
            element = fresh

    def expanded():
        now = observe()["elements"]
        refresh(now)
        me = next((e for e in now if e.get("path") == element.get("path")), None)
        return now, bool(me and "expanded" in (me.get("states") or []))

    # A list already open (an earlier try left it) would count as on the page
    # before the click, so none of its entries would count as appearing.
    now, is_open = expanded()
    # Escape can close the enclosing row editor as well as its dropdown
    # (the web app). Use already-visible options instead of dismissing the list.
    known = {e["path"] for e in now if not (is_open and e["role"] in ("option", "menuitem"))}
    replaced = _in_place_of(element, option)
    if replaced:
        raise ComputerError(SUBSTITUTE.format(field=el.describe(element), wanted=replaced, entry=option))
    if not is_open:
        cdp.click(element)
    if element["role"] not in _SEARCHED_ROLES:
        # A cell of a table that becomes its editor when pressed: the field
        # that took the keyboard is the one to search. Found live: select
        # aimed at a row's "Item" cell (plain text until pressed) searched
        # nothing, reported the list as it opens - eight entries of thirteen -
        # as all there was, and the task stopped for a record the list had.
        editor = _editor_opened_by(element)
        if editor is not None:
            element = editor
    from decurix_agent.computer import resolve

    # (search, the identifier it is for or None): the identifiers' searches first, whole to digits, then the name's.
    searches = [(s, None) for s in _searches(option)] if element["role"] in _SEARCHED_ROLES else []
    if ids and element["role"] in _SEARCHED_ROLES:
        searches = [(s, ident) for ident in ids for s in resolve.searches(ident)] + searches
    deadline = time.monotonic() + CUSTOM_OPEN_SECONDS
    seen = []
    # Entries whose own name lies within the value, with the search that showed each.
    within, typed, last_names, typed_at = {}, None, None, 0.0
    # A page that answers a query once sent (Enter in a search box): found out when typing alone showed nothing.
    submitted, can_send = False, bool(ids) and element["role"] in _QUERY_ROLES
    # The page's rows before anything was typed: a list page's search box narrows rows that were there already, so
    # none of them "appears" - a change in them is the list's answer.
    rows_before = None
    _selected.update(name=None, why="", by_id=False, offered=None)

    def offered(now):
        return [e for e in now if e["path"] not in known and e["role"] in _SUGGESTION_ROLES
                and e.get("source") == "cdp" and e.get("box")
                and not _NOT_A_CHOICE.match(e.get("name") or "")]

    def entries(now):
        # Once a query was sent, the page's rows are its answer: they were there before too, so none "appeared".
        return _row_entries(now) if submitted else offered(now)

    def send(now):
        # Enter, in a field whose form only asks (cdp.submit_query); then the page's answer. False when not sent.
        if not cdp.submit_query(element):
            return False
        if _wait_page_change({e.get("path") for e in now}) and track is not None:
            track["answered"] = True
        return True

    look_took = 0.0
    while True:
        looked_at = time.monotonic()
        now = observe()["elements"]
        look_took = time.monotonic() - looked_at
        refresh(now)
        fresh = [e for e in now if e["path"] not in known and e.get("source") == "cdp" and e.get("box")]
        if track is not None and fresh and not submitted:
            track["answered"] = True  # the list answered (entries, or its own "create" or "no results" line)
        if ids and element["role"] in _QUERY_ROLES:
            rows = tuple(e.get("name") for e in _row_entries(now))
            if rows_before is None:
                rows_before = rows
            elif track is not None and typed is not None and rows != rows_before:
                track["answered"] = True  # a list page narrowed its rows to the search: that is its answer
        candidates = entries(now)
        # Everything any search offered, in order: the engine chooses among real entries when none is the value.
        seen += [n for n in (e.get("name") or "" for e in candidates) if n and n not in seen]
        # (What every search showed so far: the entries the list offered, as far as they were seen.)
        _selected["offered"] = list(seen)
        if track is not None:
            track["offered"] = list(seen)
        # An entry whose text holds the document's identifier is the record, whatever its name (resolve.py). Among
        # what appeared; or, once a page's search box was typed into or sent, among the whole page: a list page's
        # rows were there before the search narrowed them, and a row holding the identifier is a record holding it.
        whole_page = submitted or (typed is not None and element["role"] in _QUERY_ROLES)
        if ids and _matched_by_identifier(element, now if whole_page else fresh, ids, track):
            return
        hit = _pick(candidates, option)
        if hit is not None:
            cdp.click(hit)
            forget_failed_search()
            return
        for entry in candidates:
            if _named_within(entry["name"], option):
                # The latest search that showed it: the list as it first opened is not one that can be typed again.
                within[entry["name"]] = typed if typed is not None else within.get(entry["name"])

        # A list that reads the same twice running has answered the search: no need to wait it out.
        names = tuple(e.get("name") for e in candidates)
        # (Not before the list has had time to answer at all: on a slow host the old list reads the same twice.)
        settled = typed is not None and names == last_names and time.monotonic() - typed_at >= list_answer_wait(look_took)
        last_names = names
        if time.monotonic() > deadline or settled:
            if can_send and typed is not None and not fresh and not submitted:
                # Typing alone showed nothing: a search box of a page that answers a query once sent. Sent (only a
                # form that asks, never one that saves), the page after it is the list's answer.
                can_send = False
                if send(now):
                    submitted = True
                    # Each page load costs seconds: the name is searched for in its most telling forms only - its
                    # first forms (whole, head, runs from inside it) and its first word, which a list page's search
                    # finds a trade name's relative by.
                    by_name = [s for s in searches if not s[1]]
                    searches = ([s for s in searches if s[1]]
                                + list(dict.fromkeys(by_name[:MAX_SENT_NAME_SEARCHES - 1] + by_name[-1:])))
                    deadline, last_names, typed_at = (time.monotonic() + max(CUSTOM_OPTIONS_SECONDS, 2 * list_answer_wait(look_took)),
                                                       None, time.monotonic())
                    continue
            if not searches:
                break
            typed, ident = searches.pop(0)
            if track is not None:
                (track["searched"][ident] if ident else track["name_searches"]).append(typed)
            cdp.type_text(element, typed)
            if submitted:
                send(now)
            typed_at = time.monotonic()
            deadline, last_names = typed_at + max(CUSTOM_OPTIONS_SECONDS, 2 * list_answer_wait(look_took)), None
        time.sleep(0.3)

    # No entry is spelt as the value is - but exactly one entry of all the
    # searches has a name that lies wholly within it: the application's record
    # of the thing the value describes. Found live, again and again: handed a
    # document's line for a value, an engine searched with the wrong words,
    # clicked entries by where it expected them and asked a person for a
    # record the list had. Two such entries are not chosen between.
    if len(within) == 1:
        (name, search), = within.items()
        # Brought back by its own name first - that is what the list holds -
        # then by the search that showed it. Found live on a slow host: a
        # list answered one search after the next had been typed, the entry
        # was remembered under a search that does not show it, and select
        # said "no option" for a record it had just seen.
        own = _own_label(name) if typed is not None else ""
        again = [s for s in dict.fromkeys([own, search]) if s] or [None]
        for search in again:
            if search is not None:
                cdp.type_text(element, search)
                if submitted:
                    send(now)
            deadline = time.monotonic() + CUSTOM_OPTIONS_SECONDS + (MIN_LIST_ANSWER if search else 0)
            while True:
                now = observe()["elements"]
                refresh(now)
                entry = next((e for e in entries(now) if e.get("name") == name), None)
                if entry is not None:
                    try:
                        cdp.click(entry)
                    except ComputerError:
                        # The list was drawn again between the look and the click: look again.
                        if time.monotonic() >= deadline:
                            raise
                        time.sleep(0.3)
                        continue
                    _selected.update(name=name, why="its whole name is part of that text and no other entry's is: it is the record for it - go on")
                    forget_failed_search()
                    return
                if time.monotonic() >= deadline:
                    break
                time.sleep(0.3)

    # Leave the page as it was found: an open list covers what is under it.
    if expanded()[1]:
        cdp.close_list(element)
    if seen:
        message = (f"{el.describe(element)} has no option {option!r}. Searching for it and for parts of it, it "
                   f"offers: " + ", ".join(seen[:30]) + ". If one of these is the same thing under this system's own "
                   "name, select it by that name; if none is, say so")
        if len(within) > 1:
            message += (". More than one of them is named within that text (" + ", ".join(sorted(within)[:6])
                        + "), so none was taken")
        # A value written with its variant ("Stool (Navy)") against an entry
        # that is only its general item ("Stool", shown with its code as
        # "StoolD_0045"): say so, and what selecting it leads to.
        head = re.split(r"[(,]", option)[0].strip()
        general = [n for n in seen if head and head != option.strip() and n.lower().startswith(head.lower())]
        if len(general) == 1:
            message += (f". {general[0]!r} starts with {head!r}: it is likely the general item that {option!r} is a "
                        f"variant of - select {head!r} (or that entry), then choose the variant in what the page asks next")
        _note_failed_search(element, option)
        raise ComputerError(message)
    if element["role"] not in _SEARCHED_ROLES:
        # Nothing was searched, so nothing is known about what the list has.
        raise ComputerError(f"{el.describe(element)} is not a field and pressing it opened no list: nothing was "
                            f"searched for {option!r}, so this says nothing about whether it exists. Press the place "
                            "where the value goes (a row of a table opens its fields when pressed), look at the "
                            "screen, and select in the field that appears, by its ref")
    _note_failed_search(element, option)
    if element.get("source") == "cdp" and element.get("role") in _SEARCHED_ROLES:
        # The last, shorter search must not stay in the field (found live, 2026-10-26: "Sales@18" was left where
        # "Sales@18%" was asked for, and the application refused the save): the value asked for is put back whole.
        try:
            cdp.type_text(element, option)
        except ComputerError:
            pass
    raise ComputerError(f"{el.describe(element)} showed no options for {option!r}")
