"""
The guards every door to the screen applies before core.act, in one place: the `gui` command (cli.py, TheBot's door)
calls them, and so would any later door (the MCP server, Claude Code's door, went with the legacy harnesses, O-4).

Found in review (2026-11-03, D-23): the guards lived in cli.py, so the MCP door - the strongest engine's - pressed a
named control by its point, took a guessed ref, clicked a native drop-down open, walked forty Tabs and typed a lone
"\\f" as characters, where `gui` refused each; and its looks and checks never reached the evidence the final check
reads. Argument parsing stays with each door; what is refused, and what is noted, is the same for both.
"""

from decurix_agent.computer import core, shown
from decurix_agent.computer.errors import ComputerError

PRESSES = ("click", "double_click", "right_click")


def refuse(kind, ref=None, keys=None, text=None):
    """Raise ComputerError when an action is one no door does (a guessed ref, a click that opens a native drop-down,
    a blind walk of keys, a backslash sequence typing cannot type); else return. Nothing is done either way."""

    from decurix_agent.computer import cli

    if ref and shown.unseen(ref):
        raise ComputerError(f"{ref} was not in anything shown to you in this task - a ref is not a number to guess, and "
                            "a guessed one is another control. Nothing was done. Observe, and use the ref the look "
                            "gives the control you mean.")
    if kind == "click" and ref:
        cli._refuse_native_dropdown(ref)
    if kind == "key" and keys:
        cli._refuse_blind_walk(keys)
    if kind == "type" and text:
        escape = cli._unsupported_escape(text)
        if escape:
            raise ComputerError(f"the text holds {escape}, which typing cannot type as a key - it would put the two "
                                f"characters {escape} in the document. Nothing was typed. Type the text in parts and "
                                "press the key between them.")


def act(kind, **kwargs):
    """core.act behind refuse(), the way every door calls it: a press on a named control by its ref (named), and keys
    or typing aimed at no control checked against the window the task works in (keys_from_task)."""

    refuse(kind, ref=kwargs.get("ref"), keys=kwargs.get("keys"), text=kwargs.get("text"))
    if kind in PRESSES:
        kwargs.setdefault("named", True)
    if kind in ("type", "key"):
        kwargs.setdefault("keys_from_task", True)
    return core.act(kind, **kwargs)


def note_look(rendered, observation):
    """A look's text, kept as evidence of the task (evidence.note_look) - never failing the look."""

    try:
        from decurix_agent.computer import evidence
        evidence.note_look(rendered, observation)
    except Exception:  # noqa: BLE001 - evidence is kept beside the look, never instead of it
        pass


def note_check(text):
    """A check of the screen's result, kept as evidence (evidence.note) - never failing the check."""

    try:
        from decurix_agent.computer import evidence
        evidence.note("a check of the screen then - " + str(text)[:1200])
    except Exception:  # noqa: BLE001
        pass
