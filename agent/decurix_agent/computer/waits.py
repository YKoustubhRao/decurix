"""
waits.py - how long a replay waits for a control or a step's result before it calls the step stopped: its fixed base
(FIND_SECONDS, EXPECT_SECONDS), stretched while the screen is still changing and while the host is loaded, never past
MAX_FACTOR times the base. A stop is then a real difference from the recording, not a slow machine.
"""

import hashlib
import os
import time


# ============================================================
# Load-aware waits (B5 of the 2026-11-11 diagnosis)
#
# Three of the five replay stops measured on the curves were waits that ran out on a loaded host (load 23-39 on a
# 2-vCPU desktop): "no element at path ... Voucher No.", "expected 'Voucher saved.', which never appeared". Each cost
# a model takeover of the rest of the run (13-84 turns). A fixed 12 s / 10 s is right for an idle desktop and wrong for
# a busy one, and nothing in the replay looked at which it was.
#
# A Patience starts with the base wait, stretched by the host's load per CPU (os.getloadavg; 1 where there is none),
# and while the screen keeps changing between polls (another element, another title, another address) its deadline
# moves on by half the base each time - an application still drawing is not one that has finished drawing something
# else. It never waits longer than MAX_FACTOR times the base, and an unchanging screen on an idle host waits exactly the
# base, as before. What it waited is kept (waited(), stretched()) so a run can say how long, and why.
# ============================================================

try:
    MAX_FACTOR = max(1.0, float(os.environ.get("DECURIX_REPLAY_WAIT_MAX_FACTOR", "4")))
except ValueError:
    MAX_FACTOR = 4.0


def load_factor():
    """How much longer to wait on this host now: 1 + the 1-minute load per CPU above 1, at most MAX_FACTOR (1 where
    the load cannot be read)."""

    try:
        per_cpu = os.getloadavg()[0] / max(1, os.cpu_count() or 1)
    except (AttributeError, OSError):
        return 1.0
    return min(MAX_FACTOR, 1.0 + max(0.0, per_cpu - 1.0))


def fingerprint(observation):
    """What a screen looks like for "is it still changing": its window, address and named elements (not their boxes)."""

    if not observation:
        return ""
    parts = [str(observation.get("active_title") or ""), str(observation.get("page_url") or "")]
    for element in (observation.get("elements") or [])[:400]:
        parts.append(f"{element.get('role')}|{element.get('name')}|{element.get('value')}")
    return hashlib.sha1("\n".join(parts).encode("utf-8", "replace")).hexdigest()


class Patience:
    """One wait: more(observation) says whether to keep polling."""

    def __init__(self, base, clock=time.monotonic, load=load_factor):
        self.base = float(base)
        self.clock = clock
        self.started = clock()
        self.load = load()
        self.deadline = self.started + self.base * self.load
        self.cap = self.started + self.base * MAX_FACTOR
        self.last = None
        self.changes = 0

    def more(self, observation=None):
        """Whether to keep waiting, given the screen just seen (None: not looked at this time)."""

        now = self.clock()
        if observation is not None:
            seen = fingerprint(observation)
            if self.last is not None and seen != self.last:
                self.changes += 1
                self.deadline = max(self.deadline, min(self.cap, now + self.base / 2))
            self.last = seen
        return now < min(self.deadline, self.cap)

    def waited(self):
        return self.clock() - self.started

    def stretched(self):
        """Why it waited longer than the base ("" when it did not): the load, the screen still changing."""

        if self.waited() <= self.base + 0.5:
            return ""
        why = []
        if self.load > 1.0:
            why.append(f"host load x{self.load:.1f}")
        if self.changes:
            why.append(f"the screen still changing ({self.changes} changes)")
        return f"waited {self.waited():.0f}s instead of {self.base:.0f}s: " + (", ".join(why) or "polling")
