import sys
import threading
from http.server import ThreadingHTTPServer

from decurix_agent import config, harness, shared
from decurix_agent.computer import demonstration, recording
from decurix_agent.http_api import Handler
from decurix_agent.logging_utils import log, log_banner
from decurix_agent.watchers import command_watcher, watchdog


def tidy_recordings():
    """
    Bring the desktop's recordings up to date at startup: move an audit trail
    from before recordings existed into them, and close a demonstration that
    was being recorded when the desktop last stopped.
    """

    try:
        count = recording.import_legacy_audit(config.DECURIX_DIR / "audit")
        if count:
            log(f"Moved {count} audited actions into recordings.")
    except Exception as exc:
        log(f"Could not move the old audit trail into recordings: {exc}")

    try:
        from decurix_agent.computer import screen_call
        if screen_call.remote() and not screen_call.wait_for_broker():
            # Broker mode: the recordings a demonstration leaves are the screen side's to close, through the broker.
            log("Could not close an interrupted demonstration: the screen broker did not start within 2 minutes")
            return
        demonstration.recover()
    except Exception as exc:
        log(f"Could not close an interrupted demonstration: {exc}")


# ============================================================
# Startup
# ============================================================

def main():

    log_banner("Decurix VM Agent")

    # Where every thread is, written to the agent's log on SIGUSR1: a run that went silent mid-replay can be read
    # without stopping it (code positions only - no values). Found in the learning curves (2026-11-17): a replay hung
    # 30 minutes after a key press with nothing logged, and nothing showed where.
    try:
        import faulthandler
        import signal
        faulthandler.register(signal.SIGUSR1, file=sys.stdout, all_threads=True)
    except (AttributeError, ValueError, OSError, ImportError):
        pass

    # What was kept from a reply and not yet copied to the control plane, tried once more (messaging.post_learned).
    try:
        from decurix_agent import messaging
        threading.Thread(target=messaging.resend_learned, daemon=True, name="resend-learned").start()
    except Exception as exc:  # noqa: BLE001
        log(f"Could not resend what was kept from replies: {exc}")

    # The code this agent runs, named once at start for every run's manifest (retention.agent_sha).
    try:
        from decurix_agent import retention
        log(f"Agent code: {retention.agent_sha()}")
    except Exception as exc:  # noqa: BLE001 - the manifest says it is unknown; the agent still starts
        log(f"Could not hash the agent's code: {exc}")

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

    # The one folder of the person's computer this worker may use, when it has one: ~/PC leads to its mount.
    if shared.folder() or shared.link_path().is_symlink():
        outcome = shared.ensure_link()
        log(f"Shared folder {shared.folder() or '(none)'}: {outcome or 'nothing to do'}")
        # And the work folder's PC: a planned run's outputs in the person's folder are checked through it.
        log(f"Work-folder link to the shared folder: {shared.ensure_work_link() or 'nothing to do'}")

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
    # unbound forever. The control plane's SendRequest then got
    # "connection refused" and every human request - from the chat, the
    # requests page and a query reply alike - came back as HTTP 500, with
    # the desktop itself looking perfectly healthy in the dashboard.
    #
    # The request channel is not the model's to gate: a task that arrives
    # while the backend is down fails inside the harness, which reports it
    # back through ask_for_query as a reply in the chat - an answer the
    # human can act on, instead of a 500.
    threading.Thread(
        target=harness.wait_for_backend,
        daemon=True,
    ).start()

    # Start command watcher.
    threading.Thread(
        target=command_watcher,
        daemon=True,
        name="command-watcher",
    ).start()

    threading.Thread(
        target=watchdog,
        daemon=True,
        name="command-watchdog",
    ).start()

    threading.Thread(
        target=tidy_recordings,
        daemon=True,
    ).start()

    server = ThreadingHTTPServer(
        (config.AGENT_BIND, config.AGENT_PORT),
        Handler,
    )

    log(
        "VM agent listening on "
        f"{config.AGENT_BIND}:{config.AGENT_PORT}"
    )

    server.serve_forever()
