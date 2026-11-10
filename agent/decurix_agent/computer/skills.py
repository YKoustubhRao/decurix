import copy
import json
import os
import re
from pathlib import Path

from decurix_agent import config
from decurix_agent.computer import constraints, experience, recording, replay


def _rename_file(text, old, new):
    """`text` with the file name `old` replaced by `new` where it stands whole - never inside a longer name
    ("bill.xlsx" is not in "prebill.xlsx"; D-36, review round 3: a substring replace rewrote other files' titles)."""

    return re.sub(r"(?<![\w.-])" + re.escape(old) + r"(?![\w-]|\.\w)", lambda m: new, text)


def bind_sources(skill, bindings):
    """Explicitly bind recorded local input opens to new files, on a run copy.

    No URLs, app launch names, extension changes, implicit basename matching,
    typed values or save destinations are rebound. Callers choose every mapping.
    """
    if not isinstance(bindings, dict):
        raise ValueError("source-file bindings must be an object")
    bound = copy.deepcopy(skill)
    opens = {step["open"] for step in bound.get("steps", []) if step.get("kind") == "open" and isinstance(step.get("open"), str)}
    for source, destination in bindings.items():
        if not isinstance(source, str) or not isinstance(destination, str):
            raise ValueError("source-file bindings need path strings")
        old, new = Path(source), Path(destination)
        if source not in opens or not old.is_absolute() or not new.is_absolute() or not new.is_file():
            raise ValueError("source-file binding must name a recorded local file open and an existing absolute file")
        if not old.suffix or old.suffix.lower() != new.suffix.lower():
            raise ValueError("source-file binding must preserve file type")
        if isinstance((bound.get("starts") or {}).get("window"), str):
            bound["starts"]["window"] = _rename_file(bound["starts"]["window"], old.name, new.name)
        for step in bound["steps"]:
            if step.get("kind") == "open" and step.get("open") == source:
                step["open"] = str(new)
            for key in ("window", "phase_label"):
                if isinstance(step.get(key), str):
                    step[key] = _rename_file(step[key], old.name, new.name)
            for check in step.get("expect", []):
                if check.get("type") in {"window", "active_window"}:
                    check["value"] = _rename_file(check["value"], old.name, new.name)
    return bound


# ============================================================
# Skills
#
# What a desktop has learned to do by being shown (computer/learning.py builds
# one from demonstrations and the answers to its questions). A skill is one
# JSON file that carries its own steps, so it keeps working after the
# recordings it came from are pruned or deleted:
#
#   ~/.decurix/skills/<name>.json
#
# status is "draft" while its questions are still being answered - saved after
# every answer, so an interrupted teaching session picks up where it stopped -
# and "ready" once a person has confirmed it. Only a ready skill runs.
# ============================================================

ROOT = Path(os.environ.get("DECURIX_SKILLS_DIR") or (config.DECURIX_DIR / "skills"))

SCHEMA = "decurix.skill/v1"

NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,47}$")

READY = "ready"
DRAFT = "draft"

# A loop (learning.py) is a block of steps a person said to repeat once per
# item of a list given at run time. Items are separated by ";" or a new line.
ITEM_SPLIT = re.compile(r"\s*(?:;|\n)\s*")
MAX_LOOP_ITEMS = 200
# The item of a list for a document that shows that field's place empty: the step that types it is left out for that
# item (expand), the rest replays.
EMPTY_ITEM = constraints.EMPTY_ITEM


def slug(text):
    """A title as a skill name: "Pay an invoice!" -> "pay-an-invoice". "skill" when nothing is left."""

    name = re.sub(r"[^a-z0-9]+", "-", (text or "").lower()).strip("-")[:48].strip("-")
    return name or "skill"


def valid(name):
    return bool(NAME_RE.match(name or ""))


def _path(name):
    if not valid(name):
        raise ValueError(f"not a skill name: {name!r}")
    return ROOT / f"{name}.json"


def save(skill):
    """Write a skill, whole and atomically."""

    path = _path(skill["name"])
    path.parent.mkdir(parents=True, exist_ok=True)

    recording.write_whole(path, skill, json_kw={"ensure_ascii": False, "indent": 1})

    return skill


