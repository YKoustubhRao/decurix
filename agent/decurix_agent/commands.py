import os
import posixpath
import re
import subprocess

from decurix_agent import config
from decurix_agent.logging_utils import log


# ============================================================
# Command execution
# ============================================================

# Paths that must never be deleted, recursively or wholesale, without a
# person saying so: the filesystem root, the system directories, and the
# whole home directory (which holds the work dir, the agent's own state and
# every recording).
_PROTECTED = (
    r"/|/\*|~/?|~/\*|\$HOME/?|\$\{HOME\}/?|\$HOME/\*|"
    r"/(?:etc|usr|var|bin|sbin|lib|lib32|lib64|boot|home|opt|root|sys|proc|dev|srv|mnt|run)(?:/\*?)?|"
    r"/home/[^/\s]+/?(?:\*)?"
)

_RM_RECURSIVE = re.compile(
    r"(?:^|[;&|(]\s*|\bsudo\s+)rm\s+(?:-[a-zA-Z-]*\s+)*?"
    r"(?:-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\b(?:\s+-[a-zA-Z-]+)*\s+(?:--\s+)?"
    r"(?:[^;&|]*?\s)?(?:" + _PROTECTED + r")(?=\s|$|[;&|])"
)

# What a word list cannot read is not guessed at (a list of spellings is never complete): a command whose program is
# named by a variable or a substitution, text piped into a shell or an interpreter, eval, and an interpreter's inline
# code that deletes or runs commands. Each waits for a person, who sees the command as written. Ordinary work does not
# look like these: `python3 script.py`, `grep x | sort`, `echo "$(date)"` all run.
_COMMAND_START = r"(?:^|[;&|(]\s*|\bsudo\s+|\bxargs\s+|\b(?:then|do|else)\s+)"
_UNREADABLE = (
    # ($HOME/bin/tool and $PWD/tool are paths, not a program chosen at run time: round-3 recheck.)
    (re.compile(_COMMAND_START + r"(?:\$(?!(?:HOME|PWD)/|\{(?:HOME|PWD)\}/)\{?\w+\}?|\$\(|`)", re.MULTILINE),
     "it runs a program named by a variable or a substitution, which cannot be checked"),
    (re.compile(_COMMAND_START + r"eval\b", re.MULTILINE), "it runs text built at run time (eval)"),
    (re.compile(r"\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:env\s+(?:\S+=\S*\s+)*)?(?:/\S*/)?(?:ba|z|da|k|c)?sh(?:\s+-s?)?\s*(?:$|[;&|)\n])",
                re.MULTILINE),
     "it pipes text into a shell, which cannot be checked"),
    (re.compile(r"\|\s*(?:sudo\s+(?:-\S+\s+)*)?(?:/\S*/)?(?:python[0-9.]*|perl|ruby|node|nodejs|php|deno)\s*(?:-\s*)?(?:$|[;&|)\n])",
                re.MULTILINE),
     "it pipes text into an interpreter, which cannot be checked"),
    (re.compile(r"\b(?:python[0-9.]*|perl|ruby|node|nodejs|php)\s+-[a-zA-Z]*[ce]\b[^\n]*?"
                r"(?:rmtree|os\.(?:remove|unlink|rmdir|removedirs|system|popen|exec\w*)|\bunlink\b|\bsystem\s*\(|"
                r"subprocess|execSync|child_process|\brm\s+-)", re.IGNORECASE),
     "an interpreter's inline code deletes files or runs commands, which cannot be checked"),
)

# find running rm on what it matches, from where it starts: inside the work folder (a relative path, or none - find
# starts where the command runs, the work folder) it is ordinary clean-up; anywhere else the matches cannot be checked.
_FIND_RM = re.compile(r"\bfind\b([^;&|\n]*?)-exec(?:dir)?\s+(?:/\S*/)?rm\b")


def _in_work_folder(start):
    """Whether a path find starts from is the work folder or inside it."""

    start = start.strip("'\"")
    if not start.startswith(("/", "~", "$")):
        return ".." not in start.split("/")
    home = os.environ.get("HOME") or posixpath.expanduser("~")
    path = re.sub(r"^(?:\$\{HOME\}|\$HOME|~)(?=/|$)", lambda _: home, start)
    if "$" in path or "~" in path:
        return False
    path = posixpath.normpath(path)
    work = posixpath.normpath(str(config.WORK_DIR).replace("\\", "/"))
    return path == work or path.startswith(work + "/")


def _find_rm_outside_work(text):
    for m in _FIND_RM.finditer(text or ""):
        starts = []
        for word in m.group(1).split():
            if word.startswith(("-", "(", "!", "\\(")):
                break
            starts.append(word)
        if any(not _in_work_folder(s) for s in starts):
            return True
    return False

