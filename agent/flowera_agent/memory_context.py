from flowera_agent import config
from flowera_agent.logging_utils import log


# ============================================================
# Worker memory context
#
# Shared by every harness: both the Ollama loop and the Claude Code
# harness inject this into their reasoning as extra context.
# ============================================================

def get_memory_context():
    """
    Load persisted worker memory files, most recent first, up to
    config.MEMORY_CONTEXT_CHARS of content, so the agent reasons with its
    learned workflows without the prompt growing without bound as more
    entries pile up (memory is never pruned, and sharing/spawning can copy
    another desktop's whole history in at once).
    """
    paths = sorted(config.MEMORY_DIR.glob("*.txt"), key=lambda p: p.name, reverse=True)

    entries = []
    budget = config.MEMORY_CONTEXT_CHARS

    for path in paths:
        if budget <= 0:
            break

        try:
            content = path.read_text(encoding="utf-8").strip()
        except Exception as exc:
            log(f"Could not read memory file {path}: {exc}")
            continue

        if not content:
            continue

        entry = f"Memory file: {path.name}\n\n{content}"

        # A single oversized entry (memory is never pruned, and
        # sharing/spawning can copy another desktop's whole history in at
        # once) must not blow the budget just because it's the one being
        # looked at right now - skip it and keep looking for one that fits,
        # unless nothing has been included yet, in which case take it anyway
        # rather than returning no context at all.
        if entries and len(entry) > budget:
            continue

        budget -= len(entry)
        entries.append(entry)

    if not entries:
        return ""

    entries.reverse()  # oldest first again - chronological order for the model

    return "\n\n---\n\n".join(entries)
