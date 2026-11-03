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

# The rm rule above reads the text as written, so every new spelling of the same command got past it (round-5
# recheck: rm -rf "$HOME", bash -c 'rm -rf ~', /bin/rm, env/nohup/timeout before rm, ~/.., cd / && rm -rf *). The
# word reader below splits a command the way the shell does, unquotes each word, follows bash -c into its script and
# cd into its folder, and resolves each path rm is given before deciding. It is still a guard rail, not the gate.
_SYSTEM_DIRS = ("/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32", "/media", "/mnt",
                "/opt", "/proc", "/root", "/run", "/sbin", "/srv", "/sys", "/usr", "/var")
_SHELLS = {"sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "su"}
# Programs that run the program after them: the letters of their options that take a value, and how many plain words
# come before that program ("timeout 5 rm ...", "sudo -u root rm ...", "xargs -I {} rm ..."). Shell keywords too.
_WRAPPERS = {"sudo": ("ugCDhprtUT", 0), "doas": ("uC", 0), "env": ("uC", 0), "nohup": ("", 0), "nice": ("n", 0),
             "ionice": ("cnp", 0), "timeout": ("sk", 1), "time": ("fo", 0), "command": ("", 0), "builtin": ("", 0),
             "exec": ("a", 0), "setsid": ("", 0), "stdbuf": ("ioe", 0), "chrt": ("", 1), "taskset": ("", 1),
             "xargs": ("IEdLnPsa", 0), "unbuffer": ("", 0), "flock": ("wE", 1), "chroot": ("", 1), "busybox": ("", 0),
             "watch": ("nd", 0), "strace": ("eopsu", 0), "ltrace": ("eou", 0),
             "if": ("", 0), "then": ("", 0), "else": ("", 0), "elif": ("", 0), "do": ("", 0), "while": ("", 0),
             "until": ("", 0), "!": ("", 0), "{": ("", 0)}
_ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
_SEPARATORS = set(";&|()\n")
_GLOB = re.compile(r"[*?\[]")


def _home():
    return posixpath.normpath(os.environ.get("HOME") or posixpath.expanduser("~") or "/home/headless")


def _words(text):
    """The simple commands of a shell text, each as its list of unquoted words; None when the text cannot be split."""

    import shlex

    lexer = shlex.shlex(text, posix=True, punctuation_chars=";&|()\n")
    lexer.whitespace = " \t\r"
    lexer.whitespace_split = True
    commands, current = [], []
    try:
        for token in lexer:
            if token and set(token) <= _SEPARATORS:
                if current:
                    commands.append(current)
                current = []
                continue
            # A substitution's opening ("$(rm", "`rm") starts a command of its own.
            parts = re.split(r"\$\(|`", token)
            for k, part in enumerate(parts):
                if k and current:
                    commands.append(current)
                    current = []
                if part:
                    current.append(part)
    except ValueError:
        return None
    if current:
        commands.append(current)
    return commands


def _resolve(word, cwd):
    """The absolute path a word names, or None when it is decided at run time (another variable, a substitution)."""

    home = _home()
    path = re.sub(r"^(?:\$\{HOME\}|\$HOME|~)(?=/|$)", lambda _: home, word)
    path = re.sub(r"^~root(?=/|$)", "/root", path)
    path = re.sub(r"^~([a-z_][a-z0-9_-]*)(?=/|$)", r"/home/\1", path)
    if "$" in path or "`" in path or path.startswith("~"):
        return None
    if not path.startswith("/"):
        path = posixpath.join(cwd, path)
    return "/" + posixpath.normpath(path).lstrip("/")


def _protected_path(path):
    """Whether deleting a resolved path (or everything a glob in it matches) wipes the root, a system directory, a
    home or the home itself."""

    parts = path.split("/")
    for i, part in enumerate(parts):
        if _GLOB.search(part):
            # What a glob matches lies in the folder above it: "/h*" and "~/*" match what is in / and in the home.
            parent = "/".join(parts[:i]) or "/"
            return _protected_path(parent)
    home = _home()
    if path in ("/",) + _SYSTEM_DIRS or path == home or home.startswith(path.rstrip("/") + "/"):
        return True
    return bool(re.fullmatch(r"/home/[^/]+|/(?:usr|var|etc)/[^/]+", path))


def _rm_wipes(words, start, cwd):
    """Whether `rm` at words[start] deletes recursively something protected."""

    recursive, operands, options_done = False, [], False
    for word in words[start + 1:]:
        if not options_done and word == "--":
            options_done = True
        elif not options_done and word.startswith("--") and len(word) > 2:
            recursive = recursive or "--recursive".startswith(word.split("=", 1)[0])
        elif not options_done and word.startswith("-") and len(word) > 1:
            recursive = recursive or "r" in word or "R" in word
        else:
            operands.append(word)
    if not recursive:
        return False
    for word in operands:
        path = _resolve(word, cwd)
        if path is not None and _protected_path(path):
            return True
    return False


def _rm_wipe_reason(text, depth=0):
    """Why a command deletes a system directory, the root or the home, read word by word; or None."""

    if depth > 4:
        return "it nests shells too deeply to be checked"
    # A here-document's body is data, unless a shell reads it (bash <<EOF): then its lines are commands.
    bodies = []

    def _blank(m):
        head, _, body = m.group(0).partition("\n")
        line = m.string[m.string.rfind("\n", 0, m.start()) + 1:m.start()]
        if re.search(r"(?:^|[\s;&|(])(?:\S*/)?(?:ba|z|da|k|mk)?sh\b", line):
            bodies.append(body)
        return head + "\n"

    text = _HEREDOC.sub(_blank, text or "")
    for body in bodies:
        inner = _rm_wipe_reason(body, depth + 1)
        if inner:
            return inner
    commands = _words(text)
    if commands is None:
        # Unbalanced quotes: read each line alone, so one line of prose does not hide the next.
        commands = [c for line in text.splitlines() for c in (_words(line) or [])]
    cwd = posixpath.normpath(str(config.WORK_DIR).replace("\\", "/"))
    for words in commands:
        i, scripts = _program(words)
        for script in scripts:
            inner = _rm_wipe_reason(script, depth + 1)
            if inner:
                return inner
        if i >= len(words):
            continue
        name = posixpath.basename(words[i])
        if name == "cd":
            args = [w for w in words[i + 1:] if w != "--" and not re.fullmatch(r"-[LPe@]+", w)]
            path = _resolve(args[0] if args else "~", cwd)
            cwd = path if path is not None else cwd
        elif name == "rm" and _rm_wipes(words, i, cwd):
            return "it deletes a system directory, the filesystem root or the whole home directory"
        elif name in _SHELLS:
            # bash -c 'script', bash -lc "...", su -c '...': the script is a command too.
            for j in range(i + 1, len(words) - 1):
                if re.fullmatch(r"-[a-zA-Z]*c[a-zA-Z]*", words[j]):
                    inner = _rm_wipe_reason(words[j + 1], depth + 1)
                    if inner:
                        return inner
                    break
    return None


def _program(words):
    """Where the program of a simple command stands, past assignments and wrappers (sudo, env, timeout 5, xargs -I {}),
    and the scripts a wrapper runs from a string of its own (env -S '...')."""

    i, scripts = 0, []
    while i < len(words):
        if _ASSIGNMENT.match(words[i]):
            i += 1
            continue
        wrapper = posixpath.basename(words[i])
        if wrapper not in _WRAPPERS:
            return i, scripts
        value_letters, plain = _WRAPPERS[wrapper]
        i += 1
        while i < len(words) and words[i].startswith("-") and words[i] != "-":
            word = words[i]
            i += 1
            if word == "--":
                break
            if wrapper == "env" and (word.startswith("-S") or word.startswith("--split-string")):
                script = word.partition("=")[2] if word.startswith("--") else word[2:]
                if not script and i < len(words):
                    script, i = words[i], i + 1
                scripts.append(script)
            elif not word.startswith("--") and len(word) == 2 and word[1] in value_letters:
                i += 1  # the option's value is the next word
        while i < len(words) and _ASSIGNMENT.match(words[i]):
            i += 1
        i += min(plain, max(0, len(words) - i - 1))
    return i, scripts


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
    wipe = _rm_wipe_reason(text)
    if wipe:
        return wipe

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
