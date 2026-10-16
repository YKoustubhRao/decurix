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

# Pictures are described (format and size in pixels), not read: found live (2026-10-15), product photos resized
# and exported in GIMP were "not shown by the screen", and the check could not confirm a correct task.
IMAGES = {".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff"}
READABLE = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv", ".tsv", ".txt", ".md", ".json", ".xml", ".html", ".htm", ".eml",
            ".ods", ".odt", ".odp", ".svg"} | IMAGES

MAX_FILES = 4
# What a person asked for, ahead of what a worker made on the way when only MAX_FILES are read. Found live
# (2026-10-14): JSON specs and notes written after a workbook pushed it out of the four newest.
DELIVERABLES = {".xlsx", ".xlsm", ".docx", ".pptx", ".pdf", ".csv", ".ods", ".odt", ".odp", ".svg"} | IMAGES
MAX_FILE_CHARS = 7000
MAX_DEPTH = 3
MAX_ENTRIES = 4000

# Folders that hold tools' own files, never a task's result.
SKIP_DIRS = {"node_modules", "__pycache__", "venv", "site-packages", "target", "dist", "build"}

# A file a request asks for by name ("make contract_changes.docx in my work folder").
REQUESTED_FILE = re.compile(r"\b[\w-][\w.-]*\.(?:xlsx|xlsm|docx|pptx|pdf|csv|tsv|txt|md|json|xml|html?|eml|ods|odt|odp|svg|png|jpe?g|gif|webp|bmp|tiff?)\b", re.I)
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


def _picture(path):
    """A picture as the check needs it: its format and size in pixels."""

    try:
        from PIL import Image
        with Image.open(path) as im:
            return f"a {im.format} image, {im.size[0]} x {im.size[1]} pixels, {Path(path).stat().st_size} bytes"
    except Exception as exc:
        return f"(a picture that could not be opened: {str(exc)[:120]})"


def text(paths):
    """What those files say now, read the way the worker's own tools read them; unreadable ones say so."""

    from decurix_agent.tools import office

    parts = []
    for path in paths:
        if Path(path).suffix.lower() in IMAGES:
            parts.append(f"--- {path}\n{_picture(path)}")
            continue
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


# ============================================================
# A file a task was asked to save on the person's computer
#
# A host app (Excel, Word, PowerPoint on the person's computer) saves to
# C:\..., which this desktop cannot see, so the check above never read the
# result. Found live (2026-10-16): asked to save sales_q3_report.xlsx, Excel
# kept the file's CSV type and saved sales_q3_report.xlsx.csv - and the
# screen, where Excel hides the extension, showed the name asked for; the task
# was reported done. The control plane can see that disk (its host mount) and
# answers, for a path the desktop's own open request names, whether the file
# is there - never its content. Decided with no model.
# ============================================================

# A folder or file on the host's C: drive as a request writes it; a folder may hold spaces, so it runs to a quote,
# a bracket or a line's end, and is then cut at a sentence's end.
HOST_PATH = re.compile(r"\b[cC]:[\\/][^\n\"'()<>|*?]*")
SAVE_SENTENCE = re.compile(r"\b(?:save[ds]?|saving|export(?:s|ed)?|store|named|call(?:ed)?)\b", re.I)
WRITTEN_SLACK = 5


def _host_paths(text):
    """The C:\\ paths a request names, each cut at a sentence's end."""

    out = []
    for match in HOST_PATH.finditer(text):
        found = re.split(r"[.,;:](?:\s|$)", match.group(0))[0].rstrip(" .,;:\\/")
        if len(found) > 3:
            out.append(found)
    return list(dict.fromkeys(out))


def _leaf(path):
    return path.replace("\\", "/").rsplit("/", 1)[-1]


def _ask_host(path):
    """What the control plane says of a host path: a dict, or None when it could not say."""

    import urllib.parse
    import urllib.request

    url = (config.CONTROL_PLANE_URL.rstrip("/") + "/api/host-files/stat?"
           + urllib.parse.urlencode({"desktop": config.DESKTOP_ID, "path": path}))
    try:
        with urllib.request.urlopen(url, timeout=10) as answer:
            found = json.loads(answer.read().decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - not knowing is never a verdict
        log(f"produced: could not ask about {path}: {exc}")
        return None
    return None if not isinstance(found, dict) or found.get("unknown") else found


def missing_on_host(request, ask=None):
    """
    What of the files a request said to save on the person's computer is not
    there, or "": a file named in a sentence that saves something, looked for
    in the C:\\ folders the request names that do exist (or at the whole path
    the request gives it). A folder that is not there, or a control plane that
    cannot be asked, decides nothing.
    """

    ask = ask or _ask_host
    text = attachments.text(request or "")
    paths = _host_paths(text)
    if not paths:
        return ""
    saved = " ".join(s for s in re.split(r"(?<=[.!?])\s+|\n+", text) if SAVE_SENTENCE.search(s))
    wanted = list(dict.fromkeys(REQUESTED_FILE.findall(saved)))
    if not wanted:
        return ""
    whole = [p for p in paths if REQUESTED_FILE.fullmatch(_leaf(p))]
    folders = []
    for path in paths:
        if path in whole:
            continue
        found = ask(path)
        if found is None:
            return ""
        if found.get("exists") and found.get("folder"):
            folders.append(path)
    began = since()
    out = []
    for name in wanted:
        places = [p for p in whole if _leaf(p).lower() == name.lower()] + [f + "\\" + name for f in folders]
        if not places:
            continue
        there = []
        for place in places:
            found = ask(place)
            if found is None:
                return ""
            if found.get("exists") and not found.get("folder"):
                there.append((place, found))
        if not there:
            folder = places[0].rsplit("\\", 1)[0]
            out.append(f"{name} is not in {folder} on the person's computer")
        elif any(_wrong_kind(name, found) for _, found in there):
            kind = next(found.get("kind") for _, found in there if _wrong_kind(name, found))
            out.append(f"{there[0][0]} is there, but it is not a real {name.rsplit('.', 1)[-1]} file (its bytes are "
                       f"{KIND_WORDS.get(kind, kind)}) - it was saved with another file type")
        elif began is not None and all(_older(found, began) for _, found in there):
            out.append(f"{there[0][0]} is there but was last written before this task began - it was not saved now")
    return "; ".join(out)


# What a file's name says its first bytes must be (the control plane reads eight of them, never more).
KIND_OF = {".xlsx": "zip", ".xlsm": "zip", ".docx": "zip", ".pptx": "zip", ".ods": "zip", ".odt": "zip",
           ".odp": "zip", ".pdf": "pdf"}
KIND_WORDS = {"text": "plain text, as a CSV or a text file is", "zip": "a zip package", "pdf": "a PDF",
              "ole": "an old Office file", "other": "something else"}


def _wrong_kind(name, found):
    """Whether a host file's bytes are not what its extension says (a CSV saved under a workbook's name)."""

    want = KIND_OF.get(os.path.splitext(name)[1].lower())
    kind = found.get("kind")
    return bool(want and kind and kind != want)


def _older(found, began):
    """Whether a host file the control plane described was last written before the task began."""

    from datetime import datetime

    try:
        written = datetime.fromisoformat(str(found.get("modified", "")).replace("Z", "+00:00")).timestamp()
    except ValueError:
        return False
    return written < began - WRITTEN_SLACK
