"""
Where a script the worker wrote for itself runs with nobody watching.

playbooks.py keeps, for a kind of request done by hand and checked, a script that does it again; tool_task.py then
runs it with no engine, and playbooks._prove runs it once to prove it. Nothing asks the person before such a script
acts, so it may only read and write files. The first answer was a list of what a script may not hold (curl, gui,
xdotool, urllib, ...): a review (2026-11-02) walked past it with `/usr/bin/curl`, `"cu" + "rl"`, an aliased socket
import, a subprocess list and shutil.rmtree. A list of forbidden words is not a boundary. This is:

  - the script runs inside a network namespace of its own (bubblewrap's --unshare-all when it works here, else
    util-linux `unshare --user --map-root-user --net --pid --fork --kill-child`): no network at all - not the internet,
    not the control plane, not the browser's DevTools port, not the agent's or soul's ports on localhost - and nothing
    it starts outlives it;
  - with an environment built from nothing (PATH, locale, Python's path), HOME and the worker's own folder pointed
    into a scratch copy, no display, no session bus, no agent token, no proxy, no control plane address;
  - in a copy of the work folder; what it writes there is copied back into the task's work folder only (regular
    files, never through a link, never outside the folder), and the emails it drafts are kept in the scratch copy and
    drafted for real afterwards by the agent itself (office email only drafts: a person sends).

Whether that holds is tried, not assumed (probe): a child must fail to reach a port this process listens on. A desktop
where neither works runs no learned script unattended - the engine does the request, and says why.

What this is not, said plainly: with `unshare` alone (no bubblewrap - the desktop's seccomp profile denies mount, so
bubblewrap cannot build its filesystem view and fails its probe), the script still sees the filesystem as the worker's
user does. The network and the display's environment are gone, but a script that writes by absolute path into
~/.decurix, or speaks X11 over /tmp/.X11-unix by hand, is stopped only by playbooks._sound's reading of it - an early
refusal, not a boundary. The real fix is a separate Linux user for what runs unattended (and for the approval gate).
"""

import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

from decurix_agent import config
from decurix_agent.logging_utils import log

# How long the probe's child may take, and how long a probe's answer is kept in this process.
PROBE_SECONDS = 15
PROBE_KEEP_SECONDS = 600

MAX_FILES = 2000
MAX_BYTES = 100 * 1024 * 1024

# The only variables carried into the sandbox from the agent's own environment: none of them reaches anything.
_KEPT_ENV = ("PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "PYTHONPATH", "PYTHONIOENCODING", "TERM",
             # (a Windows development host's, for the tests' stand-in: a process there does not start without them)
             "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT")

# What the probe's child runs: it connects to the port the parent listens on, and must not reach it; then it tries a
# unix socket the parent listens on by its path, and says whether that was reached (reported, not required: with
# unshare alone the filesystem - and so the display's and the session bus's sockets - is still the worker's).
_PROBE = (
    "import socket, sys\n"
    "try:\n"
    "    socket.create_connection(('127.0.0.1', int(sys.argv[1])), timeout=3).close()\n"
    "    print('reached')\n"
    "except OSError:\n"
    "    print('isolated')\n"
    "try:\n"
    "    s = socket.socket(socket.AF_UNIX)\n"
    "    s.settimeout(3)\n"
    "    s.connect(sys.argv[2])\n"
    "    s.close()\n"
    "    print('unix-reached')\n"
    "except (OSError, AttributeError, IndexError):\n"
    "    print('unix-isolated')\n"
)

_probed = {"at": 0.0, "how": None, "unix": None}
_probe_lock = threading.Lock()


def _bwrap_prefix(bwrap, scratch):
    # The whole filesystem read-only, the worker's home and /tmp (the display's socket, the session bus) replaced by
    # empty ones, and only the scratch folder writable.
    # The agent's own code and interpreter stay readable where they live under the home (`office` imports them).
    home = str(Path.home())
    code = [p for p in (os.environ.get("PYTHONPATH") or "").split(os.pathsep) + [sys.prefix]
            if p and os.path.isdir(p) and (p == home or p.startswith(home + os.sep))]
    readable = [arg for p in code for arg in ("--ro-bind", p, p)]
    return ([bwrap, "--unshare-all", "--die-with-parent", "--new-session", "--ro-bind", "/", "/", "--dev", "/dev",
             "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run", "--tmpfs", home]
            + readable + ["--bind", str(scratch), str(scratch), "--"])


