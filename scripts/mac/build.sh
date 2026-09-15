#!/bin/bash

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

cleanup() {
    echo ""
    echo "=== Interrupted: stopping Flowera and its containers ==="

    docker compose down || true

    containers=$(docker ps -a --filter "name=desktop_" --format "{{.ID}}")
    if [ -n "$containers" ]; then
        echo "$containers" | xargs docker rm -f
    fi

    docker rm -f flowera-ollama >/dev/null 2>&1 || true
}

trap 'cleanup; exit 130' INT TERM

./scripts/mac/build-images.sh

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

DEFAULT_OLLAMA_URL="http://host.docker.internal:11434"
BUNDLED_OLLAMA_URL="http://flowera-ollama:11434"

if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

docker network inspect flowera-network >/dev/null 2>&1 \
    || docker network create flowera-network >/dev/null

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
        sed -i '' "s|^$1=.*|$1=$2|" .env
    else
        printf '%s=%s\n' "$1" "$2" >> .env
    fi
}

read_env_var() {
    grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true
}

EMBED_MODEL="nomic-embed-text"

pull_ollama_model() {
    docker run --rm \
        --network flowera-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        flowera-desktop:basic \
        -c "
import json, sys, urllib.request
req = urllib.request.Request(
    '${1%/}/api/pull',
    data=json.dumps({'model': '$2', 'stream': False}).encode(),
    headers={'Content-Type': 'application/json'},
)
try:
    urllib.request.urlopen(req)
except Exception as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)
"
}

OLLAMA_URL_OK=""

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

if [ -z "$OLLAMA_URL_OK" ] && probe_ollama "$DEFAULT_OLLAMA_URL"; then
    echo "Host Ollama is reachable from containers."
    OLLAMA_URL_OK="$DEFAULT_OLLAMA_URL"
fi

if [ -z "$OLLAMA_URL_OK" ] \
    && curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then

    echo ""
    echo "Ollama is running on this host but listening only on 127.0.0.1,"
    echo "so no desktop container can reach it."
    echo "Setting launchctl OLLAMA_HOST=0.0.0.0..."
    launchctl setenv OLLAMA_HOST 0.0.0.0 || true

    for i in {1..5}; do
        if probe_ollama "$DEFAULT_OLLAMA_URL"; then
            echo "Host Ollama is now reachable from containers."
            OLLAMA_URL_OK="$DEFAULT_OLLAMA_URL"
            break
        fi
        sleep 2
    done

    if [ -z "$OLLAMA_URL_OK" ]; then
        echo "Please restart Ollama with 'export OLLAMA_HOST=0.0.0.0 && ollama serve'"
        echo "or restart the Ollama app after setting launchctl."
    fi
fi

if [ -z "$OLLAMA_URL_OK" ]; then
    echo ""
    echo "Falling back to the bundled flowera-ollama container."
    OLLAMA_URL_OK="$BUNDLED_OLLAMA_URL"
    set_env_var OLLAMA_MODEL "qwen2.5-coder:3b"
fi

set_env_var OLLAMA_URL "$OLLAMA_URL_OK"

if [ "$OLLAMA_URL_OK" != "$BUNDLED_OLLAMA_URL" ]; then
    WANTED_MODEL="$(read_env_var OLLAMA_MODEL)"
    WANTED_MODEL="${WANTED_MODEL:-qwen2.5-coder:3b}"
    AVAILABLE_MODELS="$(list_ollama_models "$OLLAMA_URL_OK")"

    if echo "$AVAILABLE_MODELS" | grep -qE "^$WANTED_MODEL(:latest)?$"; then
        echo "Chat model $WANTED_MODEL is present."
        set_env_var OLLAMA_MODEL "$WANTED_MODEL"
    else
        echo ""
        echo "Pulling the chat model $WANTED_MODEL (once; ~2GB)..."

        if pull_ollama_model "$OLLAMA_URL_OK" "$WANTED_MODEL"; then
            echo "Chat model $WANTED_MODEL is ready."
            set_env_var OLLAMA_MODEL "$WANTED_MODEL"
        elif [ -n "$AVAILABLE_MODELS" ]; then
            FIRST_MODEL="$(echo "$AVAILABLE_MODELS" | head -1)"
            echo "WARNING: could not pull $WANTED_MODEL; using $FIRST_MODEL."
            set_env_var OLLAMA_MODEL "$FIRST_MODEL"
        else
            echo "WARNING: could not pull $WANTED_MODEL. Run: ollama pull $WANTED_MODEL"
            set_env_var OLLAMA_MODEL "$WANTED_MODEL"
        fi
    fi
fi

if [ "$OLLAMA_URL_OK" != "$BUNDLED_OLLAMA_URL" ]; then
    if echo "$(list_ollama_models "$OLLAMA_URL_OK")" \
        | grep -qE "^$EMBED_MODEL(:latest)?$"; then

        echo "Embedding model $EMBED_MODEL is present."
    else
        echo ""
        echo "Pulling the embedding model $EMBED_MODEL (~270MB, once)..."

        if pull_ollama_model "$OLLAMA_URL_OK" "$EMBED_MODEL"; then
            echo "Embedding model $EMBED_MODEL is ready."
        else
            echo "WARNING: could not pull $EMBED_MODEL."
            echo "The thebot harness will answer, but cannot remember learned context."
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
containers=$(docker ps -a --filter "name=desktop_" --format "{{.ID}}")
if [ -n "$containers" ]; then
    echo "$containers" | xargs docker rm -f
fi

echo ""
echo "=== Removing old desktop volumes ==="
vols=$(docker volume ls --filter "name=desktop_" --format "{{.Name}}")
if [ -n "$vols" ]; then
    echo "$vols" | xargs docker volume rm
fi

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
echo " Flowera is running: http://localhost:8080"
echo "========================================"

if ! command -v cloudflared >/dev/null 2>&1; then
    echo ""
    echo "cloudflared is not installed (optional quick tunnel skipped)."
    echo "Install via: brew install cloudflared"
    echo "Press Ctrl+C to stop Flowera."
    while true; do sleep 3600; done
fi

echo ""
echo "=== Starting Cloudflare Tunnel ==="
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
