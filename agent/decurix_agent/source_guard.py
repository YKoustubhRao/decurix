"""
The person's files in the shared folder are theirs: a task reads its sources and never changes them unless the
request asks for that.

Found live (2026-10-20, xl_overdue on a shared-folder worker): the deliverable was right, but the person's INPUT
workbook had been changed too - a "Days Overdue" column added and saved 50 s after the result, most likely by the app
saving the source when it closed. Nothing said "you changed my file"; the final check read the input as if it were an
output and called it a coverage gap.

So, when a task starts on a desktop with a shared folder, every file already in the folder is noted (size, time and a
hash) and the ones the request names - then others, within a budget - are copied into the agent's own space. When the
work is reported, a file the request gives (attached, named, or in a folder it names: `given`) that was there before
and is now different, and that the request did not ask to be changed, is put back from its copy and the result is not
done, said in plain words. Any other file of the folder is the person's business, never judged or put back. The same notes tell the checks which files
are sources, so a check of what was made never runs on them.

Since review 2026-11-03 (E-12) the gate takes the fact only (changed_sources): which given file changed and how, for
the judge and always for the person. Nothing is put back before a verdict, and whether the request allowed the change
is the judge's, not a verb list's. check_and_restore is the old rule, kept until its last caller is moved.
"""

import hashlib
import json
import os
import re
import shutil
import time
from pathlib import Path

from decurix_agent import attachments, config, shared, task_context
from decurix_agent.logging_utils import log

HASH_MAX = 64 * 1024 * 1024      # files larger than this are compared by size and time only
COPY_MAX = 25 * 1024 * 1024      # a file larger than this is not copied (and the person is told)
COPY_BUDGET = 300 * 1024 * 1024  # all copies of one task together
KEEP_TASKS = 5                   # snapshots of the last few tasks are kept, so the person can undo

_EXT = r"\.(?:xlsx|xlsm|xls|docx|doc|pptx|ppt|pdf|csv|tsv|txt|md|json|xml|html?|ods|odt|odp|png|jpe?g|gif|bmp|svg|webp|tiff?)\b"
# A file name a request mentions: a plain one (receivables.xlsx), or one with spaces in quotes ("Q3 review.xlsx").
# An opening quote never follows a letter: found (review 2026-11-03) "Record today's payments in ledger.xlsx" read
# "s payments in ledger.xlsx" as one quoted name, and the guard never noticed ledger.xlsx at all.
_REQUESTED = re.compile(r"(?<![\w.-])[\w-][\w.-]*" + _EXT + r"|(?<!\w)[\"'“]([^\"'”\n]{1,80}" + _EXT + r")", re.I)
# A verb asking for a file itself to be changed.
_EDIT = re.compile(r"\b(?:update|change|edit|modify|correct|fix|fill(?:\s+in)?|add|insert|append|remove|delete|"
                   r"highlight|colou?r|format|sort|rename|replace|overwrite|clean|mark|tidy)\b", re.I)
# A sentence that keeps the original: the result goes elsewhere.
_KEEPS_ORIGINAL = re.compile(r"\bsave\s+(?:\w+\s+){0,3}?as\b|\bas\s+a\s+(?:new|separate)\b|\bnew\s+(?:file|workbook|"
                             r"document|name|copy)\b|\ba\s+copy\b|\bkeep\s+(?:the|my)\s+original\b|\bleave\s+\w+\s+as\s+it\s+is\b|"
                             r"\bdo(?:n'?t| not)\s+(?:change|touch|modify|edit)\b", re.I)


def _root():
    try:
        return os.path.realpath(str(shared.link_path()))
    except Exception:
        return ""


def _rel(path):
    root = _root()
    real = os.path.realpath(str(path))
    if not root or not (real == root or real.startswith(root.rstrip(os.sep) + os.sep)):
        return None
    return os.path.relpath(real, root).replace(os.sep, "/")


def _digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _folder(task_id):
    return config.DECURIX_DIR / "snapshots" / (task_id or "current")


def _manifest_path(task_id):
    return _folder(task_id) / "manifest.json"


def _names(request):
    return {Path((m.group(1) or m.group(0)).replace("\\", "/")).name.strip().lower()
            for m in _REQUESTED.finditer(attachments.text(request or ""))}


