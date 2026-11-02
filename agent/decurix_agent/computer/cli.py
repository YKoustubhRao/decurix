import argparse
import os
import re
import sys
import time

from decurix_agent import logging_utils, progress
from decurix_agent.computer import approval, cdp, core, replay, shown, skill_engine, skills
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


# A page with fewer elements than this is taken to be still drawing itself, and looked at again.
EMPTY_PAGE_ELEMENTS = 6
EMPTY_PAGE_LOOKS = 5
EMPTY_PAGE_PAUSE = 3.0

# Other spellings of a command's name.
COMMAND_ALIASES = {
    "dblclick": "double-click", "doubleclick": "double-click", "double_click": "double-click",
    "dbl-click": "double-click", "rightclick": "right-click", "right_click": "right-click",
    "press": "key", "keys": "key", "look": "observe",
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
  gui type CELL "a\\tb\\nc\\td"     rows of a spreadsheet: a Tab between cells, \\n between rows,
                                 from that cell (D1, or its ref); every cell is read back (exit 4 if
                                 one differs) - also in a host app's viewer
  gui type [TARGET] - <<'EOF'    the text verbatim from stdin (quotes, real Tabs and new lines)
  gui read [TARGET]              the whole text of a field or document, with line numbers
                                 (observe shows only its first 60 characters)
  gui key KEYS...                press keys: enter, tab, ctrl+s, alt+f4 ...
  gui scroll [TARGET] down [N]   scroll up/down/left/right, N notches (about 100 px each)
  gui select TARGET "option"     choose an option in a dropdown
     ... --id IDENTIFIER         a party by the document's identifier (tax/account number, email; repeatable):
                                 searched for first; a record holding it is the one, whatever its name;
                                 the result says matched / none holds it / ambiguous
  gui open URL|FILE|APP          open a web page, a document, or an app
  gui front TITLE                bring an open window to the front (a few words of its title)
  gui upload TARGET FILE...      upload file(s) to a file input element
  gui drag TARGET X,Y            drag an element (or a point) and drop it at X,Y
  gui verify [--text T]          look again: what changed, is T on screen?

  gui record KEY --from FILE [--scope "entity/book/period"] --fields - <<'EOF'
    {"Label as the form shows it": "value", ...}      EOF
                                 declare a record BEFORE typing it: every value must stand in
                                 its source document (or on a screen read, the request, the
                                 person's words); before the save its form is checked against
                                 this (a field holding another value stops the save), and the
                                 page is read back after. --from screen for a record read on
                                 the screen. Use the form's own field labels.
  gui records                    the records declared in this task, with what was read back

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

  gui guide [NAME]               how a well-known app is worked here (Excel, Calc,
                                 TallyPrime, Word, PowerPoint, Impress, GIMP...)
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


def _refuse_native_dropdown(ref):
    """A click on a native drop-down (<select>) opens a list drawn outside the page, whose entries no look can read: the
    engine then clicks them by looking, slowly and by guess. Found live (2026-10-25): 15 such clicks averaged 32 s in
    one task. It is told to choose by name instead, with the options - nothing is clicked."""
    state = core._load_state() or {}
    element = next((e for e in state.get("elements") or [] if e.get("ref") == ref), None)
    if not element or element.get("role") != "combobox" or element.get("source") != "cdp":
        return
    try:
        options = cdp.native_options(element)
    except Exception:  # noqa: BLE001 - a look that fails leaves the click as it was
        return
    if options is None:
        return
    shown = ", ".join(el._quote(o, 40) for o in options[:40]) + (f" (+{len(options) - 40} more)" if len(options) > 40 else "")
    raise ComputerError(f"Not clicked: {el.describe(element)} is a drop-down list, and an open list's entries cannot be "
                        f"read from the screen. Choose by name instead: gui select {ref} \"<option>\". Its options: {shown}")


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
        if shown.unseen(value):
            raise ComputerError(f"{value} was not in anything shown to you in this task - a ref is not a number to "
                                "guess, and a guessed one is another control. Nothing was done. gui observe, and use "
                                "the ref the look gives the control you mean.")
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


# A Windows path: a drive (C:\), a share (\\host\share\) or an environment variable (%USERPROFILE%\), then folders and
# a file. In one, a backslash is a separator: "C:\work\notes\t1" holds no new line and no tab, and
# "C:\Users\me\Documents\Decurix demo\receivables.xlsx" holds no carriage return (found live, 2026-10-19: the path
# was cut at its first space, " demo\receivables.xlsx" was read as text, and \r refused every typing of the path).
_PATH_START = re.compile(r"(?:[A-Za-z]:\\|\\\\[\w.$-]+\\|%[A-Za-z_][A-Za-z0-9_]*%\\)")
# What ends a path as a file: after one of these, \n or \t is an escape again ("C:\a\b.csv\nTotal").
_FILE_EXT = re.compile(r"\.(?:xlsx|xlsm|xls|ods|csv|tsv|txt|md|log|json|xml|html?|docx?|odt|rtf|pptx?|odp|pdf|png|jpe?g|"
                       r"gif|bmp|tiff?|webp|svg|zip|eml|msg|py|sh|ini|cfg|bak|tmp)$", re.I)
_NOT_IN_A_NAME = '\\/:*?"<>|\r\n\t'


def path_spans(text):
    """
    Where path-looking text stands in `text`, as (start, end) pairs: from a drive, share or %VAR% to the end of its
    last folder or file - spaces inside names included - or, when a file name comes first (a known extension) and a
    \\n or \\t escape follows it, to the end of that file. Backslashes inside are literal.
    """

    spans, pos, n = [], 0, len(text or "")
    while pos < n:
        start = _PATH_START.search(text, pos)
        if not start:
            break
        before = text[start.start() - 1:start.start()] if start.start() else ""
        # A drive letter is not the end of a word ("Note:\n") - unless the word is the n or t of a \n or \t before it.
        if before.isalnum() and text[start.start() - 2:start.start()] not in ("\\n", "\\t"):
            pos = start.start() + 1
            continue
        i = start.end()
        while True:
            j = i
            while j < n and text[j] not in _NOT_IN_A_NAME:
                j += 1
            if j < n and text[j] == "\\":
                if text[j + 1:j + 2] in ("n", "t") and _FILE_EXT.search(text[i:j].rstrip()):
                    i = j
                    break
                i = j + 1
                continue
            i = j
            break
        spans.append((start.start(), i))
        pos = max(i, start.end())
    return spans


def _outside_paths(text, change):
    """`text` with `change` applied to every stretch that is not path-looking."""

    out, last = [], 0
    for begin, end in path_spans(text):
        out.append(change(text[last:begin]))
        out.append(text[begin:end])
        last = end
    out.append(change(text[last:]))
    return "".join(out)


def _unescape(text):
    """
    The text of `gui type` with \\n and \\t read as a new line and a tab - outside path-looking text (see path_spans).
    Found by bench/native: a file name typed into a Save dialog, C:\\...\\workers\\t1\\home\\..., was refused as "a Tab
    in the text" (its \\t1), and a folder called \\new would have been cut in two by an Enter.
    """

    return _outside_paths(text, lambda part: part.replace("\\n", "\n").replace("\\t", "\t"))


# A backslash sequence gui type has no key for: \f, \r, \v, \b, \a - typed, the two characters. Refused only where it
# stands alone as a token (nothing word-like after it): "\receivables" is a path's folder, "\fKaveri" is run-on text
# the final check catches in the saved file (produced.document_findings).
UNSUPPORTED_ESCAPE = re.compile(r"(?<!\\)\\[frvba](?![A-Za-z0-9_.])")


def _unsupported_escape(text):
    """The first lone backslash sequence in typed text that gui type would type as characters, or ""."""

    rest = _outside_paths(text or "", lambda part: part)
    for begin, end in reversed(path_spans(rest)):
        rest = rest[:begin] + " " + rest[end:]
    found = UNSUPPORTED_ESCAPE.search(rest)
    return found.group(0) if found else ""


# A spreadsheet cell named by its coordinate as gui type's target (D1, $B$14). Not a ref: refs are e and digits.
CELL_TARGET = re.compile(r"^\$?[A-Za-z]{1,3}\$?[0-9]{1,7}(?::\$?[A-Za-z]{1,3}\$?[0-9]{1,7})?$")


def _is_target(token):
    return bool(el.REF_RE.match(token) or el.parse_point(token))


def _one_target(words, args):
    """A command's words that name one control - a ref, a point, `role name`, or a name in words - as act() keywords."""

    if words and not (_is_target(words[0]) or (len(words) == 2 and _is_role_word(words[0]))):
        words = [" ".join(words)]
    if len(words) == 2 and _is_role_word(words[0]):
        args.role, args.name = args.role or words[0], args.name or words[1]
        words = []
    return _target(words[0] if words else None, args)


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


def _check_form(command, args):
    """What core.act's pre-save checks are told for a press: a click or double-click is checked, and --checked confirms
    a listed form; a right-click opens a menu and commits nothing."""

    if command == "right-click":
        return False
    return "checked" if getattr(args, "checked", False) else True


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
        # The form's filled fields were listed and checked: send it (core._form_check).
        p.add_argument("--checked", action="store_true")

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

    for name, nargs in (("scroll", "*"), ("select", "+"), ("upload", "+")):
        p = sub.add_parser(name, add_help=False)
        p.add_argument("words", nargs=nargs)
        p.add_argument("--role")
        p.add_argument("--name")
        if name == "select":
            # The identifiers the document gives the record: the list is searched by them first (resolve.py).
            p.add_argument("--id", dest="ids", action="append", default=[])

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

    recorder = sub.add_parser("record", add_help=False)
    recorder.add_argument("key")
    recorder.add_argument("--from", dest="source", required=True)
    recorder.add_argument("--scope", default="")
    recorder.add_argument("--fields", default="-")
    recorder.add_argument("--field", action="append", default=[])

    sub.add_parser("records", add_help=False)

    listed = sub.add_parser("skills", add_help=False)
    listed.add_argument("words", nargs="*")

    recalled = sub.add_parser("recall", add_help=False)
    recalled.add_argument("words", nargs="+")

    described = sub.add_parser("skill", add_help=False)
    described.add_argument("name")
    described.add_argument("--look", type=int, default=None)

    doer = sub.add_parser("do", add_help=False)
    doer.add_argument("name")
    doer.add_argument("--param", action="append", default=[])
    doer.add_argument("--source-file", action="append", default=[])
    doer.add_argument("--from", dest="start", type=int, default=1)

    guide = sub.add_parser("guide", add_help=False)
    guide.add_argument("name", nargs="*")
    guide.add_argument("--learn", default="")
    guide.add_argument("--applies-to", dest="applies_to", default="")
    guide.add_argument("--description", default="")

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
    target = _one_target(_strip_descriptor(list(args.target)), args)

    if target.get("point"):
        element = el.at_point(observation["elements"], *target["point"], windows=observation.get("windows"),
                               popups=observation.get("popups"))
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

    # A command under the name an engine reaches for. Found live: `gui dblclick` and `gui doubleclick`, refused
    # on run after run before the engine wrote it the way the usage does.
    argv = [COMMAND_ALIASES.get(argv[0].lower(), argv[0])] + list(argv[1:])

    # "click X twice" / "click X double-click": the double click it asks for. Found live: the trailing word was
    # taken for part of the target, or the click was made once.
    if argv[0] == "click" and len(argv) > 2 and argv[-1].lower() in ("twice", "double", "double-click", "doubleclick", "dblclick"):
        argv = ["double-click"] + list(argv[1:-1])

    # A flag written between a command's words (`gui type e13 --append "text"`) is still its flag:
    # argparse stops collecting a command's words at the first one.
    if argv[0] == "type":
        flags = [a for a in argv[1:] if a in ("--append", "--replace-all")]
        argv = [argv[0]] + flags + [a for a in argv[1:] if a not in flags]

    args = parser.parse_args(argv)

    command = args.command

    if command == "observe":
        observation = core.observe()
        # A page that draws itself after it has "loaded" shows next to nothing for a while. Found live on a slow
        # host: an engine looked at a record just saved, was shown an empty page, reloaded it, and then could not
        # say whether its work had been saved. An almost empty page is looked at again before it is reported.
        loading = False
        before = None
        for _ in range(EMPTY_PAGE_LOOKS):
            on_page = [e for e in observation.get("elements") or [] if e.get("source") == "cdp"]
            loading = bool(observation.get("page_url")) and len(on_page) < EMPTY_PAGE_ELEMENTS
            # A small page that reads the same on two looks has settled: it is small, not loading. Found live:
            # a sign-in page (a heading, two fields, a button) was called "almost nothing, still loading" after
            # fifteen seconds of looks, and the engine was told not to touch it.
            shape = [(e.get("role"), e.get("name")) for e in on_page]
            if not loading or (on_page and shape == before):
                loading = False
                break
            before = shape
            time.sleep(EMPTY_PAGE_PAUSE)
            observation = core.observe()
        core.report_observation(observation)
        budget = TEXT_BUDGET * 3 if args.window else TEXT_BUDGET
        shown = el.render(observation, budget=budget, coords=args.coords, only_group=args.window)
        print(shown)
        if loading:
            print("The page shows almost nothing: it is still loading, or drawing itself. Wait and observe again - do not "
                  "reload it, close it or open it a second time; what was saved before stays saved.")
        # What this look read is evidence of the task: the page may be closed before the task is checked.
        from decurix_agent.computer import evidence
        evidence.note_look(shown, observation)
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
        target = " ".join(args.target)
        print(core.open_target(target, new_tab=args.new_tab))
        hint = _guide_hint(target)
        if hint:
            print(hint)
        return EXIT_OK

    if command == "guide":
        # A guide to a well-known app (app_skills.py), for an engine that opened an app its request did not name.
        from decurix_agent import app_skills

        if args.learn:
            # What the engine worked out about an app with no guide, kept for later tasks (app_skills.learn): the
            # guide's text verbatim on stdin, held until this task is checked done.
            from decurix_agent import task_context

            body = "" if sys.stdin is None or sys.stdin.isatty() else sys.stdin.read()
            try:
                print(app_skills.learn(args.learn, args.applies_to, args.description, body, task_context.current(),
                                       task_context.current_stage()[0]))
            except ValueError as exc:
                raise ComputerError(f"guide not kept: {exc}") from None
            return EXIT_OK
        wanted = " ".join(args.name)
        if not wanted:
            for g in app_skills.guides():
                print(f"{g['name']}: {g['description']}" + (f" (learned here {g['learned']})" if g.get("learned") else ""))
            return EXIT_OK
        found = app_skills.named(wanted)
        if found is None:
            raise ComputerError(f"no guide for {wanted!r}; gui guide lists them")
        print(app_skills.render([found]))
        return EXIT_OK

    if command == "front":
        print(core.bring_to_front(" ".join(args.title)))
        return EXIT_OK

    if command == "verify":
        result = core.verify(expect_text=args.text, role=args.role, name=args.name)
        print(result["text"])
        from decurix_agent.computer import evidence
        evidence.note("a check of the screen then - " + result["text"][:1200])
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

    if command == "record":
        # A record's intent, declared before it is typed: its form is checked against it before the save and the page
        # read back after (records.py).
        from decurix_agent.computer import records

        fields = {}
        if args.fields == "-":
            fields.update(records.parse_fields("" if sys.stdin is None or sys.stdin.isatty() else sys.stdin.read()))
        elif args.fields:
            try:
                fields.update(records.parse_fields(open(args.fields, encoding="utf-8").read()))
            except OSError as exc:
                raise ComputerError(f"cannot read the fields file: {exc}")
        for given in args.field:
            if "=" not in given:
                raise ComputerError(f'--field needs Label=value, not {given!r}')
            k, v = given.split("=", 1)
            fields[k.strip()] = v.strip()
        _, lines = records.declare(args.key, args.source, fields, scope=args.scope)
        print("\n".join(lines))
        return EXIT_OK

    if command == "records":
        from decurix_agent.computer import records

        print("\n".join(records.listing()))
        return EXIT_OK

    if command == "skills":
        found = skills.search(" ".join(args.words))
        if not found:
            print("No skills on this desktop" + (" match that." if args.words else " yet - a person teaches one with /learn in the chat."))
            return EXIT_OK
        for skill in found:
            print(skill_engine.summary_line(skill))
        print("`gui skill NAME` shows one's steps; `gui do NAME` does it with no model.")
        return EXIT_OK

    if command == "recall":
        from decurix_agent import memory_context

        print(memory_context.search(" ".join(args.words)) or "Nothing in this worker's memory has those words.")
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

        given = _params(args.param)
        refused = guidance.taught_values_check(skill, given)
        if refused:
            raise ComputerError(refused)
        params = guidance.with_run_values(given, skill.get("params"), skill=skill["name"])
        return _print_replay(skill_engine.run(skill, params=params, start=args.start, source_files=_params(args.source_file)),
                             command=f"gui do {skill['name']}")

    if command in ("click", "double-click", "right-click"):
        words = _strip_descriptor(list(args.target))
        if args.see:
            if words:
                raise ComputerError(f"{command} takes a target or --see \"what to look for\", not both")
            # The pre-save checks stand however the control is found (core._commit_check).
            result = core.act(command.replace("-", "_"), see=args.see,
                              check_form=_check_form(command, args))
            _print_act(result)
            return EXIT_NEEDS_APPROVAL if result["status"] == "needs_approval" else EXIT_OK
        if len(words) > 1 and all(el.parse_point(w) for w in words):
            # Found live: an engine wrote `gui click 360,690 360,710 360,730 ...` to search for a row below the
            # screen's edge, only the first point was clicked, and it never learnt the rest were not.
            raise ComputerError(f"{command} takes one target, and {len(words)} points were given; nothing was "
                                "clicked. To reach something below the visible part of a page, gui scroll it "
                                "(gui scroll down 5), then gui observe and click it by its ref.")
        target = _one_target(words, args)
        if not target:
            raise ComputerError(f"{command} what? Give a ref like e12 (run gui observe first).")
        if command == "click" and target.get("ref"):
            _refuse_native_dropdown(target["ref"])
        # named: a press on a control that has a name is made by its ref, not by where it is (core.act).
        result = core.act(command.replace("-", "_"), named=True, check_form=_check_form(command, args), **target)

    elif command == "type":
        words = _strip_descriptor(list(args.words))
        if len(words) >= 3 and words[0].lower() == "at" and el.parse_point(words[1]) is not None:
            words = words[1:]
        if len(words) == 3 and _is_role_word(words[0]) and not (args.role or args.name):
            # gui type textbox "Site title" "Decurix Test Site"
            args.role, args.name, words = words[0], words[1], [words[2]]
        if args.see:
            target, text = {"see": args.see}, " ".join(words)
        elif len(words) >= 2 and CELL_TARGET.match(words[0]) and not el.REF_RE.match(words[0]):
            # gui type D1 "..." - a spreadsheet's cell by its coordinate (a ref is lower-case e and digits: e12).
            # A range (F3:F13) names where the block starts: its top-left cell. Found live (2026-10-19): refused as "one
            # field at a time", and the worker fell back to typing the figures.
            target, text = {"cell": words[0].split(":")[0]}, " ".join(words[1:])
        elif args.role or args.name:
            target, text = _target(None, args), " ".join(words)
        elif len(words) >= 2 and (_is_target(words[0]) or len(words) == 2):
            target, text = _target(words[0], args), " ".join(words[1:])
        else:
            target, text = {}, " ".join(words)
        if text == "-":
            # The text verbatim on stdin (a heredoc): quotes, Tabs and new lines exactly as given, nothing for the
            # shell to eat. Found live (2026-10-19): "=IF(D2=""x"",1,0)" reached the app as =IF(D2=x,1,0).
            text = sys.stdin.read()
            text = text[:-1] if text.endswith("\n") else text
            if not text:
                raise ComputerError("nothing on stdin to type: gui type REF - <<'EOF' then the text then EOF")
        else:
            text = _unescape(text)
        escape = _unsupported_escape(text)
        if escape:
            raise ComputerError(
                f"the text holds {escape}, which gui type cannot type as a key - it would put the two characters "
                f"{escape} in the document. Nothing was typed. Only \\n (Enter) and \\t (Tab) are read as keys. Type "
                "the text in parts and press the key between them with gui key (a page break in a word processor "
                "is gui key ctrl+Return).")
        result = core.act("type", text=text, append=args.append, replace_all=args.replace_all, keys_from_task=True,
                          check_form=True, **target)

    elif command == "key":
        target = _target(args.on, args) if args.on else {}
        _refuse_blind_walk(args.keys)
        result = core.act("key", keys=args.keys, keys_from_task=True, check_form=True, **target)

    elif command == "scroll":
        words, direction, amount = scroll_words(args.words)
        target = _target(words[0] if words else None, args)
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
        ids = [i.strip() for i in (getattr(args, "ids", None) or []) if i and i.strip()]
        result = core.act("select", option=option, ids=ids, **target)

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

    if result["status"] == "needs_approval":
        return EXIT_NEEDS_APPROVAL
    # A block whose cells do not all hold what was typed was done, and is not right: the read-back says which.
    return EXIT_VERIFY_FAILED if result.get("unverified") else EXIT_OK


# Keys that move the keyboard from one control to the next, and how many in a
# row one command may press.
WALKING_KEYS = {"tab", "shift+tab"}
MAX_WALK = 5
# The most different keys one command presses.
MAX_CHAIN = 6


def _guide_hint(target):
    """One line naming the guide to the app just opened, or "" (the line is printed only when there is one)."""

    try:
        from decurix_agent import app_skills
        found = app_skills.matching(target)
    except Exception:
        return ""
    if not found:
        return ""
    return "There is a guide to this app: gui guide " + " / gui guide ".join(g["name"] for g in found) +         " - read it before working it (it is already in your context when the request named the app)."


def _refuse_blind_walk(keys):
    """
    Refuse a long run of Tab: it is walking a page without looking. Found
    live: to get from a field at the top of a form to a table further down,
    an engine pressed Tab some forty times in one command; the keyboard ran
    through the table, leaving its last cell made a new row each time, and the
    rest of the task went on deleting rows.
    """

    run = longest = 0
    for key in keys:
        run = run + 1 if str(key).strip().lower() in WALKING_KEYS else 0
        longest = max(longest, run)
    if longest > MAX_WALK:
        raise ComputerError(f"that is {longest} Tab presses in a row, and nothing was pressed: Tab moves through every "
                            "control on the way, and in a table of entries it adds rows. Go to the field itself - "
                            "gui click REF, or gui type REF \"text\" - with a ref from gui observe.")
    # A long chain of different keys is a plan run blind. Found live (2026-10-15) in real Excel: one command pressed
    # twenty keys - shortcuts, guessed ribbon key-tips ("alt+h s s"), the letters "t a b" - and every look after it
    # met a sheet in a state nobody had seen form. Repeats of a key or two (Down Down Down, Tab Tab Enter) are a walk,
    # not a plan, and are judged as one above.
    if len(keys) > MAX_CHAIN and len({str(k).strip().lower() for k in keys}) > 3:
        raise ComputerError(f"that is {len(keys)} keys in one command, and nothing was pressed: press at most "
                            f"{MAX_CHAIN} at a time and look at what they did before the next (an app's key-tips and "
                            "dialogs change what the following keys do). Type text with gui type, not as keys.")


GONE = re.compile(r"^(e\d+) is no longer on the screen")


def _moved_note(message):
    """
    For a ref that has gone, what it was and its ref now, when the screen shows
    exactly one control of that role and name. Found live (2026-10-16): Excel's
    Data tab had a new ref once a file was open; told only that "e21070 is no
    longer on the screen", the engine pressed e21270 - a cell - and went round in
    circles for forty commands.
    """

    gone = GONE.match(message)
    if not gone:
        return ""
    known = shown.was(gone.group(1))
    if not known:
        return ""
    role, name = known
    try:
        now = [e for e in core.observe()["elements"]
               if e.get("role") == role and (e.get("name") or "") == name and e.get("ref")]
    except ComputerError:
        return ""
    if len(now) != 1:
        return f" It was {role} {el._quote(name)}."
    return f" It was {role} {el._quote(name)}, which is now {now[0]['ref']} - use that ref."


class _Kept:
    """stdout, keeping a copy of what was printed (the refs in it are what the engine was shown)."""

    def __init__(self, out):
        self.out, self.kept = out, []

    def write(self, text):
        self.kept.append(text)
        return self.out.write(text)

    def __getattr__(self, name):
        return getattr(self.out, name)


def main(argv=None):
    logging_utils.log_to_file(logging_utils.log_file("decurix-gui.log"))
    argv = sys.argv[1:] if argv is None else argv
    kept = sys.stdout = _Kept(sys.stdout)

    try:
        code = run(argv)
    except ComputerError as exc:
        print(f"gui: {exc}{_moved_note(str(exc))}")
        code = EXIT_ERROR
    except KeyboardInterrupt:
        code = EXIT_ERROR
    except BrokenPipeError:
        # The reader took what it wanted and left (gui observe | head): the action itself was done. Found by hand:
        # the engine was handed two tracebacks for a look that had worked. Nothing more can be printed there.
        try:
            sys.stdout = open(os.devnull, "w")
        except OSError:
            pass
        code = EXIT_OK
    except Exception as exc:
        # A bug here must not hand an engine a traceback to reason about.
        import traceback
        logging_utils.log(traceback.format_exc())
        print(f"gui: internal error ({type(exc).__name__}: {exc}); details in {logging_utils.log_file('decurix-gui.log')}")
        code = EXIT_ERROR
    finally:
        if isinstance(kept, _Kept):
            try:
                shown.note("".join(kept.kept))
            except Exception:  # noqa: BLE001 - remembering what was shown must never fail the command
                pass
            if sys.stdout is kept:
                sys.stdout = kept.out
        save_screenshot(argv)
        # A short-lived command: the audit step it queued has to leave before
        # the process does.
        progress.flush()

    return code
