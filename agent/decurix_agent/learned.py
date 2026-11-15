"""
What a worker has learned, kept to account: every write to a learned artefact on one ledger, a person's lock, how
often each is used, and which have gone stale (docs/skills.md, "The change ledger").

The artefacts are the files a worker writes about how to do its work: a taught skill (~/.decurix/skills/<name>.json,
branches and corrections included - they live in it), a kind of task's method (methods.py), a guide to an app
(app_skills.py), a playbook and the tool script written from one (playbooks.py), and a standing procedure
(procedures.py). Each store's writers call write() or remove() here, and nothing else writes those files:

    <store>/.ledger.jsonl     one line per change: {id, at, actor, task_id, kind, name, path, op, before, after}
                              (before/after: the file's sha256, None where there was no file)
    <store>/.blobs/<sha256>   the file's bytes at that hash, kept once however many entries name them
    <store>/.learned.json     per artefact: locked, pinned, archived, use_count, view_count, patch_count, last_used,
                              created_by, created_at

The ledger sits beside what it accounts for, so a store's history moves, backs up and is isolated with it.

`actor` is the call site's to say, never guessed here: "person" (a person's own action - teaching, a delete, a page's
button), "install" (a person installing from the org's registry), "rollback" (a person putting an earlier version
back), "learner" (the worker keeping what a checked run taught it), "correction" (a person's word kept by a run),
"branch" (a repair a run found, kept on the skill), "relearn" (the learner revising a kept artefact from a newer run),
"run" (how a run went: counts and verdicts), "retire" (the worker dropping what stopped working) and "agent" (the
engine itself writing one). A write that names none is recorded as "unknown" and treated as the worker's.

A locked artefact refuses every write but a person's (Locked, an OSError: each writer's existing "could not write"
path). One exception keeps the record honest: a "run" write to a locked skill or playbook still keeps how the run
went (RUN_RECORD's keys), and nothing else of it - a run's verdict is evidence, never a change to how the work is done.
The lock stops the worker's own learning; it is not a boundary against code running as the worker's user (that is the
approval gate's, docs/gate-boundary.md).

`stale` is worked out on read and never stored or acted on: an artefact unused for STALE_DAYS (120 by default:
monthly and quarterly work is normal) that is not pinned and that no schedule or mail trigger names. `archived` is only
ever a person's: it sets the artefact aside (not offered to tasks on its own; a person can still run it by name) and
deletes nothing; unarchive brings it back.

Techniques after Hermes Agent's tools/skill_ledger.py (content-addressed before/after, rollback that fails closed),
tools/skill_lock.py (a person's lock on every agent write path) and tools/skill_usage.py with agent/curator.py's
deterministic pass (use/view/patch counts; a never-used artefact anchors on when it was made).
"""

import contextlib
import hashlib
import json
import os
import threading
import time
import uuid
from pathlib import Path

from decurix_agent.logging_utils import log

# Who may write: the call site names one (module docstring).
PERSON, INSTALL, ROLLBACK = "person", "install", "rollback"
LEARNER, CORRECTION, BRANCH, RELEARN, RUN, RETIRE, AGENT = (
    "learner", "correction", "branch", "relearn", "run", "retire", "agent")
UNKNOWN = "unknown"
ACTORS = frozenset({PERSON, INSTALL, ROLLBACK, LEARNER, CORRECTION, BRANCH, RELEARN, RUN, RETIRE, AGENT})
# Writes a lock does not stop: a person's own.
PERSON_ACTORS = frozenset({PERSON, INSTALL, ROLLBACK})

KINDS = ("skill", "method", "app_guide", "playbook", "tool_script", "procedure")
# The keys of a JSON artefact that record how its runs went (never how the work is done): what a "run" write still
# keeps on a locked one.
RUN_RECORD = {"skill": ("runs", "experience"), "playbook": ("uses", "misses")}

LEDGER, BLOBS, STATE, LOCK = ".ledger.jsonl", ".blobs", ".learned.json", ".learned.lock"

ACTIVE, STALE, ARCHIVED = "active", "stale", "archived"
FLAGS = ("locked", "pinned", "archived")


def _stale_days():
    try:
        return max(1, int(os.environ.get("DECURIX_STALE_DAYS") or 120))
    except ValueError:
        return 120


