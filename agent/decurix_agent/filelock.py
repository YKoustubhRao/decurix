"""
One exclusive lock on an open file, on whichever system the agent runs.

A Linux desktop has fcntl.flock; a native Windows worker has msvcrt.locking.
Both are released when the file is closed, which is how every caller here lets
go of one.
"""

import os
import time

if os.name == "nt":
    import msvcrt

    def try_exclusive(handle):
        """Take the lock without waiting. False when something else holds it."""

        try:
            # Locks one byte at the file's current position; the lock file is
            # opened fresh by every holder, so that is always byte 0.
            msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            return True
        except OSError:
            return False

    def exclusive(handle):
        """Take the lock, waiting for whoever holds it."""

        # msvcrt's own blocking mode gives up after ten tries; this does not.
        while not try_exclusive(handle):
            time.sleep(0.02)

else:
    import fcntl

    def try_exclusive(handle):
        """Take the lock without waiting. False when something else holds it."""

        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return True
        except BlockingIOError:
            return False

    def exclusive(handle):
        """Take the lock, waiting for whoever holds it."""

        fcntl.flock(handle, fcntl.LOCK_EX)
