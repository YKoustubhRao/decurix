import json
import os
import re
from pathlib import Path

from decurix_agent import config
from decurix_agent.logging_utils import log


# ============================================================
# The files a task made or changed
#
# A task that ends in a file - a workbook, a summary, a merged PDF - used to
# be reported on the worker's word alone: the final check looked only at
# tasks that acted on the screen. Found live: asked to compare two
# workbooks, a worker's report said two entries "were correctly matched"
# while the workbook it had just written listed both as unmatched; nothing
# compared the two, and the task was recorded done.
#
# So what a task wrote is read back and judged against its report
# (skill_task._gate -> verify.judge(produced=...)). What counts is decided
# with no model: a readable file in the work folder, or one the report names,
# last written after the task began.
# ============================================================

READABLE = {".xlsx", ".xlsm", ".docx", ".pdf", ".csv", ".tsv", ".txt", ".md", ".json", ".xml", ".html", ".htm", ".eml"}

MAX_FILES = 4
MAX_FILE_CHARS = 7000
MAX_DEPTH = 3
MAX_ENTRIES = 4000

# Folders that hold tools' own files, never a task's result.
SKIP_DIRS = {"node_modules", "__pycache__", "venv", "site-packages", "target", "dist", "build"}

NAMED_PATH = re.compile(r"(?:~|/|\b[a-z]:[\\/])[\w.~/\\-]*[\w-]\.[a-z0-9]{2,5}\b", re.I)


def since():
    """When the task being worked on began (seconds), or None when that is not known."""

    try:
        record = json.loads((config.DECURIX_DIR / "active-task.json").read_text(encoding="utf-8"))
        return float(record["since"])
    except (OSError, ValueError, KeyError, TypeError):
        return None


def _walk(root):
    """Readable files under root, a few levels deep, hidden and tool folders left out."""

    seen = 0
    stack = [(Path(root), 0)]
    while stack:
        folder, depth = stack.pop()
        try:
            children = list(os.scandir(folder))
        except OSError:
            continue
        for child in children:
            seen += 1
            if seen > MAX_ENTRIES:
                return
            if child.name.startswith("."):
                continue
            try:
                if child.is_dir(follow_symlinks=False):
                    if depth < MAX_DEPTH and child.name not in SKIP_DIRS:
                        stack.append((Path(child.path), depth + 1))
                elif child.is_file(follow_symlinks=False):
                    yield Path(child.path)
            except OSError:
                continue


def files(report="", started=None, root=None):
    """
    The readable files this task wrote, newest first, at most MAX_FILES:
    those in the work folder and those the report names, last written after
    the task began. Empty when the task's start is not known - never a guess
    from the files that happen to be there.
    """

    started = since() if started is None else started
    if started is None:
        return []

    candidates = set(_walk(root or config.WORK_DIR))
    for named in NAMED_PATH.findall(report or ""):
        candidates.add(Path(os.path.expanduser(named)))

    made = []
    for path in candidates:
        if path.suffix.lower() not in READABLE:
            continue
        try:
            stat = path.stat()
        except OSError:
            continue
        if stat.st_mtime > started and stat.st_size > 0:
            made.append((stat.st_mtime, str(path)))

    return [path for _, path in sorted(made, reverse=True)[:MAX_FILES]]


def drafts():
    """The emails this task drafted into the Outbox (tools/outbox.py keeps a note of each)."""

    from decurix_agent.tools import outbox

    try:
        return outbox.drafted()
    except Exception as exc:
        log(f"produced: could not read this task's drafts: {exc}")
        return []


def drafts_text(made=None):
    """Those drafts as the check reads them, or ""."""

    parts = []
    for n, draft in enumerate(drafts() if made is None else made, 1):
        body = str(draft.get("body") or "")
        if len(body) > MAX_FILE_CHARS:
            body = body[:MAX_FILE_CHARS] + "\n[cut here]"
        parts.append(f"--- email draft {n} (in the Outbox, not sent)\nTo: {', '.join(draft.get('to') or [])}\n"
                     f"Cc: {', '.join(draft.get('cc') or []) or '-'}\nSubject: {draft.get('subject') or ''}\n"
                     f"Attached: {', '.join(draft.get('attachments') or []) or 'nothing'}\n\n{body}")
    return "\n".join(parts)


def text(paths):
    """What those files say now, read the way the worker's own tools read them; unreadable ones say so."""

    from decurix_agent.tools import office

    parts = []
    for path in paths:
        try:
            body = (office.read_document(path).get("text") or "").strip()
        except Exception as exc:  # a file the reader cannot open is reported as such, not left out
            log(f"produced: could not read {path}: {exc}")
            parts.append(f"--- {path}\n(could not be read: {str(exc)[:200]})")
            continue
        if len(body) > MAX_FILE_CHARS:
            body = body[:MAX_FILE_CHARS] + f"\n[cut here: {len(body) - MAX_FILE_CHARS} more characters not shown]"
        parts.append(f"--- {path}\n{body or '(empty)'}")
    return "\n".join(parts)
