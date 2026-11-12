"""
Test-process setup. Import this module before anything that imports the config.

1. The task file. task_context.CURRENT_FILE defaults to the machine-wide /tmp/decurix-current-task, which every
   process on a desktop shares on purpose (the agent, the `gui` command, the MCP server). Two test runs on one machine
   then share it too: one run's task, request or stage file changed another's outcome, and a stale file from an
   earlier run did the same (test_apprentice, test_lessons and test_draft_proof failed that way, review 2026-11-03).
   Each test process gets a folder of its own: DECURIX_TASK_FILE and TMPDIR point into it (a child process the tests
   start inherits both), and a task_context imported before this module is pointed there as well, so the import order
   of test modules does not matter. Unittest discovery imports every test module before the first test runs, so one
   import anywhere in a discovered suite isolates the whole run.

2. decurix_agent.config creates the desktop's directories under /home/headless when imported. Where that raises it is
   replaced by a stub.

Not a test module (no test_ prefix): unittest discovery never imports it on its own.
"""

import atexit
import os
import shutil
import sys
import tempfile
import types

_OWN = tempfile.mkdtemp(prefix="decurix-test-")
atexit.register(shutil.rmtree, _OWN, True)
TASK_FILE = os.path.join(_OWN, "current-task")
os.environ["DECURIX_TASK_FILE"] = TASK_FILE
os.environ["TMPDIR"] = _OWN
# 3. The agent's own folder (config.DECURIX_DIR): a test that does not point it elsewhere wrote into the machine's real
#    ~/.decurix, where a run's marks outlive the test run - a LABELLED run (retention.py) kept a later run's prune test
#    from pruning it. Each test process gets one of its own, unless the run names one.
os.environ.setdefault("DECURIX_DIR", os.path.join(_OWN, "decurix-dir"))
# 4. Nowhere to send to. The stub below names http://127.0.0.1:9 (refused at once), but only where the real config
#    cannot be imported; where it can (a Windows host) tests got its default, http://host.docker.internal:8080, and on
#    a machine whose hosts file maps that name to an address that drops packets (Docker Desktop's stale LAN entry) every
#    accidental send waited ~21 s and was retried for messaging.SEND_RETRY_SECONDS (180): the full suite hung for hours
#    (found 2026-11-12). Unless the run names a control plane, a test process sends nowhere, and is told so at once.
os.environ.setdefault("CONTROL_PLANE_URL", "http://127.0.0.1:9")
if "decurix_agent.task_context" in sys.modules:
    sys.modules["decurix_agent.task_context"].CURRENT_FILE = TASK_FILE
if "decurix_agent.computer.core" in sys.modules:
    from pathlib import Path

    _core = sys.modules["decurix_agent.computer.core"]
    _core.FORM_CHECK_FILE = Path(_OWN) / _core.FORM_CHECK_FILE.name

try:
    from decurix_agent import config  # noqa: F401
except OSError:
    import decurix_agent

    stub = types.ModuleType("decurix_agent.config")
    stub.DESKTOP_ID = "test-box"
    stub.DESKTOP_NAME = "test-box"
    stub.CONTROL_PLANE_URL = "http://127.0.0.1:9"
    stub.AGENT_HARNESS = "thebot"
    stub.CREATED_HARNESS = "thebot"
    # The agent token (config.agent_token): read from the environment each time, as the real one is.
    stub.AGENT_TOKEN_ENV = "DECURIX_AGENT_TOKEN"
    stub.agent_token = lambda: os.environ.get(stub.AGENT_TOKEN_ENV, "").strip()
    stub.agent_auth_headers = lambda: {"Authorization": "Bearer " + stub.agent_token()} if stub.agent_token() else {}
    sys.modules["decurix_agent.config"] = stub
    decurix_agent.config = stub

# (4, continued) A note that cannot be sent is kept at once instead of being tried again for three minutes; a test of
# the retries sets SEND_RETRY_SECONDS itself (test_kept_notes, test_unsent_outcomes).
from decurix_agent import messaging as _messaging  # noqa: E402  (after the config above)

_messaging.SEND_RETRY_SECONDS = 0
