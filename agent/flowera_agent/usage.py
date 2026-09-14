import threading


# ============================================================
# What the current task has used
#
# The control plane's task records keep, per execution, the model that did
# the work, the tokens it read and wrote, and what it cost. Each harness
# adds what its engine reports - the Ollama loop the token counts on every
# /api/chat, Claude Code the cost and usage on each run's result event -
# and messaging.send_helptext sends the running totals with every reply.
#
# Totals, not deltas: a task that pauses for a human and resumes is one
# execution across several engine runs, so each reply carries everything
# so far and the control plane keeps the latest.
# ============================================================

_lock = threading.Lock()
_totals = {}


def reset():
    """Start counting for a new task."""

    with _lock:
        _totals.clear()


def set_model(model):
    if not model:
        return

    with _lock:
        _totals["model"] = str(model)


def add(input_tokens=None, output_tokens=None, cost_usd=None):
    """
    Add one engine call's usage. A None leaves that figure unknown rather
    than zero: a local model's cost is a known $0, a count nobody reported
    is not.
    """

    with _lock:
        for key, value in (
            ("input_tokens", input_tokens),
            ("output_tokens", output_tokens),
            ("cost_usd", cost_usd),
        ):
            if value is None:
                continue

            try:
                number = float(value) if key == "cost_usd" else int(value)
            except (TypeError, ValueError):
                continue

            if number < 0:
                continue

            _totals[key] = _totals.get(key, 0) + number


def snapshot():
    """The totals so far, as the fields the control plane's callback takes."""

    with _lock:
        totals = dict(_totals)

    if "cost_usd" in totals:
        totals["cost_usd"] = round(totals["cost_usd"], 6)

    return totals
