import base64
import os
import json
import sys
import traceback

from decurix_agent import logging_utils, progress
from decurix_agent.computer import core, replay, skill_engine, skills
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


# ============================================================
# The action layer as MCP tools, for Claude Code
#
# claude_harness.py starts `claude` with --mcp-config pointing here, so
# observe / act / verify / open show up as mcp__computer__* tools. Unlike the
# `gui` command the other two engines use, every result carries the
# screenshot as an image: Claude can actually read one, and seeing the screen
# is what lets it recover when the accessibility tree is incomplete.
#
# The protocol is MCP's stdio transport - newline-delimited JSON-RPC 2.0 -
# implemented with the standard library, because a desktop image has no MCP
# SDK in it. stdout is the protocol channel, so nothing else may print to it:
# logs go to a file.
# ============================================================

PROTOCOL_VERSION = "2025-06-18"

TREE_BUDGET = 12000

TOOLS = [
    {
        "name": "observe",
        "description": (
            "Look at this desktop's screen. Returns a screenshot and a list of the open windows "
            "and their interface elements, each with a ref (e12), role, name, value, state and "
            "centre coordinates (@x,y). Call this before acting, and whenever you are unsure what "
            "is on screen."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "window": {"type": "string", "description": "Only list elements of windows whose title contains this."},
            },
        },
    },
    {
        "name": "act",
        "description": (
            "Do one thing on the screen, then report what changed and return the new screenshot. "
            "Target an element by ref from the latest observe (preferred), or by role and name; use "
            "x/y screen coordinates only when no element fits. Actions: click, double_click, "
            "right_click, type (one field per call: replaces its text unless append is true; \\n is Enter, a Tab "
            "is refused - type the next field by its own ref), key "
            "(keys like [\"ctrl+s\"] or [\"Return\"]; sent to the target or the focused window), "
            "scroll (direction up/down/left/right, amount), select (pick an option in a dropdown - also one a page draws itself, and a field that suggests entries as you type, such as a customer or product: it types the value and clicks the matching suggestion), "
            "drag (from the target, or x/y, to end_x/end_y), upload (put files from this desktop into a file input - "
            "target the input or its Browse/Upload button; files are paths). "
            "Controls that send, pay, buy or publish are stopped for human approval: the result "
            "then starts with NEEDS_APPROVAL and nothing happened."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "action": {"type": "string", "enum": ["click", "double_click", "right_click", "type", "key", "scroll", "select", "drag", "upload"]},
                "ref": {"type": "string", "description": "Element ref from observe, e.g. e12."},
                "role": {"type": "string"},
                "name": {"type": "string"},
                "x": {"type": "integer"},
                "y": {"type": "integer"},
                "see": {"type": "string", "description": "For click/type on something with no ref: describe it "
                        "(\"the bell icon at the top right\") and a grounding model finds it on the screen - "
                        "better than x/y read off the picture. The result says which element it landed on."},
                "text": {"type": "string", "description": "For type."},
                "append": {"type": "boolean", "description": "For type: add to the field instead of replacing it."},
                "replace_all": {"type": "boolean", "description": "For type into a field that holds several lines "
                                "(a document): really replace all of it. Without this such a type is refused."},
                "keys": {"type": "array", "items": {"type": "string"}, "description": "For key."},
                "direction": {"type": "string", "enum": ["up", "down", "left", "right"]},
                "amount": {"type": "integer"},
                "option": {"type": "string", "description": "For select: the option's label."},
                "end_x": {"type": "integer", "description": "For drag: where to drop."},
                "end_y": {"type": "integer"},
                "files": {"type": "array", "items": {"type": "string"}, "description": "For upload: file paths on this desktop."},
                "checked": {"type": "boolean", "description": "For click/double_click on a control that saves or "
                            "submits a form: the form was listed by the click before and every value is right - send it."},
            },
            "required": ["action"],
        },
    },
    {
        "name": "recordings",
        "description": (
            "Recordings of work done on this desktop's screen: demonstrations a person recorded to show how "
            "something is done, and tasks agents finished. Without id, lists those matching query, best first. "
            "With id, returns that recording's steps, its parameters (the text each field took) and what each "
            "step should leave on the screen. When a demonstration matches your task, read it before working "
            "the task out yourself."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "id": {"type": "string"},
            },
        },
    },
    {
        "name": "replay",
        "description": (
            "Run a recording again with no model. Each step's element is found again by its path or role and "
            "name, and each step's expected result has to appear before the next step runs; it stops rather "
            "than guess. The outcome is one of verified, completed_unverified, halted_before_effect, halted, "
            "reconciliation_required (a consequential step ran but its result never appeared - do not repeat "
            "it), needs_human, or NEEDS_APPROVAL. params overrides the typed text by parameter name; secret "
            "parameters have to be given."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "id": {"type": "string"},
                "params": {"type": "object", "additionalProperties": {"type": "string"}},
                "from": {"type": "integer", "minimum": 1, "description": "Step to start at, to continue a paused run."},
            },
            "required": ["id"],
        },
    },
    {
        "name": "skills",
        "description": (
            "Tasks this desktop was taught by a person's demonstration and their answers to its questions. Without "
            "name, lists the skills matching query, best first. With name, returns that skill's steps, what it needs "
            "and how it is run. When a skill matches your task, run it with run_skill instead of working the task "
            "out yourself: it is faster, and it has already been confirmed by the person who taught it."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {"query": {"type": "string"}, "name": {"type": "string"}},
        },
    },
    {
        "name": "run_skill",
        "description": (
            "Do a taught task with no model. Same outcomes and approval rules as replay. If it needs a value it was "
            "not given (params lists them by name), it does nothing and says which: ask the human for it, then call "
            "again. A list parameter takes its items separated by ';'. It does not stop to ask on its own - when it "
            "halts, tell the human what it says."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string"},
                "params": {"type": "object", "additionalProperties": {"type": "string"}},
                "from": {"type": "integer", "minimum": 1, "description": "Step to start at, to continue a paused run."},
            },
            "required": ["name"],
        },
    },
    {
        "name": "verify",
        "description": (
            "Look at the screen again and report what changed since the last observation. "
            "Optionally check that some text, or an element with a role/name, is now on screen - "
            "use it to confirm an action really worked before reporting success."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "expect_text": {"type": "string"},
                "role": {"type": "string"},
                "name": {"type": "string"},
            },
        },
    },
    {
        "name": "open",
        "description": "Open a web page in Chromium (a URL), a document (a file path), or an app (writer, calc, mousepad, files, terminal).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string"},
                "new_tab": {"type": "boolean"},
            },
            "required": ["target"],
        },
    },
]


