"""
A stand-in for script_sandbox in tests: the network and user namespaces it needs do not exist on a Windows development
host, so a test that runs a learned script runs it with the sandbox's own environment and folders but no namespace.
What the namespace itself holds back is tested by test_script_sandbox (which runs only where it can be built).
"""

import subprocess
import tempfile
from pathlib import Path
from unittest.mock import patch

from decurix_agent import script_sandbox


def start(test, root):
    """Patch script_sandbox for one test: available, unisolated, scratch folders under `root`."""

    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)

    def run(argv, scratch, timeout):
        return subprocess.run(list(argv), cwd=str(Path(scratch) / "work"), capture_output=True, text=True,
                              timeout=timeout, env=script_sandbox.clean_env(scratch))

    for p in (patch.object(script_sandbox, "available", lambda: True),
              patch.object(script_sandbox, "run", run),
              patch.object(script_sandbox, "new_scratch", lambda: Path(tempfile.mkdtemp(prefix=".run-", dir=str(root))))):
        p.start()
        test.addCleanup(p.stop)
