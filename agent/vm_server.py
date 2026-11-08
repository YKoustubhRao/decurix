"""The agent's daemon. Started as `python3 -I /opt/vm_server.py` (start_agent_terminal.sh): isolated mode reads no
PYTHON* variable, no user site-packages and no .pth or usercustomize.py from the worker's home, and from Python 3.11
puts no script folder on sys.path - so the agent's own folder is put there here, and nothing else (review round 11)."""

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)


def _take_token(config):
    """Non-dumpable first, then this process's token out of its file (decurix-entrypoint.sh), so nothing of the
    worker's can read either from this process. The agent package beside this file may be older than it (an eval arm
    pinned to an earlier revision copies its own /opt/decurix_agent onto this image): one with no take_agent_token reads
    its token only from its environment, so the file's token is put there for it."""

    no_dump, take = getattr(config, "no_dump", None), getattr(config, "take_agent_token", None)
    token_file = os.environ.get("DECURIX_AGENT_TOKEN_FILE", "")
    if no_dump is None or take is None:
        _no_dump_here()
        if token_file and not os.environ.get("DECURIX_AGENT_TOKEN"):
            try:
                with open(token_file, encoding="ascii", errors="replace") as f:
                    os.environ["DECURIX_AGENT_TOKEN"] = f.read(4096).strip()
            except OSError:
                pass
            try:
                os.unlink(token_file)
            except OSError:
                pass
        return
    if not no_dump() and token_file:
        print("[vm_server] could not make this process non-dumpable; its token is readable by the worker", flush=True)
    take()


def _no_dump_here():
    """config.no_dump for an agent package that has none (prctl PR_SET_DUMPABLE 0 on Linux)."""

    if sys.platform.startswith("linux"):
        try:
            import ctypes

            ctypes.CDLL(None).prctl(4, 0, 0, 0, 0)
        except (OSError, AttributeError):
            pass


if __name__ == "__main__":
    from decurix_agent import config  # noqa: E402

    _take_token(config)

    from decurix_agent.app import main  # noqa: E402

    main()
