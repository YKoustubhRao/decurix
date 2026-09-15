import os

from flowera_agent import config


# ============================================================
# Desktop identity
#
# What this desktop is, in the words its own agent needs in order to
# answer for itself. Shared by all three harnesses for the same reason
# memory_context.py is: knowing your own name is not a property of which
# reasoning engine you happen to be running on.
#
# This exists because a question as ordinary as "what's your desktop
# name?" was genuinely unanswerable. No system prompt said what the
# desktop was called, so the model's only honest move was to ask the
# human - which arrived as a query that blocked the task and
# paged a person to supply a fact the container already had in its own
# environment. An agent that cannot answer "who are you" looks broken
# long before it is asked to do anything real.
# ============================================================

def get_identity_context(engine=""):
    """
    Describe this desktop to its own agent, one fact per line.

    `engine` is whatever the calling harness wants to say about the brain
    it runs on ("qwen2.5-coder:3b via Ollama", "Claude Code"); the caller
    passes it because harness.py is the only module allowed to know which
    harness is active.
    """

    facts = [
        f"- Desktop name: {config.DESKTOP_NAME}",
        "- Desktop ID (what the control plane calls you): "
        f"{config.DESKTOP_ID}",
    ]

    if engine:
        facts.append(f"- Reasoning engine: {engine}")

    facts.extend(
        [
            f"- Shell user: {os.environ.get('USER', 'headless')} "
            f"(home {os.path.expanduser('~')})",
            f"- Task working directory: {config.WORK_DIR}",
            "- Machine: a disposable Ubuntu XFCE desktop in a Docker "
            "container, with Chromium installed, viewed over VNC.",
        ]
    )

    return (
        "Facts about yourself. These are already known - answer questions "
        "about them directly, never ask the human for them:\n"
        + "\n".join(facts)
    )
