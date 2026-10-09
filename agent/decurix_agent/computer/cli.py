import argparse
import os
import re
import sys

from decurix_agent import logging_utils, progress
from decurix_agent.computer import approval, core, replay, skill_engine, skills
from decurix_agent.computer import elements as el
from decurix_agent.computer.errors import ComputerError


# ============================================================
# `gui` - the screen as a shell command
#
# How the Ollama loop and TheBot's operator see and work the screen: they
# already run one shell command per turn and read what it prints, so the
# action layer is just one more command, with no change to either engine's
# COMMAND:/DONE:/HELP: contract. What it prints is text - the element list,
# and after an action what changed - because a 3B model on a CPU cannot read
# a screenshot, and pretending otherwise would be the fastest way to make it
# click the wrong thing.
#
# An engine whose model CAN see asks for the screen too: with
# DECURIX_GUI_SCREENSHOT set (TheBot's operator sets it for a vision brain),
# every command that looks at or acts on the screen leaves a PNG of the screen
# as it ended up there, shrunk by DECURIX_GUI_SCREENSHOT_SCALE, for the engine
# to attach to its next turn. The text is printed either way.
#
# Exit codes are part of the contract, and harnesses read them:
#   0 done   1 could not do it   3 NEEDS_APPROVAL (nothing happened)   4 verify failed
# ============================================================

EXIT_OK = 0
EXIT_ERROR = 1
EXIT_NEEDS_APPROVAL = approval.EXIT_NEEDS_APPROVAL
EXIT_VERIFY_FAILED = 4

TEXT_BUDGET = int(os.environ.get("DECURIX_GUI_TEXT_CHARS", "3500"))

# The commands that look at or act on the screen: after these, a vision
# engine is shown the screen (save_screenshot).
SCREEN_COMMANDS = {
    "observe", "click", "double-click", "right-click", "type", "key", "scroll",
    "select", "open", "upload", "drag", "verify", "replay", "do", "front",
}


def _look(skill, n):
    """
    `gui skill NAME --look N`: the screen the person did step N on, left where a
    vision engine is shown a command's picture (DECURIX_GUI_SCREENSHOT) - so a
    model doing a taught task can see what a step recorded as a point on the
    screen was pressing, instead of guessing from coordinates.
    """

    from decurix_agent.computer import evidence

    step = next((s for s in skill["steps"] if s["n"] == n), None)
    if step is None:
        raise ComputerError(f"{skill['name']} has steps 1 to {len(skill['steps'])}, not {n}.")

    png = evidence.step_picture(step)
    if png is None:
        raise ComputerError(f"the recording step {n} came from is not on this desktop any more, so there is no picture of it.")

    print(f"Step {n} of {skill['name']}: {step.get('summary')}.")
    if step.get("intent"):
        print(f"The person said: {step['intent']}")
    if step.get("point"):
        print(f"The red ring marks where the person pressed ({step['point'][0]},{step['point'][1]}).")
    print("The picture with this result is the RECORDING's screen before that step, not the screen now - "
          "run gui observe to see the screen now.")

    path = os.environ.get("DECURIX_GUI_SCREENSHOT", "").strip()
    if path:
        try:
            tmp = path + ".part"
            with open(tmp, "wb") as f:
                f.write(png)
            os.replace(tmp, path)
        except OSError as exc:
            print(f"(The picture could not be attached: {exc})")
    else:
        print("(This engine is not shown pictures; the step's description above is all there is.)")
    return EXIT_OK


def save_screenshot(argv):
    """
    Leave the screen where DECURIX_GUI_SCREENSHOT says, after a screen command.

    Taken after the command, whether it worked or not: a click that "could
    not find e12" is exactly when seeing the screen helps. Never raises -
    the picture is extra, the command's own result is what counts.
    """

    path = os.environ.get("DECURIX_GUI_SCREENSHOT", "").strip()
    if not path or not argv or argv[0] not in SCREEN_COMMANDS:
        return

    try:
        scale = max(1, int(os.environ.get("DECURIX_GUI_SCREENSHOT_SCALE", "1")))
    except ValueError:
        scale = 1

    try:
        from decurix_agent.computer import screen

        frame = screen.grab()
        if scale == 1 and os.environ.get("DECURIX_GUI_RULER", "") == "1":
            # So a vision model can say where something is (pointing.py).
            from decurix_agent.computer import pointing

            frame = pointing.ruled(frame)
        data = frame.png(scale=scale)
        tmp = path + ".part"
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, path)
    except Exception as exc:
        logging_utils.log(f"gui: could not save the screenshot: {exc}")

