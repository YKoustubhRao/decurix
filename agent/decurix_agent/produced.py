import json
import os
import re
from pathlib import Path

from decurix_agent import attachments, config
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

READABLE = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv", ".tsv", ".txt", ".md", ".json", ".xml", ".html", ".htm", ".eml"}

MAX_FILES = 4
# What a person asked for, ahead of what a worker made on the way when only MAX_FILES are read. Found live
# (2026-10-14): JSON specs and notes written after a workbook pushed it out of the four newest.
DELIVERABLES = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv"}
MAX_FILE_CHARS = 7000
MAX_DEPTH = 3
MAX_ENTRIES = 4000

# Folders that hold tools' own files, never a task's result.
SKIP_DIRS = {"node_modules", "__pycache__", "venv", "site-packages", "target", "dist", "build"}

# A file a request asks for by name ("make contract_changes.docx in my work folder").
REQUESTED_FILE = re.compile(r"\b[\w-][\w.-]*\.(?:xlsx|xlsm|docx|pptx|pdf|csv|tsv|txt|md|json|xml|html?|eml)\b", re.I)
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


def files(report="", started=None, root=None, request=""):
    """
    The readable files this task wrote, newest first, at most MAX_FILES:
    those in the work folder and those the report names, last written after
    the task began. Empty when the task's start is not known - never a guess
    from the files that happen to be there.

    When the request names the files it wants (contract_changes.docx), those
    and the ones the report names are the result; a working file the worker
    made on the way - a JSON spec it fed a tool, a script - is not judged as
    one. Found live (2026-10-13): a correct document was refused because an
    earlier draft of its spec, still in the work folder, "conflicted" with it.
    """

    wanted = {name.lower() for name in REQUESTED_FILE.findall(attachments.text(request or ""))}
    named = {Path(os.path.expanduser(p)).name.lower() for p in NAMED_PATH.findall(report or "")}

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
        if wanted and path.name.lower() not in wanted and path.name.lower() not in named:
            continue
        if stat.st_mtime > started and stat.st_size > 0:
            made.append((path.suffix.lower() in DELIVERABLES or path.name.lower() in named, stat.st_mtime, str(path)))

    return [path for _, _, path in sorted(made, reverse=True)[:MAX_FILES]]


WORK_FOLDER = re.compile(r"\b(?:in|to|into)\s+(?:my|the|our)\s+work\s+folder\b", re.I)


def misplaced(request, root=None):
    """
    Where a file the request said to put in the work folder ended up instead,
    or "": a file it names that exists under an inbox folder (an attachment
    saved over in place) and not in the work folder itself. Decided with no
    model. Found live (2026-10-13): asked to "save it as grades.xlsx in my work
    folder", a worker pressed Ctrl+S on the attachment - twice - and reported
    "saved in the work folder"; the check took the report's word for it.
    """

    text = attachments.text(request or "")
    if not WORK_FOLDER.search(text):
        return ""
    root = Path(root or config.WORK_DIR)
    began = since()
    out = []
    # Only a file named in a sentence that says where it goes: "update last_invoice_number.txt when done" in the same
    # request as "save the invoices in my work folder" asks for the attachment itself to be updated (found live).
    placed = " ".join(s for s in re.split(r"(?<=[.!?])\s+|\n+", text) if WORK_FOLDER.search(s))
    for name in dict.fromkeys(REQUESTED_FILE.findall(placed)):
        if (root / name).exists():
            continue
        elsewhere = sorted((root / "inbox").glob(f"*/{name}")) if (root / "inbox").is_dir() else []
        # An attachment this task never wrote is one of its inputs ("... already in paid.csv"), not a
        # result saved in the wrong place. Found live (2026-10-13): a correct register was refused for
        # want of the paid list the request had only named.
        if began is not None:
            elsewhere = [p for p in elsewhere if _written_since(p, began)]
        if elsewhere:
            out.append(f"{name} is not in the work folder ({root}); it was saved at {elsewhere[-1]}")
    return "; ".join(out)


def _written_since(path, began):
    """Whether a file was last written after the task began (a second's slack for the copy that put it there)."""

    try:
        return path.stat().st_mtime > began + 1
    except OSError:
        return False


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