class Locked(PermissionError):
    """A write a person's lock refused, in words a person can act on."""


class Refused(ValueError):
    """A rollback that cannot be made safely: nothing was changed."""


def root(kind):
    """The folder a kind of artefact is kept in (read at call time, so a test's or an env's folder holds)."""

    if kind == "skill":
        from decurix_agent.computer import skills
        return Path(skills.ROOT)
    if kind == "method":
        from decurix_agent import methods
        return Path(methods.ROOT)
    if kind == "app_guide":
        from decurix_agent import app_skills
        return Path(app_skills.LEARNED)
    if kind == "playbook":
        from decurix_agent import playbooks
        return Path(playbooks.ROOT)
    if kind == "tool_script":
        from decurix_agent import playbooks
        return Path(playbooks.TOOLS)
    if kind == "procedure":
        from decurix_agent import procedures
        return Path(procedures.DIR)
    raise ValueError(f"not a kind of learned artefact: {kind!r}")


# ============================================================
# One lock for a store's ledger, state and artefacts
# ============================================================

_thread_lock = threading.RLock()
_held = threading.local()


@contextlib.contextmanager
def _locked(folder):
    """The store's lock: a read-then-write of an artefact, its ledger line and its state is one step for every
    process (the agent and each `gui`). Re-entered by the same thread without waiting on itself."""

    from decurix_agent import filelock
    key = str(Path(folder).resolve())
    with _thread_lock:
        held = _held.__dict__.setdefault("folders", set())
        if key in held:
            yield
            return
        folder.mkdir(parents=True, exist_ok=True)
        with open(folder / LOCK, "a+") as handle:
            filelock.exclusive(handle)
            held.add(key)
            try:
                yield
            finally:
                held.discard(key)


# ============================================================
# Reading
# ============================================================

def _sha(data):
    return hashlib.sha256(data).hexdigest() if data is not None else None


def _read(path):
    try:
        return Path(path).read_bytes()
    except FileNotFoundError:
        return None
    except IsADirectoryError:
        return None


def _inside(path, folder):
    try:
        return Path(path).resolve().is_relative_to(Path(folder).resolve())
    except (OSError, ValueError):
        return False


def _check(kind, name, path, actor):
    if kind not in KINDS:
        raise ValueError(f"not a kind of learned artefact: {kind!r}")
    if not name or not isinstance(name, str):
        raise ValueError("a learned artefact needs its name")
    if actor is not None and actor not in ACTORS:
        raise ValueError(f"not an actor: {actor!r}")
    folder = root(kind)
    if not _inside(path, folder):
        raise ValueError(f"{path} is not in the {kind} store")
    return folder


