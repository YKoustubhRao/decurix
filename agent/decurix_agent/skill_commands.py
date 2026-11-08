"""
What a person types to a desktop's taught skills: the "/learn", "/skill" and "/restore" commands, and the plain words
a reply to a stuck or paused run is read by (skill_task, docs/skills.md).

Split out of skill_task.py (follow-up O-10): skill_task teaches, runs and checks; this module only reads and writes
the words - a command parsed into its parts (parse_learn, parse_skill, skill_context, parse_restore), the command
that runs or teaches a skill written from its parts (request_text, learn_request), the words a reply is matched by
(ABANDON_RE, RETRY_RE, CARRY_ON_RE, NUMBER_RE, ANYWAY_RE, RESTART_RE, USE_RE), the names the two commands report as
(LEARN_ENGINE, SKILL_ENGINE), and the refusal a request ends with when it cannot be carried out (_Refused).

skill_task, teaching, engine_questions and final_gate import this module; it imports none of them.
"""

import re

from decurix_agent.replay_task import OPTION_RE


LEARN_ENGINE = "learn"
SKILL_ENGINE = "skill"

LEARN_RE = re.compile(r"^/learn(?:\s+(.*))?$", re.DOTALL)
SKILL_RE = re.compile(r"^/skills?(?:\s+(.*))?$", re.DOTALL)
# The reply to a report's offer to put a changed file back (source_guard.offer): "/restore <offer id> [file ...]".
RESTORE_RE = re.compile(r"^/restore(?:\s+(.*))?$", re.DOTALL)

ABANDON_RE = re.compile(r"^\W*(cancel|abort|stop|quit|never ?mind)\b", re.IGNORECASE)
RETRY_RE = re.compile(r"^\W*(retry|again|try again|look again)\b", re.IGNORECASE)
CARRY_ON_RE = re.compile(
    r"^\W*(skip|leave (?:it )?out|continue|carry on|go on|move on|done|it worked|i did (?:it|that)|looks (?:right|fine|good))\b",
    re.IGNORECASE,
)
NUMBER_RE = re.compile(r"^\W*(?:option\s*|number\s*|#)?(\d{1,2})\W*$", re.IGNORECASE)
# A person who can see the screen saying a point click is right where it is:
# "click anyway", "click it", "click there". Only offered, and only read, for a
# click recorded as a point whose picture did not match (see _unanchor).
ANYWAY_RE = re.compile(r"^\W*(click\s+(it\s+)?anyway|click\s+(it|there)|anyway)\W*$", re.IGNORECASE)
# Close the app a step found already open, and open it fresh (see replay's
# "reopen"). Only offered, and only read, when that is what stopped the run.
RESTART_RE = re.compile(r"^\W*(restart|re-?open|re-?start|open it (?:again|fresh)|start it (?:again|fresh)|close it and (?:re-?)?open it)\b",
                        re.IGNORECASE)
USE_RE = re.compile(r"^\W*(?:use|try|click|press|select|choose)\b\s*(.+?)\s*$", re.IGNORECASE | re.DOTALL)


class _Refused(Exception):
    """A request that cannot be carried out, in words a person can act on."""


# ============================================================
# Parsing
# ============================================================

def parse_learn(content):
    """([recording ids], name) of a "/learn" request, or None for any other request. No IDs means the latest demonstration."""

    match = LEARN_RE.match((content or "").strip())
    if not match:
        return None

    rest, name = match.group(1) or "", None

    # name="Google a phrase" may have spaces in it, so options are read the
    # way "/skill" reads its values, before the rest is split into IDs.
    for option in OPTION_RE.finditer(rest):
        if option.group(1) == "name":
            name = next(v for v in option.groups()[1:] if v is not None).strip() or None
    ids = OPTION_RE.sub(" ", rest).split()

    return ids, name


def parse_skill(content):
    """
    (what was asked for, {param: value}) of a "/skill" request, or None. Nothing
    asked for lists the skills. Only the first line is the command: lines after
    it are the run's context (an email, the files it brought - skill_context),
    which values the command does not give are read from.
    """

    match = SKILL_RE.match((content or "").strip().split("\n", 1)[0].strip())
    if not match:
        return None

    rest = match.group(1) or ""
    params = {}

    for option in OPTION_RE.finditer(rest):
        params[option.group(1)] = next(v for v in option.groups()[1:] if v is not None)

    query = OPTION_RE.sub("", rest).strip()
    return ("" if query.lower() == "list" else query), params


def skill_context(content):
    """What follows a "/skill" request's command line: the documents and email of this run."""

    parts = (content or "").strip().split("\n", 1)
    return parts[1].strip() if len(parts) > 1 else ""


def request_text(name, params=None):
    """
    The "/skill" request that runs `name` with `params` - what the control plane
    sends and the chat shows. Values are read back by parse_skill, so one that
    cannot be written into a request (it holds both kinds of quote) is refused
    rather than bent. A new line is the same as ";" in a list, and is written
    as one.
    """

    parts = [f"/skill {name}"]

    for key in sorted(params or {}):
        if not re.fullmatch(r"[A-Za-z_]\w*", key):
            raise ValueError(f"not a parameter name: {key!r}")

        value = ";".join(line.strip() for line in str(params[key]).splitlines() if line.strip())
        if not value:
            continue

        if '"' not in value:
            parts.append(f'{key}="{value}"')
        elif "'" not in value:
            parts.append(f"{key}='{value}'")
        else:
            raise ValueError(f"the value of {key} has both kinds of quote in it")

    return " ".join(parts)


def learn_request(sessions, name=None):
    """
    The "/learn" request that teaches from `sessions`, naming the skill `name` -
    what the control plane sends and the chat shows (httpapi.learnContent
    builds the same string). A name holding both kinds of quote is refused.
    """

    content = "/learn " + " ".join(sessions)
    name = " ".join((name or "").split())
    if not name:
        return content

    if '"' not in name:
        return f'{content} name="{name}"'
    if "'" not in name:
        return f"{content} name='{name}'"
    raise ValueError("the name has both kinds of quote in it")


def parse_restore(content):
    """
    (offer id, [file names]) of a "/restore" request - the person's reply to a report's offer to put a changed file of
    theirs back (source_guard.offer) - or None for any other request. No names means every file the offer holds.
    """

    match = RESTORE_RE.match((content or "").strip().split("\n", 1)[0].strip())
    if not match:
        return None
    import shlex
    try:
        words = shlex.split(match.group(1) or "")
    except ValueError:
        words = (match.group(1) or "").split()
    return (words[0] if words else ""), words[1:]
