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

# set_env_var NAME VALUE writes NAME=VALUE into .env, replacing every line
# that sets NAME. The value goes through the environment, never into a sed
# expression, so a "|" or "&" in it is kept as written.
set_env_var() {
    local tmp
    tmp="$(mktemp "${TMPDIR:-/tmp}/decurix-env.XXXXXX")"
    DECURIX_SET_NAME="$1" DECURIX_SET_VALUE="$2" awk '
        BEGIN { name = ENVIRON["DECURIX_SET_NAME"]; value = ENVIRON["DECURIX_SET_VALUE"]; done = 0 }
        {
            line = $0; sub(/\r$/, "", line)
            if (index(line, name "=") == 1) { if (!done) print name "=" value; done = 1; next }
            print line
        }
        END { if (!done) print name "=" value }
    ' .env > "$tmp" && cat "$tmp" > .env
    rm -f "$tmp"
}

# read_env_var NAME prints NAME's value from .env as compose reads it: the
# last line that sets it, without a Windows line ending, without one pair of
# surrounding quotes, and without a trailing " # comment" on an unquoted value.
read_env_var() {
    [ -f .env ] || return 0
    DECURIX_GET_NAME="$1" awk '
        BEGIN { name = ENVIRON["DECURIX_GET_NAME"]; found = 0; dq = sprintf("%c", 34); sq = sprintf("%c", 39) }
        {
            line = $0; sub(/\r$/, "", line)
            if (index(line, name "=") == 1) { value = substr(line, length(name) + 2); found = 1 }
        }
        END {
            if (!found) exit
            q = substr(value, 1, 1)
            if ((q == dq || q == sq) && length(value) >= 2 && substr(value, length(value), 1) == q) {
                value = substr(value, 2, length(value) - 2)
            } else {
                sub(/[ \t]+#.*$/, "", value)
                sub(/[ \t]+$/, "", value)
            }
            print value
        }
    ' .env
}

# A setting this run needs but the person did not choose (a fallback server or
# model) is used for this run only: it goes into a compose override file, never
# into .env, so the next run asks again and the person's own choice survives a
# moment when their server was down. Callers start compose with
#   docker compose -f docker-compose.yaml ${DECURIX_COMPOSE_OVERRIDE:+-f "$DECURIX_COMPOSE_OVERRIDE"} up ...
run_only_settings=""
DECURIX_COMPOSE_OVERRIDE=""

# run_only NAME VALUE uses VALUE for NAME in this run, loudly, and keeps .env.
run_only() {
    local nl='
'
    case "$2" in *[\"\\]*|*"$nl"*) echo "WARNING: not using $1=$2 for this run (it holds a quote, a backslash or a line break)."; return;; esac
    run_only_settings="$run_only_settings$1=$2$nl"
    echo ""
    echo "!! THIS RUN ONLY: $1=$2"
    echo "!! .env is left as you set it; the next run checks again."
}

# effective_env_var NAME prints the value this run uses: a run-only one, else .env's.
effective_env_var() {
    local value
    value="$(printf '%s' "$run_only_settings" | awk -v name="$1" 'index($0, name "=") == 1 { v = substr($0, length(name) + 2) } END { print v }')"
    if [ -n "$value" ]; then echo "$value"; else read_env_var "$1"; fi
}

# write_run_override writes the run-only settings as a compose override and
# exports DECURIX_COMPOSE_OVERRIDE naming it (empty when there are none).
write_run_override() {
    DECURIX_COMPOSE_OVERRIDE=""
    if [ -n "$run_only_settings" ]; then
        DECURIX_COMPOSE_OVERRIDE="$(mktemp "${TMPDIR:-/tmp}/decurix-run.XXXXXX")"
        {
            echo "services:"
            echo "  desktop-manager:"
            echo "    environment:"
            printf '%s' "$run_only_settings" | while IFS= read -r line; do
                [ -n "$line" ] && printf '      %s: "%s"\n' "${line%%=*}" "${line#*=}"
            done
        } > "$DECURIX_COMPOSE_OVERRIDE"
    fi
    export DECURIX_COMPOSE_OVERRIDE
}

# narrow_old_ollama_bind takes back what an earlier version of these scripts
# did: launchctl's OLLAMA_HOST set to 0.0.0.0, which put Ollama (it has no login) on
# every address. Docker Desktop reaches the Mac's own 127.0.0.1 through
# host.docker.internal, so Ollama needs no more than its default.
narrow_old_ollama_bind() {
    if [ "$(launchctl getenv OLLAMA_HOST 2>/dev/null)" = "0.0.0.0" ]; then
        echo "An earlier Decurix put Ollama on every address (OLLAMA_HOST set to 0.0.0.0 via"
        echo "launchctl). Removing that; restart the Ollama app to listen on 127.0.0.1 only."
        launchctl unsetenv OLLAMA_HOST || echo "WARNING: could not unset it. Run: launchctl unsetenv OLLAMA_HOST"
    fi
    local rc="$HOME/.zshrc" tmp
    if [ -f "$rc" ] && grep -qx 'export OLLAMA_HOST="0[.]0[.]0[.]0"' "$rc"; then
        tmp="$(mktemp "${TMPDIR:-/tmp}/decurix-zshrc.XXXXXX")"
        grep -vx 'export OLLAMA_HOST="0[.]0[.]0[.]0"' "$rc" > "$tmp" && cat "$tmp" > "$rc" \
            && echo "Removed the OLLAMA_HOST line an earlier Decurix added to ~/.zshrc."
        rm -f "$tmp"
    fi
}

# Ensures OLLAMA_URL/OLLAMA_MODEL name a server the desktop containers can
# reach, with the chat and embedding models pulled onto it. A fallback is used
# for this run only (run_only); callers pass DECURIX_COMPOSE_OVERRIDE to
# "docker compose up".
ensure_ollama_ready() {
    local default_ollama_url="http://host.docker.internal:11434"
    local bundled_ollama_url="http://decurix-ollama:11434"
    local embed_model="nomic-embed-text"
    local ollama_url_ok=""

    docker network inspect decurix-network >/dev/null 2>&1 \
        || docker network create decurix-network >/dev/null

    narrow_old_ollama_bind

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

    # Docker Desktop forwards host.docker.internal to the Mac's 127.0.0.1, so
    # an Ollama answering here but not there is not a binding problem, and
    # putting it on every address would only hand it to the whole network.
    if [ -z "$ollama_url_ok" ] \
        && curl -fsS -m 5 http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then
        echo ""
        echo "Ollama answers on this Mac but not from a desktop container. Docker Desktop"
        echo "normally forwards host.docker.internal to this Mac; check that Docker Desktop"
        echo "(not another engine) is running, then re-run."
    fi

    if [ -z "$ollama_url_ok" ]; then
        echo ""
        echo "WARNING: no Ollama the desktops can reach. Falling back to the bundled"
        echo "decurix-ollama container for this run."
        ollama_url_ok="$bundled_ollama_url"
        # A pinned model is replaced by a small one that always works; "auto"
        # stays auto - the control plane pulls what fits into the container.
        case "$(read_env_var OLLAMA_MODEL)" in
            ""|auto) ;;
            *) run_only OLLAMA_MODEL "qwen2.5-coder:3b" ;;
        esac
    fi

    # .env keeps the person's server. A URL is written only when .env named
    # none and the one found is not the fallback; anything else is this run's.
    if [ "$ollama_url_ok" != "$configured_ollama_url" ]; then
        if [ -z "$configured_ollama_url" ] && [ "$ollama_url_ok" != "$bundled_ollama_url" ]; then
            set_env_var OLLAMA_URL "$ollama_url_ok"
        else
            run_only OLLAMA_URL "$ollama_url_ok"
        fi
    fi

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
        else
            echo ""
            echo "Pulling the chat model $wanted_model (once; ~2GB)..."

            if pull_ollama_model "$ollama_url_ok" "$wanted_model"; then
                echo "Chat model $wanted_model is ready."
            elif [ -n "$available_models" ]; then
                local first_model
                first_model="$(echo "$available_models" | head -1)"
                echo "WARNING: could not pull $wanted_model; using $first_model for this run."
                run_only OLLAMA_MODEL "$first_model"
            else
                echo "WARNING: could not pull $wanted_model. Run: ollama pull $wanted_model"
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

    write_run_override
    echo ""
    echo "Desktops will use OLLAMA_URL=$(effective_env_var OLLAMA_URL) OLLAMA_MODEL=$(effective_env_var OLLAMA_MODEL)"
    if [ -n "$DECURIX_COMPOSE_OVERRIDE" ]; then
        echo "!! Some of that is for this run only (see above); .env is unchanged there."
    fi
}