USAGE = """gui - see and use this desktop's screen

  gui observe                    list windows and elements, each with a ref like e12
  gui observe --window TITLE     one window's elements in full
  gui click TARGET               click (also: double-click, right-click)
  gui click --see "DESCRIPTION"  click what the description says, found by looking at the
                                 screen ("the bell icon at the top right"): for what has no
                                 ref. Also gui type --see "the Notes field" "text"
  gui type TARGET "text"         replace a field's text (--append to add; \\n = new line)
                                 A field holding several lines is a document: replacing
                                 all of it needs --replace-all
  gui type "text"                type into whatever has focus
  gui read [TARGET]              the whole text of a field or document, with line numbers
                                 (observe shows only its first 60 characters)
  gui key KEYS...                press keys: enter, tab, ctrl+s, alt+f4 ...
  gui scroll [TARGET] down [N]   scroll up/down/left/right, N notches (about 100 px each)
  gui select TARGET "option"     choose an option in a dropdown
  gui open URL|FILE|APP          open a web page, a document, or an app
  gui front TITLE                bring an open window to the front (a few words of its title)
  gui upload TARGET FILE...      upload file(s) to a file input element
  gui drag TARGET X,Y            drag an element (or a point) and drop it at X,Y
  gui verify [--text T]          look again: what changed, is T on screen?

  gui recordings [WORDS]         recorded demonstrations and tasks, best match first
  gui recording ID               one recording's steps and parameters
  gui replay ID [--param NAME=VALUE]... [--from N]
                                 run a recording again, with no model

  gui skills [WORDS]             tasks this desktop was taught, best match first
  gui skill NAME                 one skill's steps and what it needs
  gui skill NAME --look N        the screen the person did step N on (the spot
                                 they pressed ringed), as this command's picture
  gui do NAME [--param NAME=VALUE]... [--from N]
                                 do a taught task, with no model

  gui ask SERVICE "QUESTION"     put one question to chatgpt, google, duckduckgo,
                                 wikipedia or youtube and print its answer

TARGET is a ref from observe (e12), a role and name ('button "Save"'),
a name ("Save"), or screen coordinates (640,360) as a last resort.
Buttons that send, pay, buy or publish stop for human approval (exit 3).
"""


# One wheel notch moves a page about 100 px; an amount above NOTCHES_UP_TO is read as pixels.
# Engines write `gui scroll X,Y 420` meaning pixels and `-522` meaning up: as notches, 420 was
# a 13-second burst that threw the page to its end, and a negative amount was not read at all.
NOTCHES_UP_TO = 30
PIXELS_PER_NOTCH = 100
MAX_NOTCHES = 25


def scroll_words(words):
    """`gui scroll`'s words -> (target words, direction, notches or None)."""
    words = list(words)
    amount = None
    if words and re.fullmatch(r"[+-]?\d+", words[-1]):
        amount = int(words.pop())
    direction = None
    if words and words[-1].lower() in ("up", "down", "left", "right"):
        direction = words.pop().lower()
    if amount is not None and amount < 0:
        amount = -amount
        direction = {"down": "up", "up": "down", "right": "left", "left": "right"}.get(direction or "down")
    if amount is not None:
        if amount > NOTCHES_UP_TO:
            amount = round(amount / PIXELS_PER_NOTCH)
        amount = min(max(amount, 1), MAX_NOTCHES)
    return words, direction or "down", amount


