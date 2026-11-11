"""Whether this desktop received a request: what the control plane asks after a restart cut a send short.

The control plane writes a request down before it sends it and records it once the desktop has it; a crash between
the two leaves it unsure. At its next start it asks here (GET /requests/<task_id>?since=<epoch seconds>): did a
request for that task arrive at or after the moment it began sending? A reply and a retry carry the task id of the
request they continue, so only a command file that arrived since then counts, never the task's earlier ones.

The answer rests on the command directory: a request is saved there on arrival (messaging.save_request) and kept,
renamed, after it is handled (watchers._mark_handled) for DONE_KEEP_SECONDS. Past that window nothing is left to say
either way, and the answer is "unknown" (received: null), never a guessed "no".
"""

import time

from decurix_agent import config, task_context

# How long handled command files are kept (watchers.DONE_KEEP_SECONDS), less a margin for the sweep.
_KEPT_FOR = 3600 - 60

# Clock skew allowed between the control plane and this desktop (they share a host).
_SKEW = 2.0


def received(task_id, since, now=None):
    """True or False when the command directory can say whether a request for task_id arrived at or after since
    (epoch seconds); None when it cannot (no valid task id, or since is older than the files are kept)."""

    if not task_context.valid(task_id):
        return None
    now = time.time() if now is None else now
    try:
        since = float(since)
    except (TypeError, ValueError):
        return None
    if since < now - _KEPT_FOR:
        return None

    marker = f".{task_id}.txt"
    try:
        entries = list(config.COMMAND_DIR.iterdir())
    except OSError:
        return None
    for path in entries:
        name = path.name
        # Saved (".txt") or handled (".txt.done"); a ".tmp" never finished arriving.
        if not (name.endswith(marker) or name.endswith(marker + ".done")):
            continue
        try:
            if path.stat().st_mtime >= since - _SKEW:
                return True
        except OSError:
            continue
    return False
