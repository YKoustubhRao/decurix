import time

from decurix_agent.computer import skills


# ============================================================
# Skill catalog
#
# Skills offered for a desktop to install by name (GET /catalog, POST
# /catalog/install on this desktop's API). It ships empty: a skill nobody here
# demonstrated is not a skill this worker knows. It used to carry two dozen
# invented department skills (made-up steps, controls and file paths) that
# installed as ready, so the engines were told a person had shown and
# confirmed them when no person had.
#
# An entry installs as a draft, never ready: only a person confirming it in
# the learning loop (learning.py, mimic.py) makes a skill ready to run.
# ============================================================

CATALOG_SKILLS = []


def get_catalog():
    """Return all skills in the catalog."""

    return list(CATALOG_SKILLS)


def get_catalog_skill(name):
    """Find a catalog skill by its exact name or slug."""

    target = (name or "").strip().lower()
    return next((s for s in CATALOG_SKILLS if s["name"] == target), None)


def install_catalog_skill(name, target_name=None):
    """
    Install a skill from the catalog as a draft Decurix skill in
    ~/.decurix/skills/, for a person to confirm before it runs.
    Returns the saved skill dict.
    """

    entry = get_catalog_skill(name)
    if not entry:
        raise ValueError(f"unknown catalog skill: {name!r}")

    final_name = target_name or entry["name"]

    skill = {
        "schema": skills.SCHEMA,
        "name": final_name,
        "title": entry["title"],
        "goal": entry["goal"],
        "done_when": entry["done_when"],
        "department": entry.get("department", "general"),
        "status": skills.DRAFT,
        "created": int(time.time()),
        "sources": [],
        "params": entry.get("params", []),
        "steps": entry.get("steps", []),
    }

    if entry.get("optimization"):
        skill["optimization"] = entry["optimization"]

    return skills.save(skill)
