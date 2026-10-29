"""
A skill as a bundle the control plane can share between desktops.

A skill lives on the desktop that learned it, as one JSON file (computer/skills.py). The control plane's skill
registry (internal/skillhub, docs/skillhub.md) reads a skill off one desktop as a bundle (export) and writes it onto
another (install), so a task taught once can be done by every worker that should know it.

A skill never holds a typed secret: a password a person typed while teaching becomes a step they do by hand
("manual", learning.py), and its value was never recorded. Both directions check that again, so a skill that somehow
carries one is neither handed out nor taken in.
"""

import copy
import time

from decurix_agent.computer import privacy, skills
from decurix_agent.computer.errors import ComputerError

FORMAT = "decurix-skill/1"

# Fields one desktop's use of a skill writes: its run count and its own run history (experience.py). An installed
# skill starts without them, so a cheaper path it earned on another desktop's screen is earned again on this one.
LOCAL_FIELDS = ("runs", "experience")

# Where typed values sit in a skill: a step's text and samples, a parameter's default and example.
_STEP_VALUES = ("text", "samples", "value")
_PARAM_VALUES = ("default", "example")


def _values(value):
    """Every string inside value (a string, or lists of them)."""

    if isinstance(value, str):
        yield value
    elif isinstance(value, (list, tuple)):
        for item in value:
            yield from _values(item)


# What grows when a person teaches or corrects a skill on this desktop, or a run learns from the screen: kept at
# install, so a later install can tell a skill that was changed here from one that was only used.
_TAUGHT_LISTS = ("corrections", "run_answers", "learned", "notes")


def _marks(skill):
    """How far a skill's own teaching had got: its version and the length of each list teaching adds to."""

    marks = {"skill_version": skill.get("version") or 1}
    for key in _TAUGHT_LISTS:
        marks[key] = len(skill.get(key) or [])
    return marks


def changed_here(skill):
    """Whether a skill installed from the registry was taught or corrected on this desktop since."""

    came = skill.get("installed_from")
    if not isinstance(came, dict):
        return True
    now = _marks(skill)
    return any(now[key] > (came.get(key) or (1 if key == "skill_version" else 0)) for key in now)


def _credential(text):
    """The kind of credential text holds, or None: one privacy.py recognises, or one written after its name."""

    found = privacy.credential_in(text)
    if found:
        return found
    if privacy.withhold_spoken_secrets(text) != text:
        return "credential written after its name"
    return None


def secret_problem(skill):
    """Why a skill holds a typed secret, or None. A secret step must be one done by hand, carrying no value."""

    for step in skill.get("steps") or []:
        if not isinstance(step, dict):
            return "a step is not an object"
        where = f"step {step.get('n', '?')}"
        if step.get("secret") and step.get("kind") != "manual":
            return f"{where} types a secret instead of leaving it to a person"
        if step.get("secret") and any(step.get(key) for key in _STEP_VALUES):
            return f"{where} keeps a secret's value"
        for key in _STEP_VALUES:
            for text in _values(step.get(key)):
                found = _credential(text)
                if found:
                    return f"{where} holds what looks like a {found}"

    for param in skill.get("params") or []:
        if not isinstance(param, dict):
            return "a parameter is not an object"
        if param.get("secret") and any(param.get(key) not in (None, "") for key in _PARAM_VALUES):
            return f"parameter {param.get('name') or '?'} keeps a secret's value"
        for key in _PARAM_VALUES:
            for text in _values(param.get(key)):
                found = _credential(text)
                if found:
                    return f"parameter {param.get('name') or '?'} holds what looks like a {found}"

    return None


def export(name):
    """A skill as a bundle: {"format", "skill": the whole skill as saved, "exported_at"}. Raises FileNotFoundError."""

    skill = skills.load(name)
    problem = secret_problem(skill)
    if problem:
        raise ComputerError(f"skill {name} cannot be shared: {problem}")

    return {"format": FORMAT, "skill": skill, "exported_at": int(time.time())}


def install(bundle, version=None, sha=None, replace=False):
    """
    Save the skill a bundle carries on this desktop and return its summary.

    It keeps the bundle's status, notes where it came from ("installed_from": registry version and sha, and how far
    the skill's own teaching had got then, _marks), and drops what another desktop's use wrote (LOCAL_FIELDS). A skill of the same name
    already here is replaced only when it came from the registry and was not changed here since, or when `replace` is
    given: a skill taught or corrected on this desktop is never overwritten silently.

    Raises ValueError for a bundle that is not one, ComputerError when it holds a secret or would overwrite local work.
    """

    if not isinstance(bundle, dict) or bundle.get("format") != FORMAT:
        raise ValueError(f"not a {FORMAT} skill bundle")
    skill = bundle.get("skill")
    if not isinstance(skill, dict):
        raise ValueError("the bundle holds no skill")
    name = skill.get("name")
    if not isinstance(name, str) or not skills.valid(name):
        raise ValueError(f"not a skill name: {name!r}")
    if not isinstance(skill.get("steps"), list):
        raise ValueError("the skill has no steps")
    if version is not None and (not isinstance(version, int) or isinstance(version, bool) or version < 1):
        raise ValueError("version must be a positive number")
    if sha is not None and not isinstance(sha, str):
        raise ValueError("sha must be text")

    problem = secret_problem(skill)
    if problem:
        raise ComputerError(f"skill {name} was not installed: {problem}")

    if not replace:
        try:
            here = skills.load(name)
        except FileNotFoundError:
            here = None
        if here is not None and not isinstance(here.get("installed_from"), dict):
            raise ComputerError(f"this desktop has its own skill {name}, taught here; install with replace to "
                                "overwrite it")
        if here is not None and changed_here(here):
            raise ComputerError(f"skill {name} was taught or corrected on this desktop since it was installed; "
                                "install with replace to overwrite it")

    saved = copy.deepcopy(skill)
    for key in LOCAL_FIELDS:
        saved.pop(key, None)
    saved["installed_from"] = {"version": version, "sha": sha, "at": int(time.time()), **_marks(saved)}
    skills.save(saved)

    return skills.summary(saved)
