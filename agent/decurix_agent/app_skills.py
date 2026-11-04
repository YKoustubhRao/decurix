"""
Guides to well-known apps, in the Agent Skills format: app_skills/<name>/SKILL.md, each a short, checked account of
how to do the common things in one app the way that works on a Decurix desktop (keys that reach the app, the dialogs
that come up, what to check before saving). The code knows no app: each guide's front matter says which words name
its app ("applies-to"), and a task that names one - in its request, or by the app it opens - gets that guide.

The guides are knowledge, never values: a guide says how a field or a dialog works, the task's own request and
documents say what goes in it.

A desktop also LEARNS guides (learn/promote): an app nobody wrote a guide for is worked out by the engine - the
screen, `lookup` on the web - and what worked is written down with `gui guide --learn` so the next task does not
work it out again. A learned guide is held as pending under the task that wrote it and becomes a guide only when that
task is checked done (promote, from the same place a playbook is kept): a guide from work that failed its check is
never kept. It carries no values: no secrets, no e-mail addresses, no long numbers.
"""

import datetime
import os
import re
import shutil
from pathlib import Path

from decurix_agent import config
from decurix_agent.logging_utils import log

ROOT = Path(os.environ.get("DECURIX_APP_SKILLS_DIR", str(Path(__file__).with_name("app_skills"))))
# Guides this desktop learned (on its persistent volume), and the ones written by tasks not yet checked done.
LEARNED = Path(os.environ.get("DECURIX_LEARNED_APP_SKILLS_DIR", str(config.DECURIX_DIR / "app_skills")))
PENDING = ".pending"
# A learned guide's size: long enough for an app's menus, dialogs and checks; short, as it is read on every turn.
MIN_GUIDE, MAX_GUIDE = 120, 6000
# A pending guide whose task never ended done is dropped after this long.
PENDING_DAYS = 7
_SLUG = re.compile(r"^[a-z0-9][a-z0-9-]{1,48}$")
# What a guide must not carry: a person's data (the same line `lookup` draws for a question sent to the web).
_EMAIL = re.compile(r"[\w.+-]+@[\w-]+\.[\w.-]+")
_LONG_DIGITS = re.compile(r"\d{8,}")
# How much of all matching guides goes to a model at once: a guide is read on every turn of the operator.
MAX_CHARS = 9000

_FRONT = re.compile(r"\A---\s*\n(.*?)\n---\s*\n", re.S)


def _parse(text):
    meta, body = {}, text
    m = _FRONT.match(text)
    if m:
        body = text[m.end():]
        for line in m.group(1).splitlines():
            key, _, value = line.partition(":")
            if key.strip():
                meta[key.strip()] = value.strip().strip('"')
    terms = [t.strip().strip('"').lower() for t in re.split(r"[,;]", meta.get("applies-to", "")) if t.strip()]
    return {"name": meta.get("name", ""), "description": meta.get("description", ""), "terms": terms,
            "body": body.strip(), "learned": meta.get("learned", "")}


def _read(root):
    found = []
    try:
        folders = sorted(p for p in root.iterdir() if p.is_dir() and not p.name.startswith("."))
    except OSError:
        return []
    for folder in folders:
        try:
            g = _parse((folder / "SKILL.md").read_text(encoding="utf-8"))
        except OSError:
            continue
        if g["name"] and g["terms"]:
            found.append(g)
    return found


def guides():
    """Every guide: [{name, description, terms, body, learned}] - the shipped ones, then the ones this desktop
    learned (`learned` is the date for those, "" for a shipped one), each in name order."""

    shipped = _read(ROOT)
    names = {g["name"] for g in shipped}
    return shipped + [g for g in _read(LEARNED) if g["name"] not in names]


def unsafe(text):
    """Why text may not be kept as a guide, or "" - a guide is how an app works, never a person's data."""

    from decurix_agent.computer import privacy

    if privacy.credential_in(text) or privacy.SPOKEN_SECRET.search(text):
        return "it holds something shaped like a password, key or card number"
    if _EMAIL.search(text):
        return "it holds an e-mail address"
    if _LONG_DIGITS.search(text):
        return "it holds a long number (an account, invoice or phone number)"
    return ""


def _part(text):
    return re.sub(r"[^\w.-]", "_", text or "") or "_"


