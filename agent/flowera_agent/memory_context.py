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
    Load all persisted worker memory files so the agent can reason
    with its learned workflows continuously.
    """
    entries = []

    for path in sorted(config.MEMORY_DIR.glob("*.txt"), key=lambda p: p.name):
        try:
            content = path.read_text(encoding="utf-8").strip()
        except Exception as exc:
            log(f"Could not read memory file {path}: {exc}")
            continue

        if not content:
            continue

        entries.append(
            f"Memory file: {path.name}\n\n{content}"
        )

    if not entries:
        return ""

    return "\n\n---\n\n".join(entries)
