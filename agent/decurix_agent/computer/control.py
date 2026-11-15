"""
Who drives the screen: the worker (the default) or a person, and a control epoch that counts every change between them.

Nothing on a desktop yet says "a person has taken the screen": a person on the screen page works beside the worker
(the demonstration recorder tells their input apart by device), and the screen broker separates users, not turns. This
module is the state and the fence for when something does - a take-over on the screen page, a private step - so that
whoever wires it calls take() and give_back() and every screen tool already obeys (technique from Hermes' bot-desktop
lease, tools/bot_desktop/lease.py, and the epoch `_fence` in tools/computer_use/tool.py):

- while a person holds the screen, the worker's `gui` refuses everything that looks at or acts on it, observation
  included (they may be typing a secret): admit() raises PersonHasControl;
- the epoch goes up at every change of holder, so an action or a look admitted under one epoch that ends under another
  is discarded - not recorded, not kept as evidence, not shown to the engine: fence(epoch) raises ControlChanged, even
  when control has already come back (what it saw may be the person's turn).

The state is a small JSON file read on every call (the gui command, the daemon's children and the agent are separate
processes) and changed only under a lock. A file that is there but cannot be read is a person holding: the worker never
acts on a screen a person may be using because a write was torn. A hold that names the process holding it (`pid`) ends
when that process is gone, with the epoch moved on.

A tool-level fence, like the approval gate's first layer: a program the worker runs by hand against the display is not
stopped by it (docs/gate-boundary.md, "What this does not cover").
"""

import json
import os
import time
from contextlib import contextmanager
from pathlib import Path

from decurix_agent import config, filelock
from decurix_agent.computer.errors import ComputerError

WORKER = "worker"
PERSON = "person"


class PersonHasControl(ComputerError):
    """A person holds the screen: nothing is looked at or done until they hand it back."""


class ControlChanged(ComputerError):
    """Control changed hands while an action or a look ran: what it did or saw is discarded."""


HELD_MESSAGE = ("a person has taken this desktop's screen ({reason}) - nothing was looked at or done, since they may "
                "be typing something private. Wait for them to hand it back, or say in your reply what you need them "
                "to do.")
CHANGED_MESSAGE = ("a person took this desktop's screen while this ran, so what it did and saw is discarded - not "
                   "recorded and not shown. It may or may not have happened: once the screen is yours again, observe "
                   "before doing anything again.")


def path():
    """The control state's file: DECURIX_CONTROL_FILE, else ~/.decurix/computer/control.json."""

    return Path(os.environ.get("DECURIX_CONTROL_FILE") or (config.DECURIX_DIR / "computer" / "control.json"))


