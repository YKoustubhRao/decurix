#!/bin/bash
#
# Shared by scripts/mac/build.sh and scripts/mac/rebuild.sh. Meant to be
# sourced, not executed: it defines ensure_ollama_ready, which finds an
# Ollama the desktop containers can reach, pulls the chat and embedding
# models onto it, and records OLLAMA_URL/OLLAMA_MODEL in .env.

probe_ollama() {
    docker run --rm \
        --network decurix-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        decurix-desktop:basic \
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
        --network decurix-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        decurix-desktop:basic \
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

pull_ollama_model() {
    docker run --rm \
        --network decurix-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        decurix-desktop:basic \
        -c "
import json, sys, urllib.request
req = urllib.request.Request(
    '${1%/}/api/pull',
    data=json.dumps({'model': '$2', 'stream': True}).encode(),
    headers={'Content-Type': 'application/json'},
)
try:
    with urllib.request.urlopen(req) as resp:
        for line in resp:
            try:
                data = json.loads(line.decode().strip())
                status = data.get('status', '')
                total = data.get('total', 0)
                completed = data.get('completed', 0)
                if total > 0:
                    pct = int(completed * 100 / total)
                    print(f'\r  {status}: {pct}% ({completed // 1048576}MB / {total // 1048576}MB)', end='', flush=True)
                elif status:
                    print(f'\r  {status}...', end='', flush=True)
            except Exception:
                pass
        print('')
except Exception as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)
"
}

set_env_var() {
    if grep -qE "^$1=" .env; then
        sed -i '' "s|^$1=.*|$1=$2|" .env 2>/dev/null || sed -i "s|^$1=.*|$1=$2|" .env
    else
        printf '%s=%s\n' "$1" "$2" >> .env
    fi
}

read_env_var() {
    grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- || true
}

ensure_ollama_ready() {
    local default_ollama_url="http://host.docker.internal:11434"
    local bundled_ollama_url="http://decurix-ollama:11434"
    local embed_model="nomic-embed-text"
    local ollama_url_ok=""

    docker network inspect decurix-network >/dev/null 2>&1 \
        || docker network create decurix-network >/dev/null

    local configured_ollama_url
    configured_ollama_url="$(read_env_var OLLAMA_URL)"

    if [ -n "$configured_ollama_url" ]; then
        if probe_ollama "$configured_ollama_url"; then
            echo "Using OLLAMA_URL from .env: $configured_ollama_url"
            ollama_url_ok="$configured_ollama_url"
        else
            echo "OLLAMA_URL in .env is not reachable from a container:"
            echo "  $configured_ollama_url"
            echo "Looking for one that is."
        fi
    fi

    if [ -z "$ollama_url_ok" ] && probe_ollama "$default_ollama_url"; then
        echo "Host Ollama is reachable from containers."
        ollama_url_ok="$default_ollama_url"
    fi

    if [ -z "$ollama_url_ok" ] \
        && curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then

        echo ""
        echo "Ollama is running on this host but listening only on 127.0.0.1,"
        echo "so no desktop container can reach it."
        echo "Setting launchctl OLLAMA_HOST=0.0.0.0..."
        launchctl setenv OLLAMA_HOST 0.0.0.0 || true

        local i
        for i in {1..5}; do
            if probe_ollama "$default_ollama_url"; then
                echo "Host Ollama is now reachable from containers."
                ollama_url_ok="$default_ollama_url"
                break
            fi
            sleep 2
        done

        if [ -z "$ollama_url_ok" ]; then
            echo "Please restart Ollama with 'export OLLAMA_HOST=0.0.0.0 && ollama serve'"
            echo "or restart the Ollama desktop app."
        fi
    fi

    if [ -z "$ollama_url_ok" ]; then
        echo ""
        echo "Falling back to the bundled decurix-ollama container."
        ollama_url_ok="$bundled_ollama_url"
        # A pinned model is replaced by a small one that always works; "auto"
        # stays auto - the control plane pulls what fits into the container.
        case "$(read_env_var OLLAMA_MODEL)" in
            ""|auto) ;;
            *) set_env_var OLLAMA_MODEL "qwen2.5-coder:3b" ;;
        esac
    fi

    set_env_var OLLAMA_URL "$ollama_url_ok"

    if [ "$ollama_url_ok" != "$bundled_ollama_url" ]; then
        local wanted_model available_models
        wanted_model="$(read_env_var OLLAMA_MODEL)"
        available_models="$(list_ollama_models "$ollama_url_ok")"

        # "auto" (or unset): the control plane chooses the chat model for this
        # machine from what the server has (internal/ollama, SelectModel), so
        # .env is left alone rather than pinned. A server with no chat model
        # at all gets a small one to start from.
        if [ -z "$wanted_model" ] || [ "$wanted_model" = "auto" ]; then
            local chat_models
            chat_models="$(echo "$available_models" | grep -v "^$embed_model" || true)"
            if [ -n "$chat_models" ]; then
                echo "Chat model: chosen automatically for this machine from what the server has."
            else
                echo ""
                echo "That server has no chat model; pulling qwen2.5-coder:3b (~2GB) as a starting point."
                pull_ollama_model "$ollama_url_ok" "qwen2.5-coder:3b" \
                    || echo "WARNING: could not pull it. Run: ollama pull qwen2.5-coder:3b"
            fi
        elif echo "$available_models" | grep -qE "^$wanted_model(:latest)?$"; then
            echo "Chat model $wanted_model is present."
            set_env_var OLLAMA_MODEL "$wanted_model"
        else
            echo ""
            echo "Pulling the chat model $wanted_model (once; ~2GB)..."

            if pull_ollama_model "$ollama_url_ok" "$wanted_model"; then
                echo "Chat model $wanted_model is ready."
                set_env_var OLLAMA_MODEL "$wanted_model"
            elif [ -n "$available_models" ]; then
                local first_model
                first_model="$(echo "$available_models" | head -1)"
                echo "WARNING: could not pull $wanted_model; using $first_model."
                set_env_var OLLAMA_MODEL "$first_model"
            else
                echo "WARNING: could not pull $wanted_model. Run: ollama pull $wanted_model"
                set_env_var OLLAMA_MODEL "$wanted_model"
            fi
        fi
    fi

    if [ "$ollama_url_ok" != "$bundled_ollama_url" ]; then
        if echo "$(list_ollama_models "$ollama_url_ok")" \
            | grep -qE "^$embed_model(:latest)?$"; then

            echo "Embedding model $embed_model is present."
        else
            echo ""
            echo "Pulling the embedding model $embed_model (~270MB, once)..."

            if pull_ollama_model "$ollama_url_ok" "$embed_model"; then
                echo "Embedding model $embed_model is ready."
            else
                echo "WARNING: could not pull $embed_model."
                echo "The thebot harness will answer, but cannot remember learned context."
            fi
        fi
    fi

    echo ""
    echo "Desktops will use OLLAMA_URL=$(read_env_var OLLAMA_URL) OLLAMA_MODEL=$(read_env_var OLLAMA_MODEL)"
}