def _state_all(folder):
    try:
        data = json.loads((folder / STATE).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def _state_put(folder, data):
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(folder / STATE, data, atomic=True, json_kw={"indent": 1, "ensure_ascii": False})


def _entries(folder):
    try:
        with open(folder / LEDGER, encoding="utf-8") as f:
            lines = f.readlines()
    except OSError:
        return []
    out = []
    for line in lines:
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if isinstance(entry, dict) and entry.get("id"):
            out.append(entry)
    return out


# ============================================================
# Writing
# ============================================================

def _keep_blob(folder, data):
    """The bytes kept under their hash (once). True when the blob on disk holds exactly them."""

    if data is None:
        return True
    digest = _sha(data)
    blob = folder / BLOBS / digest
    if _sha(_read(blob)) == digest:
        return True
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        return False  # (a store's artefacts are text; bytes that are not cannot be withheld, so they are not copied)
    from decurix_agent.computer import privacy
    blob.parent.mkdir(parents=True, exist_ok=True)
    try:
        privacy.write_scrubbed(blob, text, atomic=True, private=True, newline="")
    except OSError as exc:
        log(f"Learned: could not keep a copy of {digest[:12]}: {exc}")
        return False
    # Withholding changed it (a secret the artefact itself still holds): what is kept is not that version.
    return _sha(_read(blob)) == digest


def _append(folder, entry):
    from decurix_agent.computer import privacy
    privacy.write_scrubbed(folder / LEDGER, entry, append=True, json_kw={"ensure_ascii": False}, end="\n",
                           exempt=("id", "before", "after", "of", "task_id"))


def _current_task():
    """The task this process is doing, when there is one (task_context), else None."""

    try:
        from decurix_agent import task_context
        return task_context.current() or None
    except Exception:  # noqa: BLE001 - no task known is no task named
        return None


def _record(folder, kind, name, path, op, actor, task_id, before, after, **extra):
    entry = {"id": uuid.uuid4().hex[:16], "at": round(time.time(), 3), "actor": actor or UNKNOWN,
             "task_id": task_id or _current_task(), "kind": kind, "name": name, "path": str(path), "op": op,
             "before": _sha(before), "after": _sha(after)}
    entry.update(extra)
    kept = _keep_blob(folder, before) and _keep_blob(folder, after)
    if not kept:
        entry["copies"] = False  # (a rollback to or from this entry cannot be made: it says so)
    _append(folder, entry)
    state = _state_all(folder)
    mine = state.setdefault(name, {})
    if op in ("write", "delete"):
        mine["patch_count"] = int(mine.get("patch_count") or 0) + 1
    if before is None and after is not None and "created_by" not in mine:
        mine["created_by"] = actor or UNKNOWN
        mine["created_at"] = entry["at"]
    _state_put(folder, state)
    return entry


# A kind whose artefact belongs to another's: locking the owner locks it too (a playbook's tool script).
LOCKED_WITH = {"tool_script": "playbook"}


def _held_lock(kind, name, state=None):
    mine = (state if state is not None else _state_all(root(kind))).get(name) or {}
    if mine.get("locked"):
        return True
    owner = LOCKED_WITH.get(kind)
    return bool(owner and (_state_all(root(owner)).get(name) or {}).get("locked"))


def is_locked(kind, name):
    """Whether a person locked this artefact (or the one it belongs to: LOCKED_WITH)."""

    try:
        return _held_lock(kind, name)
    except Exception:  # noqa: BLE001 - what cannot be read is not locked (the artefact's own write still checks)
        return False


def _run_record_only(kind, before, content):
    """The artefact on disk with only how-the-run-went keys taken from `content` (a locked artefact's "run" write),
    or None when that cannot be done."""

    keys = RUN_RECORD.get(kind)
    if not keys or before is None or not isinstance(content, dict):
        return None
    try:
        current = json.loads(before.decode("utf-8"))
    except (UnicodeDecodeError, ValueError):
        return None
    if not isinstance(current, dict):
        return None
    merged = dict(current)
    for key in keys:
        if key in content:
            merged[key] = content[key]
        else:
            merged.pop(key, None)
    return merged


def write(kind, name, path, content, actor=None, task_id=None, **write_kw):
    """
    Write a learned artefact - the one way any of them is written: `content` through privacy.write_scrubbed
    (`write_kw` its options), the change on the store's ledger with copies of the bytes before and after.

    Raises Locked when a person locked the artefact and `actor` is not a person's (a "run" write to a locked skill or
    playbook keeps only RUN_RECORD's keys, module docstring). Returns the ledger entry, or None when nothing changed.
    """

    path = Path(path)
    folder = _check(kind, name, path, actor)
    with _locked(folder):
        before = _read(path)
        if actor not in PERSON_ACTORS and _held_lock(kind, name, _state_all(folder)):
            refused = (f"{kind} {name} is locked by a person, so it was not changed ({actor or UNKNOWN}); "
                       "unlock it to let the worker change it")
            if actor != RUN:
                raise Locked(refused)
            # A run's end: how it went is kept, what it would have changed is not - and it never fails the run.
            merged = _run_record_only(kind, before, content)
            if merged is None:
                log(f"Learned: {refused}")
                return None
            if merged != content:
                log(f"Learned: {kind} {name} is locked by a person - kept how the run went, nothing else")
            content = merged
        path.parent.mkdir(parents=True, exist_ok=True)
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(path, content, **write_kw)
        after = _read(path)
        if after == before:
            return None
        return _record(folder, kind, name, path, "write", actor, task_id, before, after)


def remove(kind, name, path, actor=None, task_id=None):
    """Delete a learned artefact's file, on the ledger (its last bytes kept: rollback brings them back). Raises Locked
    as write() does. Returns the entry, or None when there was no file."""

    path = Path(path)
    folder = _check(kind, name, path, actor)
    if not path.is_file():
        return None  # (nothing to remove: the store is left untouched, its lock file not even made)
    with _locked(folder):
        before = _read(path)
        if before is None:
            return None
        if actor not in PERSON_ACTORS and _held_lock(kind, name, _state_all(folder)):
            raise Locked(f"{kind} {name} is locked by a person, so it was not removed ({actor or UNKNOWN}); "
                         "unlock it to let the worker change it")
        entry = _record(folder, kind, name, path, "delete", actor, task_id, before, None)
        path.unlink(missing_ok=True)
        return entry


# ============================================================
# History and rollback
# ============================================================

def history(kind, name, limit=20):
    """The ledger's entries for one artefact, newest first."""

    found = [e for e in _entries(root(kind)) if e.get("name") == name and e.get("kind") == kind]
    return list(reversed(found))[:limit]


def _find(entry_id):
    for kind in KINDS:
        try:
            folder = root(kind)
        except Exception:  # noqa: BLE001 - a store that cannot be named holds no entry
            continue
        for entry in _entries(folder):
            if entry.get("id") == entry_id and entry.get("kind") == kind:
                return folder, entry
    return None, None


def rollback(entry_id, actor=PERSON, task_id=None):
    """
    Put an artefact back as it was before one change: only when its file still holds exactly what that change wrote
    (else Refused - a later change would be lost without anyone seeing it), and only from a kept copy whose hash is
    the one recorded. A change that made the file is undone by removing it. Appends its own entry and returns it.
    """

    if actor not in PERSON_ACTORS:
        raise Locked("only a person rolls a learned artefact back")
    folder, entry = _find(str(entry_id or "").strip())
    if entry is None:
        raise Refused(f"no change {entry_id} on this desktop's ledgers")
    if entry.get("op") not in ("write", "delete", "rollback"):
        raise Refused(f"change {entry_id} is a {entry.get('op')}, not a change to the file")
    kind, name, path = entry["kind"], entry["name"], Path(entry["path"])
    if not _inside(path, folder):
        raise Refused(f"change {entry_id} names a file outside its store")
    with _locked(folder):
        current = _read(path)
        if _sha(current) != entry.get("after"):
            raise Refused(f"{kind} {name} has changed since {entry_id}; roll back its later changes first "
                          "(history shows them)")
        if entry.get("before") is None:
            path.unlink(missing_ok=True)
            return _record(folder, kind, name, path, "write", ROLLBACK, task_id, current, None, of=entry["id"])
        blob = _read(folder / BLOBS / entry["before"])
        if _sha(blob) != entry["before"]:
            raise Refused(f"no exact copy of {kind} {name} as it was before {entry_id} was kept")
        from decurix_agent.computer import privacy
        path.parent.mkdir(parents=True, exist_ok=True)
        privacy.write_scrubbed(path, blob.decode("utf-8"), atomic=True, newline="")
        after = _read(path)
        restored = _record(folder, kind, name, path, "write", ROLLBACK, task_id, current, after, of=entry["id"])
        if _sha(after) != entry["before"]:
            log(f"Learned: {kind} {name} was put back with something withheld that the old version held")
        return restored


# ============================================================
# A person's flags, use, and what has gone stale
# ============================================================

def set_flag(kind, name, flag, value, actor=PERSON):
    """Lock, pin or archive an artefact (or undo it): only a person's, and on the ledger. Returns the entry."""

    if flag not in FLAGS:
        raise ValueError(f"not a flag: {flag!r}")
    if actor != PERSON:
        verb = {"locked": "lock", "pinned": "pin", "archived": "archive"}[flag]
        raise Locked(f"only a person may {'' if value else 'un'}{verb} a learned artefact")
    folder = root(kind)
    with _locked(folder):
        state = _state_all(folder)
        mine = state.setdefault(name, {})
        if bool(mine.get(flag)) == bool(value):
            return None
        mine[flag] = bool(value)
        _state_put(folder, state)
        op = {"locked": "lock", "pinned": "pin", "archived": "archive"}[flag]
        entry = {"id": uuid.uuid4().hex[:16], "at": round(time.time(), 3), "actor": actor, "task_id": None,
                 "kind": kind, "name": name, "path": "", "op": op if value else "un" + op,
                 "before": None, "after": None}
        _append(folder, entry)
        return entry


def _note(kind, name, counter, at):
    try:
        folder = root(kind)
        with _locked(folder):
            state = _state_all(folder)
            mine = state.setdefault(name, {})
            mine[counter] = int(mine.get(counter) or 0) + 1
            if at:
                mine[at] = round(time.time(), 3)
            _state_put(folder, state)
    except Exception as exc:  # noqa: BLE001 - counting is bookkeeping: it never costs the task
        log(f"Learned: could not count a {counter[:-6]} of {kind} {name}: {exc}")


def note_use(kind, name):
    """Count one use: the artefact was put to work in a task (run, replayed, or given to the engine as its way)."""

    if name:
        _note(kind, name, "use_count", "last_used")


def note_view(kind, name):
    """Count one view: the artefact was read out in full on request."""

    if name:
        _note(kind, name, "view_count", "last_viewed")


def _made_at(kind, name):
    """When an artefact's file was last written, for one with no use and no recorded making (made before this)."""

    folder = root(kind)
    for candidate in (folder / f"{name}.json", folder / name / "SKILL.md", folder / f"{name}.py", folder / f"{name}.sh"):
        try:
            return candidate.stat().st_mtime
        except OSError:
            continue
    return None


def status(kind, name, now=None, referenced=()):
    """
    One artefact's usage and state: {use_count, view_count, patch_count, last_used, created_by, locked, pinned,
    archived, state}. `state` is "archived" (a person's), "stale" (unused for STALE_DAYS, anchored on when it was
    made when it was never used, and never for a pinned one or one in `referenced` - the names a schedule or a mail
    trigger runs) or "active". Worked out now; nothing is stored or changed by it.
    """

    mine = dict(_state_all(root(kind)).get(name) or {})
    out = {"use_count": int(mine.get("use_count") or 0), "view_count": int(mine.get("view_count") or 0),
           "patch_count": int(mine.get("patch_count") or 0), "last_used": mine.get("last_used"),
           "created_by": mine.get("created_by"), "locked": bool(mine.get("locked")),
           "pinned": bool(mine.get("pinned")), "archived": bool(mine.get("archived"))}
    if out["archived"]:
        out["state"] = ARCHIVED
        return out
    anchor = mine.get("last_used") or mine.get("created_at") or _made_at(kind, name)
    now = time.time() if now is None else now
    stale = (anchor is not None and now - float(anchor) > _stale_days() * 86400
             and not out["pinned"] and name not in set(referenced or ()))
    out["state"] = STALE if stale else ACTIVE
    return out


def archived(kind):
    """The names of a kind a person set aside (not offered to tasks on their own)."""

    try:
        return {n for n, s in _state_all(root(kind)).items() if isinstance(s, dict) and s.get("archived")}
    except Exception:  # noqa: BLE001 - nothing readable is set aside
        return set()


def names(kind):
    """Every artefact of a kind the store holds now, by name."""

    folder = root(kind)
    try:
        if kind == "app_guide":
            return sorted(p.name for p in folder.iterdir()
                          if p.is_dir() and not p.name.startswith(".") and (p / "SKILL.md").is_file())
        if kind == "tool_script":
            return sorted({p.stem for p in folder.iterdir() if p.suffix in (".py", ".sh") and p.is_file()})
        return sorted(p.stem for p in folder.glob("*.json") if not p.name.startswith("."))
    except OSError:
        return []


def listing(now=None, referenced=()):
    """Every learned artefact on this desktop with its status(): [{kind, name, ...}], by kind then name."""

    out = []
    for kind in KINDS:
        for name in names(kind):
            out.append({"kind": kind, "name": name, **status(kind, name, now=now, referenced=referenced)})
    return out


# ============================================================
# What a person types: "/skill <verb> <target>" (skill_commands.parse_manage, skill_task)
# ============================================================

# The words a person names a kind by.
KIND_WORDS = {"skill": "skill", "method": "method", "guide": "app_guide", "app_guide": "app_guide",
              "playbook": "playbook", "tool": "tool_script", "tool_script": "tool_script", "procedure": "procedure"}
_VERB_FLAG = {"lock": ("locked", True), "unlock": ("locked", False), "pin": ("pinned", True),
              "unpin": ("pinned", False), "archive": ("archived", True), "unarchive": ("archived", False)}


def target(text):
    """(kind, name) of "<name>" (a skill) or "<kind>:<name>". Raises ValueError for a kind nobody keeps."""

    word, sep, name = (text or "").partition(":")
    if not sep:
        return "skill", word
    if word.lower() not in KIND_WORDS:
        kinds = ", ".join(sorted(set(KIND_WORDS) - {"app_guide", "tool_script"}))
        raise ValueError(f"{word!r} is not a kind of learned thing: {kinds}")
    return KIND_WORDS[word.lower()], name


def _when(at):
    if not at:
        return "never"
    return time.strftime("%Y-%m-%d %H:%M", time.localtime(float(at)))


def _exists(kind, name):
    return name in names(kind) or bool(_state_all(root(kind)).get(name)) or bool(history(kind, name, limit=1))


def answer(verb, text, referenced=()):
    """The reply to a person's "/skill <verb> <target>". Changes only what the verb says; raises ValueError (a
    target nobody keeps), Refused (a rollback that cannot be made) or Locked with words the person can act on."""

    if verb == "rollback":
        entry = rollback(text, actor=PERSON)
        return (f"Put {entry['kind'].replace('_', ' ')} {entry['name']} back as it was before change {entry['of']} "
                f"(this is change {entry['id']}; roll it back to undo this).")
    if verb == "learned":
        kinds = KINDS if text.lower() == "all" else (KIND_WORDS.get(text.lower()),)
        if None in kinds:
            raise ValueError(f"{text!r} is not a kind of learned thing")
        rows = [r for r in listing(referenced=referenced) if r["kind"] in kinds]
        if not rows:
            return "Nothing of that kind is kept on this desktop."
        return "What this desktop keeps:\n" + "\n".join(
            f"- {r['kind'].replace('_', ' ')} `{r['name']}`: {r['state']}"
            + "".join(f", {f}" for f in FLAGS[:2] if r[f])
            + f"; used {r['use_count']} times, last {_when(r['last_used'])}; changed {r['patch_count']} times"
            + (f"; made by {r['created_by']}" if r.get("created_by") else "") for r in rows)
    kind, name = target(text)
    if not name or not _exists(kind, name):
        raise ValueError(f"no {kind.replace('_', ' ')} {name!r} on this desktop")
    if verb == "history":
        note_view(kind, name)
        shown = status(kind, name, referenced=referenced)
        lines = [f"{kind.replace('_', ' ').capitalize()} {name}: {shown['state']}"
                 + (", locked" if shown["locked"] else "") + (", pinned" if shown["pinned"] else "")
                 + f"; used {shown['use_count']} times, last {_when(shown['last_used'])}."]
        for e in history(kind, name):
            lines.append(f"- {e['id']} {_when(e.get('at'))} {e.get('op')} by {e.get('actor')}"
                         + (f" (task {e['task_id']})" if e.get("task_id") else "")
                         + (f", undoing {e['of']}" if e.get("of") else "")
                         + ("" if e.get("copies", True) else " - no copy kept, cannot be rolled back"))
        if len(lines) == 1:
            lines.append("No change to it is on the ledger yet.")
        else:
            lines.append("`/skill rollback <id>` puts it back as it was before that change.")
        return "\n".join(lines)
    flag, value = _VERB_FLAG[verb]
    done = set_flag(kind, name, flag, value, actor=PERSON)
    said = {"lock": "Locked: the worker will not change it until you unlock it (how its runs go is still kept).",
            "unlock": "Unlocked: the worker may change it again as it learns.",
            "pin": "Pinned: it is never counted stale.", "unpin": "Unpinned.",
            "archive": "Set aside: it is not offered to tasks on its own, and nothing of it is deleted; "
                       "`/skill unarchive` brings it back.",
            "unarchive": "Back in use."}[verb]
    past = {"lock": "locked", "unlock": "unlocked", "pin": "pinned", "unpin": "unpinned", "archive": "archived",
            "unarchive": "in use"}[verb]
    return f"{kind.replace('_', ' ').capitalize()} {name}: " + (said if done else f"already {past}.")
