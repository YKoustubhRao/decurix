"""
The shared folder: the one folder of the person's computer this worker may read and write.

The control plane mounts it when the worker is made (internal/desktop/sharedfolder.go: a read-write bind at
/mnt/pc, outside $HOME because the base image's startup runs `chown -R $UID $HOME`), and tells the agent which folder
it is, as the person writes it, in DECURIX_SHARED_FOLDER. This module makes it reachable at ~/PC and answers the
questions every route asks: is a path the person wrote under that folder, and where is it here?

    C:\\Users\\me\\Documents\\Decurix demo\\x.xlsx   (any case, / or \\, or /mnt/c/...)   ->   /home/headless/PC/x.xlsx

Nothing else of the person's computer is here, and nothing is reached by a link: a path is used only when its real
location is inside the mount (a link in the folder cannot lead the worker out of it - it resolves in this container -
but the check is made rather than assumed). Stdlib only: office.py imports it.
"""

import os
import re
from datetime import datetime, timezone
from pathlib import Path

ENV_FOLDER = "DECURIX_SHARED_FOLDER"
ENV_MOUNT = "DECURIX_SHARED_MOUNT"
ENV_LINK = "DECURIX_SHARED_LINK"  # where ~/PC is, for tests

# How much of a shared folder is looked through for files a task made or was given.
MAX_ENTRIES = 4000
# A person's folders run deep: "GST Returns/<client>/2026-27/06. SEPTEMBER/invoice.pdf" is four levels down. At 3, every
# file of a month folder was invisible to the final check (a workbook just made there was "not made") and to the copy of
# the person's files taken before a task (GST suite T6, 2026-10-21). MAX_ENTRIES is what bounds a big folder.
MAX_DEPTH = 8

_WINDOWS = re.compile(r"^[A-Za-z]:(?:/|$)")
_WSL_DRIVE = re.compile(r"^/mnt/([A-Za-z])(?=/|$)")


def folder():
    """The shared folder as the person writes it, or "" when this worker has none."""

    return os.environ.get(ENV_FOLDER, "").strip()


def mount_path():
    """Where the shared folder is mounted here, or None."""

    if not folder():
        return None
    return Path(os.environ.get(ENV_MOUNT, "").strip() or "/mnt/pc")


def link_path():
    """Where the worker and the person's requests see it: ~/PC."""

    if os.environ.get(ENV_LINK):
        return Path(os.environ[ENV_LINK])
    base = Path("/home/headless") if Path("/home/headless").exists() else Path.home()
    return base / "PC"


def active():
    """Whether this worker has a shared folder that is there."""

    mount = mount_path()
    return bool(mount and mount.is_dir())


# The name, in the work folder, of a link to the shared folder: a planned run's output in the person's folder is
# "PC/<path in it>", so every check that reads the work folder reads the person's file (long_task.spec_from_plan).
WORK_LINK = "PC"


def ensure_work_link(work_dir=None):
    """Make <work folder>/PC lead to the mount (a real folder or file called PC is never touched)."""

    from decurix_agent import config

    return _make_link(Path(work_dir or config.WORK_DIR) / WORK_LINK,
                      "removed a work-folder link to a shared folder this worker no longer has", make_parent=True)


def plan_path(path):
    """
    "PC/<path in the shared folder>" for a path a planner wrote that is in the person's shared folder - the folder as
    the person writes it (C:\\...\\Decurix demo\\x.pptx), ~/PC/x.pptx, or the folder's own name and the folders above
    it written as a relative path ("Documents/Decurix demo/process/P1/x.pptx") - or None. Found live (2026-10-20, P1):
    that relative path was taken as a folder of the work folder; PowerPoint saved the person's file where asked, and
    the stage's check read a stale copy in the work folder.
    """

    if not active():
        return None
    text = str(path or "").strip().strip("\"'")
    local = to_local(text)
    if local is None:
        n = _norm(text)
        if n.startswith("~/PC/") or n.startswith(_norm(link_path().as_posix()) + "/"):
            local = to_local(n.replace("~/PC", link_path().as_posix(), 1))
    if local is not None:
        rel = os.path.relpath(local, link_path()).replace(os.sep, "/")
        return None if rel.startswith("..") or rel == "." else f"{WORK_LINK}/{rel}"
    # A relative path that starts with the shared folder's own name, or the folders above it ending in it.
    parts = [p for p in _norm(text).split("/") if p]
    host = [p for p in _norm(folder()).split("/") if p and not p.endswith(":")]
    for k in range(min(len(host), len(parts) - 1), 0, -1):
        if [p.casefold() for p in parts[:k]] == [h.casefold() for h in host[-k:]]:
            rest = "/".join(parts[k:])
            return f"{WORK_LINK}/{rest}" if rest and ".." not in parts[k:] else None
    return None


