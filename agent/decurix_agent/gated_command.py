"""
A shell command held for a person's yes, on a desktop with an approval gate (review 2026-11-03, F-09).

The legacy harnesses (Azure, the Ollama task runner) decided that yes in their own process: a reading of the reply,
plus a word list. The approval gate is the only thing that may make a yes on a gated desktop (CLAUDE.md, gate-boundary),
and it already holds commands the way it holds screen actions (approval.from_material, kind "command": what TheBot's
soul uses). Here the held command is put to the gate, the person is asked in the gate's own words (messaging attaches
the gate's signed question to an approval note), and the command runs only when the gate, asked again for exactly the
same command, finds the one-shot grant the person's signed reply made. A gate that does not answer allows nothing.

Without a gate (an old desktop, a development machine) the caller keeps its own reading, as before.
"""

from decurix_agent.computer import approval

NOT_RUN = ("Not run: this desktop's approval gate did not answer, so nothing that needs a yes can run here. "
           "Recreate the desktop if this goes on.")


def gated():
    """Whether a held command's yes is the gate's to make on this desktop."""

    return approval.remote()


def material(command, request="", workdir=""):
    """The command as the gate makes its fingerprint and words from it - the same shape soul sends."""

    return {"kind": "command", "command": str(command or ""), "request": str(request or ""),
            "workdir": str(workdir or "")}


def check(held):
    """
    The gate's decision on the held command `held` (material()): "allowed" (a grant for exactly it was there, and is now
    used), "asked" (the gate holds it as its pending request; ask the person with kind "approval") or "no gate" (the gate
    did not answer: nothing runs). Never raises.
    """

    try:
        said = approval.gate_call("check", material=dict(held), reason="it needs your approval")
    except Exception:  # noqa: BLE001 - a gate that does not answer allows nothing
        return "no gate"
    if said.get("allowed"):
        return "allowed"
    return "asked" if isinstance(said.get("request"), dict) else "no gate"


def granted(held, reply, question):
    """
    Whether the person's reply to a question asked for `held` (material(), asked with kind "approval" after check()
    said "asked") is a yes for exactly it. With an approval gate, only the gate's one-shot grant counts - the reply's
    words never do. Without a gate, a plain yes with no condition in the reply's one reading (approval.reading), as
    an approval's. The caller makes sure the reply came as the reply to that question (watchers.is_reply_file).
    Never raises.
    """

    if gated():
        return check(held) == "allowed"
    try:
        said = approval.reading(reply, question)
    except Exception:  # noqa: BLE001 - a reply that cannot be read is no yes
        return False
    return said.get("decision") == "yes" and not (said.get("condition") or "").strip()
