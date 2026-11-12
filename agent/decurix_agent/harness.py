import threading

from decurix_agent import config, state

# The only place that branches on which reasoning engine drives tasks. Since
# 2026-11-07 there is one, TheBot (thebot_harness.py); what a desktop chooses
# is its brain (config.BRAIN), not an engine. The ollama, claude and azure
# harnesses were removed then: a desktop created with one of them still has
# its old AGENT_HARNESS (config.CREATED_HARNESS), and runs TheBot.
# watchers.py's command_watcher() and app.py's startup call only what this
# module exports.

from decurix_agent.logging_utils import log
from decurix_agent.ollama_client import ask_decided as _ask_decided, ask_once as _ask_once
from decurix_agent.thebot_harness import (
    perform_task,
    continue_after_query,
    wait_for_backend as _wait_for_backend,
    warm_after_ready as _after_ready,
)

if getattr(config, "CREATED_HARNESS", "thebot") != "thebot":
    log(f"This desktop was created with the {config.CREATED_HARNESS!r} harness, removed on 2026-11-07: it runs TheBot.")


def wait_for_backend():
    """
    Block until the engine's reasoning backend is usable, then publish it.

    Runs on a background thread (app.py) so it never gates the agent's own
    :8081 listener. Setting the flag here keeps "what ready means" in the one
    file allowed to know the engine, and gives watchers.py a single
    harness-agnostic thing to read.
    """

    _wait_for_backend()

    state.backend_ready = True

    # Whatever the engine wants done once it is up, off this thread's path:
    # a request that arrives meanwhile is already free to run.
    threading.Thread(target=_after_ready, daemon=True, name="after-ready").start()


def ask_model(system, prompt, *, purpose):
    """
    One question, not a task - the reply text, or RuntimeError. No history,
    no tools, no query. It is how the computer layer's grounder and judge
    (computer/escalation.py) get a second opinion without knowing which
    engine is behind them. The control plane's hosted model answers it when
    one is set up for this desktop (llm_client.available); a desktop whose
    brain is hosted raises when that fails, never loading a local model
    nobody chose. Otherwise the local model answers it through Ollama (soul
    has no single-question endpoint).
    `purpose` names what the question is for (llm_client.chat): required, so no call goes untagged.
    """

    # A hosted model, when the control plane has one, answers these:
    # choosing among candidates, judging a screen, placing a
    # click - exactly the one-off calls a small local model gets wrong.
    from decurix_agent import llm_client

    if llm_client.available():
        try:
            return llm_client.ask_once(system, prompt, purpose=purpose)
        except llm_client.LLMError as exc:
            # A desktop whose brain is the hosted model has no local one to
            # fall back to: asking Ollama would load a model nobody chose
            # for it, minutes on a small host. The caller treats this as
            # "not sure", the same as any failed question.
            if config.HOSTED_BRAIN:
                raise RuntimeError(str(exc)) from exc
            # otherwise fall back to the local model

    return _ask_once(system, prompt, purpose=purpose)


def ask_decided(system, prompt, via_soul, *, purpose):
    """
    ask_model for a question soul words and reads itself (plan rule 3, phase 2: the escalation ladder's, behind
    DECURIX_SOUL_ESCALATION): (text, decision). `via_soul(brain, **call)` asks soul's decision route on that brain
    (skill_task._ask_decided: soul_call.escalation with the question's evidence). The same chain as ask_model - the
    hosted model when the control plane has one, never a local model on a desktop whose brain is hosted, else the
    local one - and, wherever soul cannot make the call, today's question (`system`, `prompt`) asked as ask_model asks
    it, with no decision: the caller reads the text itself.
    """

    from decurix_agent import llm_client

    if llm_client.available():
        try:
            return llm_client.ask_decided(
                system, prompt, lambda max_tokens: via_soul("hosted", max_tokens=max_tokens,
                                                            timeout=llm_client.TIMEOUT_SECONDS), purpose=purpose)
        except llm_client.LLMError as exc:
            if config.HOSTED_BRAIN:
                raise RuntimeError(str(exc)) from exc

    return _ask_decided(system, prompt, via_soul, purpose=purpose)


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


def drives_screen_with_hosted_model():
    """Whether this desktop's engine does screen work with a hosted model: TheBot on a hosted brain (skill_task asks,
    so that only this file knows the engine)."""

    return bool(config.HOSTED_BRAIN)


def resume_waiting(record):
    """After a restart: wait again on the person for a task whose paused job the engine kept (thebot_harness)."""
    from decurix_agent import thebot_harness
    return thebot_harness.resume_waiting(record)


def cancel_running():
    """
    End the engine's work in progress (a person stopped a long request
    mid-stage). True when something was cancelled; otherwise the stop takes
    effect after the engine's turn.
    """

    from decurix_agent import thebot_harness
    return thebot_harness.cancel_current()


__all__ = ["perform_task", "continue_after_query", "wait_for_backend", "ask_model", "ask_decided", "cancel_running",
           "resume_waiting"]
