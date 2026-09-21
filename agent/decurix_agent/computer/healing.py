import difflib
import re

from decurix_agent.computer import volatility


# ============================================================
# Healing: finding a control again when it is not quite where it was
#
# replay.resolve finds a step's target by its path, then by role and exact
# name, and halts otherwise. That is the right default and it stays the
# default - but it turns a renamed button ("Save" -> "Save changes"), a
# control that moved into a new container, or two identical "Delete" links
# into a person being paged for something the screen itself can settle.
#
# Orby's Large Action Model and openadapt-flow both keep a ladder of
# increasingly loose ways to re-find a control before giving up. This is its
# first, model-free rung: every plausible candidate on the screen is scored on
# several independent signals, and one is taken only when it clearly stands
# out. "Clearly" is a rule with two halves, so a guess never passes for a match:
#
#   - it scores above ACCEPT, and
#   - it beats the runner-up by MARGIN.
#
# A step that could do harm if it hit the wrong control (one the approval gate
# guards, or one that only sometimes happens) is held to STRICT_ACCEPT, which a
# renamed label alone cannot reach: the position on screen, the container it
# sits in and the controls around it have to agree as well.
#
# Nothing here acts on the screen or calls a model. escalation.py decides what
# happens to a verdict that is not confident.
# ============================================================

ACCEPT = 0.72
STRICT_ACCEPT = 0.85
MARGIN = 0.12
NAME_FLOOR = 0.5
STRICT_NAME_FLOOR = 0.85
DOUBT = 0.40
POOL_FLOOR = 0.30
MAX_POOL = 8
NEIGHBOURS = 4

WEIGHTS = {"name": 0.50, "context": 0.20, "position": 0.15, "role": 0.15}

# Roles that can stand in for one another: a "Save" that became a link, a
# list item that became an option. Anything else is a different control.
FAMILIES = [
    {"button", "link", "menuitem", "toggle", "tab", "icon"},
    {"textbox", "combobox", "spinbutton"},
    {"listitem", "treeitem", "option", "cell"},
    {"checkbox", "radio", "toggle"},
]

# Words a product renames one another to. Deliberately short: a synonym only
# ever lifts a name to 0.8, never to a match, so this list is for the ordinary
# renames ("Save" -> "Submit"), not for meaning.
SYNONYMS = [
    {"save", "submit", "apply", "confirm", "update", "done", "ok"},
    {"cancel", "close", "dismiss", "discard"},
    {"delete", "remove", "trash"},
    {"add", "new", "create"},
    {"search", "find", "filter"},
    {"edit", "modify", "change"},
    {"next", "continue", "proceed"},
    {"back", "previous", "return"},
    {"login", "signin", "logon"},
    {"logout", "signout", "logoff"},
    {"open", "view", "show"},
]

WORD = re.compile(r"[a-z0-9]+")
PATH_SPLIT = " > "


# ============================================================
# Signals
# ============================================================

def _norm(text):
    return " ".join(WORD.findall(volatility.without_counters(text or "").lower()))


def _tokens(text):
    return set(_norm(text).split())


def _synonym_groups(word):
    return [group for group in SYNONYMS if word in group]


def name_similarity(wanted, actual):
    """
    How alike two labels are, 0 to 1. 1 is only ever the same label; a
    synonym is 0.8, and a containing label ("Save" in "Save as") 0.75 - low on
    purpose, because "Delete" and "Delete all" are different actions, so a name
    like that is only taken when the position and the container agree too.
    """

    a, b = _norm(wanted), _norm(actual)

    if not a or not b:
        return 0.0
    if a == b:
        return 1.0

    ta, tb = set(a.split()), set(b.split())
    jaccard = len(ta & tb) / len(ta | tb)
    # Letter overlap is a fair measure of a typo and a poor one of anything
    # else: "save" and "archive" share enough letters to reach 0.6. Only a
    # near-match counts in full.
    ratio = difflib.SequenceMatcher(None, a, b).ratio()
    ratio = ratio * 0.95 if ratio >= 0.8 else ratio * 0.5
    contained = 0.75 if min(len(a), len(b)) >= 3 and (a in b or b in a) else 0.0

    synonym = 0.0
    if len(ta) <= 3 and len(tb) <= 3:
        for word in ta:
            groups = _synonym_groups(word)
            if any(other in group for group in groups for other in tb - ta):
                synonym = 0.8
                break

    return round(min(0.95, max(jaccard, ratio, contained, synonym)), 3)


def compatible(recorded_role, actual_role):
    """1 for the same role, 0.6 for one a control of the same family, else 0."""

    if not recorded_role or recorded_role == actual_role:
        return 1.0
    if any(recorded_role in family and actual_role in family for family in FAMILIES):
        return 0.6
    return 0.0


def parents_of(path):
    """The containers in a path, outermost first: everything before the control itself."""

    return [_norm(part) for part in (path or "").split(PATH_SPLIT)[:-1] if _norm(part)]


def _center(element):
    box = element.get("box")
    if not box:
        return None
    x, y, w, h = box
    return (x + w / 2, y + h / 2)


