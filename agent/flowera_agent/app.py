import threading
from http.server import HTTPServer

from flowera_agent import config
from flowera_agent.http_api import Handler
from flowera_agent.logging_utils import log
from flowera_agent.ollama_client import wait_for_ollama
from flowera_agent.watchers import command_watcher


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
        f"Ollama: "
        f"{config.OLLAMA_URL}"
    )

    log(
        f"Model: "
        f"{config.OLLAMA_MODEL}"
    )

    # Wait until the shared Ollama instance is alive.
    wait_for_ollama()

    # Start command watcher.
    threading.Thread(
        target=command_watcher,
        daemon=True,
    ).start()

    server = HTTPServer(
        ("0.0.0.0", 8081),
        Handler,
    )

    log(
        "VM agent listening on "
        "0.0.0.0:8081"
    )

    server.serve_forever()
