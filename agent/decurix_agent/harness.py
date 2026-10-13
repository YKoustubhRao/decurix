import threading

from decurix_agent import config, state

# The only place that branches on which reasoning engine drives tasks.
# Every branch exposes perform_task(task, task_file),
# continue_after_query(query_response) and wait_for_backend() with identical
# signatures, so watchers.py's command_watcher() and app.py's startup
# don't need to know which is active.

if config.AGENT_HARNESS == "claude":
    from decurix_agent.claude_harness import perform_task, continue_after_query, ask_once as _ask_once

    def _wait_for_backend():
        """
        No backend to wait for: the claude CLI is a local binary.
        """
        return

    def _after_ready():
        return

elif config.AGENT_HARNESS == "azure":
    from decurix_agent.azure_harness import (
        perform_task,
        continue_after_query,
        ask_once as _ask_once,
        wait_for_backend as _wait_for_backend,
        warm_after_ready as _after_ready,
    )

elif config.AGENT_HARNESS == "thebot":
    from decurix_agent.thebot_harness import (
        perform_task,
        continue_after_query,
        wait_for_backend as _wait_for_backend,
        warm_after_ready as _after_ready,
    )
    from decurix_agent.ollama_client import ask_once as _ask_once

else:
    from decurix_agent.task_runner import perform_task, continue_after_query
    from decurix_agent.ollama_client import ask_once as _ask_once
    from decurix_agent.ollama_client import wait_for_ollama as _wait_for_backend
    from decurix_agent.ollama_client import prime_prompt_cache as _after_ready


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

    # A hosted model, when the control plane has one, answers these for
    # every engine: choosing among candidates, judging a screen, placing a
    # click - exactly the one-off calls a small local model gets wrong.
    from decurix_agent import llm_client

    if llm_client.available():
        try:
            return llm_client.ask_once(system, prompt)
        except llm_client.LLMError as exc:
            # A desktop whose brain is the hosted model has no local one to
            # fall back to: asking Ollama would load a model nobody chose
            # for it, minutes on a small host. The caller treats this as
            # "not sure", the same as any failed question.
            if config.HOSTED_BRAIN:
                raise RuntimeError(str(exc)) from exc
            # otherwise fall back to this desktop's own engine

    return _ask_once(system, prompt)


# Start the next durable stage only after the previous engine call has returned
# and released its per-turn state. This also handles stages paused on a query.
_engine_perform_task = perform_task
_engine_continue_after_query = continue_after_query


def perform_task(task, task_file):
    from decurix_agent import workflow_task
    try:
        return _engine_perform_task(task, task_file)
    finally:
        workflow_task.engine_returned()


def continue_after_query(query_response):
    from decurix_agent import workflow_task
    try:
        return _engine_continue_after_query(query_response)
    finally:
        workflow_task.engine_returned()


__all__ = ["perform_task", "continue_after_query", "wait_for_backend", "ask_model"]
