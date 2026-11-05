#!/bin/bash
#
# Shared by scripts/linux/build.sh and scripts/linux/rebuild.sh. Meant to be
# sourced, not executed: it only defines ensure_ollama_ready, which finds an
# Ollama the desktop containers can actually reach, pulls the chat and
# embedding models onto it, and records OLLAMA_URL/OLLAMA_MODEL in .env (a
# fallback is used for the run only, never written there).
#
# Requires: decurix-desktop:basic already built (the probes run inside it),
# decurix-network already created, and .env already existing. The caller is
# responsible for all three - this file only does the Ollama part, so build.sh
# and rebuild.sh don't each carry their own copy of it.

# Asked from inside a container on purpose. A desktop talks to Ollama from a
# container, so that is the only place the question is honest: the host can
# always reach its own 127.0.0.1, which is precisely the configuration that
# leaves every desktop unable to reach anything.
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

# host_gateway_ip prints the address host.docker.internal resolves to inside
# a desktop (Docker's host-gateway): the one address the desktops dial for
# this host's Ollama, and so the one it needs to listen on.
host_gateway_ip() {
    docker run --rm \
        --network decurix-network \
        --add-host host.docker.internal:host-gateway \
        --entrypoint python3 \
        decurix-desktop:basic \
        -c "import socket; print(socket.gethostbyname('host.docker.internal'))" 2>/dev/null \
        | grep -E '^[0-9]+(\.[0-9]+){3}$' | head -1
}

# narrow_old_ollama_bind takes back what an earlier version of this script
# did: a systemd drop-in that put Ollama, which has no login, on every
# address (0.0.0.0). It is narrowed to the address the desktops dial.
narrow_old_ollama_bind() {
    local dropin=/etc/systemd/system/ollama.service.d/decurix.conf
    grep -qs 'OLLAMA_HOST=0[.]0[.]0[.]0' "$dropin" || return 0
    local bind_ip
    bind_ip="$(host_gateway_ip)"
    if [ -z "$bind_ip" ]; then
        echo "WARNING: $dropin puts Ollama on every address (0.0.0.0), and the address"
        echo "desktops reach this host on could not be found to narrow it. Edit it by hand."
        return 0
    fi
    echo "An earlier Decurix put Ollama on every address (0.0.0.0). Narrowing it to"
    echo "$bind_ip, the address desktops reach this host on (needs sudo, once)."
    sudo sed -i "s|OLLAMA_HOST=0[.]0[.]0[.]0|OLLAMA_HOST=$bind_ip|" "$dropin" \
        && sudo systemctl daemon-reload \
        && sudo systemctl restart ollama \
        || echo "WARNING: could not narrow it; Ollama still listens on every address."
}

# Ensures OLLAMA_URL/OLLAMA_MODEL name a server the desktop containers can
# reach, with the chat and embedding models pulled onto it. .env is given to
# the control plane by compose (env_file) when it starts, so callers run this
# before "docker compose up", and pass DECURIX_COMPOSE_OVERRIDE to it.
ensure_ollama_ready() {
    local default_ollama_url="http://host.docker.internal:11434"
    local bundled_ollama_url="http://decurix-ollama:11434"
    # The chat model is not the only model. TheBot's harness embeds every
    # fact it learns, every document it indexes and every failure it
    # records, and Ollama refuses to embed with a chat model - so a server
    # with only qwen2.5-coder pulled answers questions perfectly while
    # silently losing everything it was supposed to remember, one
    # `model "nomic-embed-text" not found` at a time.
    local embed_model="nomic-embed-text"
    local ollama_url_ok=""

    docker network inspect decurix-network >/dev/null 2>&1 \
        || docker network create decurix-network >/dev/null

    narrow_old_ollama_bind

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

            # Only the address the desktops dial (host.docker.internal is
            # Docker's host-gateway), never 0.0.0.0: Ollama has no login, and
            # on every address it hands model management to the whole LAN.
            local bind_ip
            bind_ip="$(host_gateway_ip)"
            if [ -z "$bind_ip" ]; then
                echo "Could not find the address desktops reach this host on; not changing Ollama."
            else
            echo "Giving its service OLLAMA_HOST=$bind_ip (the address desktops reach this host on) and"
            echo "CPU priority/thread limits (needs sudo, once). The ollama command on this host then"
            echo "needs OLLAMA_HOST=$bind_ip:11434 too."
            echo ""

            local ncpus threads quota_pct
            ncpus="$(nproc 2>/dev/null || echo 4)"
            if [ "$ncpus" -gt 4 ]; then
                threads=$((ncpus - 2))
            elif [ "$ncpus" -gt 2 ]; then
                threads=$((ncpus - 1))
            else
                threads=1
            fi
            quota_pct=$((threads * 100))

            if sudo mkdir -p /etc/systemd/system/ollama.service.d \
                && printf '[Service]\nEnvironment="OLLAMA_HOST=%s"\nEnvironment="OLLAMA_FLASH_ATTENTION=1"\nEnvironment="OLLAMA_KV_CACHE_TYPE=q8_0"\nEnvironment="OLLAMA_NUM_PARALLEL=1"\nEnvironment="OLLAMA_NUM_THREAD=%d"\nNice=10\nCPUShares=512\nCPUQuota=%d%%\n' "$bind_ip" "$threads" "$quota_pct" \
                    | sudo tee /etc/systemd/system/ollama.service.d/decurix.conf >/dev/null \
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
            fi
        else
            echo "No systemd ollama service to reconfigure."
        fi
    fi

    # Last resort: the container Decurix manages itself. Small model, but it
    # needs nothing from the host and always works, which beats handing every
    # desktop a URL that answers nowhere. qwen2.5-coder:3b, not qwen3:0.6b -
    # the 0.6b model was too weak to reliably follow the agent's own
    # COMMAND/DONE/HELP contract (see internal/ollama.Model's comment).
    # For this run only (run_only): a server that was down a moment must not
    # cost the person their own server and model for good.
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

    # A reachable server running a model nobody asked for fails exactly as
    # opaquely as an unreachable one, so make sure OLLAMA_MODEL is something
    # that server actually has. The model named in .env is a choice, so it is
    # pulled rather than swapped out; only if that pull fails does
    # OLLAMA_MODEL fall back to a model the server already has. Unset or
    # "auto" is not a choice to pull: the control plane picks a model that
    # fits this machine (internal/ollama, SelectModel).
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
            echo "Pulling the chat model $wanted_model (once; a 3B model is ~2GB)."

            if pull_ollama_model "$ollama_url_ok" "$wanted_model"; then
                echo "Chat model $wanted_model is ready."
            elif [ -n "$available_models" ]; then
                local first_model
                first_model="$(echo "$available_models" | head -1)"
                echo "WARNING: could not pull $wanted_model; using $first_model for this run."
                run_only OLLAMA_MODEL "$first_model"
            else
                echo "WARNING: could not pull $wanted_model, and that server has"
                echo "no models at all. Desktops will not be able to reason until"
                echo "you run: ollama pull $wanted_model"
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

    write_run_override
    echo ""
    echo "Desktops will use OLLAMA_URL=$(effective_env_var OLLAMA_URL) OLLAMA_MODEL=$(effective_env_var OLLAMA_MODEL)"
    if [ -n "$DECURIX_COMPOSE_OVERRIDE" ]; then
        echo "!! Some of that is for this run only (see above); .env is unchanged there."
    fi
}