def _target(value, args):
    """Turn a TARGET argument (plus --role/--name) into act() keywords."""

    keywords = {}

    if getattr(args, "role", None):
        keywords["role"] = args.role
    if getattr(args, "name", None):
        keywords["name"] = args.name

    if value is None:
        return keywords

    value = value.strip()
    if value.lower().startswith("at ") and el.parse_point(value[3:].strip()) is not None:
        value = value[3:].strip()
    point = el.parse_point(value)

    if el.REF_RE.match(value):
        keywords["ref"] = value
    elif point:
        keywords["point"] = point
    elif "role" not in keywords and _is_role_word(value):
        # `gui type textbox "hi"` - the only text field on screen.
        keywords["role"] = value
    else:
        role, name = _split_role_name(value)
        if role:
            keywords["role"] = role
        keywords["name"] = name

    return keywords


def _split_role_name(value):
    """'button "Save"' -> ("button", "Save"); 'Save' -> (None, "Save")."""

    for quote in ('"', "'"):
        if value.endswith(quote) and quote in value[:-1]:
            start = value.index(quote)
            role = value[:start].strip()
            name = value[start + 1:-1]
            if role and el.normalize_role(role) and len(role.split()) <= 3:
                return role, name
    return None, value.strip("\"'")


def _is_role_word(value):
    word = value.strip().lower()
    return word in el.INTERACTIVE_ROLES or el.ROLE_ALIASES.get(word) in el.INTERACTIVE_ROLES


def _strip_descriptor(words):
    """
    Drop the element description a model copies after a ref.

    observe lists `e11 checkbox "Maintenance mode"`, and a small model writes
    that whole line back: `gui click e11 checkbox "Maintenance mode"`, or
    `gui type e10 textbox "Site title" "Decurix Test Site"`. The ref already
    says which element; the role word, and the name when it is that ref's
    name in the last observation, are removed so what is left is the command
    that was meant.
    """

    if len(words) < 2 or not el.REF_RE.match(words[0]):
        return words

    rest = words[1:]

    if not _is_role_word(rest[0]):
        return words

    rest = rest[1:]

    # What follows the role is the name only when something still follows
    # it (the text to type, the option to pick) - and, when the last
    # observation is there to ask, only when it really is that ref's name, so
    # unquoted text (`gui type e10 textbox Decurix Test Site`) survives.
    if len(rest) >= 2:
        known = _last_name(words[0])
        if (known is None and len(rest) == 2) or (known and rest[0].strip().lower() == known.lower()):
            rest = rest[1:]

    return [words[0]] + rest


def _last_name(ref):
    """The name the last observation gave `ref`, or None."""

    state = core._load_state() or {}
    for element in state.get("elements", []):
        if element.get("ref") == ref:
            return element.get("name") or ""
    return None


# A Windows path: a drive or a share, then folders. In one, a backslash is a
# separator - "C:\\work\\notes\\t1" holds no new line and no tab.
WINDOWS_PATH = re.compile(r"(?:^|[\s\"\'(=])(?:[A-Za-z]:|\\\\[\w.-]+)\\[^\s\"\']*")


def _unescape(text):
    """
    The text of `gui type` with \\n and \\t read as a new line and a tab - outside a
    Windows path. Found by bench/native: a file name typed into a Save dialog,
    C:\\...\\workers\\t1\\home\\..., was refused as "a Tab in the text" (its \\t1), and
    a folder called \\new would have been cut in two by an Enter.
    """

    out, last = [], 0
    for match in WINDOWS_PATH.finditer(text):
        out.append(text[last:match.start()].replace("\\n", "\n").replace("\\t", "\t"))
        out.append(match.group(0))
        last = match.end()
    out.append(text[last:].replace("\\n", "\n").replace("\\t", "\t"))
    return "".join(out)


def _is_target(token):
    return bool(el.REF_RE.match(token) or el.parse_point(token))


class _Parser(argparse.ArgumentParser):
    """
    Reports a malformed command as one line, not argparse's usage dump.

    Measured on qwen2.5-coder:3b: a rejected `gui click` printed the whole
    usage text twice, and the model's next reply was a query saying
    the command "is not recognized" - it read the wall of text as the tool
    being broken, not as its own typo.
    """

    def error(self, message):
        raise ComputerError(f"could not read that command ({message}). Run `gui help` for the forms.")