def _image(frame):
    return {"type": "image", "data": base64.b64encode(frame.png()).decode("ascii"), "mimeType": "image/png"}


def _text(text):
    return {"type": "text", "text": text}


def _absolute(files):
    """Upload paths as the browser needs them: absolute, ~ expanded, relative ones from the work dir."""

    if not files:
        return None
    from decurix_agent import config
    out = []
    for f in files if isinstance(files, list) else [files]:
        p = os.path.expanduser(str(f))
        out.append(p if os.path.isabs(p) else os.path.join(str(config.WORK_DIR), p))
    return out


def _run_answer(result, what, tool):
    """The answer to a replay or skill run: its outcome and text, and how to carry on from where it stopped."""

    text = f"outcome: {result['outcome']}\n{result['text']}"

    if result["outcome"] == replay.NEEDS_APPROVAL:
        return [_text(
            "NEEDS_APPROVAL: " + result["text"]
            + f"\n\nStop here. End your turn with a reply that begins NEEDS_APPROVAL: saying what the {what} "
            f"wants to do. When the human approves, call {tool} again with from={result['resume_from']}."
        )], False

    if result.get("resume_from") and result["outcome"] in (replay.HALTED, replay.NEEDS_HUMAN):
        text += f"\n\nTo carry on from there later, call {tool} with from={result['resume_from']}."

    return [_text(text)], result["outcome"] not in replay.SUCCESS


