import threading


# ============================================================
# Agent state
#
# These are plain module attributes, not per-module globals: every other
# module does `from decurix_agent import state` and reads/writes
# `state.current_task` etc. Since Python caches modules as singletons, an
# attribute write here is immediately visible to every module that imported
# it - the same sharing a single-file `global` gave us before the split.
# ============================================================

state_lock = threading.Lock()

current_task = None
current_task_file = None

# The control plane's ID for the task running now (task_context.py), which
# every progress line and reply carries back. "" when the request had none.
current_task_id = ""
waiting_for_query = False
# When that question was asked (time.time()): a request saved after it, but not as its reply, means the control
# plane holds no open question for this desktop (watchers._orphaned_question).
query_asked_at = 0.0

# The question the agent is waiting on and its kind ("query" or "approval"),
# so the person's reply can be kept with what it answered (lessons.py).
last_query_text = ""
last_query_kind = ""
# Whether that question was raised for a secret (a password or code field, a step the recorder kept private): its reply
# may be the secret itself, so it is withheld whole wherever it is kept (messaging "secret", watchers.answer_waiting).
last_query_secret = False
# The id of the query the agent is waiting on (messaging.ask_for_query): a reply answers that one only - the date
# question is answered by the reply to its own query (task_context.take_date_answer, review round 3, R3).
asked_query_id = ""

# Whether the harness's reasoning backend has been reached at least once.
# Set by harness.wait_for_backend() on the thread app.py starts, read by
# watchers.command_watcher() to decide whether a task can start yet. False
# means "not up yet", which is a reason to leave a request queued - never a
# reason to fail it.
backend_ready = False

# A replay (replay_task.py) paused for approval or for a step a person has to
# do: what to resume, and from which step. While it is set, the reply the
# watcher routes to a waiting agent goes to the replay rather than the engine.
paused_replay = None

# The same for a question skill_task.py asked (teaching a skill, or a skill
# that stopped to ask): what to resume when the reply arrives.
paused_skill = None

processed_commands = set()


def begin(task, task_file):
    """A task starts: it is the current one, nothing waits on a question, and no reply is being handled (a secret
    reply of an earlier task must not make this task keep none of the person's corrections)."""

    global current_task, current_task_file, waiting_for_query, reply_secret, reply_judgement
    current_task, current_task_file, waiting_for_query = task, task_file, False
    reply_secret, reply_judgement = False, ""


# What the control plane said about each message it sent, by the message's command file (http_api._request):
# {"secret": bool, "judgement": id or ""}. The watcher reads it for exactly that message.
message_facts = {}
# The judgement of the reply being handled now (watchers.answer_waiting): what is kept from it cites this one.
reply_judgement = ""
# Whether the reply being handled now is secret (watchers.answer_waiting): nothing keeps its words.
reply_secret = False