def anchors_near(element, elements, count=NEIGHBOURS):
    """
    Names of the controls closest to `element` on the screen, nearest first:
    what a person means by "the button next to Cancel". Recorded when a step
    is found and used when it is not.
    """

    here = _center(element)
    if here is None:
        return []

    near = []
    for other in elements:
        if other is element or not other.get("name") or not _center(other):
            continue
        if other.get("group") != element.get("group"):
            continue
        x, y = _center(other)
        near.append(((x - here[0]) ** 2 + (y - here[1]) ** 2, _norm(other["name"])))

    near.sort()
    seen, names = set(), []
    for _, name in near:
        if name and name not in seen and name != _norm(element.get("name")):
            seen.add(name)
            names.append(name)
        if len(names) >= count:
            break

    return names


def _window_signal(recorded, actual):
    if not recorded or not actual:
        return None
    wanted, have = _tokens(recorded), _tokens(actual)
    if not wanted or not have:
        return None
    return len(wanted & have) / len(wanted)


def context_signal(locate, target, candidate, observation):
    """
    How well the surroundings agree, 0 to 1, or None when the recording kept
    nothing to compare: the containers in the path, the window, and the
    controls that stood around it.
    """

    parts = []

    wanted = parents_of((target or {}).get("path"))
    if wanted:
        have = set(parents_of(candidate.get("path")))
        parts.append(sum(1 for parent in wanted if parent in have) / len(wanted))

    window = _window_signal((target or {}).get("window_title"), candidate.get("window_title"))
    if window is not None:
        parts.append(window)

    anchors = (locate or {}).get("anchors") or []
    if anchors:
        near = set(anchors_near(candidate, observation.get("elements") or []))
        parts.append(len(near & set(anchors)) / len(anchors))

    return sum(parts) / len(parts) if parts else None


def position_signal(locate, candidate, observation):
    """
    How close the candidate sits to where the control was, 0 to 1, or None
    when the screen is not the size it was recorded on or nothing was kept.
    """

    box = (locate or {}).get("box")
    size = observation.get("size")
    screen = (locate or {}).get("screen")

    if not box or not size or (screen and list(screen) != list(size)):
        return None

    there = _center(candidate)
    if there is None:
        return None

    recorded = (box[0] + box[2] / 2, box[1] + box[3] / 2)
    diagonal = (size[0] ** 2 + size[1] ** 2) ** 0.5
    distance = ((there[0] - recorded[0]) ** 2 + (there[1] - recorded[1]) ** 2) ** 0.5

    return max(0.0, 1.0 - distance / (0.35 * diagonal))


# ============================================================
# Ranking and deciding
# ============================================================

def aliases(step):
    """The names a step's control is known by: what it was recorded as, and what runs have proven since."""

    target = step.get("target") or {}
    names = [target.get("name")] + [a.get("name") for a in step.get("alternates") or []]
    return [name for name in names if name]


def rank(step, observation):
    """
    Every plausible candidate for a step's target on this screen, best first:
    [{"element", "score", "name", "context", "position"}]. A candidate scoring
    under POOL_FLOOR is not listed - it is not a candidate.
    """

    target = step.get("target") or {}
    locate = step.get("locate") or {}
    recorded_role = target.get("role")
    names = aliases(step)
    ranked = []

    for element in observation.get("elements") or []:
        role = compatible(recorded_role, element.get("role"))
        if role == 0.0:
            continue

        name = max((name_similarity(n, element.get("name")) for n in names), default=0.0)
        context = context_signal(locate, target, element, observation)
        position = position_signal(locate, element, observation)

        # A signal the recording could not supply says nothing either way.
        score = (WEIGHTS["name"] * name + WEIGHTS["role"] * role
                 + WEIGHTS["context"] * (0.5 if context is None else context)
                 + WEIGHTS["position"] * (0.5 if position is None else position))

        states = element.get("states") or []
        if "disabled" in states:
            score -= 0.15
        if "offscreen" in states:
            score -= 0.05

        if score >= POOL_FLOOR:
            ranked.append({"element": element, "score": round(score, 3), "name": round(name, 3),
                           "context": context, "position": position})

    ranked.sort(key=lambda c: c["score"], reverse=True)
    return ranked[:MAX_POOL]


def strict(step):
    """Whether a wrong control would cost something: an approval-gated step, or one that only sometimes happens."""

    return bool(step.get("consequential") or step.get("optional"))


def decide(step, ranked):
    """
    ("confident" | "doubt" | "none", the chosen candidate or None).

    Confident needs the score, the label and the lead over the runner-up all
    to clear their bar - and a stricter bar for a strict step.
    """

    if not ranked:
        return "none", None

    top = ranked[0]
    accept, floor = (STRICT_ACCEPT, STRICT_NAME_FLOOR) if strict(step) else (ACCEPT, NAME_FLOOR)
    lead = top["score"] - (ranked[1]["score"] if len(ranked) > 1 else 0.0)

    if top["score"] >= accept and top["name"] >= floor and lead >= MARGIN:
        return "confident", top

    if top["score"] >= DOUBT:
        return "doubt", top

    return "none", None


def describe(candidate):
    """`button "Save changes" (0.78)`: a candidate as a person or a model reads it."""

    element = candidate["element"]
    name = " ".join((element.get("name") or "").split())[:60]
    return f"{element.get('role', 'element')} \"{name}\" ({candidate['score']:.2f})"