def copy_file(src, dst):
    """
    Copy a file's contents to `dst`, then its times and permission bits where the filesystem allows. shutil.copy2 does
    both and fails on the person's shared folder: every file of a Windows drive mount shows as owned by root, so setting
    its mode as this desktop's user is refused (EPERM) - after the contents were written. Found live (GST suite T4,
    2026-10-21): the first `office update` of a person's workbook failed "Operation not permitted" while keeping its
    .orig backup, the engine worked in a copy instead, and the restore of a person's file would have failed the same way.
    """

    import shutil

    shutil.copyfile(src, dst)
    try:
        shutil.copystat(src, dst)
    except OSError:
        pass
    return dst


def ensure_link():
    """
    Make ~/PC lead to the mount (the agent does this when it starts), and take a link that leads nowhere any more away
    (a worker made again without a shared folder). A real folder or file called PC is never touched. Returns what it
    did, for the log.
    """

    return _make_link(link_path(), "removed a link to a shared folder this worker no longer has")


def _make_link(link, removed, make_parent=False):
    """Make `link` lead to the mount, or take away a link when there is no mount (`removed` says so)."""

    mount = mount_path()
    try:
        if mount and mount.is_dir():
            if link.is_symlink():
                if os.path.realpath(link) == os.path.realpath(mount):
                    return "linked"
                link.unlink()
            elif link.exists():
                return f"{link} exists and is not a link: left as it is"
            if make_parent:
                link.parent.mkdir(parents=True, exist_ok=True)
            link.symlink_to(mount, target_is_directory=True)
            return f"linked {link} to {mount}"
        if link.is_symlink():
            link.unlink()
            return removed
    except OSError as exc:
        return f"could not link {link}: {exc}"
    return ""


def _norm(text):
    """A path as one comparable spelling: / only, no repeats, no trailing /, /mnt/c/... as C:/..."""

    t = re.sub(r"/{2,}", "/", str(text or "").strip().strip("\"'").replace("\\", "/"))
    wsl = _WSL_DRIVE.match(t)
    if wsl:
        t = wsl.group(1).upper() + ":" + t[wsl.end():]
    if len(t) > 3 and t.endswith("/"):
        t = t.rstrip("/")
    return t


def contained(path):
    """Whether a path's real location is inside the shared folder's mount (a link out of it is not)."""

    mount = mount_path()
    if not mount:
        return False
    real, root = os.path.realpath(str(path)), os.path.realpath(str(mount))
    return real == root or real.startswith(root.rstrip(os.sep) + os.sep)


def to_local(path):
    """
    Where a path the person wrote under their shared folder is on this desktop (under ~/PC), or None when it is not
    under it, this worker has none, or the real location would be outside the mount. Windows paths are matched
    without regard to case and with either slash; the WSL spelling /mnt/c/... is the same folder as C:\\...; a path
    already under ~/PC is accepted as it is. A path that goes up a folder is never mapped.
    """

    host = _norm(folder())
    if not host or not mount_path():
        return None
    text = str(path or "").strip().strip("\"'")
    if not text:
        return None
    link = link_path()
    as_link = _norm(link.as_posix())
    n = _norm(text)
    if n == as_link or n.startswith(as_link + "/"):
        rel = n[len(as_link):].lstrip("/")
    else:
        fold = (lambda s: s.casefold()) if _WINDOWS.match(host) else (lambda s: s)
        if fold(n) == fold(host):
            rel = ""
        elif fold(n).startswith(fold(host) + "/"):
            rel = n[len(host) + 1:]
        else:
            return None
    if any(part == ".." for part in rel.split("/")):
        return None
    local = link / rel if rel else link
    return str(local) if contained(local) else None