def take(request, task_id, notify=None):
    """
    Note every file in the shared folder before the task touches it, and copy the ones the request names (then the
    rest, small ones first) into the agent's own space. Nothing when the desktop has no shared folder. Returns the
    manifest written, or None.
    """

    if not shared.active():
        return None
    folder = _folder(task_id)
    shutil.rmtree(folder, ignore_errors=True)
    (folder / "files").mkdir(parents=True, exist_ok=True)
    named = _names(request)
    files = {}
    for path in shared.walk():
        rel = _rel(path)
        if rel is None or Path(rel).name.startswith("~$"):
            continue
        try:
            info = os.stat(path)
        except OSError:
            continue
        files[rel] = {"size": info.st_size, "mtime": info.st_mtime_ns, "named": Path(rel).name.lower() in named,
                      "sha256": None, "copy": None}
    budget, skipped = COPY_BUDGET, []
    for rel in sorted(files, key=lambda r: (not files[r]["named"], files[r]["size"])):
        entry = files[rel]
        local = Path(_root()) / rel
        try:
            if entry["size"] <= HASH_MAX:
                entry["sha256"] = _digest(local)
            if entry["size"] <= COPY_MAX and entry["size"] <= budget:
                # Kept under a number, not the person's name and folders: found live (2026-10-21), an
                # engine searching the desktop for the client's workbook by name found this copy and worked on it.
                # The manifest says which file each copy is.
                kept = f"{len([e for e in files.values() if e.get('copy')]) + 1:05d}{Path(rel).suffix.lower()}"
                target = folder / "files" / kept
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(local, target)
                entry["copy"] = kept
                budget -= entry["size"]
            elif entry["named"]:
                skipped.append(rel)
        except OSError as exc:
            log(f"source_guard: could not note {rel}: {exc}")
    manifest = {"task_id": task_id, "at": time.time(), "request": attachments.text(request or "")[:4000], "files": files}
    _manifest_path(task_id).write_text(json.dumps(manifest, indent=1), encoding="utf-8")
    _prune(task_id)
    if skipped and notify:
        notify("Too large to keep a copy of before I start (I will still notice if it changes): " + ", ".join(skipped))
    return manifest


def _prune(keep):
    root = config.DECURIX_DIR / "snapshots"
    try:
        dirs = sorted((d for d in root.iterdir() if d.is_dir()), key=lambda d: d.stat().st_mtime, reverse=True)
    except OSError:
        return
    for old in dirs[KEEP_TASKS:]:
        if old.name != (keep or "current"):
            shutil.rmtree(old, ignore_errors=True)


