"""
decurix_agent.config creates the desktop's directories under /home/headless when imported, which a development machine
does not have: there it is replaced by a stub. Import this module before anything that imports the config.

Not a test module (no test_ prefix): unittest discovery never imports it on its own.
"""

import os
import sys
import types

try:
    from decurix_agent import config  # noqa: F401
except OSError:
    import decurix_agent

    stub = types.ModuleType("decurix_agent.config")
    stub.DESKTOP_ID = "test-box"
    stub.DESKTOP_NAME = "test-box"
    stub.CONTROL_PLANE_URL = "http://127.0.0.1:9"
    stub.AGENT_HARNESS = "ollama"
    # The agent token (config.agent_token): read from the environment each time, as the real one is.
    stub.AGENT_TOKEN_ENV = "DECURIX_AGENT_TOKEN"
    stub.agent_token = lambda: os.environ.get(stub.AGENT_TOKEN_ENV, "").strip()
    stub.agent_auth_headers = lambda: {"Authorization": "Bearer " + stub.agent_token()} if stub.agent_token() else {}
    sys.modules["decurix_agent.config"] = stub
    decurix_agent.config = stub
