"""
The PreToolUse hook the Claude harness gives Claude Code (claude_harness.GUARD_SETTINGS): it refuses a tool call that
would drive the screen or the browser around the approval gate, or touch the gate's own state and channels, and tells
the model to use the computer tools instead.

Claude Code runs headless with every tool allowed (claude_harness), so its Bash could `xdotool` a click on Pay, drive
Chrome over its DevTools port, call the agent's or soul's ports on localhost, or write a reply or a grant into the
files the gate reads (review 2026-11-02). This hook refuses those calls:

  - programs that send input or drive windows: xdotool, xte, ydotool, wmctrl, xinput's test events;
  - the browser's DevTools port (9222), the agent's own ports (8080, 8081, 4900) and the approval gate's reply port
    (8083) on this machine;
  - the gate's state and channels: ~/.decurix/command (the human's replies), ~/.decurix/computer (approval.json, the
    form-check note, the screen's state), the task file, the agent's token.

It also sees the computer tools' own calls (mcp__computer__*): a call it cannot read is refused, and `open` may not
name a program to run, this desktop's own ports, or the gate's state (review 2026-11-03, D-05: the hook did not see
the MCP door at all). What is typed or clicked goes through the approval gate in the tool itself.

THIS IS A GUARD RAIL, NOT A BOUNDARY. It reads the words of a command; `x"do"tool`, a script written to a file and run,
or Python's socket module get past it, because the shell Claude runs is the same Linux user as the worker. The
approval gate does run as a separate Linux user (decurix-entrypoint.sh), so a yes cannot be made from here; but the
screen, the browser and the worker's own files can still be reached directly, which this only makes harder. What this
does is stop the plain ways a model reaches for, and say why, so it uses the gate's own tools.

Standard library only: Claude Code runs it as a plain script. It fails closed - a call it cannot read is refused.
Exit 2 with the reason on stderr is how a PreToolUse hook blocks a call (Claude Code shows the reason to the model).
"""

import json
import re
import sys

USE_GUI = ("Use the computer tools (mcp__computer__observe / act / verify / open) or the `gui` command for anything "
           "on the screen or in the browser: they go through the approval gate, which this does not.")

# Programs that send keys, clicks or window commands to the display directly.
INPUT_PROGRAMS = re.compile(r"(?<![\w-])(?:xdotool|xte|ydotool|wmctrl|xinput\s+test|xautomation|pyautogui|pynput)"
                            r"(?![\w-])", re.IGNORECASE)
# The browser's DevTools port, the agent's and soul's ports and the gate's, on this machine (any spelling of it) or bare.
LOCAL_PORTS = re.compile(r"(?:[:=\s/]|^)(?:9222|8080|8081|4900|8083)(?![\d])|remote-debugging|devtools/(?:page|browser)",
                         re.IGNORECASE)
# The gate's state and channels.
GATE_STATE = re.compile(r"\.decurix/(?:command|computer)|approval\.(?:json|lock)|-reply\.txt|decurix-current-task|"
                        r"decurix-form-check|DECURIX_AGENT_TOKEN|chrome-devtools|\.decurix/(?:secrets|privacy)",
                        re.IGNORECASE)

FILE_TOOLS = {"Write", "Edit", "MultiEdit", "NotebookEdit"}


def verdict(call):
    """Why this tool call is refused, or "" to let it run."""

    tool = str(call.get("tool_name") or "")
    given = call.get("tool_input") or {}
    if not isinstance(given, dict):
        return "the tool call could not be read, so it is not run"
    if tool == "Bash":
        command = str(given.get("command") or "")
        if INPUT_PROGRAMS.search(command):
            return f"this command sends input to the screen directly ({INPUT_PROGRAMS.search(command).group(0)}). " + USE_GUI
        if LOCAL_PORTS.search(command):
            return ("this command reaches the browser's DevTools port or one of this desktop's own agent ports. "
                    + USE_GUI)
        if GATE_STATE.search(command):
            return ("this command touches the approval gate's own state or the person's reply channel, which only the "
                    "agent writes. " + USE_GUI + " To ask the person, end your reply with NEEDS_APPROVAL: or a question.")
        return ""
    if tool in FILE_TOOLS:
        path = str(given.get("file_path") or given.get("notebook_path") or "")
        if GATE_STATE.search(path):
            return "this file belongs to the approval gate or the person's reply channel, which only the agent writes."
        return ""
    if tool == "WebFetch":
        url = str(given.get("url") or "")
        if re.search(r"//(?:localhost|127\.\d+\.\d+\.\d+|0\.0\.0\.0|\[::1?\])(?::\d+)?", url, re.IGNORECASE) \
                or LOCAL_PORTS.search(url):
            return "this fetches from this desktop's own agent or browser ports. " + USE_GUI
    if tool.startswith(COMPUTER_TOOLS):
        return _computer_verdict(tool[len(COMPUTER_TOOLS):], given)
    return ""


# The computer MCP server's tools (decurix_agent/computer/mcp_server.py), as Claude Code names them.
COMPUTER_TOOLS = "mcp__computer__"


def _computer_verdict(name, given):
    """Why a computer tool's call is refused, or "". What it types or presses is the approval gate's to decide (inside
    the tool); here only `open`'s target is read, which names a page, a file or an app - never a program to run."""

    if any(not isinstance(value, (str, int, float, bool, type(None), list, dict)) for value in given.values()):
        return "the tool call could not be read, so it is not run"
    if name != "open":
        return ""
    target = given.get("target")
    if not isinstance(target, str) or not target.strip():
        return "open needs a target: a web address, a file or an app's name"
    if INPUT_PROGRAMS.search(target):
        return f"open was given a program that sends input to the screen ({INPUT_PROGRAMS.search(target).group(0)}). " \
               + USE_GUI
    if GATE_STATE.search(target):
        return "open was given the approval gate's own state or the person's reply channel, which only the agent opens."
    if re.search(r"//(?:localhost|127\.\d+\.\d+\.\d+|0\.0\.0\.0|\[::1?\])(?::\d+)?", target, re.IGNORECASE) \
            or LOCAL_PORTS.search(target):
        return "open was given this desktop's own agent or browser ports. " + USE_GUI
    return ""


def main():
    try:
        call = json.loads(sys.stdin.read() or "{}")
        why = verdict(call if isinstance(call, dict) else {})
    except Exception as exc:  # noqa: BLE001 - fail closed: a call the guard cannot read is not run
        why = f"the guard could not read this tool call ({type(exc).__name__}), so it is not run"
    if why:
        sys.stderr.write("Refused by this desktop's guard: " + why + "\n")
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