def _unshare_prefix(unshare, scratch):
    # A pid namespace of its own, with the script as its first process (--fork): when the script ends, the kernel ends
    # everything it started, and --kill-child ends the script when the run is cut off (a timeout kills unshare). Found
    # in review (2026-11-03, F-11): a child the script left running in the background outlived the run.
    return [unshare, "--user", "--map-root-user", "--net", "--ipc", "--pid", "--fork", "--kill-child", "--"]


def _candidates():
    found = []
    bwrap, unshare = shutil.which("bwrap"), shutil.which("unshare")
    if bwrap:
        found.append(("bwrap", lambda scratch: _bwrap_prefix(bwrap, scratch)))
    if unshare:
        found.append(("unshare", lambda scratch: _unshare_prefix(unshare, scratch)))
    return found


def _isolates(prefix_of):
    """(whether a child started with this prefix cannot reach a port this process listens on, whether it can reach a
    unix socket this process listens on by its path - None when that could not be tried)."""

    if os.name == "nt":
        return False, None
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    scratch = Path(tempfile.mkdtemp(prefix=".sandbox-probe-"))
    unix, unix_path = None, ""
    try:
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        try:
            unix = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            unix_path = str(Path(tempfile.mkdtemp(prefix=".sandbox-sock-")) / "s")
            unix.bind(unix_path)
            unix.listen(1)
        except (OSError, AttributeError):
            unix, unix_path = None, ""
        done = subprocess.run(prefix_of(scratch) + [sys.executable, "-c", _PROBE, str(port), unix_path],
                              capture_output=True, text=True, timeout=PROBE_SECONDS, env=clean_env(scratch),
                              cwd=str(scratch))
        said = done.stdout.split()
        isolated = done.returncode == 0 and said[:1] == ["isolated"]
        reached = None if not unix_path else ("unix-reached" in said)
        return isolated, reached
    except (OSError, subprocess.SubprocessError):
        return False, None
    finally:
        listener.close()
        if unix is not None:
            unix.close()
        if unix_path:
            shutil.rmtree(Path(unix_path).parent, ignore_errors=True)
        shutil.rmtree(scratch, ignore_errors=True)


def how():
    """The sandbox this desktop can run a script in: "bwrap", "unshare", or "" when none holds (probed, then kept)."""

    with _probe_lock:
        if time.time() - _probed["at"] < PROBE_KEEP_SECONDS:
            return _probed["how"] or ""
        chosen, unix = "", None
        for name, prefix_of in _candidates():
            isolated, unix = _isolates(prefix_of)
            if isolated:
                chosen = name
                break
        _probed.update(at=time.time(), how=chosen, unix=unix if chosen else None)
        if not chosen:
            log("Sandbox: neither bubblewrap nor unshare can cut a script off from the network here")
        else:
            log(f"Sandbox: {status()}")
        return chosen


def available():
    """Whether a learned script can be run unattended on this desktop (how)."""

    return bool(how())


def status():
    """What the sandbox chosen here holds and what it does not, in words for the person and the log."""

    chosen = _probed.get("how")
    if not chosen:
        return "no sandbox: " + why_not()
    said = f"{chosen}: no network"
    if _probed.get("unix"):
        said += ("; unix sockets on this machine (the display's, the session bus's) are still reachable from inside "
                 "it - only the early reading of a script (playbooks._sound) stands before them")
    elif _probed.get("unix") is False:
        said += "; unix sockets are not reachable"
    return said


def why_not():
    """Why no learned script runs unattended here, in words for the person."""

    return ("this desktop cannot shut a script off from the network and the screen (neither bubblewrap nor a user "
            "and network namespace works here), and a script that runs with nobody watching runs only shut off")


def prefix(scratch):
    """The command line a script is started behind, for the sandbox chosen (how); [] when there is none."""

    chosen = how()
    for name, prefix_of in _candidates():
        if name == chosen:
            return prefix_of(Path(scratch))
    return []


# Where the network of anything that honours these variables goes: nowhere (a port nothing listens on). Inside the
# namespace there is no network at all; this is for a run with no namespace (a proof's stand-in in a test).
NOWHERE = "http://127.0.0.1:9"


