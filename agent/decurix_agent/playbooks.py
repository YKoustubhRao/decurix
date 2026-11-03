"""
Playbooks: what a worker keeps from a task it worked out for itself.

A task nobody taught is found out by the engine one command at a time: look,
try, look again. On a hosted model every one of those turns is paid for, and
the next request of the same kind used to be found out again from nothing -
the worker was exactly as slow, and as expensive, on its hundredth order as on
its first. A person teaching the task (/learn) fixes that for the tasks they
get round to teaching; this is the same thing for the rest, with nobody asked.

When an engine's task ends and its result was checked done (verify.py, on the
first attempt), the commands that worked - in order, with the looks left out -
are kept here with the request they were for. The next request shaped like it
is handed that list as the way through: the same model, the same judgement,
the same gates, but the path already known. Each use is recorded with what it
cost, so a playbook shows whether it is paying for itself, the shorter of two
ways that both worked is the one kept, and a playbook that stops working is
dropped.

What this is not: a replay. Nothing here runs a command. The engine reads the
list and still decides each step, the approval gate and the grounding of typed
values apply as on any task, and a result is still checked before it is
reported. A value the earlier run typed that its request did not hold is not
kept at all (`<value>`): it came from that run's documents or screen - or was
a password - and this run has its own.

  ~/.decurix/playbooks/<name>.json
    request    the request it was made from (its words, without attachments)
    shape      the words that say what kind of request it is
    skill      the taught skill it was a run of, when it was one
    commands   what worked, in order
    made       {"at", "tokens", "turns", "commands"} of the run it came from
    uses       the last runs it was offered to: {"at", "ok", "tokens", "turns", "commands"}
"""

import json
import os
import posixpath
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

from decurix_agent import attachments, commands as shell_rules, config, llm_client, produced, script_sandbox, usage
from decurix_agent.computer import privacy
from decurix_agent.logging_utils import log

ROOT = Path(os.environ.get("DECURIX_PLAYBOOKS_DIR") or (config.DECURIX_DIR / "playbooks"))
# The scripts a worker wrote for itself from work it did by hand (_distil).
TOOLS = Path(os.environ.get("DECURIX_TOOLS_DIR") or (config.DECURIX_DIR / "tools"))

# 0 switches playbooks off: nothing is kept and nothing is offered.
try:
    ENABLED = int(os.environ.get("DECURIX_PLAYBOOKS", "1")) > 0
except ValueError:
    ENABLED = True

# A task the model finished in fewer turns than this had nothing to find out:
# one command and its answer is two. Turns, not commands, are what is paid
# for - found live: a workbook built from a file took four turns (a look, a
# script that failed, the script that worked, the answer) and left two
# commands that worked, one of them the whole script worth keeping.
MIN_TURNS = 3
# What stands in for it when the engine did not say how many turns it took.
MIN_COMMANDS = 3
MAX_COMMANDS = 60
# A command is kept whole up to here: a script written with a heredoc is the
# logic worth keeping, and is longer than a click.
MAX_COMMAND_CHARS = 1500
MAX_PLAYBOOKS = 200
USE_HISTORY = 10

# How much of a playbook's shape a request must share, and the other way
# round, to be the same kind of request - and never on fewer words than this.
MATCH = 0.6
MIN_SHARED = 3

# How many different paths clean runs of one kind of request are remembered by.
KEEP_PATHS = 4

# Offered and not done this many times running, a playbook is dropped: the
# application has moved on from it, and a wrong way through costs more than none.
RETIRE_AFTER = 2
# A way that worked but took more commands replaces the kept one only when it
# has done so this many times running - by then the shorter way no longer fits.
REPLACE_LONGER_AFTER = 2

VALUE = "<value>"

FILLER = {
    "please", "pls", "can", "could", "would", "you", "kindly", "just", "now", "again", "for", "me", "the", "a", "an",
    "hey", "hi", "ok", "okay", "to", "of", "in", "on", "at", "and", "it", "this", "that", "is", "are", "with", "from",
    "into", "my", "our", "i", "we", "as", "by", "be", "do", "its",
}
QUOTED = re.compile(r"\"[^\"]*\"|“[^”]*”|`[^`]*`")
WORD = re.compile(r"[^\W\d_][^\W_]*")
# Looking is not part of the way: the engine looks for itself.
LOOK = re.compile(r"^\s*gui\s+(observe|verify|read|skills?|recordings?|recording)\b")
TYPES = re.compile(r"^\s*gui\s+(type|select)\b")

_lock = threading.Lock()
_run = {"segments": [], "turns": 0, "offered": None, "said": False, "paused": False, "declined": False,
        "answered": False, "shell": []}


# ============================================================
# The store
# ============================================================

def _path(name):
    return ROOT / f"{name}.json"


def load(name):
    try:
        with open(_path(name), encoding="utf-8") as f:
            book = json.load(f)
    except (OSError, ValueError):
        return None
    return book if isinstance(book, dict) and isinstance(book.get("commands"), list) else None


def save(book):
    ROOT.mkdir(parents=True, exist_ok=True)
    tmp = _path(book["name"]).with_suffix(".tmp")
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(book, f, indent=1, ensure_ascii=False)
    os.replace(tmp, _path(book["name"]))


def delete(name):
    try:
        _path(name).unlink()
    except OSError:
        pass


def all_books():
    try:
        names = sorted(p.stem for p in ROOT.glob("*.json"))
    except OSError:
        return []
    return [book for book in (load(name) for name in names) if book]


def _unique_name(shape):
    base = "-".join(shape[:6])[:60].strip("-") or "task"
    name, n = base, 2
    while _path(name).exists():
        name, n = f"{base}-{n}", n + 1
    return name


# ============================================================
# What kind of request this is
# ============================================================

def shape(request):
    """
    The words that say what kind of request this is, in order: its own words
    (not its attachments), without what is quoted, numbers and filler. Two
    orders for two customers have the same shape but for the names.
    """

    words = attachments.text(request or "").strip().splitlines()
    text = QUOTED.sub(" ", " ".join(words))
    seen, out = set(), []
    for word in WORD.findall(text.lower()):
        if word in FILLER or word in seen or len(word) < 2:
            continue
        seen.add(word)
        out.append(word)
    return out


def _alike(mine, theirs):
    """How alike two shapes are (0..1): the smaller of each one's share of words found in the other."""

    if not mine or not theirs or mine[0] != theirs[0]:
        # The first word is what is to be done: "delete the order" is not "create the order".
        return 0.0
    shared = len(set(mine) & set(theirs))
    if shared < min(MIN_SHARED, len(mine), len(theirs)) or shared < 2:
        return 0.0
    return min(shared / len(set(mine)), shared / len(set(theirs)))


def find(request, skill=None):
    """The playbook for this kind of request (a taught skill's own, when `skill` names one), or None."""

    if not ENABLED:
        return None
    books = all_books()
    if skill:
        return next((b for b in books if b.get("skill") == skill), None)
    mine = shape(request)
    best, score = None, 0.0
    for book in books:
        if book.get("skill"):
            continue
        alike = _alike(mine, book.get("shape") or [])
        if alike > score:
            best, score = book, alike
    return best if score >= MATCH else None


# ============================================================
# What a command list keeps
# ============================================================

