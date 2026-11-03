"""
A bench desktop's agent token.

A desktop's agent takes requests on :8081 only with its token (agent http_api: a desktop made without one refuses
every call but /health, and a call without the token is refused too). The control plane gives each desktop it makes
a token; a benchmark that starts its own container with `docker run` gives it one here, and sends it on every call.

    from agent_token import docker_env, PY_AGENT

    subprocess.run(["docker", "run", ..., *docker_env(), image])
    # inside the desktop (docker exec keeps the container's environment, so the token is there):
    subprocess.run(["docker", "exec", name, "python3", "-c", PY_AGENT + "print(agent('/skills'))"])

The token is random per bench process and never written to disk.
"""

import secrets

ENV = "DECURIX_AGENT_TOKEN"

TOKEN = secrets.token_hex(24)


def docker_env(token=None):
    """`docker run -e` arguments that give a bench desktop its agent token."""

    return ["-e", f"{ENV}={token or TOKEN}"]


def headers(token=None, body=False):
    """Headers for a call to a bench desktop's agent: the token, and the JSON type a write must say it is."""

    h = {"Authorization": "Bearer " + (token or TOKEN)}
    if body:
        h["Content-Type"] = "application/json"
    return h


# Python run inside the desktop: defines agent(path, body=None) -> str, which calls the desktop's own agent on
# localhost:8081 with the token from the container's environment, and raises on an HTTP error. Passed to
# `python3 -c` ahead of the statement that uses it.
PY_AGENT = (
    "import os,json,urllib.request as _u\n"
    "def agent(path, body=None, timeout=20):\n"
    "    t = os.environ.get('" + ENV + "', '')\n"
    "    h = {'Authorization': 'Bearer ' + t}\n"
    "    d = None\n"
    "    if body is not None:\n"
    "        d = (body if isinstance(body, str) else json.dumps(body)).encode()\n"
    "        h['Content-Type'] = 'application/json'\n"
    "    r = _u.Request('http://localhost:8081' + path, data=d, headers=h, method='POST' if d is not None else 'GET')\n"
    "    return _u.urlopen(r, timeout=timeout).read().decode()\n"
)