def build_parser():
    parser = _Parser(prog="gui", add_help=False, usage=USAGE)
    sub = parser.add_subparsers(dest="command", parser_class=_Parser)

    observe = sub.add_parser("observe", add_help=False)
    observe.add_argument("--window")
    observe.add_argument("--coords", action="store_true")
    observe.add_argument("--screenshot")

    for name in ("click", "double-click", "right-click"):
        p = sub.add_parser(name, add_help=False)
        p.add_argument("target", nargs="*")
        p.add_argument("--role")
        p.add_argument("--name")
        p.add_argument("--see")

    typing = sub.add_parser("type", add_help=False)
    typing.add_argument("words", nargs="+")
    typing.add_argument("--role")
    typing.add_argument("--name")
    typing.add_argument("--see")
    typing.add_argument("--append", action="store_true")
    typing.add_argument("--replace-all", dest="replace_all", action="store_true")

    reader = sub.add_parser("read", add_help=False)
    reader.add_argument("target", nargs="*")
    reader.add_argument("--role")
    reader.add_argument("--name")

    keys = sub.add_parser("key", add_help=False)
    keys.add_argument("keys", nargs="+")
    keys.add_argument("--on")

    scroll = sub.add_parser("scroll", add_help=False)
    scroll.add_argument("words", nargs="*")
    scroll.add_argument("--role")
    scroll.add_argument("--name")

    select = sub.add_parser("select", add_help=False)
    select.add_argument("words", nargs="+")
    select.add_argument("--role")
    select.add_argument("--name")

    upload = sub.add_parser("upload", add_help=False)
    upload.add_argument("words", nargs="+")
    upload.add_argument("--role")
    upload.add_argument("--name")

    opener = sub.add_parser("open", add_help=False)
    opener.add_argument("target", nargs="+")
    opener.add_argument("--new-tab", action="store_true")

    front = sub.add_parser("front", add_help=False)
    front.add_argument("title", nargs="+")

    verify = sub.add_parser("verify", add_help=False)
    verify.add_argument("--text")
    verify.add_argument("--role")
    verify.add_argument("--name")

    drag = sub.add_parser("drag", add_help=False)
    drag.add_argument("words", nargs="+")
    drag.add_argument("--role")
    drag.add_argument("--name")

    recordings = sub.add_parser("recordings", add_help=False)
    recordings.add_argument("words", nargs="*")

    shown = sub.add_parser("recording", add_help=False)
    shown.add_argument("session")

    replayer = sub.add_parser("replay", add_help=False)
    replayer.add_argument("session")
    replayer.add_argument("--param", action="append", default=[])
    replayer.add_argument("--from", dest="start", type=int, default=1)

    listed = sub.add_parser("skills", add_help=False)
    listed.add_argument("words", nargs="*")

    described = sub.add_parser("skill", add_help=False)
    described.add_argument("name")
    described.add_argument("--look", type=int, default=None)

    doer = sub.add_parser("do", add_help=False)
    doer.add_argument("name")
    doer.add_argument("--param", action="append", default=[])
    doer.add_argument("--from", dest="start", type=int, default=1)

    asker = sub.add_parser("ask", add_help=False)
    asker.add_argument("service")
    asker.add_argument("words", nargs="+")

    return parser


def _params(items):
    """--param NAME=VALUE arguments as {name: value}."""

    params = {}
    for item in items:
        name, sep, value = item.partition("=")
        if not sep or not name.strip():
            raise ComputerError(f"--param takes NAME=VALUE, not {item!r}")
        params[name.strip()] = _unescape(value)

    return params


def _load_program(session_id):
    try:
        return replay.load_program(session_id)
    except (FileNotFoundError, ValueError):
        raise ComputerError(f"there is no recording {session_id!r} here. Run `gui recordings` to list them.")


def _print_replay(result, command=None):
    """Print how a replay ended, and exit with what `gui` exit codes mean. `command` is what runs it again."""

    outcome = result["outcome"]
    again = f"{command or 'gui replay ' + result['session']} --from {result['resume_from']}"

    if outcome == replay.NEEDS_APPROVAL:
        print(f"{approval.MARKER} {result['text']}")
        print(f"Once it is approved, continue with: {again}")
        return EXIT_NEEDS_APPROVAL

    print(f"{outcome}: {result['text']}")

    if outcome == replay.NEEDS_HUMAN:
        print(f"Ask the human to do that step, then continue with: {again}")
    elif outcome == replay.HALTED and result.get("resume_from"):
        print(f"To carry on after putting it right: {again}")

    if outcome in replay.SUCCESS:
        return EXIT_OK
    if outcome == replay.RECONCILIATION_REQUIRED:
        return EXIT_VERIFY_FAILED
    return EXIT_ERROR


