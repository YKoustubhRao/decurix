"""
Files a person attached to a request, or to a ticket they are recording.

The control plane writes them into this desktop's ~/work/inbox/<id>/ before
it hands the request over, and names them at the end of the request, one path
a line under MARK (httpapi/attachments.go's attachmentsMark builds the same
block: change one and the other's test says so). The paths are in the
request's own text on purpose - that is where every engine, the check of a
finished task (verify.py) and a taught skill filling its values (binding.py)
already look for a run's documents.

What needs the request *without* them is anything that compares its words: a
ticket's title, a recorded task matched against a new request (mimic.py,
request_reading.py), a question for a web app (ask_task.py).
"""

import os
import re
import shutil
import zipfile
import zlib

from decurix_agent.logging_utils import log

MARK = "Files attached to this request, saved on this desktop:"

# How many of a lesson's documents the model is shown, and how much of each.
MAX_DOCUMENTS = 4
MAX_DOCUMENT_CHARS = 6000
MAX_DOCUMENT_PICTURES = 4


def split(content):
    """(the request as the person wrote it, [paths of the files it brought])."""

    text, found, rest = (content or "").partition(MARK)
    if not found:
        return (content or ""), []

    return text.rstrip(), [line.strip() for line in rest.splitlines() if line.strip()]


def text(content):
    """A request without the files it brought."""

    return split(content)[0]


def files(content):
    """The paths of the files a request brought."""

    return split(content)[1]


def block(paths):
    """The lines that name `paths` in a request ("" for none)."""

    if not paths:
        return ""

    return MARK + "".join(f"\n  {path}" for path in paths)


def names(paths):
    """The files' own names, for a line a person reads."""

    return ", ".join(os.path.basename(str(path).replace("\\", "/")) for path in paths)


def parts(paths, heading):
    """
    The documents at `paths` as content parts for a hosted model: each one's
    text, and the pages of a scan as pictures - what a person working from
    them had in front of them. Best-effort: a file that is gone or cannot be
    read is said to be so, never left out silently.
    """

    # Imported here: office.py pulls in the document tools, and this module is
    # read by the watcher for every request.
    from decurix_agent import llm_client
    from decurix_agent.tools import office

    out, pictures = [], 0

    for path in list(paths or [])[:MAX_DOCUMENTS]:
        if not os.path.isfile(path):
            out.append({"type": "text", "text": f"\n[{path}: no longer on this desktop]"})
            continue

        try:
            doc = office.read_document(path)
        except (office.OfficeError, OSError, ValueError) as exc:
            log(f"attachments: could not read {path}: {exc}")
            out.append({"type": "text", "text": f"\n[{path}: could not be read - {exc}]"})
            continue

        out.append({"type": "text", "text": f"\n--- Document {path} ---\n{(doc.get('text') or '')[:MAX_DOCUMENT_CHARS]}"})
        for image in doc.get("images") or []:
            if pictures == MAX_DOCUMENT_PICTURES:
                break
            out.append(llm_client.image_part(path=image))
            pictures += 1

    if out:
        out.insert(0, {"type": "text", "text": heading})

    return out


# A copy of each attached file as it arrived, per task. A task may change an attachment in place ("update
# last_invoice_number.txt when done"), and the check of its result then read the changed file as what the
# person had sent: found live (2026-10-13), invoices correctly numbered on from INV-2026-0087 were called
# wrong because the counter they had already advanced to 0090 was taken for the starting number.
ORIGINALS_KEPT = 40
MAX_ORIGINAL_BYTES = 30 * 1024 * 1024


def _originals_dir(task_id):
    from decurix_agent import config
    return config.DECURIX_DIR / "originals" / task_id


# A file's place inside the folder one upload was written to ("close/delhi/branch.xlsx"), else its own name: the same
# name in two folders of an archive must keep two originals.
_INBOX_PLACE = re.compile(r"^.*/inbox/[^/]+/(.+)$")


def _key(path):
    p = str(path).replace("\\", "/")
    found = _INBOX_PLACE.match(p)
    return found.group(1) if found else os.path.basename(p)