def _without_value(command, request):
    """
    A `gui type` / `gui select` command with what it typed left out, unless
    the request itself holds it: a value from that run's documents or screen
    is that run's, and a password must not be kept at all.
    """

    try:
        words = shlex.split(command)
    except ValueError:
        return f"{TYPES.match(command).group(0).strip()} ... {VALUE}"
    positional = [i for i, w in enumerate(words) if i >= 2 and not w.startswith("--")]
    if not positional:
        return command
    last = positional[-1]
    if " ".join(words[last].lower().split()) in " ".join((request or "").lower().split()) and len(words[last]) > 1:
        return command
    words[last] = VALUE
    return " ".join(w if w == VALUE else shlex.quote(w) for w in words)


def tidy(commands, request=""):
    """What of a task's successful commands is the way through it: no looks, no repeats, no credentials, no values of its own."""

    out = []
    for command in commands or []:
        command = (command or "").strip()
        if not command or LOOK.match(command):
            continue
        if TOOLS.as_posix() in command:
            # A run that used its own tool: the tool is what is kept, not the call.
            continue
        if privacy.credential_in(command):
            out.append("(a command that held a credential is not kept: do that step yourself)")
            continue
        if TYPES.match(command):
            command = _without_value(command, request)
        if len(command) > MAX_COMMAND_CHARS:
            command = command[:MAX_COMMAND_CHARS] + " ...(cut)"
        if out and out[-1] == command:
            continue
        out.append(command)
    return out[:MAX_COMMANDS]


# ============================================================
# The task being worked on
# ============================================================

def begin():
    """A new task: nothing of the last one's commands or playbook carries over."""

    with _lock:
        _run.update(segments=[], turns=0, offered=None, said=False, paused=False, declined=False, answered=False,
                    shell=[])


def note_job(job):
    """
    Take the commands that worked out of a finished engine job (soul's
    operator reports them as each result's `worked`). A job that resumed a
    paused one carries the whole run so far and replaces it; a new job in the
    same task - a second attempt - is a second segment.
    """

    worked, turns = [], 0
    for result in (job or {}).get("results") or []:
        meta = result.get("Meta") or {}
        if result.get("SkillName") != "operator" or not isinstance(meta.get("worked"), list):
            continue
        worked += [c for c in meta["worked"] if isinstance(c, str)]
        try:
            turns += int(meta.get("turns") or 0)
        except (TypeError, ValueError):
            pass

    with _lock:
        if (job or {}).get("resumed"):
            _run["paused"] = True
        if (job or {}).get("resumed") and _run["segments"]:
            _run["segments"][-1] = worked
        else:
            _run["segments"].append(worked)
        _run["turns"] += turns


def ran_commands():
    """The shell commands this task's engine jobs ran that worked (soul's `worked`), oldest first."""

    with _lock:
        return [c for seg in _run["segments"] for c in seg]


# A command that can change something outside the engine's own memory: a redirect into a file, a program whose job is
# to write, move, delete, install, send or run a script. A shell-only task that ran one has side effects nothing
# read back (review 2026-11-03: such a task returned early at the final gate and went out unchecked).
#
# Round-3 recheck: every python3, curl and git counted, so read-only answers ("git status", "curl -s URL", "python3 -c
# 'print(...)'") went to the final check and came back "Not checked". Those three now count only when they write:
# git by its subcommand, curl when it sends or saves, an interpreter when its code is not plainly read-only (a script
# file the check cannot see always counts). Anything not plainly read-only still counts.
CHANGES_THINGS = re.compile(
    r"(?<![<>&0-9])>>?(?!&)(?!\s*/dev/null)|\b(tee|touch|mkdir|cp|mv|rm|rmdir|ln|install|dd|truncate|chmod|chown|"
    r"unzip|zip|tar|wget|sed\s+-i|apt(-get)?|pip3?|npm|sqlite3|psql|mysql|sendmail|mail|scp|rsync|ssh|kill|pkill|"
    r"xdotool|soffice|libreoffice)\b|\boffice\s+(?!read\b)\S")
_GIT_READS = {"status", "log", "diff", "show", "rev-parse", "ls-files", "grep", "blame", "describe", "shortlog",
              "cat-file", "ls-tree", "reflog", "help", "version"}
_GIT = re.compile(r"\bgit((?:\s+-[-\w]+(?:[ =](?!-)\S+)?)*)\s+([a-z][-a-z]*)(\s+[^;&|\n]*)?")
_CURL_WRITES = re.compile(r"(?:^|\s)(?:-[a-zA-Z]*[dFTOo][a-zA-Z]*\b|--data\S*|--form\S*|--json|--upload-file|--output"
                          r"|--remote-name\S*|-X\s*(?:POST|PUT|PATCH|DELETE)|--request\s+(?:POST|PUT|PATCH|DELETE))")
_INTERPRETER = re.compile(r"(?:^|[;&|(`]|\$\(|\s)\s*(?:sudo\s+|env\s+)?(?:\S*/)?(python[0-9.]*|node|nodejs|ruby|perl|php"
                          r"|bash|sh|zsh)(?=\s|$|;|\|)")
_INLINE_CODE = re.compile(r"\b(?:python[0-9.]*|node|nodejs|ruby|perl|php|bash|sh|zsh)\s+(?:-[a-zA-Z]*[ceE]\b|-\s*<<|<<)")
_CODE_WRITES = re.compile(
    r"open\s*\(|\.write|\bos\.|shutil|subprocess|requests|urllib|http\.client|socket|pathlib|Path\s*\(|\.to_(?:csv|excel|"
    r"json|parquet)|\.save\s*\(|json\.dump\b|pickle\.dump|\bunlink|\bremove|\brmdir|\bmkdir|\brename|smtplib|sqlite3"
    r"|\bfs\.|writeFile|child_process|\bexec|\bsystem\b|\bFile\.|\bIO\.|`|\$\(|\brm\b")


# Round-4 recheck: CHANGES_THINGS is a list of writers, and any list of writers misses some - mutt, msmtp, rclone, lp,
# pandoc -o, convert, crontab all ran "read-only", so a shell-only task that sent or converted files went out
# unchecked. So it is turned round: a command changes nothing only when every program in it is one that only reads or
# prints (what it prints can still be redirected, which CHANGES_THINGS sees), with git, curl and interpreters judged by
# what they do as above. Any other program might change something, and the task goes to the final check.
_READS = frozenset("""
ls dir vdir cat tac nl head tail less more grep egrep fgrep zgrep zcat bzcat xzcat rg ag ack wc sort uniq cut tr paste
join comm fold fmt expand unexpand rev column file stat du df free uptime ps pgrep pidof whoami id groups uname arch
nproc lscpu lsblk date cal pwd echo printf true false test [ [[ ]] sleep which whereis type hash basename dirname
realpath readlink printenv locale lsof ss netstat ping dig nslookup host getent jq yq xmllint md5sum sha1sum sha224sum
sha256sum sha384sum sha512sum cksum b2sum sum diff cmp od xxd hexdump strings seq awk gawk mawk sed find cd pushd popd
export unset read local declare typeset readonly let shift wait exit return break continue : tree base64 base32 numfmt
expr bc factor tput w who journalctl dmesg identify ffprobe pdfinfo pdffonts lsattr getfacl ldd readelf iconv shuf look
man help pdftotext set
""".split())
# Words that start a command without being its program, and which of their options take the next word as a value (the
# program comes after them).
_PREFIX_ARGS = {"sudo": {"-u", "-g", "-C", "-p", "-h"}, "env": {"-u", "-C", "-S"}, "nice": {"-n"}, "nohup": set(),
                "time": set(), "stdbuf": set(), "ionice": {"-c", "-n", "-p"}, "command": set(), "builtin": set(),
                "exec": {"-a"}, "xargs": {"-I", "-n", "-P", "-L", "-d", "-s", "-E", "-a"}, "timeout": {"-s", "-k"},
                "then": set(), "do": set(), "else": set(), "elif": set(), "if": set(), "while": set(), "until": set(),
                "!": set(), "{": set(), "}": set(), "coproc": set()}