def _print_act(result):
    print(result["text"])

    change = result.get("change")
    observation = result.get("observation")

    if change is None or observation is None:
        return

    moved = (
        change["windows_opened"]
        or change["active_before"] != change["active_after"]
        or (change.get("url_after") and change.get("url_before") != change.get("url_after"))
    )

    if moved and observation.get("groups"):
        print()
        print(el.render(observation, budget=TEXT_BUDGET - 500, only_group=observation["groups"][0]["title"]))


def read_text(args):
    """
    `gui read TARGET`: the whole text a field or document holds, line by line.

    observe shows the first 60 characters of a value, which for an editor is
    half of its first line: an engine asked to change every "colour" in a
    file could see one of three, and could not check its own edit afterwards
    (found by the GUI benchmark). With no target, the field that has the
    keyboard. A secret field's text is never shown.
    """

    from decurix_agent.computer import privacy

    observation = core.observe()
    words = _strip_descriptor(list(args.target))
    if words and not (_is_target(words[0]) or (len(words) == 2 and _is_role_word(words[0]))):
        words = [" ".join(words)]
    if len(words) == 2 and _is_role_word(words[0]):
        args.role, args.name = args.role or words[0], args.name or words[1]
        words = []
    target = _target(words[0] if words else None, args)

    if target.get("point"):
        element = el.at_point(observation["elements"], *target["point"])
    elif target:
        element = el.find(observation["elements"], active_group=core.active_group(observation), **target)
    else:
        element = next((e for e in observation["elements"] if "focused" in e.get("states", [])), None)
    if element is None:
        raise ComputerError("read what? Give a ref like e12 (run gui observe first).")

    if privacy.secret_field(element):
        return f"{el.describe(element)} is a secret field; its text is not shown."

    value = element.get("value")
    if value in (None, ""):
        value = element.get("name") or ""
    if not value:
        return f"{el.describe(element)} holds no text."

    budget = TEXT_BUDGET * 3
    lines = str(value).split("\n")
    out = [f"{el.describe(element)} holds {len(value)} characters in {len(lines)} line{'s' if len(lines) != 1 else ''}:"]
    used = 0
    for n, line in enumerate(lines, 1):
        if used + len(line) > budget:
            out.append(f"... ({len(lines) - n + 1} more lines not shown)")
            break
        out.append(f"{n:>3}| {line}")
        used += len(line) + 6
    return "\n".join(out)