def load(name):
    """A skill by name. Raises FileNotFoundError."""

    try:
        return json.loads(_path(name).read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise FileNotFoundError(f"no skill {name}") from exc


def delete(name):
    _path(name).unlink(missing_ok=True)


def all_skills():
    """Every skill on this desktop, ready ones first, then by name."""

    found = []

    try:
        paths = sorted(ROOT.glob("*.json"))
    except OSError:
        return []

    for path in paths:
        try:
            skill = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if skill.get("name") == path.stem:
            found.append(skill)

    found.sort(key=lambda s: (s.get("status") != READY, s["name"]))
    return found


def unique_name(title, taken=None):
    """A free name for a new skill titled `title`: pay-an-invoice, pay-an-invoice-2, ..."""

    base = slug(title)
    taken = set(taken if taken is not None else (s["name"] for s in all_skills()))

    name, n = base, 2
    while name in taken:
        suffix = f"-{n}"
        name = base[:48 - len(suffix)].rstrip("-") + suffix
        n += 1

    return name


def draft_for(sources):
    """The unfinished skill being taught from exactly these recordings, or None."""

    wanted = sorted(sources)

    for skill in all_skills():
        if skill.get("status") == DRAFT and sorted(skill.get("sources") or []) == wanted:
            return skill

    return None


def find(query):
    """
    The ready skill a person means by `query`: (skill, candidates). Only an exact name runs: the skill's name, the
    slug of the query, or its title word for word (identity.same). Otherwise `skill` is None and `candidates` are the
    skills sharing the most words with the query, best first - suggestions, never run. Found in review (2026-11-03,
    D-16): the one skill sharing the most words ran, so a query to remove a customer ran the skill that adds one.
    """

    from decurix_agent.computer import identity

    ready = [s for s in all_skills() if s.get("status") == READY]
    text = (query or "").strip().lower()

    for skill in ready:
        if skill["name"] == text or skill["name"] == slug(text) or identity.same(skill.get("title"), query):
            return skill, [skill]

    ranked = search(query, limit=len(ready))
    if not ranked:
        return None, []

    top = _score(ranked[0], query)
    return None, [skill for skill in ranked if _score(skill, query) == top]


def _score(skill, query):
    wanted = replay.query_words(query)
    words = set(replay.WORD.findall(f"{skill['name'].replace('-', ' ')} {skill.get('title') or ''} {skill.get('goal') or ''}".lower()))

    return len(wanted & words)


def explanation(skill):
    """
    What the person who taught a skill said about it, in their own words, or
    "". Kept whole: `done_when` is only the part that named no step, and a
    person's "I will send you a list and you do them one by one" is how they
    want the task done, not what finished looks like. A skill taught before
    `explanation` was kept has only `done_when`.
    """

    return " ".join((skill.get("explanation") or skill.get("done_when") or "").split())


BROWSER_SUFFIX = re.compile(r"\s+-\s+(?:Chromium|Google Chrome)$")


def places(skill, limit=3):
    """
    Where a skill does its work: the sites and files it opens, and the windows
    its steps are in ("sales.html", "Sales"). Found live: a skill taught in a
    local app was run for a request to enter a record in a different app - nothing
    it was shown said which system the skill works in.
    """

    found = []

    def add(place):
        place = (place or "").strip()
        if place and place.lower() not in (p.lower() for p in found):
            found.append(place)

    # The site its steps were on comes first, as an address to open: a recording begun with the site already open
    # (signed in first, as people do) has no step that opens it. Found live: after a restart closed the browser, the
    # engine was told the skill "works in New Request - new-request-123", guessed
    # portal.example.com and portal.local, and asked the person where the web app was.
    for step in skill.get("steps") or []:
        match = re.match(r"(https?://[^/?#]+)", step.get("url") or "")
        if match:
            add(match.group(1))
            break

    for step in skill.get("steps") or []:
        target = step.get("open")
        if target:
            match = re.match(r"https?://([^/?#]+)", target)
            add(match.group(1) if match else os.path.basename(target.split("?")[0]) or target)
        add(BROWSER_SUFFIX.sub("", step.get("window") or ""))
        if len(found) >= limit:
            break
    return found[:limit]


def search(query="", limit=8):
    """Ready skills for `query`, best first; with no words in it, every ready skill."""

    ready = [s for s in all_skills() if s.get("status") == READY]

    if not (query or "").strip():
        return ready[:limit]

    ranked = sorted(((_score(skill, query), skill) for skill in ready), key=lambda item: item[0], reverse=True)
    return [skill for score, skill in ranked if score][:limit]


def _program(skill, steps, params):
    return {
        "schema": "decurix.replay-program/v1",
        "session": f"skill-{skill['name']}",
        "title": skill.get("title") or skill["name"],
        "kind": "skill",
        "params": [
            {"name": p["name"], "label": p["label"], "default": p.get("default"), "secret": False,
             "reason": None, "step": p["step"], "constraint": p.get("constraint"), "example": p.get("example")}
            for p in params
        ],
        "steps": steps,
    }


def program(skill):
    """
    A skill as a replay program (computer/replay.py), which is what actually
    runs it. Its steps are already in that shape; this adds the parameters
    the steps name. A skill with a loop needs expand() instead.
    """

    return _program(skill, skill.get("steps") or [], skill.get("params") or [])


def initial_values(skill, given=None):
    """
    A run's starting values: what was given, and the recorded text of every
    parameter learned to be the same each time. What must be given is left out.
    """

    values = {}

    for param in skill.get("params") or []:
        if param["name"] in (given or {}):
            values[param["name"]] = str(given[param["name"]])
        elif not param["varies"]:
            values[param["name"]] = param.get("default") or ""

    # The values of rows this run has beyond the demonstration's (fit).
    group = repeated_rows(skill)
    if group and given:
        for n in range(len(group["rows"]), MAX_LOOP_ITEMS):
            row = [name for name in row_names(group, n) if given.get(name) not in (None, "")]
            if not row:
                break
            for name in row:
                values[name] = str(given[name])

    return values


def needed_from(skill, start):
    """
    What a run starting at step `start` must be given. Steps before it are not
    run, so what only they typed is not needed to carry on - except with a
    loop, whose steps are numbered again for each item, where everything is.
    """

    if list_params(skill):
        return required(skill)

    return [p for p in required(skill) if p["step"] >= start]


# Roles of an entry picked from a list an application shows.
LIST_ENTRY_ROLES = ("option", "listitem", "menuitem", "treeitem")


def chosen_from_list(skill):
    """
    The names of the values the demonstration typed and then picked from a
    list: a typing step straight followed by a click on an entry of a list.
    The typed words found the entry; the entry is the value - so such a value
    is searched for, never left in the field as typed, and what is entered
    goes by the entry's own name.
    """

    chosen, steps = set(), skill.get("steps") or []
    for step, following in zip(steps, steps[1:]):
        if _picks_from_list(step, following):
            chosen.add(step["param"].split("__")[0])
    return chosen


def _picks_from_list(step, following):
    return (step.get("kind") == "type" and bool(step.get("param")) and following.get("kind") == "click"
            and ((following.get("target") or {}).get("role") or "") in LIST_ENTRY_ROLES)


def left_alone(steps, names):
    """
    Copies of `steps` without what enters the values `names` - fields a run leaves as the application fills them in:
    each step that types or chooses one, with the click on a list entry that its typing found (a pick), and every check
    of another step that looks for one of them. Nothing else changes.
    """

    def about(check):
        return check.get("param") in names or any(
            "{" + name + "}" in str(v) for v in check.values() if isinstance(v, str) for name in names)

    out, skip = [], False
    for i, step in enumerate(steps):
        if skip:
            skip = False
            continue
        if step.get("param") in names and step.get("kind") in ("type", "select"):
            skip = _picks_from_list(step, steps[i + 1] if i + 1 < len(steps) else {})
            continue
        each = copy.deepcopy(step)
        if each.get("expect"):
            each["expect"] = [check for check in each["expect"] if not about(check)]
        out.append(each)
    return out


def repeated_rows(skill):
    """
    The part of a task that was entered once per entry of something - one row
    per line of a document - as the demonstration shows it: the same run of
    fields, by their labels, filled two or more times one after another.
    {"fields": [labels], "rows": [[names of row 1], [names of row 2], ...]},
    or None. A demonstration shows however many entries its own document had;
    the next document has its own number, and each needs every field of a row
    (found live: taught on two lines, a document of three had no place for
    the third, and a correct third row was called "extra" and removed).
    """

    params = sorted(required(skill), key=lambda p: p.get("step") or 0)
    labels = [(p.get("label") or p["name"]).strip().lower() for p in params]
    best = None
    for start in range(len(labels)):
        for size in range(1, (len(labels) - start) // 2 + 1):
            block = labels[start:start + size]
            if len(set(block)) != size:
                continue
            reps = 1
            while labels[start + reps * size:start + (reps + 1) * size] == block:
                reps += 1
            if reps >= 2 and (best is None or reps * size > best[1] * best[2]):
                best = (start, size, reps)
    if best is None:
        return None
    start, size, reps = best
    names = [p["name"] for p in params]
    return {"fields": [params[start + k].get("label") or params[start + k]["name"] for k in range(size)],
            "rows": [names[start + r * size:start + (r + 1) * size] for r in range(reps)]}


def row_names(group, n):
    """The names of row `n` (from 0) of a repeated part: the demonstration's own, or made after its first row's."""

    if n < len(group["rows"]):
        return list(group["rows"][n])
    return [f"{name}_{n + 1}" for name in group["rows"][0]]


def split_items(text):
    """The items of a list given for a loop: separated by ";" or a new line, none empty."""

    return [item for item in ITEM_SPLIT.split((text or "").strip()) if item]


def list_params(skill):
    """The parameters that take a list: the typed fields of a loop that is on."""

    return [p for p in skill.get("params") or [] if p.get("list")]


# A value that reads as an identifier a record is found by: letters and digits, at least one of each, 10 to 20 long (a
# tax or registration number, an account code). The same shape as core._IDENTIFIER without its emails.
IDENTIFIER_SHAPE = re.compile(r"(?=[A-Za-z0-9]*\d)(?=[A-Za-z0-9]*[A-Za-z])[A-Za-z0-9]{10,20}")


def _plain(text):
    return " ".join(str(text if text is not None else "").split()).casefold()


def made_before(skill, start):
    """
    The records the person made in the steps before the per-item block (which begins at step `start`) that the block
    later picks by name: [{"pick": the list parameter whose step chooses it, "name": the parameter the making typed
    its name with, "identifier": the parameter it typed an identifier into (None when it typed none), "steps": the
    making's step numbers, from the first after the commit before it through its own commit}]. A making is the run of
    steps up to and including a press that commits (replay._step_commits) that typed, as recorded, one of the items the
    block's choosing step chose when it was taught. Nothing is guessed: no commit, no name typed as one of those
    items - no making. Found live (2026-11-10): a person created a new customer's ledger before entering the month's
    bills; the next month's new customer was not in the list and the replay stopped, with nothing telling the model
    that the lesson itself makes such a record.
    """

    if not start:
        return []
    steps = sorted(skill.get("steps") or [], key=lambda s: s.get("n") or 0)
    params = {p["name"]: p for p in skill.get("params") or []}
    before = [s for s in steps if (s.get("n") or 0) < start]
    # (A link that reads like an act - "Create ledger" - opens the form: it begins a making, never ends one.)
    commits = [s["n"] for s in before if (s.get("consequential") or replay._step_commits(s))
               and (s.get("target") or {}).get("role") != "link"]
    out = []
    for pick in list_params(skill):
        chooser = next((s for s in steps if s.get("param") == pick["name"]), None)
        if chooser is None or (chooser.get("n") or 0) < start or chooser.get("kind") != "select":
            continue
        chosen = {_plain(x) for x in pick.get("examples") or [] if _plain(x)}
        for step in before:
            param = params.get(step.get("param") or "")
            if step.get("kind") != "type" or param is None or param.get("list"):
                continue
            typed = _plain(param.get("example") if param.get("example") is not None else param.get("default"))
            if not typed or typed not in chosen:
                continue
            end = next((n for n in commits if n > step["n"]), None)
            if end is None:
                continue
            begin = max([n for n in commits if n < step["n"]], default=0) + 1
            span = [s for s in before if begin <= s["n"] <= end]
            ident = next((params[s["param"]]["name"] for s in span
                          if s.get("kind") == "type" and s.get("param") in params
                          and IDENTIFIER_SHAPE.fullmatch(str(params[s["param"]].get("example")
                                                             or params[s["param"]].get("default") or "").strip())),
                         None)
            out.append({"pick": pick["name"], "name": param["name"], "identifier": ident,
                        "steps": [s["n"] for s in span]})
            break
    return out


def identifier_in(example, text, others=()):
    """
    The one token of `text` (a document's) with the shape of the identifier `example` (the same length, letters and
    digits in nearly the same places) that the texts of the run's other documents, `others`, do not ALL hold too (a
    token every one of them holds is their sender's): what this document gives as the identifier a record of its own
    is found by, where the documents of one run share their sender's. None when there is not exactly one - and with no
    other document, since which of a document's identifiers is its sender's own cannot then be told (a bill to an
    unregistered customer holds only the sender's).
    """

    example = str(example or "").strip()
    if not IDENTIFIER_SHAPE.fullmatch(example) or not others:
        return None
    # (Of the same length, letters and digits mixed, and the same kind of character in all but a fifth of its places:
    # a check character or an entity's number is a letter in one identifier of a kind and a digit in another.)
    loose = len(example) // 5

    def alike(word):
        return (len(word) == len(example) and IDENTIFIER_SHAPE.fullmatch(word) is not None
                and sum(a.isdigit() != b.isdigit() for a, b in zip(word, example)) <= loose)

    def tokens(t):
        return {w.upper() for w in re.findall(r"[A-Za-z0-9]+", str(t or "")) if alike(w)}

    found = tokens(text)
    others = [tokens(t) for t in others]
    if others:
        found = {w for w in found if not all(w in o for o in others)}
    return next(iter(found)) if len(found) == 1 else None


def list_problem(skill, values):
    """What is wrong with the lists a run was given, in words for a person, or None."""

    lists = list_params(skill)
    counts = {p["name"]: len(split_items(values.get(p["name"]))) for p in lists}

    if not counts:
        return None

    empty = [name for name, count in counts.items() if not count]
    if empty:
        return f"{', '.join(empty)} has no items - give one value per item, separated by `;`."

    if len(set(counts.values())) > 1:
        return ("Those lists must be the same length, one value per item: "
                + ", ".join(f"{name} has {count}" for name, count in counts.items()) + ".")

    if max(counts.values()) > MAX_LOOP_ITEMS:
        return f"{max(counts.values())} items is more than the {MAX_LOOP_ITEMS} a run will do."

    return None


def expand(skill, values):
    """
    (program, values) for a run: the skill's steps with each loop that is on
    written out once per item, its typed fields named "<param>__<n>" and given
    that item's text. A skill with no loop comes back as it is. Steps keep
    their `id`, so what a person corrects at step 9 of a run is the skill's
    own step, in every item.
    """

    flat = dict(values)
    lists = {p["name"]: p for p in list_params(skill)}

    # A value given as EMPTY_ITEM that is no list: a field the person left as the application fills it in (no document
    # gives it - skill_task's "left to the application"). No step types or chooses into it, and no check looks for it.
    blank = {p["name"] for p in skill.get("params") or [] if not p.get("list") and flat.get(p["name"]) == EMPTY_ITEM}
    if blank:
        steps = left_alone(skill.get("steps") or [], blank)
        for n, step in enumerate(steps, 1):
            step["n"] = n
        at = {step["param"]: step["n"] for step in steps if step.get("param")}
        skill = dict(skill, steps=steps, params=[dict(p, step=at.get(p["name"], p.get("step")))
                                                 for p in skill.get("params") or [] if p["name"] not in blank])
        for name in blank:
            flat.pop(name, None)

    if not lists:
        return fit(skill, program(skill), flat), flat

    items = {name: split_items(flat.pop(name, "")) for name in lists}

    block_of = {}
    for loop in skill.get("loops") or []:
        if loop.get("enabled"):
            for step_id in loop["blocks"][0]:
                block_of[step_id] = loop["id"]

    steps = []
    params = [dict(p) for p in skill["params"] if not p.get("list")]
    written = set()

    for step in skill["steps"]:
        loop_id = block_of.get(step["id"])

        if loop_id is None:
            steps.append(copy.deepcopy(step))
            continue

        if loop_id in written:
            continue
        written.add(loop_id)

        block = [s for s in skill["steps"] if block_of.get(s["id"]) == loop_id]
        # A field typed the same on every item (learning._apply_loop keeps it
        # fixed) is in the block but is not a list: only lists are split.
        count = max(len(items[s["param"]]) for s in block if s.get("param") in lists)
        block_params = {s["param"] for s in block if s.get("param") in lists}

        for n in range(count):
            for original in block:
                each = copy.deepcopy(original)

                if each.get("param") in lists:
                    name = each["param"]
                    renamed = f"{name}__{n + 1}"

                    for check in each.get("expect") or []:
                        if check.get("param") == name:
                            check["param"] = renamed

                    if items[name][n] == EMPTY_ITEM and each.get("kind") in ("type", "select"):
                        # This document gives the field nothing: nothing is typed or chosen into it for this item.
                        continue
                    each["param"] = renamed
                    flat[renamed] = items[name][n]
                    params.append(dict(lists[name], name=renamed, list=False, default=None))

                # A result check that follows a list parameter follows this
                # item's copy of it.
                for name in block_params:
                    for check in each.get("expect") or []:
                        key = {"element": "name", "window": "value", "active_window": "value"}.get(check["type"])
                        if key:
                            check[key] = check[key].replace("{" + name + "}", "{" + f"{name}__{n + 1}" + "}")

                each["summary"] = f"{replay._summary(each)} (item {n + 1} of {count})"
                steps.append(each)

    return fit(skill, _program(skill, steps, _numbered(steps, params)), flat), flat


def summary(skill, detail=False):
    """A skill as the control plane shows it. With `detail`, its steps too."""

    steps = skill.get("steps") or []

    shown = {
        "name": skill["name"],
        "title": skill.get("title") or skill["name"],
        "status": skill.get("status"),
        "done_when": skill.get("done_when"),
        "explanation": explanation(skill) or None,
        "demos": skill.get("demos", 1),
        "runs": skill.get("runs", 0),
        "version": skill.get("version", 1),
        "sources": skill.get("sources") or [],
        "learned": skill.get("learned") or [],
        "created_at": skill.get("created_at"),
        "learned_at": skill.get("learned_at"),
        "steps": len(steps),
        "gated": sum(1 for s in steps if s.get("consequential")),
        "health": experience.health(skill),
        "approval": skill.get("approval"),
        "by_hand": sum(1 for s in steps if s["kind"] == "manual"),
        "params": [{"name": p["name"], "label": p["label"], "list": bool(p.get("list")), "example": p.get("example")}
                   for p in required(skill)],
    }

    if detail:
        shown["outline"] = [
            {"n": s["n"], "summary": s["summary"], "kind": s["kind"], "consequential": bool(s.get("consequential")),
             "optional": bool(s.get("optional")), "repeat": bool(s.get("repeat"))}
            for s in steps
        ]
        shown["notes"] = [n["text"] for n in skill.get("notes") or []]

    return shown


def note_run(name):
    """Count one more run of a skill that ran outside skill_task (an engine's `gui do`)."""

    try:
        skill = load(name)
    except FileNotFoundError:
        return

    skill["runs"] = skill.get("runs", 0) + 1
    save(skill)


def required(skill):
    """The parameters a run must be given: the ones learned to change every time."""

    return [p for p in skill.get("params") or [] if p.get("varies")]


def renumber(skill):
    """Make each step's number, and each parameter's step, follow the steps' current order."""

    skill["params"] = _numbered(skill["steps"], skill.get("params") or [])
    return skill


def _numbered(steps, params):
    """Number `steps` in order: the parameters their steps still type, each with its step's new number."""

    for n, step in enumerate(steps, 1):
        step["n"] = n

    by_name = {step["param"]: step["n"] for step in steps if step.get("param")}
    return [dict(p, step=by_name[p["name"]]) for p in params if p["name"] in by_name]


# ============================================================
# Fitting a run: this run's rows, and this run's choices
#
# A demonstration is one instance. Two things in it belong to the document
# it was made from and not to the task: how many entries it had (one row of
# fields per line of a bill), and which entry of each of the application's
# lists was the right one (the supplier, the item). Replayed as recorded, a
# run for another document typed its own words and then clicked the
# demonstration's entry, and entered the demonstration's number of rows - so
# such a run was refused a replay altogether, and a task of this shape was
# the model's to do by hand every time, however often it had gone well:
# measured, 250,000 to 550,000 tokens a bill.
#
# fit() makes the program this run's own instead, with no model:
#
#   rows     the steps the demonstration did for its last row are written out
#            once more for each further entry of this run (their values named
#            as binding names them: item_3, rate_inr_3 ...), or left out for
#            the rows this run does not have. A row counts only when every
#            one of its values was given: a row half given is not guessed at,
#            and the program is left as the demonstration's (which then says
#            what it is missing).
#   choices  "type these words, click that entry" becomes one `select` of this
#            run's value: core.act searches the list for it and takes the one
#            existing entry that is it, or stops - the same rule, and the
#            same refusals, as when an engine chooses (core._select_custom).
#            Only where this run's value is not the demonstration's own; a
#            run for the same entry clicks it as the person did.
#
# What a fitted step leaves on the screen is not something the demonstration
# showed, so it is not checked step by step: the run ends unverified and the
# final check judges the result, as it does every result.
# ============================================================

def _given(values, name):
    return values.get(name) not in (None, "")


def run_rows(skill, values):
    """
    How many rows this run has, and their names: (count, [[names]]) from the
    values given - or (None, None) when the skill has no repeated part, no row
    is given, or a row is given only in part.
    """

    group = repeated_rows(skill)
    if group is None:
        return None, None

    names, n = [], 0
    while True:
        row = row_names(group, n)
        got = [_given(values, name) for name in row]
        if not any(got):
            break
        if not all(got):
            return None, None
        names.append(row)
        n += 1
        if n > MAX_LOOP_ITEMS:
            return None, None

    # Nothing may be given for a row past the first one that is missing.
    later = {name for r in range(n, max(len(group["rows"]), n) + 12) for name in row_names(group, r)}
    if not names or any(_given(values, name) for name in later):
        return None, None
    return len(names), names


def _per_row(step, twin):
    """Whether a row's step acts on a control of its own row: its twin in the row before is the same kind of control
    (role and name) at another path. Unknown (no twin, or no path) is not."""

    a, b = (step or {}).get("target") or {}, (twin or {}).get("target") or {}
    if not a or not b or not a.get("path") or not b.get("path"):
        return False
    return a.get("role") == b.get("role") and a.get("name") == b.get("name") and a["path"] != b["path"]


def fit(skill, program, values):
    """This run's program: `program` with this run's number of rows and this run's choices from lists (see above)."""

    group = repeated_rows(skill)
    steps = [copy.deepcopy(step) for step in program["steps"]]
    params = {p["name"]: dict(p) for p in program["params"]}
    base_of = {}          # a value of a row written out here -> the demonstration's value it was copied from
    changed = False

    count, names = run_rows(skill, values)
    demo = len(group["rows"]) if group else 0

    if count and count != demo:
        of_row = {name: r for r, row in enumerate(group["rows"]) for name in row}
        last = {}
        for i, step in enumerate(steps):
            if step.get("param") in of_row:
                last[of_row[step["param"]]] = i

        if len(last) == demo:
            if count < demo:
                gone = {name for row in group["rows"][count:] for name in row}
                steps = steps[:last[count - 1] + 1] + steps[last[demo - 1] + 1:]
                for name in gone:
                    params.pop(name, None)
            else:
                template = steps[last[demo - 2] + 1:last[demo - 1] + 1]
                # Each template step's twin in the row before it (aligned from the row's end): a control that is
                # another one in each row (another path) is that row's, and is found by its place among its name.
                floor = last[demo - 3] if demo >= 3 else -1
                twin = {}
                for o in range(len(template)):
                    i = last[demo - 2] - (len(template) - 1 - o)
                    twin[o] = steps[i] if i > floor else None
                extra = []
                for r in range(demo, count):
                    rename = dict(zip(group["rows"][demo - 1], names[r]))
                    for o, original in enumerate(template):
                        each = copy.deepcopy(original)
                        old = each.get("param")
                        if old in rename:
                            each["param"] = rename[old]
                            base_of[rename[old]] = old
                            params[rename[old]] = dict(params.get(old) or {"label": old, "example": None},
                                                       name=rename[old], default=None)
                        # Which row a control is in was the demonstration's row. A control that is another one in
                        # each demonstrated row (_per_row) is row r + 1's: the (r + 1)th of its name, never the first
                        # one found by name alone, an earlier row's (review 2026-11-03, D-30). One the rows share (an
                        # add-row press, an editor drawn only in the active row) is found by what it is.
                        if each.get("target"):
                            per_row = _per_row(original, twin.get(o))
                            each["target"] = {k: v for k, v in each["target"].items() if k not in ("path", "row", "nth")}
                            if per_row:
                                each["target"]["nth"] = r + 1
                        each["expect"] = []
                        each["summary"] = f"{replay._summary(each)} (row {r + 1})"
                        extra.append(each)
                steps = steps[:last[demo - 1] + 1] + extra + steps[last[demo - 1] + 1:]
            changed = True

    chosen = chosen_from_list(skill)
    out, i = [], 0
    while i < len(steps):
        step = steps[i]
        following = steps[i + 1] if i + 1 < len(steps) else {}
        name = step.get("param")
        if _picks_from_list(step, following) and base_of.get(name, name).split("__")[0] in chosen \
                and _given(values, name) and values[name] != (params.get(name) or {}).get("example"):
            row = re.search(r" \(row \d+\)$", step.get("summary") or "")
            step["kind"], step["expect"], step["chosen"] = "select", [], True
            step["summary"] = f"choose {{{name}}} in {el_name(step)}" + (row.group(0) if row else "")
            out.append(step)
            i += 2
            changed = True
            continue
        out.append(step)
        i += 1

    if not changed:
        return program

    # From the first step that is this run's own, what a step left on the screen when it was recorded was
    # the demonstration's (the name of the item it had chosen, the record it had made). Only a result that
    # follows one of this run's values - a field holding what was typed into it - is still looked for.
    first = next((i for i, step in enumerate(out) if step.get("chosen") or step.get("param") in base_of), len(out))
    if count and count < demo:
        first = min(first, last[count - 1] + 1) if len(last) == demo else first
    for step in out[first:]:
        # ...and not one that follows a value through the demonstration's own wording ("{item} Instances").
        step["expect"] = [check for check in step.get("expect") or []
                          if check.get("param") or _only_placeholders(str(check.get("name") or check.get("value") or ""))]

    fitted = dict(program)
    fitted["steps"] = out
    fitted["params"] = _numbered(out, params.values())
    fitted["fitted"] = True
    return fitted


def _only_placeholders(text):
    return "{" in text and not re.sub(r"\{[^}]*\}", "", text).strip()


def el_name(step):
    target = step.get("target") or {}
    return f"{target.get('role') or 'the field'} \"{target.get('name') or ''}\"".strip()