# Words that end a compound command (`done < list` only reads), and ones whose segment names no program to run.
_CLOSERS = {"fi", "done", "esac"}
_NOT_COMMANDS = {"for", "case", "select", "function", "in"}
_SHELLS = {"bash", "sh", "zsh", "dash", "ksh"}
_JUDGED_ABOVE = {"git", "curl", "node", "nodejs", "ruby", "perl", "php"}
_FIND_ACTS = {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"}
_HEREDOC = re.compile(r"(<<-?\s*(['\"]?)(\w+)\2[^\n]*)\n.*?\n[ \t]*\3[ \t]*(?=\n|$)", re.S)
_ASSIGNMENT = re.compile(r"[A-Za-z_]\w*=")


def _substitutions(text):
    """The commands inside $( ), <( ), >( ) and backticks in text (each outermost one; _simple_commands recurses)."""

    found = re.findall(r"`([^`]*)`", text)
    i = 0
    while True:
        starts = [p for p in (text.find("$(", i), text.find("<(", i), text.find(">(", i)) if p >= 0]
        if not starts:
            return found
        i = min(starts)
        depth, j = 1, i + 2
        while j < len(text) and depth:
            depth += {"(": 1, ")": -1}.get(text[j], 0)
            j += 1
        found.append(text[i + 2:j - 1] if not depth else text[i + 2:])
        i = j


def _program_of(words):
    """[(program, its words)] of one simple command's words, or [] when it runs none."""

    i = 0
    while i < len(words):
        word = words[i]
        if _ASSIGNMENT.match(word):
            i += 1
        elif word in _NOT_COMMANDS:
            return []
        elif word in _PREFIX_ARGS:
            takes = _PREFIX_ARGS[word]
            i += 1
            while i < len(words) and (words[i].startswith("-") or (word == "env" and _ASSIGNMENT.match(words[i]))):
                i += 2 if words[i] in takes else 1
            if word == "timeout" and i < len(words):
                i += 1  # its duration
        else:
            break
    if i >= len(words) or words[i] in _CLOSERS:
        return []
    return [(posixpath.basename(words[i].lstrip("\\")), words[i + 1:])]


def _simple_commands(command):
    """(program, its words) of each simple command in a shell command: every pipeline part, list item and
    substitution. Raises ValueError when the quoting cannot be read."""

    text = _HEREDOC.sub(lambda m: m.group(1), command or "")
    out = []
    for inner in _substitutions(text):
        out.extend(_simple_commands(inner))
    lex = shlex.shlex(text.replace("\n", " ; "), posix=True, punctuation_chars=True)
    lex.whitespace_split = True
    segment, skip = [], False
    for token in list(lex) + [";"]:
        if skip:
            skip = False
            continue
        if token and all(c in "();<>|&" for c in token):
            if any(c in "<>" for c in token) and not any(c in "();|" for c in token):
                skip = True  # a redirect: the next word is its file, not a command
                continue
            out.extend(_program_of(segment))
            segment = []
            continue
        segment.append(token)
    return out


def _program_changes(program, args):
    """Whether one program run with args could change something (git, curl and interpreters are judged in _changes)."""

    if program in _JUDGED_ABOVE or re.fullmatch(r"python[0-9.]*", program) or program == "gui":
        return False  # (gui's acts are on the screen, which the final gate reads from the screen work)
    if program in _SHELLS:
        if "-c" in args and args.index("-c") + 1 < len(args):
            return _changes(args[args.index("-c") + 1])
        return False  # a script or stdin: the interpreter rule in _changes counted it
    if program == "office":
        return not args or args[0] != "read"
    positional = [a for a in args if a == "-" or not a.startswith("-")]
    if program == "find":
        return any(a in _FIND_ACTS for a in args)
    if program == "sed":
        return any(re.fullmatch(r"-[a-zA-Z]*i\S*|--in-place\S*", a) for a in args)
    if program in ("sort", "iconv"):
        return any(a in ("-o", "--output") or a.startswith(("--output=", "-o")) for a in args)
    if program == "xmllint":
        return any(a == "--output" or a.startswith("--output=") for a in args)
    if program in ("awk", "gawk", "mawk"):
        return any(re.search(r"\bsystem\s*\(|\|\s*getline|\bprintf?\b[^;}]*\|", a) for a in args)
    if program == "uniq":
        return len(positional) >= 2
    if program == "xxd":
        return "-r" in args
    if program == "date":
        return any(a in ("-s", "--set") or a.startswith("--set=") for a in args)
    if program == "pdftotext":
        return not positional or positional[-1] != "-"
    return program not in _READS


def _changes(command):
    """Whether one command an engine ran could have changed something (see CHANGES_THINGS and _READS)."""

    command = command or ""
    if CHANGES_THINGS.search(command):
        return True
    for m in _GIT.finditer(command):
        sub, rest = m.group(2), m.group(3) or ""
        if sub not in _GIT_READS and not (sub == "branch" and not re.search(r"\s-[dDmMcC]\b|--(delete|move|copy)", rest)
                                          ) and not (sub in ("remote", "config", "stash", "tag") and
                                                     re.fullmatch(r"\s*(-v|--list|-l|--get\S*\s+\S+|list|show\s.*)?\s*",
                                                                  rest or "")):
            return True
    if re.search(r"\bcurl\b", command) and _CURL_WRITES.search(command):
        return True
    if _INTERPRETER.search(command):
        # Inline code that plainly only reads and prints is not a change; a script file, or code that writes, runs a
        # program or reaches the network, is.
        if not _INLINE_CODE.search(command) or _CODE_WRITES.search(command):
            return True
    try:
        return any(_program_changes(program, args) for program, args in _simple_commands(command))
    except ValueError:
        return True  # quoting that cannot be read: what it runs cannot be told


def note_command(command):
    """A shell command an engine ran in this task, whichever engine (its own runner calls this): the final gate's
    "could this have changed something" sees every engine's commands, not only TheBot's (round-3 recheck). Its lines
    stay lines: two commands on two lines are two commands, not one with the second's words as its arguments."""

    command = "\n".join(" ".join(line.split()) for line in str(command or "").splitlines() if line.strip())
    if command:
        with _lock:
            if len(_run["shell"]) < 500:
                _run["shell"].append(command)


def changed_things(commands=None):
    """The commands among `commands` (the task's own, from every engine, when None) that could have changed
    something."""

    if commands is None:
        with _lock:
            shell = list(_run["shell"])
        commands = ran_commands() + shell
    return [c for c in commands if _changes(c)]


def use(ok, commands):
    """One run's record for a playbook's history (what tool_task.py adds for a run with no engine)."""

    with _lock:
        turns = _run["turns"]
    return {"at": int(time.time()), "ok": bool(ok), "tokens": usage.spent(), "turns": turns, "commands": len(commands)}


def _on_screen(commands):
    """Whether a way through a task works the screen (any gui command) or is shell and files only."""

    return any((c or "").lstrip().startswith("gui ") for c in commands or [])


def answered():
    """
    A person answered a question this task asked. What it then did was
    steered by their words, which the request does not hold: found live, a
    request to sign in stopped on a question, the answer asked for a supplier
    to be created, and the whole of it was kept as how to "open ... and sign
    in". The run is counted, never kept as the way. (A yes to an approval is
    not this: the task did what its request said.)
    """

    with _lock:
        _run["answered"] = True


def decline():
    """This task goes on without its playbook or tool: neither is offered to the engine for the rest of it."""

    with _lock:
        _run["declined"] = True


def context(request, skill=None):
    """
    What the engine is told when this desktop has done a request like this
    before, for its standing context - or "". Remembers which playbook was
    offered, so the task's end can be counted for or against it.

    Only a tool the worker wrote from what worked (_distil) is ever given;
    the commands themselves are not, for any kind of work. Measured live on
    shell work, the list of commands cost more than it saved
    there - the engine ran the same four commands in the same five turns, and
    carried the list in every one of them (17,088 tokens without it, 20,240
    and 20,961 with). Fewer tokens means fewer turns, and one command that
    does the whole job is how a turn is saved.
    """

    with _lock:
        if _run["declined"]:
            return ""

    book = find(request, skill)
    if book is None:
        return ""

    tool = book.get("tool")
    if tool and not Path(tool.get("path") or "").exists():
        tool = None
    if not tool:
        # The commands themselves are never handed back, for any kind of work. For shell work that was
        # measured to cost more (see above). For work on the screen it was measured to do no good and
        # some harm: an element's ref is not the same from one run to the next on an application that
        # redraws itself, so the list named controls that were not there - four suppliers created with
        # it cost 115,000, 140,000 and 129,000 tokens against 103,000 with nothing. What is kept of a
        # run on the screen is its recording, replayed once two runs agree (_mark_proven).
        return ""

    with _lock:
        _run["offered"] = book["name"]
        said, _run["said"] = _run["said"], True

    if not said:
        try:
            from decurix_agent import progress
            progress.emit("I have a tool I wrote for a request like this: running it.")
        except Exception as exc:  # telling the chat is decoration
            log(f"Playbooks: could not say a playbook is in use: {exc}")

    before = " ".join((book.get("request") or "").split())[:400]

    return (
        f"You have done a request like this on this desktop before (\"{before}\") and kept a tool for it, written "
        "from the commands that worked and were checked then:\n"
        f"  {tool['run']} {tool.get('usage') or ''}\n"
        f"What it does: {tool.get('does') or 'the same work, for other values'}\n"
        f"That time it was: {tool['run']} {tool.get('example') or ''}\n"
        "Run it once with this request's values, as your first command. It prints what it wrote: when that is "
        "what this request asks for, report it from that output - do not do the work again by hand, and do not "
        "read back what it has already printed. If it fails, or this request asks for something it does not do, "
        "do the work yourself as you would any task."
    )


# ============================================================
# A tool of the worker's own
# ============================================================

DISTIL_SYSTEM = (
    "You turn work a desktop worker has just done by hand into a tool it can run the next time. You are given the "
    "request and the shell commands that did it; their result was checked and is right. Write ONE script that does "
    "the same work for any request of this kind, taking what changes from request to request as command-line "
    "arguments.\n"
    "Rules:\n"
    "- Python 3 with the standard library only, or bash. It may call the programs the commands called (for example "
    "`office`), in the same way they did. It does nothing the commands did not do: no network, nothing deleted, "
    "nothing sent.\n"
    "- Every argument is something the request states word for word: a file by its name, an address, a phrase it "
    "quotes. Whatever is derived from those (a month's full name from a file's name, a label, a title) is worked "
    "out inside the script, never passed in.\n"
    "- Nothing of this request's own is fixed in it: file names, names, dates and amounts are arguments. What every "
    "request of this kind shares (the columns it reads, the layout of what it writes) may be fixed.\n"
    "- It checks its arguments and its input, and exits non-zero with a plain message when they are not what it "
    "expects. It must never write a wrong result quietly.\n"
    "- It ends by printing what it wrote: the path, and the contents or the figures that matter, so that its output "
    "alone is enough to report the result and to check it.\n"
    "Answer with JSON only: {\"worth\": true, \"language\": \"python\" or \"bash\", \"script\": \"...\", \"usage\": "
    "\"ARG1 ARG2 - what each is\", \"example\": \"the arguments for the request it was made from\", \"does\": \"one "
    "sentence\"}. Answer {\"worth\": false} when the work was a one-off, a look-up whose answer is the whole point, "
    "or cannot be made general."
)
MAX_SCRIPT_CHARS = 20000
# The script, and again - told what went wrong - when it fails its proof.
DISTIL_ATTEMPTS = 3
# How many times a script is tried for from one kind of request's clean runs
# when none written so far could be proven. Found live: the same task got a
# good script on one day's first try and none in two tries the next.
MAX_DISTIL_SESSIONS = 2
# A tool the engine was given and did not run this many times running does not fit the requests it is matched to.
DROP_UNUSED_AFTER = 2


def _drop_tool(book):
    tool = book.pop("tool", None)
    if tool and tool.get("path"):
        try:
            Path(tool["path"]).unlink()
        except OSError:
            pass


def in_request(arg, request):
    """Whether an argument is the request's own: its text, or for a path the file's name, is in the request."""

    said = " ".join((request or "").lower().split())
    arg = (arg or "").strip()
    if not arg:
        return False
    if " ".join(arg.lower().split()) in said:
        return True
    name = arg.replace("\\", "/").rstrip("/").rsplit("/", 1)[-1].lower()
    return bool(name) and name != arg.lower() and name in said


def _stated(tool, request, commands=None):
    """
    "" when every argument of the tool's example is something its request
    states word for word. Found live: a script took a "period label" the
    request never states ("January", for a file named sales_jan.csv); the next
    request's reading had to guess one - "April" was refused as a guess and
    the engine did the work, "sales_mar" passed and ended up in the email. An
    argument is what a request says; anything worked out from it is the
    script's to work out.
    """

    try:
        args = shlex.split(tool.get("example") or "")
    except ValueError as exc:
        return f"its example arguments could not be read ({exc})"
    for arg in args:
        if not in_request(arg, request):
            return (f"Its example argument {arg!r} is not in the request's own words. Every argument must be "
                    "something a request states literally - a file by its name, an address, a phrase it quotes. "
                    "Anything else (a label, a period, a title made from those) is worked out inside the script.")

    # And the other way round: what the request quotes and the work used is
    # the request's to say. Found live: a script rebuilt the email's subject
    # from the file's name instead of taking the quoted subject, and the next
    # request - which stated its subject - could not be given to it.
    given = " ".join(a.lower() for a in args)
    used = " ".join(commands or []).lower()
    for quoted in re.findall(r"\"([^\"\n]{2,200})\"", request or ""):
        phrase = " ".join(quoted.lower().split())
        if phrase in used and phrase not in given:
            return (f"The request quotes \"{quoted}\" and the commands used it, but the script does not take it as "
                    "an argument. Whatever a request states in quotes is an argument, word for word - another "
                    "request will state its own.")
    return ""


# What a kept script may not do, because nothing would ask the person first: it is run in a proof (_prove) and then,
# with no engine, by tool_task - outside the screen's approval gate. A script works on files; acting on the screen
# (the gui command, xdotool, a browser), reaching the network or another machine, or sending mail is the engine's, under
# the gate (review 2026-11-02: model-written scripts ran for real during the proof and then with no approval).
# (`office email` only drafts into the Outbox, which a person sends: a script may draft.)
# This reading is an early refusal and nothing more: a list of words cannot be made sound (`"cu" + "rl"`, an aliased
# import). The boundary is where the script runs - script_sandbox: no network, no display, a scratch copy of the work.
_UNGATED_PROGRAMS = (r"gui|xdotool|xte|ydotool|wmctrl|xdg-open|chromium|chromium-browser|google-chrome|firefox|curl|"
                     r"wget|ssh|scp|sftp|rsync|nc|ncat|netcat|telnet|ftp|sendmail|mailx|mutt|msmtp|swaks")
# In a shell script: one of those programs where a command starts (a line, after ; & | ( ` $( or sudo/env).
UNGATED_IN_SHELL = re.compile(r"(?:^|[;&|(`{]|\$\()\s*(?:(?:sudo|env|exec|command|nohup|time|do|then|else|if|while|until|"
                              r"!)\s+)*(?:[\w./~-]*/)?(?:" + _UNGATED_PROGRAMS
                              + r")(?=$|[\s;&|)`])", re.MULTILINE)
# In a Python script: a module that reaches the network, the screen or a browser, or one of those programs run.
UNGATED_IN_PYTHON = re.compile(
    r"^\s*(?:import|from)\s+(?:urllib|http|requests|httpx|aiohttp|socket|smtplib|ftplib|telnetlib|paramiko|websockets?|"
    r"selenium|playwright|pyautogui|pynput|webbrowser|imaplib|poplib|xmlrpc|ssl|ctypes|multiprocessing\.connection)\b"
    # (code it builds at run time is not read here, so it is not run: exec/eval of a string.)
    r"|(?<![\w.])(?:exec|eval)\("
    r"|\b(?:subprocess\.\w+|os\.system|os\.popen|os\.exec\w*|os\.spawn\w*)\(\s*\[?\s*[rbf]?[\"'](?:[\w./~-]*/)?(?:"
    + _UNGATED_PROGRAMS
    + r")(?=[\s\"'])"
    r"|\b__import__\(|\bimportlib\b",
    re.MULTILINE,
)


# What only the gate, the agent or the screen use, named in a script in any language: the worker's own folder (the
# approval store, the command channel), the display and its key, the agent's token, the browser's DevTools port and the
# agent's and soul's ports. With `unshare` alone the sandbox still shows the filesystem (script_sandbox), so a script
# that names these is refused before it runs - an early refusal, never the boundary.
GATE_STATE = re.compile(r"\.decurix\b|X11-unix|\.Xauthority|\b(?:DISPLAY|XAUTHORITY|DBUS_SESSION_BUS_ADDRESS|"
                        r"DECURIX_AGENT_TOKEN)\b|:(?:9222|8080|8081|4900|8092)\b|approval\.json|-reply\.txt")


def ungated_effect(script, language="python"):
    """What a script does that only the engine may do under the approval gate, or "". A shell script is read for the
    programs it runs (UNGATED_IN_SHELL); a Python one for what it imports and runs (UNGATED_IN_PYTHON) - and a Python
    one that runs a shell (`bash -c`, shell=True) is read as a shell script too."""

    script = script or ""
    touched = GATE_STATE.search(script)
    if touched:
        return " ".join(touched.group(0).split())
    found = None
    if language == "bash":
        found = UNGATED_IN_SHELL.search(script)
    else:
        found = UNGATED_IN_PYTHON.search(script)
        if found is None and re.search(r"shell\s*=\s*True|[\"'](?:ba|z)?sh[\"']\s*,\s*[\"']-c[\"']", script):
            found = re.search(r"\b(?:" + _UNGATED_PROGRAMS + r")\b(?=\s)", script)
    return " ".join(found.group(0).split()) if found else ""


def _sound(script, language):
    """Why a script the model wrote is not kept, or "": it must parse, and hold no credential, nothing commands.py
    stops, and nothing that acts beyond the work folder's files (ungated_effect)."""

    if not script.strip() or len(script) > MAX_SCRIPT_CHARS:
        return "empty or too long"
    if privacy.credential_in(script):
        return "it holds a credential"
    for line in script.splitlines():
        reason = shell_rules.dangerous_reason(line)
        if reason:
            return f"a line of it would need approval ({reason})"
    effect = ungated_effect(script, language)
    if effect:
        return (f"it acts beyond the work folder's files ({effect!r}): a kept script runs with no approval, so it may "
                "only read and write files - the screen, the network and mail are the engine's, under the approval gate")
    if language == "python":
        try:
            compile(script, "tool.py", "exec")
        except SyntaxError as exc:
            return f"it does not parse ({exc.msg}, line {exc.lineno})"
    return ""


# ------------------------------------------------------------
# Proving a tool before it is kept
#
# Found live: the first tool a model wrote was kept untested. It had been
# written from a run that updated a workbook already there, refused any
# output file that did not exist yet, and called `office update` without the
# "-" that makes it read its spec - so its first use failed, and the engine
# did the work by hand (correctly, at the full price). A tool is only worth
# keeping if it can do again what was just done and checked.
#
# So it is run, with the arguments of the request it was made from, in a
# private copy of the work folder (HOME moved there, so ~/work is the copy
# too), and what it writes must read the same as what the checked run left.
# The files the task itself wrote (produced.files) are left out of the copy:
# the tool has to make them, as the task did - a result that was there to be
# updated is not there the next time. Nothing in the real work folder is
# touched, and no model is asked.
# ------------------------------------------------------------

PROOF_SECONDS = 120
PROOF_MAX_FILES = 2000
PROOF_MAX_BYTES = 100 * 1024 * 1024
NUMBER = re.compile(r"-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?")


def _same_reading(mine, theirs):
    """Whether two files' text reads the same: the same words, and the same numbers however many digits they were printed with."""

    def parts(text):
        out = []
        for word in (text or "").split():
            for piece in re.split(f"({NUMBER.pattern})", word):
                if not piece:
                    continue
                try:
                    out.append(float(piece))
                except ValueError:
                    out.append(piece)
        return out

    a, b = parts(mine), parts(theirs)
    if len(a) != len(b):
        return False
    for x, y in zip(a, b):
        if isinstance(x, float) and isinstance(y, float):
            if abs(x - y) > 1e-6 * max(1.0, abs(x), abs(y)):
                return False
        elif x != y:
            return False
    return True


def _reading(path):
    """A file as text: a document through the office reader, anything else as it is."""

    from decurix_agent.tools import office

    try:
        return office.read_document(str(path))["text"]
    except Exception:
        return Path(path).read_bytes().decode("utf-8", errors="replace")


def _copy_work(work, sandbox, leave_out=()):
    """Copy the work folder into `sandbox`/work; False when it is too big to prove anything in (script_sandbox)."""

    return script_sandbox.copy_work(work, sandbox, leave_out)


def proof_env(sandbox):
    """
    The environment a script is proven and run in (script_sandbox.clean_env): built from nothing, HOME and the
    worker's own folder in the sandbox (a fresh approval store, no secrets, no command channel), drafts kept there, no
    display or bus, no agent token, the control plane, soul and every proxy pointed at nothing - inside a network
    namespace of its own (script_sandbox.run), which is the boundary; this and _sound's reading are fences before it.
    """

    return script_sandbox.clean_env(sandbox)


def _run_in_copy(tool, sandbox, work):
    """Run the tool with its example arguments inside the copy. Returns (written relative paths, "" or what went wrong)."""

    copy = sandbox / "work"
    try:
        args = [a.replace(str(work), str(copy)).replace("~/work", str(copy)) for a in shlex.split(tool["example"])]
    except ValueError as exc:
        return [], f"its example arguments could not be read ({exc})"

    before = script_sandbox.snapshot(copy)
    try:
        # A Python tool is tried with the interpreter this agent runs on - the one its `python3` is. The script is
        # copied into the sandbox first: it runs from there, and what it is cannot change under the run.
        interpreter = "bash" if tool["path"].endswith(".sh") else sys.executable
        script = Path(sandbox) / ("tool" + Path(tool["path"]).suffix)
        shutil.copy2(tool["path"], script)
        done = script_sandbox.run([interpreter, str(script)] + args, sandbox, PROOF_SECONDS)
    except RuntimeError as exc:
        return [], f"it cannot be tried here: {exc}"
    except (OSError, subprocess.TimeoutExpired) as exc:
        return [], f"it could not be run ({exc})"
    if done.returncode != 0:
        return [], f"it exited {done.returncode}: {(done.stderr or done.stdout or '').strip()[-600:]}"
    if not (done.stdout or "").strip():
        return [], "it printed nothing: it has to print what it wrote"

    return script_sandbox.written(copy, before), ""


def _same_drafts(sandbox):
    """
    "" when the emails the tool drafted in its trial (kept in the copy, sent
    nowhere) are the ones the task drafted: as many, to the same people, with
    the same subject, words and attachments. Otherwise what differs.
    """

    from decurix_agent.tools import outbox

    task, trial = produced.drafts(), outbox.read_log(sandbox / "drafts.jsonl")
    if len(task) != len(trial):
        return (f"The task drafted {len(task)} email(s) with `office email`; the script drafted {len(trial)}. It "
                "must draft exactly what the task did.")
    for n, (was, now) in enumerate(zip(task, trial), 1):
        for part in ("to", "cc", "attachments"):
            if sorted(x.lower() for x in was.get(part) or []) != sorted(x.lower() for x in now.get(part) or []):
                return (f"Email {n}: the task's draft had {part} {was.get(part) or []}, the script's "
                        f"{now.get(part) or []}.")
        for part in ("subject", "body"):
            if not _same_reading(str(was.get(part) or ""), str(now.get(part) or "")):
                return (f"Email {n}: its {part} does not read the same as the task's. The task wrote:\n"
                        f"{str(was.get(part) or '')[:800]}\n\nThe script wrote:\n{str(now.get(part) or '')[:800]}")
    return ""


def _prove(tool):
    """
    "" when the tool, run in a copy of the work folder with the arguments of
    the request it was made from, writes what the checked run left. Otherwise
    what went wrong, in words the model that wrote it can act on.
    """

    work = Path(config.WORK_DIR)

    # What the task wrote is not in the copy: the tool has to make it, as the
    # task did, and the next request's result will not be there either.
    made = []
    for path in produced.files(""):
        try:
            made.append(Path(path).resolve().relative_to(work.resolve()).as_posix())
        except (OSError, ValueError):
            continue

    sandbox = Path(tempfile.mkdtemp(prefix=".proof-", dir=str(TOOLS)))
    try:
        if not _copy_work(work, sandbox, tuple(made)):
            return "The work folder is too large to try it in a copy."
        written, wrong = _run_in_copy(tool, sandbox, work)
        if wrong:
            return f"Run as the task was - its input there, what it writes not yet there - {wrong}"
        wrong = _same_drafts(sandbox)
        if wrong:
            return wrong
        if not written and not produced.drafts():
            return ("It wrote no file in the work folder it was run in. It must write where its arguments say, "
                    "not to a fixed path.")
        for relative in written:
            real = work / relative
            if not real.is_file():
                return f"It wrote {relative}, which the task itself did not leave."
            if not _same_reading(_reading(sandbox / "work" / relative), _reading(real)):
                return (f"What it wrote to {relative} does not read the same as what the task left there. The "
                        f"task left:\n{_reading(real)[:1200]}\n\nThe script wrote:\n"
                        f"{_reading(sandbox / 'work' / relative)[:1200]}")
        missing = [m for m in made if m not in written]
        if missing:
            return f"It did not write {', '.join(missing)}, which the task did."
    finally:
        shutil.rmtree(sandbox, ignore_errors=True)

    return ""


def _faults_note(book):
    """What earlier scripts for this kind of request got wrong, for the model writing the next one."""

    faults = book.get("tool_faults") or []
    if not faults:
        return ""
    return ("\n\nA script was written for this kind of request before and dropped: run for ANOTHER request of the "
            "kind, its result was checked and found wrong. Do not repeat it - look for anything in your script that "
            "is true only of the request above (a month, a name, a figure, a title) and take it from the arguments "
            "or the input instead:\n" + "\n".join(f"- {fault}" for fault in faults))


def _distil(book):
    """
    Have the hosted model write one script from the commands that worked, and
    keep it beside the playbook once it has been proven (_prove). Paid once,
    for a task done without the screen: one call, and one more when the first
    script fails its proof. Nothing is kept when there is no model, the model
    says the work was a one-off, or no script it wrote reproduced the result.
    """

    book["tool_tried"] = True
    if not llm_client.available():
        return False
    if not script_sandbox.available():
        # A kept script runs with nobody watching, and only shut off from the network and the screen
        # (script_sandbox): where that cannot be done, none is written - no model call is spent on one that could
        # never be proven or run. A later run on a desktop that can may try again.
        log(f"Playbooks: no tool for {book['name']}: {script_sandbox.why_not()}")
        book["tool_tried"] = False
        return False
    book["tool_sessions"] = book.get("tool_sessions", 0) + 1

    messages = [
        {"role": "system", "content": DISTIL_SYSTEM},
        {"role": "user", "content": f"The request:\n{book['request']}\n\nThe commands that did it, in order (each "
                                    "exited 0):\n\n" + "\n\n".join(f"[{n}]\n{c}" for n, c in enumerate(book["commands"], 1))
                                    + _faults_note(book)},
    ]
    one_line = lambda text, limit: " ".join(str(text or "").split())[:limit]  # noqa: E731

    for attempt in range(DISTIL_ATTEMPTS):
        try:
            message, _ = llm_client.chat(messages, max_tokens=5000, response_format={"type": "json_object"})
            found = re.search(r"\{.*\}", message.get("content") or "", re.DOTALL)
            answer = json.loads(found.group(0)) if found else {}
        except (llm_client.LLMError, ValueError) as exc:
            log(f"Playbooks: could not write a tool for {book['name']}: {exc}")
            return False

        if not isinstance(answer, dict) or not answer.get("worth") or not isinstance(answer.get("script"), str):
            log(f"Playbooks: no tool for {book['name']}: the model does not think it is worth one")
            return False

        language = "bash" if str(answer.get("language") or "").lower().startswith(("bash", "sh")) else "python"
        path = TOOLS / (book["name"] + (".sh" if language == "bash" else ".py"))
        tool = {
            "path": str(path), "run": f"{'bash' if language == 'bash' else 'python3'} {path.as_posix()}",
            "usage": one_line(answer.get("usage"), 400), "example": one_line(answer.get("example"), 400),
            "does": one_line(answer.get("does"), 300), "unused": 0,
        }

        wrong = _sound(answer["script"], language) or _stated(tool, book["request"], book["commands"])
        if not wrong:
            try:
                TOOLS.mkdir(parents=True, exist_ok=True)
                path.write_text(answer["script"].rstrip() + "\n", encoding="utf-8", newline="\n")
                wrong = _prove(tool)
            except OSError as exc:
                wrong = f"it could not be saved or tried ({exc})"

        if not wrong:
            book["tool"] = tool
            log(f"Playbooks: wrote a tool for {book['name']} and proved it: {tool['run']} {tool['usage']}")
            return True

        log(f"Playbooks: the tool written for {book['name']} is not kept (attempt {attempt + 1}): {wrong[:300]}")
        try:
            path.unlink()
        except OSError:
            pass
        messages += [
            {"role": "assistant", "content": message.get("content") or ""},
            {"role": "user", "content": "That script was tried on this request's own files, in a copy of the work "
                                        f"folder, and is not good enough to keep. {wrong}\n\nWrite it again so that it "
                                        "does exactly what the commands did and writes exactly what they wrote. "
                                        "The same JSON."},
        ]

    # None could be proven this time: the next clean run by hand may try again, a bounded number of times.
    book["tool_tried"] = book["tool_sessions"] >= MAX_DISTIL_SESSIONS
    return False


def skeleton(events):
    """
    The path a run took on the screen, without its values: for each recorded
    action, what was done to which control - its kind, and the control's role
    and name. Not its ref: on an application that redraws itself a ref is not
    the same from one run to the next (found live: four runs along much the
    same path shared almost none), and not what was typed or chosen, which is
    the request's. Two runs with the same skeleton pressed the same controls
    in the same order for different requests.
    """

    out = []
    for event in events or []:
        if event.get("kind") in (None, "note", "manual", "observe"):
            continue
        target = event.get("element") or {}
        out.append([event["kind"], target.get("role") or "", " ".join(str(target.get("name") or "").split())])
    return out


def _path_of_this_task():
    """This task's skeleton, from its own recording; [] when it left none."""

    from decurix_agent import task_context
    from decurix_agent.computer import recording

    try:
        return skeleton(recording.load(recording.session_for_task(task_context.current()))[1])
    except (OSError, ValueError, KeyError):
        return []


def _mark_proven(request):
    """
    Mark this task's recording as the worker's own way of doing this kind of
    request on the screen, so the next one is replayed from it with no engine
    (computer/mimic.py offers it beside a person's demonstrations). One per
    kind of request: an earlier one of the same shape gives way to this one.

    Measured live: handed its earlier commands as a list, the engine created
    a supplier in 10 turns instead of 12 - 87,000 tokens against 100,000. A
    turn on the screen carries the screenshot and the element list, so only
    not taking the turns saves much; a replay takes none.
    """

    from decurix_agent import task_context
    from decurix_agent.computer import recording

    session = recording.session_for_task(task_context.current())
    words = shape(request)
    try:
        if not (recording.meta(session) or {}).get("event_count"):
            return False
        for other in recording.sessions():
            if other.get("proven") and other["id"] != session and _alike(words, shape(other.get("task") or other.get("title") or "")) >= MATCH:
                recording.update(other["id"], proven=False)
        recording.update(session, proven=True, task=attachments.text(request or "").strip()[:1000])
    except (OSError, ValueError, KeyError) as exc:
        log(f"Playbooks: could not keep this task's recording as its way: {exc}")
        return False
    log(f"Playbooks: {session} is now this desktop's own recording of that kind of request")
    return True


# A replay of the worker's own run that the engine had to take over this many
# times running is no longer the way: the application has moved on from it.
OWN_RETIRE_AFTER = 2


def own_replayed(session, worked):
    """
    A replay of this desktop's own recording ran to its end and was checked
    (`worked`), or stopped and was handed to the engine. Stopping
    OWN_RETIRE_AFTER times running takes the recording out of use, so the
    next request is worked out afresh - and that run becomes the new way.
    """

    from decurix_agent.computer import recording

    meta = recording.meta(session)
    if not meta or not meta.get("proven"):
        return
    if worked:
        if meta.get("misses"):
            recording.update(session, misses=0)
        # Counted where its cost is kept: the playbook of the same kind of request.
        book = find(meta.get("task") or meta.get("title") or "")
        if book is not None:
            record = use(True, [])
            record.update(turns=0, direct=True, replay=True)
            book["uses"] = ((book.get("uses") or []) + [record])[-USE_HISTORY:]
            try:
                save(book)
            except OSError as exc:
                log(f"Playbooks: could not count a replay: {exc}")
        return
    misses = int(meta.get("misses") or 0) + 1
    if misses >= OWN_RETIRE_AFTER:
        recording.update(session, proven=False, misses=0)
        log(f"Playbooks: {session} is no longer replayed: it stopped {misses} times running")
    else:
        recording.update(session, misses=misses)


def keep(request, skill=None):
    """
    The task ended and its result was checked done. Counts the run for the
    playbook that was offered, and keeps the way this run took when it is the
    first, or no longer than the one kept; a first way that never touched the
    screen is also written out as a tool (_distil). Returns the playbook's
    name, or None.
    """

    if not ENABLED:
        return None

    with _lock:
        segments, offered, paused = [list(s) for s in _run["segments"]], _run["offered"], _run["paused"]
        steered = _run["answered"]

    # A run the check sent back holds a wrong turn and its repair, and one a person answered a question
    # of did what they said, not only what the request says: counted, not kept as the way.
    clean = len(segments) == 1 and not steered
    ran = segments[0] if clean else []
    commands = tidy(ran, request)
    run = use(True, commands)
    worth = bool(commands) and (run["turns"] >= MIN_TURNS if run["turns"] else len(commands) >= MIN_COMMANDS)

    # A clean run on the screen, of a request nobody taught, becomes what the next one replays - once the
    # engine has taken the same path through two requests of the kind, each checked. One run shows a way
    # that worked; two that agree show it was not that request's accident. Until then the engine does the
    # work, with the earlier way as its guide: the first runs of anything are the model's, on purpose.
    path = _path_of_this_task() if clean and worth and not skill and not paused and _on_screen(commands) else None
    if path:
        earlier = load(offered) if offered else find(request, skill)
        # Any earlier clean run of the kind, not only the last: an engine that wandered once in between
        # has not unlearned the way the others took.
        if earlier is not None and path in (earlier.get("skeletons") or []):
            _mark_proven(request)

    book = load(offered) if offered else None
    if book is None and clean and worth:
        book = find(request, skill)

    try:
        if book is not None:
            book["uses"] = ((book.get("uses") or []) + [run])[-USE_HISTORY:]
            if clean:
                book["misses"] = 0
            if path:
                book["skeletons"] = ([p for p in book.get("skeletons") or [] if p != path] + [path])[-KEEP_PATHS:]

            tool = book.get("tool")
            if tool and offered:
                # Given the tool: did the engine run it? One it keeps passing over does not fit.
                if any(Path(tool["path"]).as_posix() in c for s in segments for c in s):
                    tool["unused"] = 0
                    # A request the tool did under the engine's eye, checked: what tool_task.py counts
                    # before it runs the tool with no engine at all.
                    book["uses"][-1]["tool"] = True
                else:
                    tool["unused"] = tool.get("unused", 0) + 1
                    if tool["unused"] >= DROP_UNUSED_AFTER:
                        log(f"Playbooks: dropped the tool of {book['name']}: given {tool['unused']} times and not run")
                        _drop_tool(book)
            elif clean and commands:
                if len(commands) <= len(book["commands"]):
                    book["commands"], book["longer"] = commands, 0
                else:
                    book["longer"] = book.get("longer", 0) + 1
                    if book["longer"] >= REPLACE_LONGER_AFTER:
                        book["commands"], book["longer"] = commands, 0
                # Shell work kept before there was a tool for it gets its one try at one now.
                if not _on_screen(book["commands"]) and not paused and not book.get("tool_tried"):
                    _distil(book)
            save(book)
            return book["name"]

        if not clean or not worth or len(all_books()) >= MAX_PLAYBOOKS:
            return None

        words = shape(request)
        if len(words) < 2:
            return None
        book = {
            "name": _unique_name(words), "request": attachments.text(request or "").strip()[:1000], "shape": words,
            "skill": skill or None, "commands": commands,
            "made": {k: run[k] for k in ("at", "tokens", "turns", "commands")}, "uses": [], "misses": 0,
        }
        if path:
            book["skeletons"] = [path]
        # A task that stopped for a yes is not made into one command that would not stop.
        if not _on_screen(commands) and not paused:
            _distil(book)
            book["made"]["tokens"] = usage.spent()     # what learning it cost, the tool's writing included
        save(book)
        log(f"Playbooks: kept {book['name']} ({len(commands)} commands, {book['made']['tokens']} tokens the first time)")
        return book["name"]
    except OSError as exc:
        log(f"Playbooks: could not keep this task's way: {exc}")
        return None


# How many scripts may be written for one kind of request before it is left to
# the engine for good: the first, and one more for each that proved wrong.
MAX_TOOL_FAULTS = 2


def tool_failed(book, why):
    """
    A tool's result was checked and found wrong. It is dropped at once - a
    script that wrote one wrong result is not run again - and what was wrong
    is kept, to be told to whoever writes the next one. Found live: a script
    proven on the request it was written from had that request's month fixed
    in the email it drafts; its first other request drafted "January" for
    February, which the final check refused. Proving a script on one request
    cannot show what it took from that request; its first use does.
    The caller saves.
    """

    if not book.get("tool"):
        return
    _drop_tool(book)
    faults = (book.get("tool_faults") or []) + [" ".join((why or "its result was not what the request asked").split())[:400]]
    book["tool_faults"] = faults[-MAX_TOOL_FAULTS:]
    # The next clean run by hand may have a script written from it - until too many have gone wrong.
    book["tool_tried"] = len(faults) >= MAX_TOOL_FAULTS
    log(f"Playbooks: dropped the tool of {book['name']}: its result was checked and found wrong ({faults[-1][:160]})")


def missed(why=""):
    """
    The task ended not done, or failed. Counts against the playbook that was
    offered and drops it after RETIRE_AFTER running; a tool that was run in
    the task is dropped now (tool_failed).
    """

    with _lock:
        offered, _run["offered"] = _run["offered"], None
        # The rest of this task is done without it: the second attempt works from what the check said.
        _run["declined"] = True
        ran = [c for s in _run["segments"] for c in s]
    book = load(offered) if offered else None
    if book is None:
        return

    try:
        book["uses"] = ((book.get("uses") or []) + [use(False, [])])[-USE_HISTORY:]
        tool = book.get("tool")
        if tool and any(Path(tool["path"]).as_posix() in c for c in ran):
            tool_failed(book, why)
            save(book)
            return
        book["misses"] = book.get("misses", 0) + 1
        if book["misses"] >= RETIRE_AFTER:
            _drop_tool(book)
            delete(book["name"])
            log(f"Playbooks: dropped {book['name']}: not done {book['misses']} times running with it")
        else:
            save(book)
    except OSError as exc:
        log(f"Playbooks: could not count a miss: {exc}")


# ============================================================
# Whether it pays
# ============================================================

def economy(book):
    """
    What a playbook has saved: {"first": tokens of the run it came from,
    "now": the mean of its last three good uses (None before the first),
    "uses", "ok", "saved_percent"}.
    """

    first = int((book.get("made") or {}).get("tokens") or 0)
    uses = book.get("uses") or []
    good = [int(u.get("tokens") or 0) for u in uses if u.get("ok")][-3:]
    now = round(sum(good) / len(good)) if good else None
    saved = round(100 * (first - now) / first) if first and now is not None else None
    return {"first": first, "now": now, "uses": len(uses), "ok": sum(1 for u in uses if u.get("ok")),
            "saved_percent": saved}


# ============================================================
# For a person: what this desktop worked out for itself
# ============================================================

NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,79}$")