def keep_originals(content, task_id):
    """Keep a copy of every file a request brought, as it arrived. Best-effort; never raises."""

    if not task_id:
        return
    try:
        folder = _originals_dir(task_id)
        for path in files(content):
            if os.path.isfile(path) and os.path.getsize(path) <= MAX_ORIGINAL_BYTES:
                target = folder / _key(path)
                target.parent.mkdir(parents=True, exist_ok=True)
                if not target.exists():
                    shutil.copy2(path, target)
        kept = sorted((p for p in folder.parent.iterdir() if p.is_dir()), key=lambda p: p.stat().st_mtime, reverse=True)
        for old in kept[ORIGINALS_KEPT:]:
            shutil.rmtree(old, ignore_errors=True)
    except OSError as exc:
        log(f"attachments: could not keep the originals: {exc}")


def original(path, task_id=None):
    """The copy of `path` as the person sent it with this task, when the file has changed since; else `path`."""

    from decurix_agent import task_context
    task_id = task_id or task_context.current()
    if not task_id:
        return path
    copy = _originals_dir(task_id) / _key(path)
    try:
        if copy.is_file() and os.path.isfile(path):
            with open(copy, "rb") as a, open(path, "rb") as b:
                if a.read() != b.read():
                    return str(copy)
    except OSError:
        pass
    return path


# ============================================================
# Archives: a folder of files sent as one .zip
#
# The control plane takes at most 10 files with a request (httpapi's maxAttachments: every file is named in the
# request's own text, one path a line, and one upload is capped at 64 MB). A month-end close has 19 files, and a
# person with a folder of them zips it. A .zip attached to a request is unpacked here, into a folder of its own beside
# it with its folders kept, and the request then names the files it holds - one path a line, as if each had been
# attached - so every engine, the final check and the grounding check read them as they read any attachment.
# Nothing in the archive is trusted: a path that leaves the folder, a link, a password, or more than the caps refuses
# the whole archive (said in the chat), never half of it.
# ============================================================

MAX_ARCHIVE_FILES = 200
MAX_ARCHIVE_MEMBER_BYTES = 100 * 1024 * 1024
MAX_ARCHIVE_BYTES = 300 * 1024 * 1024
# What a zip made on a Mac or Windows carries that is not the person's.
_JUNK = re.compile(r"(?:^|/)(?:__MACOSX|\.DS_Store|Thumbs\.db|desktop\.ini)(?:/|$)|(?:^|/)\._", re.I)
_UNSAFE_NAME = re.compile(r"[^A-Za-z0-9_.\-]+")


class ArchiveError(Exception):
    """An archive that is not unpacked, with why (in words a person can act on)."""


def _safe_member(name):
    """
    A member's path inside the archive folder as it is saved - segments made of letters, digits, _ . - only (what
    the readers of a request's paths stop at) - or raise ArchiveError for one that leaves the folder.
    """

    raw = (name or "").replace("\\", "/")
    if raw.startswith("/") or re.match(r"^[A-Za-z]:", raw):
        raise ArchiveError(f"{name!r} is an absolute path")
    out = []
    for segment in raw.split("/"):
        if segment in ("", "."):
            continue
        if segment == "..":
            raise ArchiveError(f"{name!r} leaves the folder it is unpacked into")
        stem, dot, ext = segment.rpartition(".")
        if dot and stem:
            clean = (_UNSAFE_NAME.sub("_", stem).strip("_.") or "file") + "." + _UNSAFE_NAME.sub("", ext)
        else:
            clean = _UNSAFE_NAME.sub("_", segment).strip("_.") or "file"
        out.append(clean)
    if not out:
        raise ArchiveError(f"{name!r} has no name")
    return "/".join(out)


