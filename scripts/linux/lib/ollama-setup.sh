#!/bin/bash
#
# Shared by scripts/linux/build.sh and scripts/linux/rebuild.sh. Meant to be
# sourced, not executed: it only defines ensure_ollama_ready, which finds an
# Ollama the desktop containers can actually reach, pulls the chat and
# embedding models onto it, and records OLLAMA_URL/OLLAMA_MODEL in .env.
#
# Requires: flowera-desktop:basic already built (the probes run inside it),
# flowera-network already created, and .env already existing. The caller is
# responsible for all three - this file only does the Ollama part, so build.sh
# and rebuild.sh don't each carry their own copy of it.

# Asked from inside a container on purpose. A desktop talks to Ollama from a
# container, so that is the only place the question is honest: the host can
# always reach its own 127.0.0.1, which is precisely the configuration that
# leaves every desktop unable to reach anything.
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
    # No timeout: pulling a model is a download, and how long it takes is
    # a property of the network, not of whether it is working.
    urllib.request.urlopen(req)
except Exception as exc:
    print(exc, file=sys.stderr)
    sys.exit(1)
"
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

# Ensures OLLAMA_URL/OLLAMA_MODEL in .env name a server the desktop
# containers can reach, with the chat and embedding models pulled onto it.
# Everything here writes .env, which docker/Dockerfile copies into the
# control plane image - so callers must run this before "docker compose
# build"/"up --build", not after.
ensure_ollama_ready() {
    local default_ollama_url="http://host.docker.internal:11434"
    local bundled_ollama_url="http://flowera-ollama:11434"
    # The chat model is not the only model. TheBot's harness embeds every
    # fact it learns, every document it indexes and every failure it
    # records, and Ollama refuses to embed with a chat model - so a server
    # with only qwen2.5-coder pulled answers questions perfectly while
    # silently losing everything it was supposed to remember, one
    # `model "nomic-embed-text" not found` at a time.
    local embed_model="nomic-embed-text"
    local ollama_url_ok=""

    docker network inspect flowera-network >/dev/null 2>&1 \
        || docker network create flowera-network >/dev/null

    # Whatever is already in .env wins, as long as it actually works.
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

    # The host running the control plane, over the same host-gateway mapping
    # every desktop already gets for CONTROL_PLANE_URL.
    if [ -z "$ollama_url_ok" ] && probe_ollama "$default_ollama_url"; then
        echo "Host Ollama is reachable from containers."
        ollama_url_ok="$default_ollama_url"
    fi

    # The common broken case: Ollama is running right here, but bound to
    # loopback, so it is reachable from this shell and from nowhere else.
    if [ -z "$ollama_url_ok" ] \
        && curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then

        echo ""
        echo "Ollama is running on this host but listening only on 127.0.0.1,"
        echo "so no desktop container can reach it."

        if command -v systemctl >/dev/null 2>&1 \
            && systemctl cat ollama >/dev/null 2>&1; then

            echo "Giving its service OLLAMA_HOST=0.0.0.0 (needs sudo, once)."
            echo ""

            if sudo mkdir -p /etc/systemd/system/ollama.service.d \
                && printf '[Service]\nEnvironment="OLLAMA_HOST=0.0.0.0"\nEnvironment="OLLAMA_FLASH_ATTENTION=1"\nEnvironment="OLLAMA_KV_CACHE_TYPE=q8_0"\nEnvironment="OLLAMA_NUM_PARALLEL=1"\n' \
                    | sudo tee /etc/systemd/system/ollama.service.d/flowera.conf >/dev/null \
                && sudo systemctl daemon-reload \
                && sudo systemctl restart ollama; then

                local i
                for i in {1..15}; do
                    if probe_ollama "$default_ollama_url"; then
                        echo "Host Ollama is now reachable from containers."
                        ollama_url_ok="$default_ollama_url"
                        break
                    fi

                    sleep 2
                done
            fi

            if [ -z "$ollama_url_ok" ]; then
                echo "Could not reconfigure the Ollama service."
            fi
        else
            echo "No systemd ollama service to reconfigure."
        fi
    fi

    # Last resort: the container Flowera manages itself. Small model, but it
    # needs nothing from the host and always works, which beats handing every
    # desktop a URL that answers nowhere. qwen2.5-coder:3b, not qwen3:0.6b -
    # the 0.6b model was too weak to reliably follow the agent's own
    # COMMAND/DONE/HELP contract (see internal/ollama.Model's comment).
    if [ -z "$ollama_url_ok" ]; then
        echo ""
        echo "Falling back to the bundled flowera-ollama container."
        ollama_url_ok="$bundled_ollama_url"
        set_env_var OLLAMA_MODEL "qwen2.5-coder:3b"
    fi

    set_env_var OLLAMA_URL "$ollama_url_ok"

    # A reachable server running a model nobody asked for fails exactly as
    # opaquely as an unreachable one, so make sure OLLAMA_MODEL is something
    # that server actually has. The model named in .env is a choice, so it is
    # pulled rather than swapped out; only if that pull fails does
    # OLLAMA_MODEL fall back to a model the server already has. The default
    # matches internal/ollama.Model, which is what the control plane gives
    # desktops when OLLAMA_MODEL is unset.
    if [ "$ollama_url_ok" != "$bundled_ollama_url" ]; then
        local wanted_model available_models
        wanted_model="$(read_env_var OLLAMA_MODEL)"
        wanted_model="${wanted_model:-qwen2.5-coder:3b}"
        available_models="$(list_ollama_models "$ollama_url_ok")"

        if echo "$available_models" | grep -qE "^$wanted_model(:latest)?$"; then
            echo "Chat model $wanted_model is present."
            set_env_var OLLAMA_MODEL "$wanted_model"
        else
            echo ""
            echo "Pulling the chat model $wanted_model (once; a 3B model is ~2GB)."

            if pull_ollama_model "$ollama_url_ok" "$wanted_model"; then
                echo "Chat model $wanted_model is ready."
                set_env_var OLLAMA_MODEL "$wanted_model"
            elif [ -n "$available_models" ]; then
                local first_model
                first_model="$(echo "$available_models" | head -1)"
                echo "WARNING: could not pull $wanted_model; using $first_model."
                set_env_var OLLAMA_MODEL "$first_model"
            else
                echo "WARNING: could not pull $wanted_model, and that server has"
                echo "no models at all. Desktops will not be able to reason until"
                echo "you run: ollama pull $wanted_model"
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
            echo "Pulling the embedding model $embed_model (~270MB, once)."

            # Not fatal. A desktop on the ollama or claude harness never
            # embeds anything, so a failed pull here must not stop the whole
            # stack coming up - it only costs TheBot its memory.
            if pull_ollama_model "$ollama_url_ok" "$embed_model"; then
                echo "Embedding model $embed_model is ready."
            else
                echo "WARNING: could not pull $embed_model."
                echo "The thebot harness will answer, but will not be able to"
                echo "remember anything it learns. The other harnesses are"
                echo "unaffected."
            fi
        fi
    fi

    echo ""
    echo "Desktops will use OLLAMA_URL=$(read_env_var OLLAMA_URL) OLLAMA_MODEL=$(read_env_var OLLAMA_MODEL)"
}
