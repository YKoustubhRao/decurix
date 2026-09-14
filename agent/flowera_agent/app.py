import threading
from http.server import ThreadingHTTPServer

from flowera_agent import config, harness
from flowera_agent.computer import demonstration, recording
from flowera_agent.http_api import Handler
from flowera_agent.logging_utils import log
from flowera_agent.watchers import command_watcher


def tidy_recordings():
    """
    Bring the desktop's recordings up to date at startup: move an audit trail
    from before recordings existed into them, and close a demonstration that
    was being recorded when the desktop last stopped.
    """

    try:
        count = recording.import_legacy_audit(config.FLOWERA_DIR / "audit")
        if count:
            log(f"Moved {count} audited actions into recordings.")
    except Exception as exc:
        log(f"Could not move the old audit trail into recordings: {exc}")

    try:
        demonstration.recover()
    except Exception as exc:
        log(f"Could not close an interrupted demonstration: {exc}")


# ============================================================
# Startup
# ============================================================

def main():

    log("========================================")
    log("Flowera VM Agent")
    log("========================================")

    log(
        f"Desktop ID: {config.DESKTOP_ID}"
    )

    log(
        f"Desktop name: {config.DESKTOP_NAME}"
    )

    log(
        f"Control plane: "
        f"{config.CONTROL_PLANE_URL}"
    )

    log(
        f"Harness: "
        f"{config.AGENT_HARNESS}"
    )

    log(
        f"Ollama: "
        f"{config.OLLAMA_URL}"
    )

    log(
        f"Model: "
        f"{config.OLLAMA_MODEL}"
    )

    # Wait for the harness's reasoning backend in the background, never
    # in front of the listener below.
    #
    # This used to be a blocking wait_for_ollama() call, which meant an
    # unreachable Ollama (a dead tunnel, a stopped container) left :8081
    # unbound forever. The control plane's SendMessage then got
    # "connection refused" and every human message - from the chat, the
    # messages page and a help reply alike - came back as HTTP 500, with
    # the desktop itself looking perfectly healthy in the dashboard.
    #
    # The message channel is not the model's to gate: a task that arrives
    # while the backend is down fails inside the harness, which reports it
    # back through ask_for_help as a message in the chat - an answer the
    # human can act on, instead of a 500.
    threading.Thread(
        target=harness.wait_for_backend,
        daemon=True,
    ).start()

    # Start command watcher.
    threading.Thread(
        target=command_watcher,
        daemon=True,
    ).start()

    threading.Thread(
        target=tidy_recordings,
        daemon=True,
    ).start()

    server = ThreadingHTTPServer(
        ("0.0.0.0", 8081),
        Handler,
    )

    log(
        "VM agent listening on "
        "0.0.0.0:8081"
    )

    server.serve_forever()
