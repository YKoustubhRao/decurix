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
waiting_for_help = False
last_help_id = None

processed_commands = set()