def learn(name, applies_to, description, body, task_id="", stage=""):
    """
    Keep a guide the engine wrote to an app, pending until the work it came from is checked done (promote): the stage
    of a planned run it was written in (stage: its "run:step" scope), else the whole task. Returns the line to print.
    Raises ValueError for a guide that cannot be kept, saying why. A name a shipped guide has is kept as "<name>-notes":
    what the desktop found beyond the checked guide, shown beside it.
    """

    name = (name or "").strip().lower()
    if not _SLUG.match(name):
        raise ValueError("a guide's name is lowercase letters, digits and dashes (e.g. zoho-books)")
    terms = [t.strip().lower() for t in re.split(r"[,;]", applies_to or "") if t.strip()]
    if not terms:
        raise ValueError("--applies-to needs the words that name the app (e.g. \"zoho books, zoho\")")
    body = (body or "").strip()
    if not MIN_GUIDE <= len(body) <= MAX_GUIDE:
        raise ValueError(f"a guide is {MIN_GUIDE}-{MAX_GUIDE} characters: how the app is worked - its menus, keys, "
                         f"dialogs, what to check before saving (this one is {len(body)})")
    why = unsafe(" ".join([name, applies_to or "", description or "", body]))
    if why:
        raise ValueError(f"not kept: {why}. A guide says how the app works; this task's values stay out of it")
    if any(g["name"] == name for g in _read(ROOT)):
        name += "-notes"
    folder = LEARNED / PENDING / _part(task_id or "no-task") / _part(stage) / name
    folder.mkdir(parents=True, exist_ok=True)
    flat = lambda s: " ".join(str(s or "").split()).replace('"', "'")  # noqa: E731
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(
        folder / "SKILL.md",
        f"---\nname: {name}\ndescription: \"{flat(description)[:200]}\"\napplies-to: \"{', '.join(terms)}\"\n"
        f"learned: {datetime.date.today().isoformat()}\nfrom-task: {task_id}\n---\n\n{body}\n")
    return (f"Kept the guide {name!r} for now: it becomes a guide on this desktop when this "
            f"{'stage' if stage else 'task'} is checked done (gui guide {name} shows it to later tasks).")


def promote(task_id, stage=None):
    """Make the guides written by work now checked done this desktop's guides: one stage's (stage: its "run:step"
    scope) or, with no stage, every guide the task wrote. Drops pending guides of work that never ended done. Returns
    the names promoted. Never raises."""

    promoted = []
    try:
        pending = LEARNED / PENDING
        task = pending / _part(task_id or "no-task")
        sources = ([task / _part(stage)] if stage else sorted(p for p in task.iterdir() if p.is_dir())
                   ) if task_id and task.is_dir() else []
        for source in (s for s in sources if s.is_dir()):
            for folder in sorted(p for p in source.iterdir() if (p / "SKILL.md").is_file()):
                target = LEARNED / folder.name
                target.mkdir(parents=True, exist_ok=True)
                from decurix_agent.computer import privacy
                if (target / "SKILL.md").is_file():
                    privacy.write_scrubbed(target / "SKILL.prev.md", (target / "SKILL.md").read_text(encoding="utf-8"))
                privacy.write_scrubbed(target / "SKILL.md", (folder / "SKILL.md").read_text(encoding="utf-8"))
                promoted.append(folder.name)
            shutil.rmtree(source, ignore_errors=True)
        if task_id and task.is_dir() and not any(task.iterdir()):
            task.rmdir()
        cutoff = datetime.datetime.now().timestamp() - PENDING_DAYS * 86400
        for old in (p for p in pending.iterdir() if p.is_dir()) if pending.is_dir() else ():
            if old.stat().st_mtime < cutoff:
                shutil.rmtree(old, ignore_errors=True)
    except OSError as exc:
        log(f"app_skills: could not keep the learned guides: {exc}")
    if promoted:
        log(f"app_skills: learned {', '.join(promoted)}")
    return promoted


def _names(term, text):
    return re.search(r"(?<![\w-])" + re.escape(term) + r"(?![\w-])", text) is not None


def matching(text):
    """The guides whose app the text names, in name order."""

    lowered = (text or "").lower()
    return [g for g in guides() if any(_names(t, lowered) for t in g["terms"])]


def named(name):
    """One guide by its name or by a word naming its app, or None."""

    wanted = (name or "").strip().lower()
    for g in guides():
        if wanted == g["name"] or wanted in g["terms"]:
            return g
    hits = matching(wanted)
    return hits[0] if len(hits) == 1 else None


def render(found, limit=MAX_CHARS):
    """Guides as one text for a model, whole guides only, within limit."""

    parts, size = [], 0
    for g in found:
        block = (f"## Guide learned on this desktop: {g['name']} ({g['description']}; learned {g['learned']} from a "
                 f"task that was checked done - where the screen differs, the screen is right)\n\n{g['body']}"
                 if g.get("learned") else f"## Guide: {g['name']} ({g['description']})\n\n{g['body']}")
        if size + len(block) > limit:
            log(f"app_skills: left out {g['name']} (over {limit} characters)")
            continue
        parts.append(block)
        size += len(block)
    if not parts:
        return ""
    return ("How to work the apps this task names - checked guides to these apps on this desktop. They say how the "
            "app works, never what to type: the values come from the request and its documents.\n\n"
            + "\n\n".join(parts))


def context(text):
    """The guides for the apps a task's text names, as one text, or ""."""

    try:
        return render(matching(text))
    except Exception as exc:  # a guide must never cost a task
        log(f"app_skills: {exc}")
        return ""
