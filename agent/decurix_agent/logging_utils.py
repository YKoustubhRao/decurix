import os
import threading
import time


# ============================================================
# Utility
# ============================================================

def timestamp():
    return time.strftime("%Y-%m-%d %H:%M:%S")


_log_path = None


def log_file(name):
    """
    Where a side process keeps its log: /tmp in a desktop container, the
    worker's own logs folder (DECURIX_LOG_DIR) for a native Windows worker,
    which has no /tmp and shares its computer with other workers.
    """

    folder = os.environ.get("DECURIX_LOG_DIR", "").strip()
    return os.path.join(folder, name) if folder else "/tmp/" + name


def log_to_file(path):
    """
    Send log lines to a file instead of stdout.

    For the processes whose stdout is somebody else's input: the `gui`
    command, whose output an engine reads as the result of its action, and
    Claude Code's MCP server, whose stdout is the JSON-RPC channel itself.
    """

    global _log_path
    _log_path = path


def log_banner(*lines):
    """Log `lines` between two rules: how a task's start and a person's reply stand out in the log."""

    log("=" * 40)
    for line in lines:
        log(line)
    log("=" * 40)


_withholding = threading.local()


def _withheld(text):
    """A log line with every secret known on this desktop withheld (privacy.withhold_known): the log is a file the
    worker's tools and the support zip read, so it is one more writer that withholds (review round 2, B1). Never
    raises; a line that cannot be checked is held back."""

    if getattr(_withholding, "on", False):  # (privacy itself may log: that line is not withheld again)
        return str(text)
    _withholding.on = True
    try:
        from decurix_agent.computer import privacy
        return privacy.withhold_known(str(text))
    except Exception:  # noqa: BLE001 - fail closed
        return "[held back: this log line could not be checked for secrets]"
    finally:
        _withholding.on = False


def log(text):
    line = f"[{timestamp()}] {_withheld(text)}"

    if _log_path is None:
        print(line, flush=True)
        return

    try:
        with open(_log_path, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass
