"""
What this desktop's tools wrote into the person's shared folder, kept so an app of their computer cannot lose it.

The named-app flow is: make the content with the tools, open the file in the person's app, check it, save it from the
app. Found live (round 5 P1, 2026-10-20, CS): Word still held the memo from the stage's first attempt; the tools
rewrote the file, Word's stale copy was saved over it, the file was then locked ("Permission denied"), and a blank
document typed over and saved left the memo with 0 headings. Three guards, all generic:

- `release`: a file an app OUR helpers started still holds is let go (the control plane's /api/host-files/release ends
  only our apps) - before the tools write it, and before an app of the person's computer is opened, so the app opens
  the file as the tools left it, never a stale copy.
- `remember`: each document the tools save there is copied aside with its words.
- `emptied`: after the app's save, a document that lost most of what the tools wrote is found, and `restore` puts the
  tools' version back (long_task's stage check, BLOCKING).
"""

import hashlib
import json
import re
import shutil
import urllib.parse
import urllib.request
from pathlib import Path

from decurix_agent import config
from decurix_agent.logging_utils import log

KINDS = {".docx", ".xlsx", ".xlsm", ".pptx"}
# A document lost what the tools wrote when fewer than this share of the tools' distinct words are still in it.
KEPT_SHARE = 0.5
MIN_WORDS = 20


def _store():
    """This task's own store: what the tools of another task made is never put back over this one's work."""

    try:
        from decurix_agent import task_context
        task = task_context.current() or "no-task"
    except Exception:  # noqa: BLE001
        task = "no-task"
    return Path(config.DECURIX_DIR) / "tool-made" / task


def _key(local):
    return hashlib.sha256(str(Path(local).resolve()).encode("utf-8")).hexdigest()[:20]


def _words(path):
    try:
        from decurix_agent.tools import office
        text = office.read_document(str(path)).get("text") or ""
    except Exception as exc:  # noqa: BLE001
        log(f"tool_made: could not read {path}: {exc}")
        return None
    return {w.lower() for w in re.findall(r"[^\W_][\w,.%-]*", text) if len(w) > 1}


def _host(local):
    try:
        from decurix_agent import shared
        return shared.to_host(str(local)) or ""
    except Exception:  # noqa: BLE001
        return ""


def release(host_paths):
    """Each file of the person's computer an app OUR helpers started holds, let go: [names released]. Never raises."""

    released = []
    for host in [h for h in host_paths if h]:
        url = (config.CONTROL_PLANE_URL.rstrip("/") + "/api/host-files/release?"
               + urllib.parse.urlencode({"desktop": config.DESKTOP_ID, "path": host}))
        try:
            with urllib.request.urlopen(urllib.request.Request(url, data=b"", method="POST"), timeout=70) as resp:
                answer = json.loads(resp.read().decode("utf-8") or "{}")
        except Exception as exc:  # noqa: BLE001 - a release that cannot be made decides nothing
            log(f"tool_made: could not release {host}: {exc}")
            continue
        if answer.get("released"):
            released.append(host.replace("\\", "/").rsplit("/", 1)[-1])
    if released:
        log(f"tool_made: released (an app of ours held them): {', '.join(released)}")
    return released


def before_write(local):
    """Before the tools write a file of the shared folder: an app of ours holding it lets it go (its copy is stale)."""

    host = _host(local)
    if host and Path(local).exists():
        release([host])


def remember(local):
    """After the tools saved a document in the shared folder: a copy of it and its words, kept aside."""

    p = Path(local)
    if p.suffix.lower() not in KINDS or not _host(p) or not p.is_file():
        return
    words = _words(p)
    if not words or len(words) < MIN_WORDS:
        return
    store = _store()
    try:
        store.mkdir(parents=True, exist_ok=True)
        key = _key(p)
        shutil.copy2(p, store / (key + p.suffix.lower()))
        from decurix_agent.computer import privacy
        privacy.write_scrubbed(store / (key + ".json"), {"path": str(p.resolve()), "words": sorted(words)})
    except OSError as exc:
        log(f"tool_made: could not keep {p}: {exc}")


def remembered():
    """The shared-folder documents the tools made here: [local path]."""

    out = []
    store = _store()
    if not store.is_dir():
        return out
    for note in sorted(store.glob("*.json")):
        try:
            out.append(json.loads(note.read_text(encoding="utf-8"))["path"])
        except (OSError, ValueError, KeyError):
            continue
    return out


def emptied(paths=None):
    """
    [(local path, the tools' word count, how many of them are still there)] for each remembered document (of `paths`,
    when given) that lost most of what the tools wrote - an app's save over it, a blank document saved in its place.
    """

    wanted = {str(Path(p).resolve()) for p in paths} if paths is not None else None
    out = []
    store = _store()
    for local in remembered():
        if wanted is not None and local not in wanted:
            continue
        note = store / (_key(local) + ".json")
        try:
            before = set(json.loads(note.read_text(encoding="utf-8"))["words"])
        except (OSError, ValueError, KeyError):
            continue
        if not Path(local).is_file():
            continue
        now = _words(local)
        if now is None:
            continue
        kept = len(before & now)
        if kept < KEPT_SHARE * len(before):
            out.append((local, len(before), kept))
    return out


def restore(local):
    """The tools' version of a document put back where it was (an app of ours holding it is released first)."""

    p = Path(local)
    kept = _store() / (_key(p) + p.suffix.lower())
    if not kept.is_file():
        return False
    before_write(p)
    try:
        from decurix_agent import shared
        shared.copy_file(kept, p)
        return True
    except OSError as exc:
        log(f"tool_made: could not put back {p}: {exc}")
        return False


def held_by_our_apps():
    """Before an app of the person's computer is opened: every document the tools made here is let go by our apps."""

    return release([_host(p) for p in remembered()])
