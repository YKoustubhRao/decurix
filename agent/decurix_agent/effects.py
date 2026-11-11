"""
effects.py - what a taught task's consequential steps leave behind that code can read back, derived from the taught
steps' own structure: a file a step writes (its path a value the run fills, saved by the press after it) and the status
the application shows after a press (the step's recorded result). A skill whose every consequential step has such an
effect may replay while it is still being learned, and a run whose effects were all read back by code counts on the
`code` basis - work that writes no record (an export, an import, an upload, a filing) can earn trust too.
"""

import json
import os
import re
from pathlib import Path

from decurix_agent.computer import replay
from decurix_agent.logging_utils import log


# ============================================================
# Code-checkable effects (B1 of the 2026-11-11 diagnosis: 66% of all tokens)
#
# A taught task that saves no record of its own - export the quarter's sales from the books into a folder, import it
# on a portal, upload, file - never replayed and could never be trusted: trust needs a check independent of the worker
# (experience.INDEPENDENT: code, ledger, person), and only a declared record (the ledger) or a workbook figure "per a
# source" gave one. So every run of such a task was the model's, click by click, 1.7-7.6M tokens, month after month.
#
# The effects a run must leave are in the taught steps already, by structure - no word of any label is read:
#   file    a step types a value that is a file path (a document kind) and a press within FILE_COMMIT_WITHIN steps after
#           it commits (replay._step_commits): the file must be there after the run, written during it (not a stale
#           copy from an earlier run), open as its kind (an Office file by office.open_status, JSON and CSV parsed), and
#           hold each identifier and amount this run typed before that press (what was entered is what was written);
#   folder  the same with a folder path: a document file written in that folder during the run (said, never failed:
#           a folder typed for a choice is not always one written to - checked or not checked, never a pass by default);
#   status  a consequential press whose recorded result (a dialog, a heading, a status, a page) the replay waited for and
#           saw appear after the press, by code (escalation.Ladder.confirmed).
# A consequential step is one the gate asked about when it was taught, or a press that commits.
#
# What this changes, and what it never does:
#   - replayable(): every consequential step has a file or status effect -> the skill may replay while apprentice, its
#     values all filled (skill_task._learning_by_model), the plan put to the person first as for a declared record;
#   - check(): after a replay, the file effects are read back (a missing, stale, broken file, or one without what the run
#     typed, fails the run: problems) and each consequential step's status is looked up; all of them by code -> `code`.
#   - the final check (the judge while apprentice, the spot check when trusted), the approval gate on every press and
#     the ledger's read-back are untouched: an effect adds a check, it never removes one.
# ============================================================

DOCUMENT_SUFFIXES = {".pdf", ".docx", ".doc", ".xlsx", ".xls", ".csv", ".jpg", ".jpeg", ".png", ".txt", ".odt", ".ods",
                     ".json", ".xml", ".zip"}
FILE_COMMIT_WITHIN = 4
AMOUNT = re.compile(r"^-?\d[\d,]*(?:\.\d+)?$")
NUMBER_IN_TEXT = re.compile(r"-?\d[\d,]*(?:\.\d+)?")
MIN_IDENTIFIER = 4


def _norm(text):
    return " ".join(str(text or "").split())


def _path_of(value):
    """The path a typed value names (absolute or under ~), or None."""

    text = _norm(value)
    if not text or "\n" in text or not (text.startswith("~") or os.path.isabs(text)):
        return None
    return Path(os.path.expanduser(text))


def consequential(program):
    """The steps of a (written-out) program that commit or that the gate asked about when they were taught."""

    return [s for s in program.get("steps") or [] if s.get("consequential") or replay._step_commits(s)]


def file_effects(program, values):
    """[{"kind": "file"|"folder", "path", "step", "commit"}]: the files and folders the run's steps write, by structure
    (a typed path, a committing press within FILE_COMMIT_WITHIN steps after it)."""

    steps = program.get("steps") or []
    out = []
    for i, step in enumerate(steps):
        if step.get("kind") != "type" or not step.get("param"):
            continue
        path = _path_of((values or {}).get(step["param"]))
        if path is None:
            continue
        commit = next((s for s in steps[i + 1:i + 1 + FILE_COMMIT_WITHIN]
                       if s.get("consequential") or replay._step_commits(s)), None)
        if commit is None:
            continue
        kind = "file" if path.suffix.lower() in DOCUMENT_SUFFIXES else ("folder" if not path.suffix else None)
        if kind:
            out.append({"kind": kind, "path": str(path), "step": step["n"], "commit": commit["n"]})
    return out


def coverage(program, values):
    """(covered, uncovered): the consequential steps whose effect code can read back (a file or folder written by the
    press, or a recorded result to wait for), and those with none - as step numbers."""

    by_commit = {e["commit"] for e in file_effects(program, values)}
    covered, uncovered = [], []
    for step in consequential(program):
        (covered if step["n"] in by_commit or step.get("expect") else uncovered).append(step["n"])
    return covered, uncovered


def replayable(program, values):
    """(ok, why): whether a skill whose run writes no declared record may replay while still being learned - it has a
    consequential step, and every one has an effect code can read back."""

    covered, uncovered = coverage(program, values)
    if not covered and not uncovered:
        return False, "nothing it does commits anything, so nothing of it can be read back"
    if uncovered:
        return False, ("step " + ", ".join(str(n) for n in uncovered[:6])
                       + " commit(s) and leave nothing code can read back (no file written, no recorded result)")
    return True, ""