def to_host(path):
    """
    How the person writes a path of this desktop that is in their shared folder (/home/headless/PC/x.docx ->
    C:\\Users\\...\\Decurix demo\\x.docx), or None when it is not in it. The other way round from to_local.
    """

    host = folder()
    mount = mount_path()
    if not host or not mount or not path:
        return None
    text = str(path).replace("\\", "/")
    for root in (link_path().as_posix(), mount.as_posix()):
        if text == root or text.startswith(root + "/"):
            rel = text[len(root):].lstrip("/")
            if any(part == ".." for part in rel.split("/")):
                return None
            sep = "\\" if "\\" in host or _WINDOWS.match(_norm(host)) else "/"
            return host.rstrip("\\/") + (sep + rel.replace("/", sep) if rel else "")
    return None


def mapping_note():
    """
    One sentence for whoever judges this desktop's work: which folder of the person's computer ~/PC is. "" when this
    desktop has no shared folder. Found live (2026-10-19): a letter saved in /home/headless/PC was judged "not in the
    Decurix demo folder" - it was that folder.
    """

    if not active():
        return ""
    return (f"On this desktop {link_path().as_posix()} IS the person's own folder {folder()} on their computer (the same "
            f"folder, shared): a file saved under {link_path().as_posix()} is saved in {folder()}.")


def kind(path):
    """What a file's first bytes make it, as the control plane says it: zip, pdf, ole, text or other."""

    try:
        with open(path, "rb") as f:
            head = f.read(8)
    except OSError:
        return "other"
    if head.startswith(b"PK\x03\x04"):
        return "zip"
    if head.startswith(b"%PDF"):
        return "pdf"
    if head.startswith(b"\xd0\xcf\x11\xe0"):
        return "ole"
    if b"\x00" not in head:
        try:
            head.decode("utf-8")
            return "text"
        except UnicodeDecodeError:
            pass
    return "other"


def stat(local):
    """
    The control plane's answer to "is this file on the person's computer" (httpapi/hostfiles.go), for a file that is
    in the shared folder and so right here: {"exists", "folder", "size", "modified", "kind"}. Never reads more than the
    file's first eight bytes.
    """

    answer = {"path": str(local), "exists": False}
    try:
        info = os.stat(local)
    except OSError:
        return answer
    answer["exists"] = True
    answer["folder"] = os.path.isdir(local)
    answer["size"] = info.st_size
    answer["modified"] = datetime.fromtimestamp(info.st_mtime, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if not answer["folder"]:
        answer["kind"] = kind(local)
    return answer


def walk():
    """The files in the shared folder, a few levels deep (what a task may have made there), hidden ones left out."""

    if not active():
        return
    yield from (p for p in walk_files(link_path()) if contained(str(p)))


def walk_files(root, skip=()):
    """Files under root, at most MAX_DEPTH levels and MAX_ENTRIES entries deep, hidden ones and folders named in `skip`
    left out."""

    return (p for p, is_folder in _walk(root, skip) if not is_folder)


def walk_folders(root, skip=()):
    """Folders under root (an empty one too), with walk_files' depth and entry budget, breadth first; hidden ones, links
    and folders named in `skip` left out."""

    return (p for p, is_folder in _walk(root, skip) if is_folder)


def _walk(root, skip=()):
    """(path, is_folder) for what is under root, breadth first, at most MAX_DEPTH levels and MAX_ENTRIES entries; a
    link is never followed or listed as a folder."""

    from collections import deque

    # Breadth first, so the entry budget is spent on the folders nearest the top before the deepest.
    seen, stack = 0, deque([(Path(root), 0)])
    while stack:
        folder_path, depth = stack.popleft()
        try:
            children = list(os.scandir(folder_path))
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
                    if child.name not in skip:
                        yield Path(child.path), True
                        if depth < MAX_DEPTH:
                            stack.append((Path(child.path), depth + 1))
                elif child.is_file(follow_symlinks=False):
                    yield Path(child.path), False
            except OSError:
                continue


def named(names):
    """
    The files of the shared folder whose names appear among `names` (file names a request mentions): name -> local
    path. A name is found at the top of the folder or a level or two below it; the first is taken.
    """

    wanted = {str(n).lower() for n in names}
    found = {}
    for path in walk():
        if path.name.lower() in wanted and path.name.lower() not in found:
            found[path.name.lower()] = str(path)
    return found
