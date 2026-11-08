"""The agent's daemon. Started as `python3 -I /opt/vm_server.py` (start_agent_terminal.sh): isolated mode reads no
PYTHON* variable, no user site-packages and no .pth or usercustomize.py from the worker's home, and from Python 3.11
puts no script folder on sys.path - so the agent's own folder is put there here, and nothing else (review round 11)."""

import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

if __name__ == "__main__":
    # Before anything else: non-dumpable, then this process's token out of its file (config.take_agent_token;
    # decurix-entrypoint.sh). Nothing of the worker's can read either from this process after this.
    from decurix_agent import config  # noqa: E402

    if not config.no_dump() and os.environ.get(config.AGENT_TOKEN_FILE_ENV):
        print("[vm_server] could not make this process non-dumpable; its token is readable by the worker", flush=True)
    config.take_agent_token()

    from decurix_agent.app import main  # noqa: E402

    main()
