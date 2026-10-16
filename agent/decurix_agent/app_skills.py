"""
Guides to well-known apps, in the Agent Skills format: app_skills/<name>/SKILL.md, each a short, checked account of
how to do the common things in one app the way that works on a Decurix desktop (keys that reach the app, the dialogs
that come up, what to check before saving). The code knows no app: each guide's front matter says which words name
its app ("applies-to"), and a task that names one - in its request, or by the app it opens - gets that guide.

The guides are knowledge, never values: a guide says how a field or a dialog works, the task's own request and
documents say what goes in it.
"""

import os
import re
from pathlib import Path

from decurix_agent.logging_utils import log

ROOT = Path(os.environ.get("DECURIX_APP_SKILLS_DIR", str(Path(__file__).with_name("app_skills"))))
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
            "body": body.strip()}


def guides():
    """Every guide: [{name, description, terms, body}] in name order."""

    found = []
    try:
        folders = sorted(p for p in ROOT.iterdir() if p.is_dir())
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
        block = f"## Guide: {g['name']} ({g['description']})\n\n{g['body']}"
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