def _alive(pid):
    try:
        os.kill(int(pid), 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except (OSError, TypeError, ValueError, OverflowError):
        return False


def _read():
    """The state as it is on disk: {holder, epoch, since, reason, pid}. No file: the worker holds at epoch 0. A file that
    cannot be read or makes no sense: a person holds (fail closed)."""

    fresh = {"holder": WORKER, "epoch": 0, "since": 0.0, "reason": "", "pid": None}
    try:
        raw = path().read_text(encoding="utf-8")
    except FileNotFoundError:
        return fresh
    except OSError:
        return dict(fresh, holder=PERSON, reason="the screen's control state could not be read")
    try:
        data = json.loads(raw)
    except ValueError:
        data = None
    if not isinstance(data, dict) or data.get("holder") not in (WORKER, PERSON) \
            or not isinstance(data.get("epoch"), int) or isinstance(data.get("epoch"), bool):
        return dict(fresh, holder=PERSON, reason="the screen's control state is damaged")
    pid = data.get("pid")
    return {"holder": data["holder"], "epoch": data["epoch"], "since": float(data.get("since") or 0.0),
            "reason": str(data.get("reason") or ""), "pid": pid if isinstance(pid, int) else None}


def _write(state):
    from decurix_agent.computer import privacy

    target = path()
    target.parent.mkdir(parents=True, exist_ok=True)
    privacy.write_scrubbed(target, state, atomic=True)


@contextmanager
def _locked():
    """The control state's own lock (a lock file beside it, no content), held for a read-then-write."""

    target = path()
    target.parent.mkdir(parents=True, exist_ok=True)
    with open(str(target) + ".lock", "a") as lock:
        filelock.exclusive(lock)
        yield


def _transition(change):
    """Apply `change` (state -> bool: whether it changed the holder) under the lock; the epoch goes up when it did."""

    with _locked():
        state = _read()
        if change(state):
            state["epoch"] += 1
            state["since"] = time.time()
            _write(state)
        return state


def _ended(state):
    """Whether a person's hold names a process that is gone (a recorder or a session that ended without handing back)."""

    return state["holder"] == PERSON and state.get("pid") is not None and not _alive(state["pid"])


def state():
    """The control state now. A hold whose process is gone is handed back first, with the epoch moved on."""

    current = _read()
    if _ended(current):
        def release(s):
            if not _ended(s):
                return False
            s.update(holder=WORKER, reason="", pid=None)
            return True
        current = _transition(release)
    return current


def take(reason="", pid=None):
    """A person takes the screen (`reason` says why, shown to the worker; `pid`: the process whose end hands it back).
    Taking it again while holding it changes nothing, so an action admitted in this hold is not voided by a repeat."""

    def hold(s):
        if s["holder"] == PERSON:
            changed = s.get("reason") != (reason or "") or s.get("pid") != pid
            s.update(reason=reason or "", pid=pid)
            if changed:
                _write(s)  # the same hold, said anew: no new epoch
            return False
        s.update(holder=PERSON, reason=reason or "", pid=pid)
        return True

    return _transition(hold)


def give_back(epoch=None):
    """The person hands the screen back to the worker. With `epoch`, only the hold of that epoch is ended (a stale
    hand-back after a new take-over changes nothing). A worker that holds already is left as it is: bumping the epoch
    then would void an action admitted a moment ago for nothing."""

    def release(s):
        if epoch is not None and s["epoch"] != epoch:
            return False
        if s["holder"] == WORKER:
            return False
        s.update(holder=WORKER, reason="", pid=None)
        return True

    return _transition(release)


_outer = []  # the epochs admitted by the enclosing calls of this process (a gui command around its core.act)


def admit():
    """The epoch the caller works under, or PersonHasControl. Inside an admitted scope (scope()) it is that scope's
    epoch, checked first: a call nested in a command never starts a new turn of its own."""

    if _outer:
        fence(_outer[-1])
        return _outer[-1]
    current = state()
    if current["holder"] == PERSON:
        raise PersonHasControl(HELD_MESSAGE.format(reason=current.get("reason") or "they are using it"))
    return current["epoch"]


def fence(epoch):
    """Raise ControlChanged when the epoch is no longer `epoch` (control changed hands since it was admitted, even and
    back), or PersonHasControl when a person holds now. Called before anything an action did or a look saw is kept,
    recorded or shown, and before input is sent."""

    if epoch is None:
        return
    current = state()
    if current["epoch"] != epoch:
        raise ControlChanged(CHANGED_MESSAGE)
    if current["holder"] == PERSON:
        raise PersonHasControl(HELD_MESSAGE.format(reason=current.get("reason") or "they are using it"))


def still(epoch):
    """Whether work admitted under `epoch` may still be kept: fence() without raising (False on any doubt)."""

    try:
        fence(epoch)
        return True
    except ComputerError:
        return False
    except Exception:  # noqa: BLE001 - not known to be the worker's turn: not kept
        return False


@contextmanager
def scope(epoch):
    """Calls made inside belong to the turn `epoch` (admit() returns it). None: no scope."""

    if epoch is None:
        yield
        return
    _outer.append(epoch)
    try:
        yield
    finally:
        _outer.pop()