def unpack_archive(archive, into=None):
    """
    Unpack a .zip into a folder of its own beside it (named for it), keeping its folders. Returns the paths of the
    files it held, sorted. Raises ArchiveError when it should not be unpacked at all: a password, a link, a path that
    leaves the folder, more files or bytes than the caps, or a file that is not a zip.
    """

    archive = os.path.abspath(str(archive))
    if not zipfile.is_zipfile(archive):
        raise ArchiveError("it is not a readable .zip file")
    stem = _UNSAFE_NAME.sub("_", os.path.splitext(os.path.basename(archive))[0]).strip("_.") or "archive"
    folder = into or os.path.join(os.path.dirname(archive), stem)
    try:
        z = zipfile.ZipFile(archive)
    except (zipfile.BadZipFile, OSError) as exc:
        raise ArchiveError(f"it could not be opened ({exc})") from exc
    with z:
        members = [i for i in z.infolist() if not i.is_dir() and not _JUNK.search(i.filename.replace("\\", "/"))]
        if not members:
            raise ArchiveError("it holds no files")
        if len(members) > MAX_ARCHIVE_FILES:
            raise ArchiveError(f"it holds {len(members)} files; at most {MAX_ARCHIVE_FILES} are unpacked")
        if sum(i.file_size for i in members) > MAX_ARCHIVE_BYTES:
            raise ArchiveError(f"it unpacks to more than {MAX_ARCHIVE_BYTES >> 20} MB")
        planned, taken = [], {}
        for info in members:
            if info.flag_bits & 0x1:
                raise ArchiveError("it is password protected")
            if (info.external_attr >> 16) & 0o170000 == 0o120000:
                raise ArchiveError(f"{info.filename!r} is a link")
            if info.file_size > MAX_ARCHIVE_MEMBER_BYTES:
                raise ArchiveError(f"{info.filename!r} is larger than {MAX_ARCHIVE_MEMBER_BYTES >> 20} MB")
            rel = _safe_member(info.filename)
            n = taken.get(rel.lower(), 0) + 1
            taken[rel.lower()] = n
            if n > 1:
                base, ext = os.path.splitext(rel)
                rel = f"{base}_{n}{ext}"
            planned.append((info, rel))
        try:
            return _extract(z, planned, folder)
        except (ArchiveError, zipfile.BadZipFile, zlib.error, OSError, RuntimeError, NotImplementedError, EOFError) as exc:
            # Whatever went wrong part way, nothing of the archive is left behind: all of it or none.
            shutil.rmtree(folder, ignore_errors=True)
            if isinstance(exc, ArchiveError):
                raise
            raise ArchiveError(f"it is damaged or cannot be read here ({exc})") from exc


def _extract(z, planned, folder):
    """Write the planned members under `folder`, counting the bytes that come out; the caps hold as it goes."""

    out, written = [], 0
    root = os.path.abspath(folder)
    for info, rel in planned:
        target = os.path.join(folder, *rel.split("/"))
        if os.path.commonpath([os.path.abspath(target), root]) != root:
            raise ArchiveError(f"{info.filename!r} leaves the folder it is unpacked into")
        os.makedirs(os.path.dirname(target), exist_ok=True)
        size = 0
        # Read by the bytes that come out, not by the size the header claims: a header can lie.
        with z.open(info) as src, open(target, "wb") as dst:
            while True:
                chunk = src.read(1024 * 1024)
                if not chunk:
                    break
                size += len(chunk)
                written += len(chunk)
                if size > MAX_ARCHIVE_MEMBER_BYTES or written > MAX_ARCHIVE_BYTES:
                    raise ArchiveError("it unpacks to more than it said it would")
                dst.write(chunk)
        out.append(target.replace("\\", "/"))
    return sorted(out)


def expand_archives(content):
    """
    The request with each .zip it brought replaced, in the block of files, by the files it holds (see above). Returns
    (the request, [what happened, one sentence a person reads, for each archive]). A request with no archive comes
    back as it was; an archive that cannot be unpacked stays listed as the file it is, and says why.
    """

    request, paths = split(content)
    if not any(str(p).lower().endswith(".zip") for p in paths):
        return content, []
    notes, listed = [], []
    for path in paths:
        if not str(path).lower().endswith(".zip"):
            listed.append(path)
            continue
        name = os.path.basename(str(path))
        try:
            made = unpack_archive(path)
        except (ArchiveError, OSError) as exc:
            log(f"attachments: {name} was not unpacked: {exc}")
            notes.append(f"{name} was not unpacked: {exc}.")
            listed.append(path)
            continue
        log(f"attachments: unpacked {name}: {len(made)} file(s)")
        notes.append(f"Unpacked {name}: {len(made)} file(s), in their folders.")
        listed.extend(made)
    return request + "\n\n" + block(listed), notes