def manifest(task_id):
    try:
        return json.loads(_manifest_path(task_id).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


# What an edit verb may be applied to that means a given file: a pronoun, "the attached ...", "the file/form ...".
_REFERS = r"(?:it|them|this|that|these|those|the\s+attached\b[^.\n]{0,40}?|the\s+(?:file|form|workbook|sheet|spreadsheet|" \
          r"document|doc|template|register|tracker|list|table|deck|presentation|report|letter))"


def may_change(name, request):
    """
    Whether the request asks for this file itself to be changed or written. Request-wide (independent review,
    2026-10-21: "The tracker is Onboarding_Tracker.xlsx in the folder. Update it with each joiner's employee ID." and
    "Fill in the attached expense form" were refused): the file is named anywhere in the request - its attachment block
    included - and an edit is asked of it anywhere: in a sentence naming it, or by an edit verb whose object is the
    file in other words (it, the attached form, the tracker, a word of its own name), and the sentence does not keep
    the original. Or it is named as where a result is saved ("save it as X", "into X").
    """

    raw = str(request or "")
    text = attachments.text(raw)
    quoted = r"(?<![\w.-])" + re.escape(name) + r"(?![\w-])"
    if not re.search(quoted, raw, re.I):
        return False  # (not a file this request gives by name)
    if re.search(r"\b(?:as|into|to|called|named|in)\s+[\"'“]?(?:[\w .-]*/)?" + quoted, text, re.I) and re.search(
            r"\b(?:save|saved|saving|write|export|put|create|make|name|call)\w*\b[^.\n]{0,80}" + quoted, text, re.I):
        return True
    own_words = [w for w in re.findall(r"[a-z]{4,}", Path(name).stem.lower().replace("_", " ").replace("-", " "))]
    target = _REFERS + (r"|the\s+(?:\w+\s+){0,2}(?:" + "|".join(map(re.escape, own_words)) + r")\b" if own_words else "")
    verb_on_it = re.compile(r"\b(?:update|change|edit|modify|correct|fix|fill(?:\s+(?:in|out))?|complete|add\s+to|"
                            r"insert\s+into|append\s+to|highlight|colou?r|format|sort|overwrite|mark|tidy|clean)\s+(?:"
                            + target + r")", re.I)
    for sentence in re.split(r"(?<=[.!?])\s+|\n+", text):
        if _KEEPS_ORIGINAL.search(sentence):
            continue
        if re.search(quoted, sentence, re.I) and _EDIT.search(sentence):
            return True
        if verb_on_it.search(sentence):
            return True
    return False


def _now(rel):
    local = Path(_root()) / rel
    try:
        info = os.stat(local)
    except OSError:
        return None
    return local, info


def _content(path):
    """
    What an office file holds, for telling a changed file from one an app saved again unchanged (found live
    2026-10-20: Excel re-saved an input workbook, 8,342 bytes for about 5,000, with the same cells): every sheet's cells
    (value or formula) for a workbook, the text and structure office reads for a document or deck. None for any other
    kind of file (its bytes decide), or one that cannot be read.
    """

    suffix = Path(path).suffix.lower()
    try:
        if suffix in {".xlsx", ".xlsm"}:
            import openpyxl
            book = openpyxl.load_workbook(path, data_only=False)
            try:
                return [(sheet.title, [(c.coordinate, c.value) for row in sheet.iter_rows() for c in row
                                       if c.value is not None]) for sheet in book.worksheets]
            finally:
                book.close()
        if suffix in {".docx", ".pptx", ".odt", ".odp", ".ods"}:
            from decurix_agent.tools import office
            return office.read_document(str(path)).get("text")
    except Exception as exc:  # noqa: BLE001 - unreadable: the bytes decide
        log(f"source_guard: could not read {path} for its content: {exc}")
    return None


def given(request):
    """
    The shared folder's files this request gives (as paths relative to the folder): the files it attaches or names,
    and every file under a folder it names (long_task.request_input_files). Only these are the task's sources to guard
    - the rest of the person's folder is theirs, and another worker or the person may well be changing it meanwhile.
    """

    out = set()
    paths = [str(p) for p in attachments.files(request or "")]
    try:
        from decurix_agent import long_task
        paths += [str(p) for p in long_task.request_input_files(request or "")]
    except Exception as exc:  # noqa: BLE001 - what cannot be listed is not guarded
        log(f"source_guard: could not list the request's inputs: {exc}")
    for path in paths:
        rel = _rel(path)
        if rel:
            out.add(rel)
    return out


def _compare(task_id, request):
    """([rel] changed in what they hold, [rel] saved again by an app with the same content) - among the files that
    were in the shared folder before the task, only those the request gives (named, attached, or in a folder it names)
    and does not ask to change. A gone one is changed. Any other file of the folder is not this task's to judge: found
    live (2026-10-21, two workers on one host folder) - a school-fees task was refused three times for "changing" a memo
    another run was writing, and could not "put it back" (Permission denied). The same happens whenever the person
    edits another document of their folder during a run."""

    data = manifest(task_id)
    if not data or not shared.active():
        return [], []
    named = _names(request)
    mine = given(request)
    out, resaved = [], []
    for rel, entry in data["files"].items():
        if may_change(Path(rel).name, request):
            continue
        if rel not in mine and Path(rel).name.lower() not in named:
            continue  # (not a source this request gives: never judged, never put back)
        now = _now(rel)
        if now is None:
            out.append(rel)
            continue
        local, info = now
        if info.st_size == entry["size"] and info.st_mtime_ns == entry["mtime"]:
            continue
        if entry.get("sha256") and info.st_size == entry["size"]:
            try:
                if _digest(local) == entry["sha256"]:
                    continue
            except OSError:
                pass
        if entry.get("copy"):
            before = _content(_folder(task_id) / "files" / entry["copy"])
            if before is not None and before == _content(local):
                resaved.append(rel)
                continue
        out.append(rel)
    return out, resaved


def changed(task_id, request):
    """The files that were in the shared folder before the task and now hold something else (or are gone), which the
    request did not ask to change: [rel]. A file an app only saved again, holding the same, is not one."""

    return _compare(task_id, request)[0]


def resaved(task_id, request):
    """Sources an app saved again with the same content: [rel] - a note at most."""

    return _compare(task_id, request)[1]


def before_copy(path, task_id=None):
    """The copy of a file of the shared folder as it was before the task began (take), or None when none was kept."""

    task_id = task_id or task_context.current()
    rel = _rel(path)
    found = manifest(task_id) if task_id and rel else None
    copy = ((found or {}).get("files") or {}).get(rel, {}).get("copy")
    if not copy:
        return None
    kept = _folder(task_id) / "files" / copy
    return kept if kept.is_file() else None


def kept_name(target):
    """Where the worker's version of a restored file is kept, beside it: "<name> (changed by worker)<ext>" - one fixed
    name, holding the worker's latest version, so the finding that names it reads the same on every attempt (a
    repeated finding becomes the person's question)."""

    target = Path(target)
    return target.with_name(f"{target.stem} (changed by worker){target.suffix}")


def restore(task_id, rels, kept=None):
    """
    Put the noted copies back over the changed files: (restored [rel], not restored [(rel, why)]). The worker's
    version is never discarded (independent review, 2026-10-21): it is kept beside the original, under kept_name, and
    `kept` (a dict, when given) gets {rel: the kept path} - the person decides which one stands.
    """

    folder = _folder(task_id) / "files"
    data = manifest(task_id) or {"files": {}}
    done, failed = [], []
    for rel in rels:
        entry = data["files"].get(rel) or {}
        if not entry.get("copy"):
            failed.append((rel, "no copy was kept (too large)"))
            continue
        try:
            target = Path(_root()) / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            if target.exists():
                beside = kept_name(target)
                shared.copy_file(target, beside)
                if kept is not None:
                    kept[rel] = str(beside)
            shared.copy_file(folder / entry["copy"], target)
            done.append(rel)
        except OSError as exc:
            failed.append((rel, f"it could not be written ({exc.strerror or exc}) - close it in the app that has it open"))
    return done, failed


def is_source(path, task_id, request):
    """Whether a file is one of the person's that was there before the task and is not to be changed: not an output."""

    data = manifest(task_id)
    rel = _rel(path)
    if not (data and rel and rel in data["files"]) or may_change(Path(rel).name, request):
        return False
    # The same files the guard puts back (_compare): those the request gives. A file of the folder it does not give,
    # that the work changed, is the work's - found live (2026-10-22): "bring the computation workbook
    # up to date" names it by what it is, so it was neither guarded nor checked, and the final check, shown none of it,
    # could not see the ARNs the work had written into it and asked three times.
    return rel in given(request) or Path(rel).name.lower() in _names(request)


def made_by_task(path, task_id=None, request=None):
    """
    Whether a file is one the current task made: created (or first written) after the task began, and neither a file
    the request brought nor one noted in the shared folder before it. False when the task's start is unknown - a file
    is then treated as the person's. Found live (round 5 P5, 2026-10-20): an in-place `office update` of the task's own
    workbook kept the wrong first version beside it as MIS_Q2_analysis.orig.xlsx.
    """

    from decurix_agent import task_context

    began = task_context.since()
    if began is None:
        return False
    task_id = task_id if task_id is not None else task_context.current()
    try:
        real = os.path.realpath(str(path))
        info = os.stat(real)
    except OSError:
        return False
    brought = {os.path.realpath(p) for p in task_context.current_files()}
    if request:
        brought |= {os.path.realpath(str(p)) for p in attachments.files(request)}
    if real in brought:
        return False
    rel = _rel(real)
    data = manifest(task_id) if task_id else None
    if rel and data and rel in data.get("files", {}):
        return False
    # Made during the task: created and written after the start (inode change time - creation on Windows - and
    # modification time both). A copy that kept an older time is taken for the person's: its backup is kept.
    return min(info.st_ctime, info.st_mtime) >= began


_BACKUP = re.compile(r"^(?P<stem>.+)\.orig(?P<ext>\.[A-Za-z0-9]+)$")


def remove_own_backups(folders, task_id=None, request=None, limit=5000):
    """
    At a task's end: remove each <name>.orig.<ext> kept for a file the task itself made (the backup made during the
    task, of content the task wrote, beside a file the task made). A backup of a file that was there before the task
    is never touched. Returns the paths removed.
    """

    from decurix_agent import task_context

    began = task_context.since()
    if began is None:
        return []
    removed, seen = [], 0
    for folder in folders:
        try:
            found = Path(folder).rglob("*.orig.*")
            for backup in found:
                seen += 1
                if seen > limit:
                    break
                match = _BACKUP.match(backup.name)
                if not match or not backup.is_file():
                    continue
                original = backup.with_name(match.group("stem") + match.group("ext"))
                if not original.is_file() or not made_by_task(original, task_id, request):
                    continue
                info = backup.stat()
                if info.st_ctime < began or info.st_mtime < began:
                    continue  # (a backup from before the task, or of content older than it: kept)
                backup.unlink()
                removed.append(str(backup))
        except OSError as exc:
            log(f"source_guard: could not tidy backups under {folder}: {exc}")
    if removed:
        log(f"source_guard: removed backups of the task's own files: {removed}")
    return removed


def tidy_own_backups(request=None):
    """remove_own_backups over the work folder and the shared folder (when there is one); never fails."""

    folders = [config.WORK_DIR]
    try:
        if shared.active():
            folders.append(shared.link_path())
    except Exception:  # noqa: BLE001
        pass
    try:
        return remove_own_backups(folders, request=request)
    except Exception as exc:  # noqa: BLE001 - tidying never fails a task
        log(f"source_guard: could not tidy backups: {exc}")
        return []


def check_and_restore(task_id, request):
    """
    The gate's fact: sources the task changed, each put back from its copy. Returns a plain finding ("" when none):
    "you changed my file X; it should have stayed as it was - I put the original back" (or why it could not be).
    """

    rels = changed(task_id, request)
    if not rels:
        return ""
    kept = {}
    done, failed = restore(task_id, rels, kept=kept)
    host = lambda path: shared.to_host(str(path)) or str(path)  # noqa: E731
    parts = [f"{CHANGED_SOURCE} {', '.join(host(Path(_root()) / r) for r in rels)}, which the request did not ask to "
             "change"]
    if done:
        parts.append(f"the original is back from the copy taken before the task ({', '.join(Path(r).name for r in done)})")
    for rel, beside in kept.items():
        parts.append(f"the worker's version of {Path(rel).name} is kept beside it as {host(beside)} - if that change is "
                     "what you wanted, use it (or say so)")
    for rel, why in failed:
        parts.append(f"{Path(rel).name} could not be put back: {why}")
    return "; ".join(parts) + "."


# How the finding of a changed source begins (a stable prefix: a repeated one becomes a question to the person).
CHANGED_SOURCE = "The task changed your file"


# ------------------------------------------------------------
# The fact, without a verdict (review 2026-11-03, E-12)
#
# check_and_restore decided from verbs (may_change) whether the request allowed a file to change, and put the original
# back inside the gate before anything judged the work: "Record today's payments in ledger.xlsx" or "Complete
# rates.xlsx" - the core of a CA firm's work - was reverted as "you changed my file". What code knows for certain is
# only the mechanical fact: this given file changed, and how. That fact goes to the judge and, always, to the person;
# the original stays kept, and it is put back only on a judge's structured verdict or the person's word.
# ------------------------------------------------------------

DIFF_CHARS = 600
DIFF_CELLS = 12


class NotChecked(OSError):
    """The source check could not be made (its notes or a changed file could not be read): not checked, never passed."""


def _strict_manifest(task_id):
    """The task's notes, None when none were taken (no shared folder, or a task begun before one): raises NotChecked
    when they exist and cannot be read."""

    path = _manifest_path(task_id)
    if not path.exists():
        return None
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise NotChecked(f"the notes of the files before the task could not be read: {exc}") from exc
    if not isinstance(data, dict) or not isinstance(data.get("files"), dict):
        raise NotChecked("the notes of the files before the task are not in their shape")
    return data


def _diff(before, after):
    """A short, plain description of how a file's content changed: cells for a workbook, lines for text."""

    import difflib

    if isinstance(before, list) and isinstance(after, list):
        old = {(s, c): v for s, cells in before for c, v in cells}
        new = {(s, c): v for s, cells in after for c, v in cells}
        moved = []
        for key in sorted(set(old) | set(new), key=lambda k: (str(k[0]), str(k[1]))):
            if old.get(key) != new.get(key):
                moved.append(f"{key[0]}!{key[1]}: {old.get(key, '(empty)')!r} -> {new.get(key, '(empty)')!r}")
        if not moved:
            return "the same cells (the file was saved again)"
        shown = "; ".join(moved[:DIFF_CELLS]) + (f"; and {len(moved) - DIFF_CELLS} more cells" if len(moved) > DIFF_CELLS else "")
        return f"{len(moved)} cell(s) changed: {shown}"
    if isinstance(before, str) and isinstance(after, str):
        lines = [line for line in difflib.unified_diff(before.splitlines(), after.splitlines(), lineterm="", n=0)
                 if line[:1] in "+-" and not line.startswith(("+++", "---"))]
        return ("lines changed: " + " | ".join(lines))[:DIFF_CHARS] if lines else "the same text (saved again)"
    return ""


_TEXT_KINDS = {".csv", ".tsv", ".txt", ".md", ".json", ".xml", ".html", ".htm"}


def _plain_text(path):
    """A text file's text (for its diff), or None for any other kind or one that cannot be read."""

    if Path(path).suffix.lower() not in _TEXT_KINDS:
        return None
    try:
        return Path(path).read_bytes()[:HASH_MAX].decode("utf-8", errors="replace")
    except OSError:
        return None


def changed_sources(task_id, request):
    """
    The given files of the request (attached, named, or in a folder it names) that were in the shared folder before the
    task and now hold something else, or are gone - whatever the request's words, which decide nothing here:
    [{"rel", "name", "path", "diff", "gone", "kept", "asked"}]. "asked" is may_change's reading (the request names
    the file near an edit verb): evidence for the judge, never a verdict. [] when there is no shared folder or nothing
    changed; raises NotChecked when the check cannot be made. Never restores anything.
    """

    if not shared.active():
        return []
    data = _strict_manifest(task_id)
    if data is None:
        return []
    named = _names(request)
    mine = given(request)
    out = []
    for rel, entry in data["files"].items():
        if rel not in mine and Path(rel).name.lower() not in named:
            continue  # (not a source this request gives: never judged)
        copy = _folder(task_id) / "files" / entry["copy"] if entry.get("copy") else None
        fact = {"rel": rel, "name": Path(rel).name, "path": shared.to_host(str(Path(_root()) / rel)) or rel,
                "gone": False, "kept": bool(copy and copy.is_file()), "asked": may_change(Path(rel).name, request),
                "diff": ""}
        now = _now(rel)
        if now is None:
            fact.update(gone=True, diff="the file is gone")
            out.append(fact)
            continue
        local, info = now
        if info.st_size == entry.get("size") and info.st_mtime_ns == entry.get("mtime"):
            continue
        try:
            if entry.get("sha256") and info.st_size == entry.get("size") and _digest(local) == entry["sha256"]:
                continue
        except OSError as exc:
            raise NotChecked(f"{Path(rel).name} could not be read to compare: {exc}") from exc
        if fact["kept"]:
            before, after = _content(copy) or _plain_text(copy), _content(local) or _plain_text(local)
            if before is not None and before == after:
                continue  # (saved again by an app, the same content: not a change)
            fact["diff"] = _diff(before, after)
        if not fact["diff"]:
            fact["diff"] = f"its bytes differ ({entry.get('size')} bytes before, {info.st_size} now)"
        out.append(fact)
    return out


def judge_lines(facts):
    """What the judge is told, one line per changed file: the fact checked in code, and the request's own wording as
    a pointer only."""

    lines = []
    for f in facts or ():
        line = f"Checked in code (not a claim): the request's file {f['path']} changed during the task - {f['diff']}."
        line += (" The request names this file next to an edit verb." if f.get("asked") else
                 " Whether the request asked for this file itself to change is for you to judge from its words.")
        lines.append(line)
    return lines


def report_lines(facts):
    """The person's line for each changed file, always shown with the result (under "Please check")."""

    out = []
    for f in facts or ():
        if f.get("kept"):
            out.append(f'Changed your file {f["path"]} ({f["diff"]}) - the original is kept; say "put back {f["name"]}" '
                       "and I will restore it.")
        else:
            out.append(f"Changed your file {f['path']} ({f['diff']}) - no copy of the original was kept (it was too "
                       "large), so it cannot be undone from here.")
    return out


def restore_sources(task_id, rels):
    """Put the originals back over the named changed files: (done [rel], failed [(rel, why)], kept {rel: the worker's
    version, kept beside it}). Only on a structured judge verdict that the change was not asked for, or the person's
    request - never as part of checking."""

    kept = {}
    done, failed = restore(task_id, list(rels or ()), kept=kept)
    return done, failed, kept
