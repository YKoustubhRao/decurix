"""
decurix_tools: the worker's READ-ONLY tools, called from a Python script the worker writes (or a script the worker
distilled from its own commands, playbooks.py), with parsed results.

    import decurix_tools as dt
    doc = dt.office_read("~/work/inbox/bill.pdf")      # {"text": ..., "images": [...]}
    screen = dt.observe()                              # {"text": ..., "elements": [{"ref", "role", "name", ...}]}

It runs the same shell commands the worker runs (`gui`, `office`, `lookup`) and reads what they print. Only reading
verbs are offered: gui observe, read, records, recall, recordings, recording, skills and skill; office read; lookup.
Every action - a click, typing, a key, an open, a save, a replay, a skill run, a record declared, an office update,
an email, a handoff - is refused here before anything runs (ToolRefused): an action is the engine's own command, under
the approval gate, never a script's.

What it returns are CLAIMS, not evidence. A script's reading of a document or a screen is what the script says it saw;
the final check still reads back every file the script produced and decides by itself whether the work is done.

Two rules a script cannot change:
- a budget of calls per script run (BUDGET, 50; DECURIX_TOOLS_BUDGET may only lower it). Past it every call raises
  BudgetSpent;
- if a call ever exits 3 (gui's "approval needed"), what the command printed is printed unchanged and the script ends
  at once with exit code 3 (os._exit: no try/except, finally or atexit in the script runs), so the approval reaches
  the engine and the person exactly as gui said it.

Standard library only; it imports nothing of the agent. The technique (a generated stub module exposing an
allow-listed subset of the agent's tools to code it runs, with a call budget) is hermes-agent's
(tools/code_execution_tool.py, SANDBOX_ALLOWED_TOOLS and max_tool_calls); here the stubs call the shell commands.
"""

import os
import re
import subprocess
import sys

__all__ = ["observe", "read", "records", "recall", "recall_runs", "recordings", "recording", "skills", "skill", "office_read",
           "lookup", "lookup_read", "gui", "office", "calls_left", "ToolError", "ToolRefused", "BudgetSpent"]

# The calls one script run may make.
BUDGET = 50
# gui's exit code for "a person must approve this first" (decurix_agent.computer.approval.EXIT_NEEDS_APPROVAL).
EXIT_NEEDS_APPROVAL = 3
# How long one call may take, in seconds (a look waits for a page to settle).
TIMEOUT = 180

# How each command is run: the commands on the desktop's PATH. (A test points these at stand-ins.)
COMMANDS = {"gui": ["gui"], "office": ["office"], "lookup": ["lookup"]}

# The reading verbs, and the flags each may take. Anything else is refused.
GUI_READ = {
    "observe": {"--window", "--coords"},  # (never --screenshot: that writes a file)
    "read": {"--role", "--name"},
    "records": set(),
    "recall": {"--runs", "--since", "--task", "--around"},  # past runs: this desktop's own, read-only
    "recordings": set(),
    "recording": set(),
    "skills": set(),
    "skill": set(),  # (never --look: it shows a picture, which is the engine's)
}
OFFICE_READ = {"read": set()}

# An element's line (gui's elements.element_line): `e12 push button "Save" value="x" [focused]` - a name or a value is
# always quoted, with no quote mark inside it.
_ELEMENT = re.compile(r'^\s*(?P<ref>e\d+) (?P<role>[^"\[@(]+?)(?= "| value=| \[| \(| @|$)(?: "(?P<name>[^"]*)")?'
                      r'(?: value="(?P<value>[^"]*)")?(?P<rest>.*)$')
_STATES = re.compile(r"^ \[([^\]]*)\]")
_RECORDING = re.compile(r'^(?P<id>\S+) "(?P<title>[^"]*)" - (?P<kind>.*?), (?P<actions>\d+) actions$')
_PAGE_IMAGE = re.compile(r"^\[page image: (.+)\]$")


class ToolError(RuntimeError):
    """A call that could not be made, or whose command failed (its exit code and what it printed)."""

    def __init__(self, message, code=None, output=""):
        super().__init__(message)
        self.code, self.output = code, output


class ToolRefused(ToolError):
    """A call this module never makes: an action, or a flag that writes."""


class BudgetSpent(ToolError):
    """This script run has made all the calls it may."""


def _budget():
    try:
        wanted = int(os.environ.get("DECURIX_TOOLS_BUDGET", BUDGET))
    except ValueError:
        wanted = BUDGET
    return max(0, min(BUDGET, wanted))


_calls = {"made": 0}


def calls_left():
    """How many calls this script run may still make."""

    return max(0, _budget() - _calls["made"])


def _check(tool, verb, args, allowed):
    """Refuse a verb that is not a reading one, and a flag the verb does not take."""

    if verb not in allowed:
        raise ToolRefused(f"{tool} {verb} is not offered to scripts: only {', '.join(sorted(allowed))} (reading). An "
                          "action is the engine's own command, under the approval gate.")
    for arg in args:
        flag = str(arg).split("=", 1)[0]
        if flag.startswith("--") and flag not in allowed[verb]:
            raise ToolRefused(f"{tool} {verb} {flag} is not offered to scripts")


