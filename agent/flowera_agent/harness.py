import threading

from flowera_agent import config, state

# The only place that branches on which reasoning engine drives tasks.
# Every branch exposes perform_task(task, task_file),
# continue_after_query(query_response) and wait_for_backend() with identical
# signatures, so watchers.py's command_watcher() and app.py's startup
# don't need to know which is active.

if config.AGENT_HARNESS == "claude":
    from flowera_agent.claude_harness import perform_task, continue_after_query, ask_once as _ask_once

    def _wait_for_backend():
        """
        No backend to wait for: the claude CLI is a local binary.
        """
        return

    def _after_ready():
        return

elif config.AGENT_HARNESS == "thebot":
    from flowera_agent.thebot_harness import (
        perform_task,
        continue_after_query,
        wait_for_backend as _wait_for_backend,
        warm_after_ready as _after_ready,
    )
    from flowera_agent.ollama_client import ask_once as _ask_once

else:
    from flowera_agent.task_runner import perform_task, continue_after_query
    from flowera_agent.ollama_client import ask_once as _ask_once
    from flowera_agent.ollama_client import wait_for_ollama as _wait_for_backend
    from flowera_agent.ollama_client import prime_prompt_cache as _after_ready


def wait_for_backend():
    """
    Block until this harness's reasoning backend is usable, then publish it.

    Runs on a background thread (app.py) so it never gates the agent's own
    :8081 listener. Setting the flag here rather than in each branch keeps
    "what ready means" in the one file allowed to know which engine is
    running, and gives watchers.py a single harness-agnostic thing to read.
    """

    _wait_for_backend()

    state.backend_ready = True

    # Whatever the engine wants done once it is up, off this thread's path:
    # a request that arrives meanwhile is already free to run.
    threading.Thread(target=_after_ready, daemon=True, name="after-ready").start()


def ask_model(system, prompt):
    """
    One question to whichever engine this desktop runs - the reply text, or
    RuntimeError. Not a task: no history, no tools, no query. It is how the
    computer layer's grounder and judge (computer/escalation.py) get a second
    opinion without knowing which engine is behind them. TheBot has no
    single-question endpoint of its own, so it asks the Ollama it already uses.
    """

    return _ask_once(system, prompt)


__all__ = ["perform_task", "continue_after_query", "wait_for_backend", "ask_model"]