def clean_env(scratch):
    """
    The environment a learned script runs with, built from nothing: PATH, the locale and Python's path from the agent;
    HOME, the worker's folder, its work folder and TMPDIR inside the scratch copy (a fresh approval store, no
    secrets, no command channel); drafts kept in the scratch copy; the control plane, soul, Ollama and every proxy
    pointed at nothing. No DISPLAY, session bus, XAUTHORITY, DECURIX_AGENT_TOKEN or any other variable of the agent's.
    """

    scratch = Path(scratch)
    env = {name: os.environ[name] for name in _KEPT_ENV if os.environ.get(name)}
    env.update(HOME=str(scratch), DECURIX_DIR=str(scratch / ".decurix"), DECURIX_WORK_DIR=str(scratch / "work"),
               TMPDIR=str(scratch / "tmp"), DECURIX_OUTBOX_DRY=str(scratch / "drafts.jsonl"),
               DECURIX_TASK_FILE=str(scratch / "task"),
               CONTROL_PLANE_URL=NOWHERE, THEBOT_URL=NOWHERE, OLLAMA_URL=NOWHERE,
               http_proxy=NOWHERE, https_proxy=NOWHERE, HTTP_PROXY=NOWHERE, HTTPS_PROXY=NOWHERE,
               all_proxy=NOWHERE, ALL_PROXY=NOWHERE, no_proxy="", NO_PROXY="")
    env.setdefault("PATH", "/usr/local/bin:/usr/bin:/bin")
    (scratch / "tmp").mkdir(parents=True, exist_ok=True)
    return env


def copy_work(work, scratch, leave_out=()):
    """Copy the work folder into scratch/work (no hidden folders, no links); False when it is too large to."""

    count = size = 0
    work = Path(work)
    for root, dirs, files in os.walk(work):
        dirs[:] = [d for d in dirs if not d.startswith(".") and not (Path(root) / d).is_symlink()]
        for name in files:
            source = Path(root) / name
            relative = source.relative_to(work)
            if relative.as_posix() in leave_out or source.is_symlink():
                continue
            try:
                size += source.stat().st_size
            except OSError:
                continue
            count += 1
            if count > MAX_FILES or size > MAX_BYTES:
                return False
            target = Path(scratch) / "work" / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
    (Path(scratch) / "work").mkdir(parents=True, exist_ok=True)
    return True


def snapshot(folder):
    """{relative path: (size, mtime_ns)} of the regular files under folder."""

    folder = Path(folder)
    out = {}
    for path in folder.rglob("*"):
        if path.is_file() and not path.is_symlink():
            st = path.stat()
            out[path.relative_to(folder).as_posix()] = (st.st_size, st.st_mtime_ns)
    return out


def written(folder, before):
    """The regular files under folder that are new or changed since `before` (snapshot)."""

    return sorted(rel for rel, mark in snapshot(folder).items() if before.get(rel) != mark)


def copy_back(scratch_work, work, relatives):
    """
    Copy what the script wrote in its copy back into the work folder: (copied, refused). Only regular files, only to
    a place inside the work folder that is no link (a link the script made, or one already in the folder, could carry
    the write to the approval store or the command channel).
    """

    work = Path(work).resolve()
    copied, refused = [], []
    for rel in relatives:
        source = Path(scratch_work) / rel
        target = Path(work) / rel
        try:
            if source.is_symlink() or not source.is_file():
                refused.append(rel)
                continue
            parent = target.parent
            parent.mkdir(parents=True, exist_ok=True)
            real_parent = parent.resolve()
            if real_parent != work and work not in real_parent.parents:
                refused.append(rel)
                continue
            if target.is_symlink() or (target.exists() and not target.is_file()):
                refused.append(rel)
                continue
            tmp = real_parent / f".{target.name}.sandbox-copy"
            shutil.copy2(source, tmp)
            os.replace(tmp, real_parent / target.name)
            copied.append(rel)
        except OSError as exc:
            log(f"Sandbox: could not copy {rel} back: {exc}")
            refused.append(rel)
    return copied, refused


def run(argv, scratch, timeout):
    """
    Run argv (the interpreter, the script, its arguments) in the sandbox, in scratch/work: a CompletedProcess. Raises
    RuntimeError when there is no sandbox here (nothing is run then).
    """

    behind = prefix(scratch)
    if not behind:
        raise RuntimeError(why_not())
    return subprocess.run(behind + list(argv), cwd=str(Path(scratch) / "work"), capture_output=True, text=True,
                          timeout=timeout, env=clean_env(scratch))


def new_scratch():
    """A fresh scratch folder for one run, inside the worker's tools folder."""

    base = Path(os.environ.get("DECURIX_TOOLS_DIR") or (config.DECURIX_DIR / "tools"))
    base.mkdir(parents=True, exist_ok=True)
    return Path(tempfile.mkdtemp(prefix=".run-", dir=str(base)))
