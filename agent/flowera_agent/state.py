import threading


# ============================================================
# Agent state
#
# These are plain module attributes, not per-module globals: every other
# module does `from flowera_agent import state` and reads/writes
# `state.current_task` etc. Since Python caches modules as singletons, an
# attribute write here is immediately visible to every module that imported
# it - the same sharing a single-file `global` gave us before the split.
# ============================================================

state_lock = threading.Lock()

current_task = None
current_task_file = None

# The control plane's ID for the task running now (task_context.py), which
# every progress line and reply carries back. "" when the message had none.
current_task_id = ""
waiting_for_help = False
last_help_id = None

# Whether the harness's reasoning backend has been reached at least once.
# Set by harness.wait_for_backend() on the thread app.py starts, read by
# watchers.command_watcher() to decide whether a task can start yet. False
# means "not up yet", which is a reason to leave a message queued - never a
# reason to fail it.
backend_ready = False

processed_commands = set()
