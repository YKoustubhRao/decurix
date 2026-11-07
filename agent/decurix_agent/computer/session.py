import os
import subprocess
from pathlib import Path

from decurix_agent.computer.errors import ComputerError


# ============================================================
# The desktop's graphical session
#
# Everything that reaches the screen - the agent itself, a `gui` command an
# engine runs, Claude Code's MCP server, a benchmark's `docker exec` - needs
# the same three things from the XFCE session: which X display, the cookie to
# open it, and the session D-Bus that the accessibility bus hangs off. The
# agent inherits them from XDG autostart; a `docker exec` has none of them.
# So they are discovered, not assumed, and filled in only where missing.
# ============================================================

DEFAULT_DISPLAY = ":1"

# The base image sets NO_AT_BRIDGE=1 for the whole container, which is the
# single reason no GTK app on a desktop used to show up over AT-SPI at all:
# the bridge a GTK app loads to publish its widgets refuses to start. Every
# app this layer launches gets it switched back on, whatever the image says.
ACCESSIBLE_APP_ENV = {
    "NO_AT_BRIDGE": "0",
    # LibreOffice only publishes its widgets over AT-SPI through its GTK
    # front end.
    "SAL_USE_VCLPLUGIN": "gtk3",
    # A GTK2 app (GIMP 2.10) publishes nothing unless it loads these modules: found live (2026-10-15), GIMP's windows
    # listed no control at all, and a worker spent 79 commands guessing at its Scale Image dialog. GTK2_MODULES is
    # read by GTK2 only, so GTK3 apps are untouched.
    "GTK2_MODULES": "gail:atk-bridge",
}


def _session_dbus_address():
    """
    The session bus of the running XFCE session, read off its own process.
    """

    uid = os.getuid()

    for proc in Path("/proc").iterdir():
        if not proc.name.isdigit():
            continue

        try:
            if proc.stat().st_uid != uid:
                continue

            if (proc / "comm").read_text().strip() not in ("xfce4-session", "xfce4-panel"):
                continue

            for item in (proc / "environ").read_bytes().split(b"\0"):
                if item.startswith(b"DBUS_SESSION_BUS_ADDRESS="):
                    return item.split(b"=", 1)[1].decode()
        except OSError:
            continue

    return None


def ensure():
    """
    Fill in DISPLAY, XAUTHORITY and the session bus for this process, where
    they are missing. Idempotent and cheap after the first call.
    """

    os.environ.setdefault("DISPLAY", DEFAULT_DISPLAY)

    if "XAUTHORITY" not in os.environ:
        cookie = Path.home() / ".Xauthority"
        if cookie.exists():
            os.environ["XAUTHORITY"] = str(cookie)

    if "DBUS_SESSION_BUS_ADDRESS" not in os.environ:
        address = _session_dbus_address()
        if address:
            os.environ["DBUS_SESSION_BUS_ADDRESS"] = address


def app_env():
    """
    The environment an app launched from here should start with.
    """

    ensure()

    env = dict(os.environ)
    env.update(ACCESSIBLE_APP_ENV)

    return env


# Broker mode (broker.py): the only program the screen side starts as itself. The desktop's launcher, which every way
# of starting the browser goes through (agent/chromium-browser).
BROWSER_COMMANDS = {"chromium-browser", "google-chrome", "google-chrome-stable", "chromium", "chrome"}

# What the screen side lets a browser start take besides its pages: nothing that names a program, a folder, an
# extension or a port (--renderer-cmd-prefix, --user-data-dir, --load-extension, --remote-debugging-port would each be
# the screen user's power handed to whoever wrote the command).
BROWSER_FLAGS = {"--no-startup-window", "--new-window"}
BROWSER_SCHEMES = ("http://", "https://", "file://", "about:")


class RefusedLaunch(ComputerError, ValueError):
    """A program the screen side will not start as asked (broker mode): said to the engine as a gui error, and a
    ValueError to the launchers that catch one."""


def browser_command(argv):
    """Whether `argv` starts the desktop's browser."""

    return bool(argv) and os.path.basename(str(argv[0])) in BROWSER_COMMANDS


def browser_arguments(args):
    """The browser's arguments when the screen side may start it with them; RefusedLaunch otherwise."""

    for arg in args:
        arg = str(arg)
        if arg in BROWSER_FLAGS:
            continue
        page = arg[len("--app="):] if arg.startswith("--app=") else arg
        if not page.lower().startswith(BROWSER_SCHEMES) or any(c in page for c in "\0\n\r"):
            raise RefusedLaunch(f"the browser is started with pages only, not {arg!r}")
    return [str(a) for a in args]


class Started:
    """A program the worker's launch helper started (broker mode): not this process's child, so there is nothing to
    wait for here - only its pid."""

    def __init__(self, pid):
        self.pid = pid
        self.returncode = None

    def poll(self):
        return None


def launch(argv):
    """
    Start a GUI program detached from the caller.

    Detached for the same reason the agent itself is (start_agent_terminal.sh):
    a program started inside a shell command whose output is being captured
    holds that pipe open, and the command then never returns.

    In broker mode, on the screen side, only the browser is started here (with its DevTools relay, cdp_relay.py, and
    pages for arguments); every other program is started as the worker by its launch helper (launch_helper.py), which
    is given the program and its arguments and nothing of this side's environment. A program the screen user started
    would be the screen user's: a terminal opened that way is typing that no gate sees.
    """

    from decurix_agent.computer import broker

    if broker.screen_side():
        if browser_command(argv):
            import sys
            # Never the worker's accessibility bus (atspi._broker_bus): the browser's own widgets on it would be the
            # worker's to press.
            env = {k: v for k, v in app_env().items() if k != "AT_SPI_BUS_ADDRESS"}
            return subprocess.Popen(
                [sys.executable, "-m", "decurix_agent.computer.cdp_relay", "--", str(argv[0])]
                + browser_arguments(argv[1:]),
                env=env,
                cwd=str(Path.home()),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
        from decurix_agent.computer import launch_helper
        return Started(launch_helper.request(argv))

    return subprocess.Popen(
        argv,
        env=app_env(),
        cwd=str(Path.home()),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
