"""Who may call ext_server.py, and where an approval for a gated action may come from.

ext_server.py drives the desktop's real, logged-in Chromium, so a call to it is as good as a click in the person's
own browser. It listens on 127.0.0.1 only, so other desktops on the Docker network cannot reach it at all; what is
left inside the desktop is:

- a web page open in that same Chromium. It can send to 127.0.0.1 (a simple POST needs no CORS), and through DNS
  rebinding it can even read the answers. refusal() turns it away: any Origin that is not the extension's own
  (chrome-extension://...), any request a browser marked with Sec-Fetch-* without one, and any Host that is not
  loopback. The server sends no CORS headers, so a page's preflight is never granted either; the extension does not
  need them (its host_permissions exempt it from CORS).
- TheBot's browser skill (soul, a plain HTTP client): it sends this desktop's agent token (DECURIX_AGENT_TOKEN, given
  by the control plane at creation) as "Authorization: Bearer <token>", which refusal() requires when the desktop
  has one. A desktop created before tokens existed has none, and such a caller is served as before.

The honest limit: the worker's own shell runs as the same Linux user, can read the token from its environment and can
send any header, so none of this stops the worker itself. Approvals() makes sure that even then no approval comes from
a request body that the approval gate never asked for.
"""
import hmac
import os
import threading

TOKEN_ENV = "DECURIX_AGENT_TOKEN"

# The names this server is reached by: it binds 127.0.0.1 only. A rebinding page reaches it by a DNS name of its own.
LOOPBACK_NAMES = frozenset({"127.0.0.1", "localhost", "::1"})

# The only browser origin allowed: the extension itself. The desktop's Chromium loads exactly one extension
# (--disable-extensions-except), so any chrome-extension:// origin is that one.
EXTENSION_SCHEME = "chrome-extension://"

# Headers Chromium puts on requests a page makes, whatever its mode: one carrying them is a browser's.
BROWSER_MARKS = ("sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest")


def token():
    """This desktop's agent token, "" when it was created without one."""

    return os.environ.get(TOKEN_ENV, "").strip()


def _get(headers, name):
    """A header by name, whatever case `headers` keeps them in (Starlette's are case-insensitive; a dict may not be)."""

    value = headers.get(name)
    if value is None:
        value = headers.get(name.title())
    return value


def hostname(host):
    """The name part of a Host header ("127.0.0.1:8092" -> "127.0.0.1", "[::1]:8092" -> "::1")."""

    host = (host or "").strip().lower()
    if host.startswith("["):
        return host[1:].split("]", 1)[0]
    return host.rsplit(":", 1)[0] if host.count(":") == 1 else host


def from_extension(headers):
    """Whether the request comes from the extension: its fetches name the chrome-extension:// origin."""

    return (_get(headers, "origin") or "").startswith(EXTENSION_SCHEME)


def has_token(headers, tok=None):
    """Whether the request carries this desktop's agent token (constant-time compare); False when there is none."""

    tok = token() if tok is None else tok
    if not tok:
        return False
    scheme, _, value = (_get(headers, "authorization") or "").partition(" ")
    return scheme.lower() == "bearer" and hmac.compare_digest(value.strip().encode("utf-8"), tok.encode("utf-8"))


def refusal(method, headers, tok=None):
    """Why a request may not be served - (status, reason) - or None when it may."""

    tok = token() if tok is None else tok

    if hostname(_get(headers, "host")) not in LOOPBACK_NAMES:
        return 403, "this server is reached on 127.0.0.1 only"

    origin = _get(headers, "origin")
    if origin is not None and not origin.startswith(EXTENSION_SCHEME):
        return 403, "web pages may not call this server"
    if origin is None and any(_get(headers, h) is not None for h in BROWSER_MARKS):
        # A browser request with no origin at all: an <img>, a link, a no-cors GET. Not the extension, not soul.
        return 403, "web pages may not call this server"

    if origin is None and tok and not has_token(headers, tok):
        return 401, "this desktop's agent token is required"

    if method not in ("GET", "HEAD"):
        ctype = (_get(headers, "content-type") or "").split(";", 1)[0].strip().lower()
        try:
            length = int(_get(headers, "content-length") or "0")
        except ValueError:
            length = -1
        if length < 0:
            return 400, "invalid content length"
        if (ctype and ctype != "application/json") or (length and ctype != "application/json"):
            return 415, "the body must be JSON (Content-Type: application/json)"

    return None


class Approvals:
    """
    The actions the extension's approval gate stopped before, each waiting for one yes.

    A request may name actions as approved only when it comes from the agent's own gate (a caller with the agent
    token: TheBot's browser skill, after the person said yes through the control plane), and only actions the gate
    actually stopped on; each is let through once. Anything else - a web page, a desktop without a token, an action
    nobody was asked about - is dropped, and the gate stops the action again.
    """

    LIMIT = 50

    def __init__(self):
        self._lock = threading.Lock()
        self._held = []

    def stopped(self, action):
        """The gate stopped before `action` (a result with status needs_approval)."""

        action = str(action or "").strip()
        if not action:
            return
        with self._lock:
            self._held.append(action)
            del self._held[:-self.LIMIT]

    def grant(self, actions, trusted):
        """The actions among `actions` that may go through once, each taken off the held list; [] unless `trusted`."""

        if not trusted or not isinstance(actions, list):
            return []
        granted = []
        with self._lock:
            for action in actions:
                action = str(action or "").strip()
                if action in self._held:
                    self._held.remove(action)
                    granted.append(action)
        return granted
