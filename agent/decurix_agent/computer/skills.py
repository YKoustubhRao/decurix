import copy
import json
import os
import re
from pathlib import Path

from decurix_agent import config
from decurix_agent.computer import experience, replay


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

    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(json.dumps(skill, ensure_ascii=False, indent=1), encoding="utf-8")
    tmp.replace(path)

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
    The ready skill a person means by `query`: (skill, candidates). An exact
    name wins; otherwise the one skill sharing the most words with it. When
    two share the same most, `skill` is None and `candidates` lists them -
    which skill to run is not something to guess.
    """

    ready = [s for s in all_skills() if s.get("status") == READY]
    text = (query or "").strip().lower()

    for skill in ready:
        if skill["name"] == text or skill["name"] == slug(text):
            return skill, [skill]

    ranked = search(query, limit=len(ready))
    if not ranked:
        return None, []

    top = _score(ranked[0], query)
    best = [skill for skill in ranked if _score(skill, query) == top]

    return (best[0] if len(best) == 1 else None), best


def _score(skill, query):
    wanted = {w for w in replay.WORD.findall((query or "").lower()) if w not in replay.STOP_WORDS}
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
        if step.get("kind") == "type" and step.get("param") and following.get("kind") == "click" \
                and ((following.get("target") or {}).get("role") or "") in LIST_ENTRY_ROLES:
            chosen.add(step["param"].split("__")[0])
    return chosen


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

    if not lists:
        return program(skill), flat

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
        count = max(len(items[s["param"]]) for s in block if s.get("param"))
        block_params = {s["param"] for s in block if s.get("param")}

        for n in range(count):
            for original in block:
                each = copy.deepcopy(original)

                if each.get("param"):
                    name = each["param"]
                    renamed = f"{name}__{n + 1}"

                    for check in each.get("expect") or []:
                        if check.get("param") == name:
                            check["param"] = renamed

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

    for number, step in enumerate(steps, 1):
        step["n"] = number

    by_name = {step["param"]: step["n"] for step in steps if step.get("param")}
    params = [dict(p, step=by_name[p["name"]]) for p in params if p["name"] in by_name]

    return _program(skill, steps, params), flat


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

    for n, step in enumerate(skill["steps"], 1):
        step["n"] = n

    by_name = {step["param"]: step["n"] for step in skill["steps"] if step.get("param")}
    skill["params"] = [dict(p, step=by_name[p["name"]]) for p in skill.get("params") or [] if p["name"] in by_name]

    return skill