def call_tool(name, arguments):
    """Run one tool; return (content blocks, is_error)."""

    arguments = arguments or {}

    if name == "observe":
        observation = core.observe()
        core.report_observation(observation)
        tree = el.render(observation, budget=TREE_BUDGET, coords=True, only_group=arguments.get("window"))
        return [_text(tree), _image(observation["frame"])], False

    if name == "act":
        point = None
        if arguments.get("x") is not None and arguments.get("y") is not None:
            point = (int(arguments["x"]), int(arguments["y"]))

        keys = arguments.get("keys")
        if isinstance(keys, str):
            keys = [keys]

        end = None
        if arguments.get("end_x") is not None and arguments.get("end_y") is not None:
            end = (int(arguments["end_x"]), int(arguments["end_y"]))

        result = core.act(
            arguments.get("action"),
            ref=arguments.get("ref"),
            role=arguments.get("role"),
            name=arguments.get("name"),
            point=point,
            see=arguments.get("see"),
            text=arguments.get("text"),
            keys=keys,
            direction=arguments.get("direction"),
            amount=arguments.get("amount"),
            option=arguments.get("option"),
            append=bool(arguments.get("append")),
            replace_all=bool(arguments.get("replace_all")),
            end=end,
            files=_absolute(arguments.get("files")),
            # The same pre-save checks as the gui command's (core._commit_check): a right-click commits nothing.
            check_form=(False if arguments.get("action") == "right_click"
                        else "checked" if arguments.get("checked") else True),
        )

        if result["status"] == "needs_approval":
            return [_text(
                result["text"]
                + "\n\nStop here: do not try another way to do this. End your turn with a reply "
                "that begins NEEDS_APPROVAL: followed by exactly what you want to do and why. "
                "When the human approves, repeat this same action and it will go through."
            )], False

        observation = result["observation"]
        tree = el.render(observation, budget=TREE_BUDGET, coords=True)
        return [_text(result["text"] + "\n\n" + tree), _image(observation["frame"])], False

    if name == "verify":
        result = core.verify(
            expect_text=arguments.get("expect_text"),
            role=arguments.get("role"),
            name=arguments.get("name"),
        )
        return [_text(result["text"]), _image(result["observation"]["frame"])], result["ok"] is False

    if name == "open":
        summary = core.open_target(arguments.get("target"), new_tab=bool(arguments.get("new_tab")))
        observation = core.observe()
        tree = el.render(observation, budget=TREE_BUDGET, coords=True)
        return [_text(summary + "\n\n" + tree), _image(observation["frame"])], False

    if name in ("recordings", "replay") and arguments.get("id"):
        try:
            meta, program = replay.load_program(arguments["id"])
        except (FileNotFoundError, ValueError):
            return [_text(f"there is no recording {arguments['id']!r} on this desktop")], True

        if name == "recordings":
            return [_text(replay.render_text(meta, program, budget=TREE_BUDGET))], False

        from decurix_agent import guidance

        params = guidance.with_run_values(arguments.get("params") or {}, program.get("params"), recording=arguments["id"])
        result = replay.run(program, params=params, start=arguments.get("from") or 1)
        return _run_answer(result, "replay", "replay")

    if name in ("skills", "run_skill"):
        wanted = arguments.get("name") or ("" if name == "skills" else None)

        if name == "skills" and not wanted:
            found = skills.search(arguments.get("query") or "")
            if not found:
                return [_text("No skills on this desktop match that.")], False
            return [_text("\n".join(skill_engine.summary_line(s) for s in found))], False

        skill, candidates = skills.find(wanted or "")
        if skill is None:
            names = ", ".join(s["name"] for s in candidates)
            return [_text(f"more than one skill fits {wanted!r}: {names}. Use its exact name." if candidates
                          else f"there is no skill {wanted!r} on this desktop")], True

        if name == "skills":
            return [_text(skill_engine.render_text(skill, budget=TREE_BUDGET))], False

        from decurix_agent import guidance

        params = guidance.with_run_values(arguments.get("params") or {}, skill.get("params"), skill=skill["name"])
        result = skill_engine.run(skill, params=params, start=arguments.get("from") or 1)
        return _run_answer(result, "skill", "run_skill")

    if name == "recordings":
        found = replay.search(arguments.get("query") or "")
        if not found:
            return [_text("No recordings on this desktop match that.")], False
        return [_text("\n".join(
            f"{m['id']} {el._quote(m.get('title') or '(untitled)', 80)} - {m.get('kind')}, {m.get('event_count', 0)} actions"
            for m in found
        ))], False

    return [_text(f"unknown tool {name!r}")], True


def handle(message):
    """Answer one JSON-RPC message, or return None for a notification."""

    method = message.get("method")
    msg_id = message.get("id")
    params = message.get("params") or {}

    if msg_id is None:
        return None

    if method == "initialize":
        return {
            "jsonrpc": "2.0", "id": msg_id,
            "result": {
                "protocolVersion": params.get("protocolVersion") or PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "decurix-computer", "version": "1.0"},
            },
        }

    if method == "ping":
        return {"jsonrpc": "2.0", "id": msg_id, "result": {}}

    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": msg_id, "result": {"tools": TOOLS}}

    if method == "tools/call":
        try:
            content, is_error = call_tool(params.get("name"), params.get("arguments"))
        except ComputerError as exc:
            content, is_error = [_text(str(exc))], True
        except Exception as exc:
            logging_utils.log(traceback.format_exc())
            content, is_error = [_text(f"internal error: {type(exc).__name__}: {exc}")], True

        return {"jsonrpc": "2.0", "id": msg_id, "result": {"content": content, "isError": is_error}}

    return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32601, "message": f"method not found: {method}"}}


def main():
    logging_utils.log_to_file(logging_utils.log_file("decurix-mcp.log"))

    try:
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue

            try:
                message = json.loads(line)
            except ValueError:
                continue

            reply = handle(message)

            if reply is not None:
                sys.stdout.write(json.dumps(reply) + "\n")
                sys.stdout.flush()
    finally:
        # Unlike cli.py's gui command, this process stays alive across many
        # tool calls for the whole turn - flushing after every single one
        # would block Claude's tool-call turnaround on the control plane
        # under any latency, contradicting progress.emit's own "never slows
        # down the thing it is describing" contract. Flush once, here, when
        # the server itself is about to exit (stdin closed), so the last
        # queued step still isn't lost.
        progress.flush()


if __name__ == "__main__":
    main()