_OTHER_DANGEROUS = (
    (re.compile(r"--no-preserve-root"), "it disables rm's protection for the filesystem root"),
    (re.compile(r"\bmkfs(\.\w+)?\b"), "it formats a filesystem"),
    (re.compile(r"\bdd\b[^;&|]*\bof=/dev/(?!null\b|zero\b|stdout\b|stderr\b)"), "it writes straight onto a device"),
    (re.compile(r">\s*/dev/(?:sd|nvme|vd|hd)\w*"), "it overwrites a disk device"),
    (re.compile(r"\bshred\b[^;&|]*\s/dev/"), "it destroys a device"),
    (re.compile(r":\s*\(\s*\)\s*\{[^}]*:\s*\|\s*:"), "it is a fork bomb"),
    (re.compile(r"\b(chmod|chown)\s+(?:-\w+\s+)*-\w*R\w*\s+(?:\S+\s+)?/(?:\s|$)"), "it changes permissions across the whole filesystem"),
    (re.compile(r"\bfind\s+(?:/|~|\$HOME)(?:\s|/)[^;&|]*-delete\b"), "it deletes everything find matches under a top-level directory"),
    (re.compile(r"\bmv\s+(?:/|~|\$HOME)/?\s"), "it moves a top-level directory away"),
)


def dangerous_reason(command):
    """
    Why a shell command must wait for a person, or None if it may run.

    Deliberately narrow: only what is catastrophic and hard to undo on this
    machine - wiping the root, a system directory or the whole home,
    formatting, writing onto a device, a fork bomb. The agent is trusted with
    an unrestricted shell (see CLAUDE.md), and `rm -rf build/` is ordinary
    work; this is the net under the model's own judgement, not a replacement
    for it. Measured live: asked to "delete every file in /etc", the small
    model's only defence was a question the next pushback talked it out of.
    """

    text = command or ""

    if _RM_RECURSIVE.search(text):
        return "it deletes a system directory, the filesystem root or the whole home directory"

    for pattern, reason in _OTHER_DANGEROUS:
        if pattern.search(text):
            return reason

    # Quoted text and here-document bodies are data, not commands: `echo "$(date)"` and a heredoc's own lines are not read.
    code = _without_data(text)
    for pattern, reason in _UNREADABLE:
        # The inline-code and find rules read the text as written (the code is inside the quotes).
        if pattern.search(text if reason.startswith(("an interpreter", "find ")) else code):
            return reason
    if _find_rm_outside_work(text):
        return "find runs rm on everything it matches outside the work folder"

    return None


_HEREDOC = re.compile(r"<<-?\s*(['\"]?)(\w+)\1[^\n]*\n.*?\n\s*\2\s*(?:\n|$)", re.DOTALL)
_QUOTED = re.compile(r"'[^'\n]*'|\"(?:\\.|[^\"\\\n])*\"")


def _without_data(text):
    """A command with its here-document bodies and quoted strings blanked, so what is left is what the shell runs."""

    text = _HEREDOC.sub(lambda m: m.group(0).split("\n", 1)[0] + "\n", text)
    return _QUOTED.sub('""', text)


# Commands run in /bin/sh (dash in the desktop images), not bash - on
# purpose, measured. Models write bash (`grep ... <<< "$text"`), and in dash
# that is a syntax error, so running them in bash looks like an obvious fix.
# On the desktops' model (qwen2.5-coder:3b) it made results worse: with the
# extraction step working, the model chained more steps, carried shell
# variables between commands that each run in a fresh shell, and wrote empty
# lines or stopped after extracting - 2 and 3 of 8 real email-triggered tasks
# right, against 6 of 8 with dash, where the failing `<<<` pushed it back to
# a plain `echo ... >> file`. Re-measure before changing it.


def run_command(command, timeout=120):
    """
    Execute a shell command inside the desktop container.

    Commands run as the headless user, in /bin/sh (see above).
    """

    log(f"Executing command: {command}")
    try:
        from decurix_agent import playbooks
        playbooks.note_command(command)
    except Exception:  # noqa: BLE001 - noting it is for the final check; the command still runs
        pass

    try:
        process = subprocess.run(
            command,
            shell=True,
            cwd=str(config.WORK_DIR),
            capture_output=True,
            text=True,
            timeout=timeout,
        )

        output = ""

        if process.stdout:
            output += process.stdout

        if process.stderr:
            output += "\n" + process.stderr

        output = output.strip()

        log(
            f"Command exit code: "
            f"{process.returncode}"
        )

        if output:
            log(output[-4000:])

        return {
            "success": process.returncode == 0,
            "exit_code": process.returncode,
            "output": output,
        }

    except subprocess.TimeoutExpired:
        return {
            "success": False,
            "exit_code": -1,
            "output": (
                "Command timed out after "
                f"{timeout} seconds."
            ),
        }

    except Exception as exc:
        return {
            "success": False,
            "exit_code": -1,
            "output": str(exc),
        }
