import time
import urllib.request

from flowera_agent.logging_utils import log


# ============================================================
# Backend readiness
#
# One HTTP reachability probe, shared by every harness that has something
# to wait for (see harness.wait_for_backend). It lives here rather than in
# ollama_client.py because TheBot waits on the same Ollama server the
# hand-rolled loop does, plus its own soul daemon - waiting is not the
# ollama harness's private business, even though Ollama is.
#
# Nothing here ever raises or gives up: "not up yet" is a state to sit in,
# not an error to report. What a caller does with the wait is its own
# business - app.py runs it on a background thread so it can never gate the
# agent's :8081 listener.
# ============================================================

POLL_SECONDS = 2

TIMEOUT_SECONDS = 5

# Log the first few attempts, then once a minute. An unreachable URL used
# to write one line every 2 seconds, which is what buried every other line
# of /tmp/flowera-startup.log while an agent sat waiting.
LOUD_ATTEMPTS = 3

QUIET_EVERY = 30


def wait_for_http_ok(url, label):
    """
    Block until url answers HTTP 200, then return.
    """

    attempt = 0

    while True:

        try:
            request = urllib.request.Request(
                url,
                method="GET",
            )

            with urllib.request.urlopen(
                request,
                timeout=TIMEOUT_SECONDS,
            ) as response:

                status = response.status

            if status == 200:
                log(f"{label} available.")
                return

            reason = f"HTTP {status}"

        except Exception as exc:
            reason = exc

        attempt += 1

        if attempt <= LOUD_ATTEMPTS or attempt % QUIET_EVERY == 0:
            log(
                f"Waiting for {label} "
                f"(attempt {attempt}): {reason}"
            )

        time.sleep(POLL_SECONDS)