def _own_recordings():
    from decurix_agent.computer import recording

    return [m for m in recording.sessions() if m.get("kind") == "task" and m.get("proven")]


def listing():
    """
    Everything this desktop keeps from work it did itself, one entry per kind
    of request: {"name", "request", "how", "commands", "tool", "recording",
    "first", "now", "uses", "ok", "saved_percent", "made_at"}. `how` is "tool"
    (a script of its own, run with no engine once it has been used), "replay"
    (its own checked run on the screen, replayed with no engine) or "way" (the
    commands that worked, kept to write a tool from or to compare runs by; the
    engine still works such a request out each time).

    A taught skill's runs are left out: the skill is what was kept of them,
    and it shows its own cost (experience.economy). Listed here, the engine's
    request for a taught task read as something worked out untaught, and the
    saving its fitted replay made was put down to commands nobody was given.
    """

    own = _own_recordings()
    taken, out = set(), []

    for book in all_books():
        if book.get("skill"):
            continue
        entry = {"name": book["name"], "request": book.get("request", ""), "skill": book.get("skill"),
                 "commands": len(book["commands"]), "made_at": (book.get("made") or {}).get("at"),
                 "tool": (book.get("tool") or {}).get("run"), "recording": None, **economy(book)}
        for meta in own:
            if _alike(book.get("shape") or [], shape(meta.get("task") or meta.get("title") or "")) >= MATCH:
                entry["recording"] = meta["id"]
                taken.add(meta["id"])
                break
        entry["how"] = "tool" if entry["tool"] else "replay" if entry["recording"] else "way"
        out.append(entry)

    for meta in own:
        if meta["id"] not in taken:
            out.append({"name": meta["id"], "request": meta.get("task") or meta.get("title") or "", "skill": None,
                        "commands": meta.get("event_count") or 0, "made_at": int(meta.get("started_at") or 0),
                        "tool": None, "recording": meta["id"], "how": "replay", "first": 0, "now": None, "uses": 0,
                        "ok": 0, "saved_percent": None})
    return out