def _typed_before(program, values, commit):
    """(identifiers, amounts) this run typed before the press `commit` (not paths): what a file it writes holds."""

    ids, amounts = [], []
    for step in program.get("steps") or []:
        if step["n"] >= commit:
            break
        if step.get("kind") != "type" or not step.get("param"):
            continue
        value = _norm((values or {}).get(step["param"]))
        if not value or _path_of(value) is not None or " " in value:
            continue
        if AMOUNT.match(value) and sum(c.isdigit() for c in value) >= 3:
            amounts.append(value)
        elif (len(value) >= MIN_IDENTIFIER and any(c.isdigit() for c in value) and any(c.isalpha() for c in value)):
            ids.append(value)
    return list(dict.fromkeys(ids)), list(dict.fromkeys(amounts))


def _amount(text):
    try:
        return round(float(str(text).replace(",", "")), 2)
    except ValueError:
        return None


def _text_of(path):
    """A written file's text and whether it parsed as its kind: (text, problem or "", not_made or "")."""

    suffix = path.suffix.lower()
    try:
        if suffix == ".json":
            raw = path.read_text(encoding="utf-8")
            json.loads(raw)
            return raw, "", ""
        if suffix in (".csv", ".txt", ".xml"):
            return path.read_text(encoding="utf-8", errors="replace"), "", ""
    except (OSError, ValueError) as exc:
        return "", f"{path} does not open as a {suffix[1:].upper()} file ({' '.join(str(exc).split())[:120]})", ""
    from decurix_agent import produced
    try:
        status = produced.open_statuses([str(path)]).get(str(path))
    except Exception as exc:  # noqa: BLE001 - a check that could not be made
        return "", "", f"{path} could not be opened to check it ({exc})"
    if status and status[0] == "broken":
        return "", f"{path} does not open: {status[1]}", ""
    if status and status[0] in ("protected", "unchecked"):
        return "", "", f"{path} could not be read: {status[1]}"
    try:
        from decurix_agent import binding
        return binding._document_text(str(path)), "", ""
    except Exception as exc:  # noqa: BLE001
        return "", "", f"{path} could not be read ({exc})"


def check_file(effect, program, values, began):
    """(problems, not_made, checked): one written file read back."""

    path = Path(effect["path"])
    step, commit = effect["step"], effect["commit"]
    if effect["kind"] == "folder":
        try:
            fresh = [p for p in path.iterdir() if p.is_file() and p.suffix.lower() in DOCUMENT_SUFFIXES
                     and p.stat().st_mtime >= began - 1]
        except OSError:
            fresh = []
        if not fresh:
            return [], [f"no document was written in {path} during this run (the folder step {step} typed, saved by step "
                        f"{commit}) - not checked"], []
        return [], [], [f"{len(fresh)} document(s) written in {path} during this run: "
                        + ", ".join(p.name for p in fresh[:6])]
    if not path.is_file():
        return [f"the file step {step} saved with step {commit}, {path}, is not there"], [], []
    try:
        written = path.stat().st_mtime
    except OSError as exc:
        return [], [f"{path} could not be read ({exc})"], []
    if written < began - 1:
        return [f"{path} was not written during this run - it is an earlier copy (step {commit} should have saved it)"], [], []
    text, problem, unread = _text_of(path)
    if problem:
        return [problem], [], []
    if unread:
        return [], [unread], []
    ids, amounts = _typed_before(program, values, commit)
    low = text.lower()
    lacking = [i for i in ids if i.lower() not in low]
    numbers = {_amount(n) for n in NUMBER_IN_TEXT.findall(text)}
    lacking += [a for a in amounts if _amount(a) not in numbers]
    if lacking:
        return [f"{path} does not hold what this run entered before saving it: " + ", ".join(lacking[:8])], [], []
    held = f", holding the {len(ids) + len(amounts)} value(s) entered before it" if ids or amounts else ""
    return [], [], [f"{path} written during this run and opens{held}"]


def check(program, values, began, confirmed=()):
    """
    Read a replayed run's effects back: {"problems", "not_made", "checked", "all_code"}. `confirmed` is the step ids
    whose recorded result the replay saw appear (escalation.Ladder.confirmed). all_code: there is at least one
    consequential step, and every one had its effect read back by code with nothing wrong.
    """

    problems, not_made, checked = [], [], []
    effects = file_effects(program, values)
    by_commit = {}
    for e in effects:
        by_commit.setdefault(e["commit"], []).append(e)
    confirmed = {str(c) for c in confirmed or ()}
    steps = consequential(program)
    for step in steps:
        written = by_commit.get(step["n"]) or []
        for effect in written:
            try:
                p, u, c = check_file(effect, program, values, began)
            except Exception as exc:  # noqa: BLE001 - a check that broke is a check not made
                log(f"effects: could not read back {effect.get('path')}: {exc}")
                p, u, c = [], [f"{effect.get('path')} could not be read back ({exc})"], []
            problems += p
            not_made += u
            checked += c
        if written:
            continue
        if step.get("expect"):
            if str(step.get("id") or step["n"]) in confirmed:
                checked.append(f"step {step['n']}: its result appeared after the press "
                               f"({', '.join(replay.describe_check(c) for c in step['expect'][:2])})")
            else:
                not_made.append(f"step {step['n']}: its result was not seen appear by code")
        else:
            not_made.append(f"step {step['n']}: it leaves nothing code can read back")
    return {"problems": problems, "not_made": not_made, "checked": checked,
            "all_code": bool(steps) and not problems and not not_made}


def report(found):
    """The lines a run's report and the final check get ("" when nothing was checked)."""

    if not found or not (found["checked"] or found["problems"] or found["not_made"]):
        return ""
    lines = ["CHECKED IN CODE, NOT CLAIMED - what this run's saves left behind:"]
    lines += [f"- {c}" for c in found["checked"]]
    lines += [f"- WRONG: {p}" for p in found["problems"]]
    lines += [f"- not checked: {u}" for u in found["not_made"]]
    return "\n".join(lines)
