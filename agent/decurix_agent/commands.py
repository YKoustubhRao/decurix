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
    r"(?:-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\b(?:\s+-[a-zA-Z-]+)*\s+"
    r"(?:[^;&|]*?\s)?(?:" + _PROTECTED + r")(?=\s|$|[;&|])"
)

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

    return None


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