def _run(tool, argv):
    """Run one command; its stdout. Spends one call of the budget. Exit 3 ends the script (see the module's doc)."""

    if _calls["made"] >= _budget():
        raise BudgetSpent(f"this script has made its {_budget()} calls; do the rest as the engine's own commands")
    _calls["made"] += 1
    try:
        done = subprocess.run(COMMANDS[tool] + [str(a) for a in argv], capture_output=True, text=True,
                              encoding="utf-8", errors="replace", timeout=TIMEOUT, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        raise ToolError(f"{tool} {argv[0]} did not finish in {TIMEOUT} seconds") from None
    except OSError as exc:
        raise ToolError(f"{tool} could not be run ({exc})") from None
    if done.returncode == EXIT_NEEDS_APPROVAL:
        try:
            sys.stdout.write(done.stdout or "")
            sys.stdout.flush()
        finally:
            os._exit(EXIT_NEEDS_APPROVAL)
    if done.returncode != 0:
        said = ((done.stdout or "") + (done.stderr or "")).strip()
        raise ToolError(f"{tool} {argv[0]} exited {done.returncode}: {said[-600:]}", done.returncode, said)
    return done.stdout or ""


def gui(verb, *args):
    """A reading gui command as written (`gui(\"read\", \"e5\")`): what it printed. Anything else is refused."""

    _check("gui", verb, args, GUI_READ)
    return _run("gui", [verb, *args])


def office(verb, *args):
    """A reading office command as written (`office(\"read\", path)`): what it printed. Anything else is refused."""

    _check("office", verb, args, OFFICE_READ)
    return _run("office", [verb, *args])


def _element(line):
    m = _ELEMENT.match(line)
    if not m or not m.group("role").strip():
        return None
    states = _STATES.search(m.group("rest"))
    return {"ref": m.group("ref"), "role": m.group("role").strip(), "name": m.group("name") or "",
            "value": m.group("value"), "states": [s.strip() for s in states.group(1).split(",")] if states else []}


def observe(window=None, coords=False):
    """`gui observe`: {"text": what it printed, "elements": [{"ref", "role", "name", "value", "states"}]}, the elements
    read from its lines (a folded toolbar or sheet row stays in the text only)."""

    args = (["--window", window] if window else []) + (["--coords"] if coords else [])
    text = gui("observe", *args)
    elements = [e for e in (_element(line) for line in text.splitlines()) if e]
    return {"text": text, "elements": elements}


def read(target=None, role=None, name=None):
    """`gui read`: the text of the screen, a window or an element (by ref, or by role and name)."""

    args = ([target] if target else []) + (["--role", role] if role else []) + (["--name", name] if name else [])
    return gui("read", *args)


def records():
    """`gui records`: the record ledger's lines."""

    return [line for line in gui("records").splitlines() if line.strip()]


def recall(words):
    """`gui recall WORDS`: what this worker's memory holds with those words."""

    return gui("recall", *str(words).split())


def recall_runs(words="", since=None, task=None, around=None):
    """`gui recall --runs`: this desktop's own past runs - the ones matching WORDS (since a time like 7d), or one run
    by task id (around one of its texts). Raw stored text, never a summary."""

    args = ["--runs"] + (["--task", str(task)] if task else []) + (["--around", str(around)] if around is not None else [])
    args += (["--since", str(since)] if since else []) + str(words).split()
    return gui("recall", *args)


def recordings(words=""):
    """`gui recordings [WORDS]`: [{"id", "title", "kind", "actions"}] (empty when there are none)."""

    out = []
    for line in gui("recordings", *str(words or "").split()).splitlines():
        m = _RECORDING.match(line.strip())
        if m:
            out.append({"id": m.group("id"), "title": m.group("title"), "kind": m.group("kind"),
                        "actions": int(m.group("actions"))})
    return out


def recording(session):
    """`gui recording ID`: one recording's steps, as text."""

    return gui("recording", session)


def skills(words=""):
    """`gui skills [WORDS]`: one line per skill (empty when there are none)."""

    lines = gui("skills", *str(words or "").split()).splitlines()
    return [line for line in lines if line.strip() and not line.startswith(("`gui ", "No skills"))]


def skill(name):
    """`gui skill NAME`: one skill's steps, as text."""

    return gui("skill", name)


def office_read(path, pages=None):
    """`office read PATH [PAGES]`: {"text": the document's text, "images": [the page images it names]}."""

    printed = office("read", path, *([str(pages)] if pages else []))
    images, lines = [], []
    for line in printed.splitlines():
        m = _PAGE_IMAGE.match(line.strip())
        if m:
            images.append(m.group(1))
        else:
            lines.append(line)
    return {"text": "\n".join(lines).rstrip("\n"), "images": images}


def lookup(question):
    """`lookup "QUESTION"`: what the web says (information, never a value to type)."""

    return _run("lookup", [str(question)])


def lookup_read(url, question=""):
    """`lookup --read URL [QUESTION]`: one page's text."""

    return _run("lookup", ["--read", str(url)] + ([str(question)] if question else []))
