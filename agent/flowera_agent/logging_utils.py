import time


# ============================================================
# Utility
# ============================================================

def timestamp():
    return time.strftime("%Y-%m-%d %H:%M:%S")


_log_path = None


def log_to_file(path):
    """
    Send log lines to a file instead of stdout.

    For the processes whose stdout is somebody else's input: the `gui`
    command, whose output an engine reads as the result of its action, and
    Claude Code's MCP server, whose stdout is the JSON-RPC channel itself.
    """

    global _log_path
    _log_path = path


def log(text):
    line = f"[{timestamp()}] {text}"

    if _log_path is None:
        print(line, flush=True)
        return

    try:
        with open(_log_path, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


def safe_filename(name):
    return "".join(
        c if c.isalnum() or c in "-_." else "_"
        for c in name
    )
