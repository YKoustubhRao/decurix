"""
Seeds a native Windows worker's browser profile from the person's own.

A worker's browser runs on a profile folder of its own (a browser asked to use
the person's profile hands the page to the copy they have open, on their
screen). Left empty, that profile has no Google sign-in: found live, a person
copied "Koustubh - Chrome" to a worker and still had to sign in.

What carries a sign-in across is the profile the shortcut opens (`Default`,
`Profile 1`...) plus the browser's `Local State`, which holds the key the
profile's secrets are encrypted with (the same Windows user, so it opens).
The refresh token a signed-in Chrome keeps in `Web Data` is what lets the
copy get its Google cookies again. The `Cookies` file itself is held
exclusively while the person's own browser runs and cannot be copied then;
what is lost with it is signed-in-ness of sites other than Google.

It is done once, into a folder no browser has used, and never again. It used
to be tried again at a later launch when files had been left behind, and that
is how a worker lost what a person had set up in it: found live (2026-10-03),
the first launch left 4 files behind, the person signed in to their sites in
the worker's Chrome, and the next launch - two and a half minutes later -
copied their own profile over the worker's, cookies and key included. From
its first start the profile is the worker's own; what a person signs in to
there is kept there.

Files the running browser holds open without sharing are read with every
sharing flag set, which is how most of a live profile can be read.
"""

import ctypes
import fnmatch
import json
import msvcrt
import os
import shutil
import time
from pathlib import Path

from decurix_agent.logging_utils import log

# Where each browser keeps the person's profiles.
USER_DATA = {
    "chrome": r"%LOCALAPPDATA%\Google\Chrome\User Data",
    "msedge": r"%LOCALAPPDATA%\Microsoft\Edge\User Data",
    "brave": r"%LOCALAPPDATA%\BraveSoftware\Brave-Browser\User Data",
}

# Caches a browser rebuilds, and components it downloads again: most of a
# profile's size and none of what a sign-in needs.
SKIP_DIRS = {
    "cache", "code cache", "gpucache", "grshadercache", "shadercache", "dawncache", "dawnwebgpucache",
    "dawngraphitecache", "graphitedawncache", "gpupersistentcache", "media cache", "cachestorage",
    "scriptcache", "crashpad", "browsermetrics", "deferredbrowsermetrics", "component_crx_cache",
    "extensions_crx_cache", "screen_ai", "optimization_guide_model_store", "ondeviceheadsuggestmodel",
    "wasmttsengine", "safe browsing", "shared dictionary", "download service", "segmentation_platform",
    "snapshots", "soda", "sodalanguagepacks", "hyphen-data", "service worker", "sessions", "sessions_encrypted", "extensions",
}
SKIP_FILES = ("Singleton*", "*lock*", "Lock*", "*.tmp", "*.pma", "LOCK", "Current Session", "Current Tabs",
              "Last Session", "Last Tabs")

MARKER = ".decurix-profile-seeded"

# How often a file that could not be read is tried within the one seeding.
COPY_ATTEMPTS = 3
COPY_RETRY_SECONDS = 0.4

_GENERIC_READ = 0x80000000
_SHARE_ALL = 0x1 | 0x2 | 0x4  # read, write, delete
_OPEN_EXISTING = 3
_INVALID = ctypes.c_void_p(-1).value


def _source_root(kind):
    template = USER_DATA.get(kind)
    return Path(os.path.expandvars(template)) if template else None


def _read_local_state(root):
    try:
        with _open_shared(root / "Local State") as handle:
            return json.loads(handle.read().decode("utf-8"))
    except (OSError, ValueError):
        return {}


def _open_shared(path):
    """Open `path` for reading even while another process has it open (not exclusively)."""

    handle = ctypes.windll.kernel32.CreateFileW(str(path), _GENERIC_READ, _SHARE_ALL, None, _OPEN_EXISTING, 0x80, None)
    if handle in (None, _INVALID, -1):
        raise OSError(f"cannot open {path}")

    return os.fdopen(msvcrt.open_osfhandle(handle, os.O_RDONLY), "rb")


def _copy(src, dst):
    dst.parent.mkdir(parents=True, exist_ok=True)

    with _open_shared(src) as reader, open(dst, "wb") as writer:
        shutil.copyfileobj(reader, writer, 1024 * 1024)


def signed_in_profile(root):
    """The profile folder the person is signed in with, else the last one used, else ""."""

    profile = _read_local_state(root).get("profile") or {}

    for name, info in (profile.get("info_cache") or {}).items():
        if isinstance(info, dict) and info.get("user_name"):
            return name

    return profile.get("last_used") or ""


def _skipped_file(name):
    return any(fnmatch.fnmatch(name, pattern) for pattern in SKIP_FILES)


def seed(kind, dest_root, profile_directory=""):
    """
    Bring the person's profile into `dest_root` once. Returns what happened as
    a sentence, or "" when there was nothing to do. Never raises: a worker
    whose seeding failed still has a working browser, just a signed-out one.
    """

    try:
        return _seed(kind, Path(dest_root), profile_directory)
    except Exception as error:  # noqa: BLE001 - a browser without sign-ins beats no browser
        log(f"profile seed failed: {error}")
        return f"could not copy the browser profile: {error}"


def _seed(kind, dest_root, profile_directory):
    root = _source_root(kind)
    if not root or not root.is_dir():
        return ""

    profile = profile_directory if profile_directory and (root / profile_directory).is_dir() else signed_in_profile(root)
    if not profile or not (root / profile).is_dir():
        return ""

    safe = "".join(c if c.isalnum() or c in "._-" else "_" for c in profile)
    marker = dest_root / f"{MARKER}-{safe}"

    # Once, whatever came of it (see the note at the top): a mark of any kind
    # means this folder has been the worker's own since.
    if marker.is_file():
        return ""

    dest_root.mkdir(parents=True, exist_ok=True)

    # A folder a browser has already run on - a worker from before the
    # seeding, or the profile the helper kept for its icon - holds what was
    # set up in it. Copying over it would replace its cookies and the key
    # they are encrypted with.
    if (dest_root / "Local State").exists() or (dest_root / profile / "Preferences").exists():
        marker.write_text("complete\nkept the worker's own profile\n", encoding="utf-8")
        return ""

    # The mark goes down before the copy: a launch that dies part way must
    # not be followed by one that copies over what the browser then wrote.
    marker.write_text("copying\n", encoding="utf-8")

    wanted = [(root / "Local State", dest_root / "Local State", "Local State")]
    for base, dirs, files in os.walk(root / profile):
        dirs[:] = [d for d in dirs if d.lower() not in SKIP_DIRS]
        relative = Path(base).relative_to(root)
        wanted += [(Path(base) / name, dest_root / relative / name, str(relative / name))
                   for name in files if not _skipped_file(name)]

    # A file the person's browser is writing at this instant is free a moment
    # later; one it holds for good (Cookies) is not. A few goes here are the
    # only ones there will be.
    failed = []
    copied = 0
    for attempt in range(COPY_ATTEMPTS):
        if attempt:
            time.sleep(COPY_RETRY_SECONDS)
        failed = []
        for source, target, name in wanted:
            try:
                _copy(source, target)
                copied += 1
            except OSError as error:
                failed.append((source, target, name))
                if name == "Local State":
                    log(f"profile seed: {error}")
        wanted = failed
        if not wanted:
            break
    failed = [name for _, _, name in failed]

    # Cookies is held exclusively by a running browser; everything else a
    # sign-in needs was readable. The mark records which it was.
    locked_cookies = [f for f in failed if f.replace("\\", "/").endswith(("Network/Cookies", "Network/Cookies-journal"))]
    needed = [f for f in failed if f not in locked_cookies]
    status = "complete" if not failed else ("partial" if locked_cookies and not needed else "incomplete")

    marker.write_text(f"{status}\n" + "\n".join(failed), encoding="utf-8")

    if not failed:
        return f"copied the {profile} profile ({copied} files)"
    if locked_cookies and not needed:
        return f"copied the {profile} profile ({copied} files); its saved cookies are in use by the browser that is open and were left behind, the Google sign-in comes from its account"
    return f"copied the {profile} profile with {len(failed)} files left behind"
