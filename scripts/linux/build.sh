#!/bin/bash

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

./scripts/linux/build-images.sh

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

# Asked from inside a container on purpose. A desktop talks to Ollama from
# a container, so that is the only place the question is honest: the host
# can always reach its own 127.0.0.1, which is precisely the configuration
# that leaves every desktop unable to reach anything. Probing from the host
# would have passed while nothing worked.
#
# Everything below writes .env, which docker/Dockerfile copies into the
# control plane image - hence running before the "docker compose up
# --build" further down, not after.

DEFAULT_OLLAMA_URL="http://host.docker.internal:11434"
BUNDLED_OLLAMA_URL="http://flowera-ollama:11434"

touch .env

docker network inspect flowera-network >/dev/null 2>&1 \
    || docker network create flowera-network >/dev/null

# flowera-desktop:basic was just built above and carries python3, so this
# needs no image pull and no extra dependency on the host.
probe_ollama() {
    docker run --rm \
        --network flowera-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        flowera-desktop:basic \
        -c "
import sys, urllib.request
try:
    urllib.request.urlopen('${1%/}/api/tags', timeout=5)
except Exception:
    sys.exit(1)
" >/dev/null 2>&1
}

list_ollama_models() {
    docker run --rm \
        --network flowera-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        flowera-desktop:basic \
        -c "
import json, urllib.request
try:
    with urllib.request.urlopen('${1%/}/api/tags', timeout=5) as r:
        for m in json.load(r).get('models', []):
            print(m.get('name', ''))
except Exception:
    pass
" 2>/dev/null
}

set_env_var() {
    if grep -qE "^$1=" .env; then
        sed -i "s|^$1=.*|$1=$2|" .env
    else
        printf '%s=%s\n' "$1" "$2" >> .env
    fi
}

read_env_var() {
    grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true
}

OLLAMA_URL_OK=""

# Whatever is already in .env wins, as long as it actually works.
CONFIGURED_OLLAMA_URL="$(read_env_var OLLAMA_URL)"

if [ -n "$CONFIGURED_OLLAMA_URL" ]; then
    if probe_ollama "$CONFIGURED_OLLAMA_URL"; then
        echo "Using OLLAMA_URL from .env: $CONFIGURED_OLLAMA_URL"
        OLLAMA_URL_OK="$CONFIGURED_OLLAMA_URL"
    else
        echo "OLLAMA_URL in .env is not reachable from a container:"
        echo "  $CONFIGURED_OLLAMA_URL"
        echo "Looking for one that is."
    fi
fi

# The host running the control plane, over the same host-gateway mapping
# every desktop already gets for CONTROL_PLANE_URL.
if [ -z "$OLLAMA_URL_OK" ] && probe_ollama "$DEFAULT_OLLAMA_URL"; then
    echo "Host Ollama is reachable from containers."
    OLLAMA_URL_OK="$DEFAULT_OLLAMA_URL"
fi

# The common broken case: Ollama is running right here, but bound to
# loopback, so it is reachable from this shell and from nowhere else.
if [ -z "$OLLAMA_URL_OK" ] \
    && curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then

    echo ""
    echo "Ollama is running on this host but listening only on 127.0.0.1,"
    echo "so no desktop container can reach it."

    if command -v systemctl >/dev/null 2>&1 \
        && systemctl cat ollama >/dev/null 2>&1; then

        echo "Giving its service OLLAMA_HOST=0.0.0.0 (needs sudo, once)."
        echo ""

        if sudo mkdir -p /etc/systemd/system/ollama.service.d \
            && printf '[Service]\nEnvironment="OLLAMA_HOST=0.0.0.0"\n' \
                | sudo tee /etc/systemd/system/ollama.service.d/flowera.conf >/dev/null \
            && sudo systemctl daemon-reload \
            && sudo systemctl restart ollama; then

            for i in {1..15}; do
                if probe_ollama "$DEFAULT_OLLAMA_URL"; then
                    echo "Host Ollama is now reachable from containers."
                    OLLAMA_URL_OK="$DEFAULT_OLLAMA_URL"
                    break
                fi

                sleep 2
            done
        fi

        if [ -z "$OLLAMA_URL_OK" ]; then
            echo "Could not reconfigure the Ollama service."
        fi
    else
        echo "No systemd ollama service to reconfigure."
    fi
fi

# Last resort: the container Flowera manages itself. Small model, but it
# needs nothing from the host and always works, which beats handing every
# desktop a URL that answers nowhere.
if [ -z "$OLLAMA_URL_OK" ]; then
    echo ""
    echo "Falling back to the bundled flowera-ollama container."
    OLLAMA_URL_OK="$BUNDLED_OLLAMA_URL"
    set_env_var OLLAMA_MODEL "qwen3:0.6b"
fi

set_env_var OLLAMA_URL "$OLLAMA_URL_OK"

# A reachable server running a model nobody asked for fails exactly as
# opaquely as an unreachable one, so pin OLLAMA_MODEL to something that
# server actually has.
if [ "$OLLAMA_URL_OK" != "$BUNDLED_OLLAMA_URL" ]; then
    WANTED_MODEL="$(read_env_var OLLAMA_MODEL)"
    AVAILABLE_MODELS="$(list_ollama_models "$OLLAMA_URL_OK")"

    if [ -n "$AVAILABLE_MODELS" ]; then
        if [ -z "$WANTED_MODEL" ]; then
            WANTED_MODEL="qwen3:latest"
        fi

        if ! echo "$AVAILABLE_MODELS" | grep -qx "$WANTED_MODEL"; then
            FIRST_MODEL="$(echo "$AVAILABLE_MODELS" | head -1)"
            echo "Model $WANTED_MODEL is not on that server; using $FIRST_MODEL."
            set_env_var OLLAMA_MODEL "$FIRST_MODEL"
        else
            set_env_var OLLAMA_MODEL "$WANTED_MODEL"
        fi
    fi
fi

echo ""
echo "Desktops will use OLLAMA_URL=$(read_env_var OLLAMA_URL) OLLAMA_MODEL=$(read_env_var OLLAMA_MODEL)"

echo ""
echo "=== Stopping Flowera ==="

docker compose down

echo ""
echo "=== Removing old desktop containers ==="

docker ps -a \
    --filter "name=desktop_" \
    --format "{{.ID}}" \
    | xargs -r docker rm -f

echo ""
echo "=== Removing old desktop volumes ==="

docker volume ls \
    --filter "name=desktop_" \
    --format "{{.Name}}" \
    | xargs -r docker volume rm

echo ""
echo "=== Starting Flowera ==="

docker compose up --build -d

echo ""
echo "=== Waiting for dashboard ==="

for i in {1..60}; do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "Flowera dashboard is running."
        break
    fi

    echo "Waiting for Flowera... ($i/60)"
    sleep 2
done

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    echo "ERROR: Flowera dashboard is not responding on port 8080."
    docker compose logs --tail=100
    exit 1
fi

echo ""
echo "========================================"
echo " Flowera is running"
echo "========================================"

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "ERROR: cloudflared is not installed."
    exit 1
fi

echo ""

# Start Cloudflare tunnel, echoing its logs while watching for the
# trycloudflare.com URL so it can be reported to the control plane for
# display in the dashboard's Mobile dropdown.
cloudflared tunnel --protocol http2 --url http://127.0.0.1:8080 2>&1 | while IFS= read -r line; do
    echo "$line"

    url=$(echo "$line" | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' || true)

    if [ -n "$url" ]; then
        curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
            -H "Content-Type: application/json" \
            -d "{\"url\":\"$url\"}" \
            >/dev/null 2>&1 || true
    fi
done