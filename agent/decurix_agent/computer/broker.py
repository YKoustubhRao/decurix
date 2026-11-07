"""
Broker mode: the screen (X server, window manager, browser) is a user of its own, `decurixscreen`, and the worker's
user drives it only through the screen broker - the gui daemon (daemon.py) running as that user, which asks the
approval gate per action as `gui` always did (docs/gate-boundary.md, "Next: the input broker").

Off unless the desktop was created with DECURIX_SCREEN_USER=1 (the control plane's DECURIX_SCREEN_BROKER=1). This
module holds what the screen side's code needs to know about it, read per call (never at import, so the daemon's
import-time comparison is unchanged):

- whether this process is the screen side (the switch, and a uid that is not the worker's);
- the DevTools relay's socket (cdp_relay.py), through which cdp.py reaches the browser - it has no port;
- the worker's launch helper's socket (launch_helper.py), which starts every program but the browser as the worker;
- deliver_to_caller: a file a command writes for its caller (a screenshot) goes back over the command's own
  connection and is written by the caller's client, as the caller (gui_client.save_output), never by the screen
  user into the worker's folders.
"""

import os
import sys

from decurix_agent.computer import gui_client

# The module a broker's child registers its caller's connection under (daemon.Child). Not under decurix_agent: a
# command whose import-time settings differ forgets and imports every decurix_agent module again, and the connection
# must outlive that.
CALLER_MODULE = "decurix_gui_caller"


def requested():
    """Whether this desktop drives its screen through the broker (DECURIX_SCREEN_USER=1)."""

    return gui_client.broker_requested()


def worker_uid():
    """The worker's uid (DECURIX_SCREEN_WORKER_UID, else the desktop user's, 1001)."""

    return gui_client.worker_uid()


def screen_side():
    """Whether this process is the screen side of a broker-mode desktop: the switch is on and it is not the worker."""

    return requested() and os.getuid() != worker_uid()


def screen_dir():
    """The broker's shared folder (/run/decurix-screen)."""

    return gui_client.screen_dir()


def relay_socket():
    """The DevTools relay's socket when this desktop is in broker mode, else None (the browser has its port then).
    Only the screen user may open it: it is in a folder of that user's alone (cdp_relay.main makes it)."""

    if not requested():
        return None
    return os.environ.get("DECURIX_CDP_RELAY_SOCKET") or os.path.join(screen_dir(), "cdp", "cdp.sock")


def launch_dir():
    """The worker's launch helper's folder: the worker's own, which others may only pass through."""

    return os.environ.get("DECURIX_LAUNCH_DIR") or "/tmp/decurix-launch-%d" % worker_uid()


def caller_connection():
    """The connection of the command this broker's child runs, or None (not a broker's child)."""

    holder = sys.modules.get(CALLER_MODULE)
    return getattr(holder, "conn", None)


def deliver_to_caller(path, data, required=False, atomic=False):
    """
    Write `data` at `path` for the command's caller. In a broker's child the bytes go back over the command's own
    connection and the caller's client writes them, as the caller, at that path in its own working folder - only if
    the caller named that path (gui_client.caller_paths). Anywhere else this process writes them itself, as it
    always did: straight to the path, or (`atomic`) to path.part and renamed over it.

    `required`: the command's result depends on the file (observe --screenshot); the caller's client makes a
    command that exited 0 exit 1 when it could not write it. Raises OSError as a write here would.
    """

    conn = caller_connection()
    if conn is None:
        target = path + ".part" if atomic else path
        with open(target, "wb") as f:
            f.write(data)
        if atomic:
            os.replace(target, path)
        return
    flags = (gui_client.OUTPUT_REQUIRED if required else 0) | (gui_client.OUTPUT_ATOMIC if atomic else 0)
    payload = gui_client.pack([b"%d" % flags, os.fsencode(path), bytes(data)])
    if len(payload) > gui_client.MAX_OUTPUT:
        raise OSError(f"{path}: {len(data)} bytes is more than a command may hand back")
    conn.sendall(b"O" + len(payload).to_bytes(4, "big") + payload)