def run(argv):
    parser = build_parser()

    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE)
        return EXIT_OK

    # A flag written between a command's words (`gui type e13 --append "text"`) is still its flag:
    # argparse stops collecting a command's words at the first one.
    if argv[0] == "type":
        flags = [a for a in argv[1:] if a in ("--append", "--replace-all")]
        argv = [argv[0]] + flags + [a for a in argv[1:] if a not in flags]

    args = parser.parse_args(argv)

    command = args.command

    if command == "observe":
        observation = core.observe()
        core.report_observation(observation)
        budget = TEXT_BUDGET * 3 if args.window else TEXT_BUDGET
        print(el.render(observation, budget=budget, coords=args.coords, only_group=args.window))
        from decurix_agent.computer import grounder
        if grounder.available():
            # Said here, by the tool, only where it is true: told in an engine's fixed instructions, a worker
            # with no grounding model tried --see first and lost its way when it failed (GUI benchmark).
            print('Something on the screen with no ref above (an icon, a picture, a drawn control): '
                  'gui click --see "what it looks like and where" finds it by looking.')
        if args.screenshot:
            with open(args.screenshot, "wb") as f:
                f.write(observation["frame"].png())
            print(f"Screenshot saved to {args.screenshot}")
        return EXIT_OK

    if command == "read":
        print(read_text(args))
        return EXIT_OK

    if command == "open":
        print(core.open_target(" ".join(args.target), new_tab=args.new_tab))
        return EXIT_OK

    if command == "front":
        print(core.bring_to_front(" ".join(args.title)))
        return EXIT_OK

    if command == "verify":
        result = core.verify(expect_text=args.text, role=args.role, name=args.name)
        print(result["text"])
        return EXIT_VERIFY_FAILED if result["ok"] is False else EXIT_OK

    if command == "recordings":
        found = replay.search(" ".join(args.words))
        if not found:
            print("No recordings on this desktop" + (" match that." if args.words else " yet."))
            return EXIT_OK
        kinds = {"demonstration": "demonstration by a person", "task": "task", "adhoc": "hand-run actions"}
        for meta in found:
            print(f"{meta['id']} {el._quote(meta.get('title') or '(untitled)', 70)} - "
                  f"{kinds.get(meta.get('kind'), meta.get('kind'))}, {meta.get('event_count', 0)} actions")
        print("`gui recording ID` shows one's steps; `gui replay ID` runs it again with no model.")
        return EXIT_OK

    if command == "recording":
        meta, program = _load_program(args.session)
        print(replay.render_text(meta, program, budget=TEXT_BUDGET))
        return EXIT_OK

    if command == "replay":
        _, program = _load_program(args.session)
        # Done for a request with other values than the recording's: this
        # run's, never the recorded ones (guidance.py).
        from decurix_agent import guidance

        params = guidance.with_run_values(_params(args.param), program.get("params"), recording=args.session)
        return _print_replay(replay.run(program, params=params, start=args.start))

    if command == "ask":
        # The same reader the `chatgpt "..."` shortcut uses (ask_task.py),
        # for an engine to call once per question it decides to ask.
        from decurix_agent import ask_task

        service = ask_task.service_named(args.service)
        question = " ".join(" ".join(args.words).split())
        if service is None or not question:
            raise ComputerError("gui ask SERVICE \"QUESTION\": SERVICE is chatgpt, google, duckduckgo, "
                                "wikipedia or youtube, and one question - run it again for each question.")
        reply, kind = ask_task._ask(service, question)
        # What the service answered, kept as evidence of this task for the
        # check before it is reported done (evidence.observed).
        from decurix_agent.computer import evidence
        evidence.note(f"{service['name']} was asked {question!r}; " + reply)
        print(reply)
        return EXIT_OK if kind == "final" else EXIT_ERROR

    if command == "skills":
        found = skills.search(" ".join(args.words))
        if not found:
            print("No skills on this desktop" + (" match that." if args.words else " yet - a person teaches one with /learn in the chat."))
            return EXIT_OK
        for skill in found:
            print(skill_engine.summary_line(skill))
        print("`gui skill NAME` shows one's steps; `gui do NAME` does it with no model.")
        return EXIT_OK

    if command in ("skill", "do"):
        skill, candidates = skills.find(args.name)
        if skill is None:
            names = ", ".join(s["name"] for s in candidates)
            raise ComputerError((f"more than one skill fits {args.name!r}: {names}. Use its exact name."
                                 if candidates else f"there is no skill {args.name!r} here. Run `gui skills` to list them."))

        if command == "skill" and args.look is not None:
            return _look(skill, args.look)

        if command == "skill":
            print(skill_engine.render_text(skill, budget=TEXT_BUDGET))
            return EXIT_OK

        from decurix_agent import guidance

        params = guidance.with_run_values(_params(args.param), skill.get("params"), skill=skill["name"])
        return _print_replay(skill_engine.run(skill, params=params, start=args.start),
                             command=f"gui do {skill['name']}")

    if command in ("click", "double-click", "right-click"):
        words = _strip_descriptor(list(args.target))
        if args.see:
            if words:
                raise ComputerError(f"{command} takes a target or --see \"what to look for\", not both")
            result = core.act(command.replace("-", "_"), see=args.see)
            _print_act(result)
            return EXIT_NEEDS_APPROVAL if result["status"] == "needs_approval" else EXIT_OK
        if len(words) > 1 and all(el.parse_point(w) for w in words):
            # Found live: an engine wrote `gui click 360,690 360,710 360,730 ...` to search for a row below the
            # screen's edge, only the first point was clicked, and it never learnt the rest were not.
            raise ComputerError(f"{command} takes one target, and {len(words)} points were given; nothing was "
                                "clicked. To reach something below the visible part of a page, gui scroll it "
                                "(gui scroll down 5), then gui observe and click it by its ref.")
        if words and not (_is_target(words[0]) or (len(words) == 2 and _is_role_word(words[0]))):
            words = [" ".join(words)]
        if len(words) == 2 and _is_role_word(words[0]):
            args.role, args.name = args.role or words[0], args.name or words[1]
            words = []
        target = _target(words[0] if words else None, args)
        if not target:
            raise ComputerError(f"{command} what? Give a ref like e12 (run gui observe first).")
        result = core.act(command.replace("-", "_"), **target)

    elif command == "type":
        words = _strip_descriptor(list(args.words))
        if len(words) >= 3 and words[0].lower() == "at" and el.parse_point(words[1]) is not None:
            words = words[1:]
        if len(words) == 3 and _is_role_word(words[0]) and not (args.role or args.name):
            # gui type textbox "Site title" "Decurix Test Site"
            args.role, args.name, words = words[0], words[1], [words[2]]
        if args.see:
            target, text = {"see": args.see}, " ".join(words)
        elif args.role or args.name:
            target, text = _target(None, args), " ".join(words)
        elif len(words) >= 2 and (_is_target(words[0]) or len(words) == 2):
            target, text = _target(words[0], args), " ".join(words[1:])
        else:
            target, text = {}, " ".join(words)
        result = core.act("type", text=_unescape(text), append=args.append, replace_all=args.replace_all, **target)

    elif command == "key":
        target = _target(args.on, args) if args.on else {}
        result = core.act("key", keys=args.keys, **target)

    elif command == "scroll":
        words, direction, amount = scroll_words(args.words)
        target = _target(words[0], args) if words else _target(None, args)
        result = core.act("scroll", direction=direction, amount=amount, **target)

    elif command == "select":
        words = _strip_descriptor(list(args.words))
        if len(words) == 3 and _is_role_word(words[0]) and not (args.role or args.name):
            args.role, args.name, words = words[0], words[1], [words[2]]
        if args.role or args.name:
            target, option = _target(None, args), " ".join(words)
        elif len(words) >= 2:
            target, option = _target(words[0], args), " ".join(words[1:])
        else:
            raise ComputerError('select needs a dropdown and an option: gui select e5 "High"')
        result = core.act("select", option=option, **target)

    elif command == "drag":
        words = list(args.words)
        drop = el.parse_point(words.pop())
        if drop is None:
            raise ComputerError("drag needs where to drop it, last, as X,Y: gui drag e12 640,360")
        target = _target(" ".join(words) if words else None, args)
        if not target:
            raise ComputerError("drag what? gui drag e12 640,360, or gui drag 100,200 640,360")
        result = core.act("drag", end=drop, **target)

    elif command == "upload":
        words = _strip_descriptor(list(args.words))
        if len(words) < 2 and not (args.role or args.name):
            raise ComputerError("upload needs a target element and at least one file path: gui upload e12 /path/to/doc.pdf")
        if args.role or args.name:
            target, files = _target(None, args), words
        else:
            target, files = _target(words[0], args), words[1:]
        result = core.act("upload", files=files, **target)

    else:
        print(USAGE)
        return EXIT_ERROR

    _print_act(result)

    return EXIT_NEEDS_APPROVAL if result["status"] == "needs_approval" else EXIT_OK


def main(argv=None):
    logging_utils.log_to_file(logging_utils.log_file("decurix-gui.log"))
    argv = sys.argv[1:] if argv is None else argv

    try:
        code = run(argv)
    except ComputerError as exc:
        print(f"gui: {exc}")
        code = EXIT_ERROR
    except KeyboardInterrupt:
        code = EXIT_ERROR
    except Exception as exc:
        # A bug here must not hand an engine a traceback to reason about.
        import traceback
        logging_utils.log(traceback.format_exc())
        print(f"gui: internal error ({type(exc).__name__}: {exc}); details in {logging_utils.log_file('decurix-gui.log')}")
        code = EXIT_ERROR
    finally:
        save_screenshot(argv)
        # A short-lived command: the audit step it queued has to leave before
        # the process does.
        progress.flush()

    return code