def detail(name):
    """One entry of listing() with what it holds: its commands, and its tool's script. None when there is none."""

    entry = next((e for e in listing() if e["name"] == name), None)
    if entry is None:
        return None
    book = load(name) or {}
    entry["steps"] = list(book.get("commands") or [])
    tool = book.get("tool") or {}
    entry["usage"], entry["does"], entry["script"] = tool.get("usage"), tool.get("does"), None
    if tool.get("path"):
        try:
            entry["script"] = Path(tool["path"]).read_text(encoding="utf-8")[:MAX_SCRIPT_CHARS]
        except OSError:
            pass
    return entry


def forget(name):
    """Drop one entry of listing(): its playbook, its tool, and its own recording's standing (the recording stays a task's)."""

    from decurix_agent.computer import recording

    entry = next((e for e in listing() if e["name"] == name), None)
    if entry is None:
        return False
    if entry.get("recording"):
        recording.update(entry["recording"], proven=False, misses=0)
    book = load(name)
    if book is not None:
        _drop_tool(book)
        delete(name)
    return True


def qualification_source(name):
    """Export the exact local tool bytes; readiness must never hash a preview."""
    if not NAME.fullmatch(name):
        raise ValueError("not a playbook name")
    book = load(name) or {}
    tool = book.get("tool") or {}
    target = Path(tool.get("path") or "")
    if not target.is_file() or target.suffix not in {".py", ".sh"}:
        raise FileNotFoundError("no saved Python or Bash tool")
    # Know-how exports are bounded and confined to the tool store. They never
    # turn an arbitrary agent-supplied path into a host/control-plane read.
    if not target.resolve().is_relative_to(TOOLS.resolve()):
        raise ValueError("the tool is outside the saved tools directory")
    content = target.read_bytes()
    if not content or len(content) > 128 * 1024:
        raise ValueError("the tool is empty or too large for qualification")
    return {"script": content.decode("utf-8"), "language": "bash" if target.suffix == ".sh" else "python"}
