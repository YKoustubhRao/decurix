import os
import subprocess
from pathlib import Path


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


def launch(argv, cwd=None):
    """
    Start a GUI program detached from the caller.

    Detached for the same reason the agent itself is (start_agent_terminal.sh):
    a program started inside a shell command whose output is being captured
    holds that pipe open, and the command then never returns.
    """

    return subprocess.Popen(
        argv,
        env=app_env(),
        cwd=cwd or str(Path.home()),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
