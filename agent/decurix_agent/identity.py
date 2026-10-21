import os

from decurix_agent import config


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

MAX_APPS = 8


def _desktop_apps():
    """The apps whose icons are on this desktop, each with the web address it opens when it is a web app."""

    try:
        from decurix_agent.computer import launchers
        desktop = launchers.PLACES[0]
        listed = []
        for entry in launchers.entries([desktop]):
            if launchers._opens_the_browser(entry):
                continue
            page = launchers.address(entry)
            listed.append(f'"{entry["name"]}"' + (f" (a web app at {page})" if page else ""))
        return sorted(listed)[:MAX_APPS]
    except Exception:
        return []


def _missing_apps():
    """One sentence per app the person asked this desktop to have and it does not (the control plane's record)."""

    try:
        from decurix_agent.computer import launchers
        return [launchers.missing_sentence(app) for app in launchers.missing()][:MAX_APPS]
    except Exception:
        return []


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

    # Where things are, from inside this desktop. Found live (2026-10-13): asked to register a vendor in a web app
    # whose server runs on the computer, an engine opened http://localhost:8000 (this desktop itself: refused),
    # then http://127.0.0.1:8000, and spent a whole attempt on "the application never became available".
    facts.append(
        "- Network: localhost and 127.0.0.1 are THIS desktop, not the computer it runs on. Servers on that "
        "computer are reached at host.docker.internal (http://host.docker.internal:PORT)."
    )
    apps = _desktop_apps()
    if apps:
        facts.append("- Apps on your desktop (open one with `gui open \"NAME\"`): " + "; ".join(apps))
    gone = _missing_apps()
    if gone:
        facts.append(
            "- Apps the person asked this desktop to have that it does NOT have: " + " ".join(gone)
            + " If a request needs one of these, say it isn't on this desktop (yet); do not do the work in another "
            "app in its place unless the person says so."
        )

    if config.DESKTOP_SPECIALIZATION:
        facts.append(
            "- Your specialization (what you were created to focus on): "
            f"{config.DESKTOP_SPECIALIZATION}"
        )

    if config.DESKTOP_PARENT_ID:
        facts.append(
            "- You were spawned as a specialized child of desktop "
            f"'{config.DESKTOP_PARENT_ID}'. You started with a copy of its "
            "learned memory, and new memory it records may be shared with "
            "you too - see your own learned-memory context below."
        )

    try:
        from decurix_agent.computer import modes
        stage_info = modes.stage_summary()
        facts.append(
            f"- Worker replication mode: {stage_info['label']}. {stage_info['description']}"
        )
    except Exception:
        pass

    return (
        "Facts about yourself. These are already known - answer questions "
        "about them directly, never ask the human for them:\n"
        + "\n".join(facts)
    )
