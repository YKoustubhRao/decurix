#!/bin/bash
#
# Rebuilds Decurix from a clean slate: no leftover desktop_* containers or
# volumes, and an EMPTY app database (decurix-data: task history, chat
# transcripts, query notes, worker memory). Images are still rebuilt (so a
# change to agent/ or a Dockerfile is picked up), but with Docker's normal
# build cache, not --no-cache - a rebuild should be thorough about clearing
# out data, not slow. This is the "reinstall" script - for "just start what
# I already have", use scripts/linux/build.sh instead.
#
# What is deliberately NOT wiped:
#   - .env (your configuration - API keys, OLLAMA_URL/MODEL, etc.)
#   - the decurix_ollama volume (downloaded models - re-pulling a chat model
#     is a multi-GB download nobody wants on every rebuild)
#
# Flags:
#   -y, --yes   skip the confirmation prompt (for scripted/CI use)
#   --cli       skip the tunnel and open the terminal CLI instead (see
#               `docker compose exec desktop-manager ./decurix --cli`, below)

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# shellcheck source=lib/ollama-setup.sh
source ./scripts/linux/lib/ollama-setup.sh
# shellcheck source=lib/lan-tunnel.sh
source ./scripts/linux/lib/lan-tunnel.sh
# shellcheck source=lib/keep-awake.sh
source ./scripts/linux/lib/keep-awake.sh

SKIP_CONFIRM=""
CLI_MODE=""

for arg in "$@"; do
    case "$arg" in
        -y|--yes) SKIP_CONFIRM="1" ;;
        --cli) CLI_MODE="1" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

echo "========================================"
echo " Decurix rebuild (clean slate)"
echo "========================================"
echo ""
echo "This will permanently delete:"
echo "  - every desktop_* container and volume"
echo "  - the decurix-data volume: ALL task history, chats, query notes and"
echo "    worker memory - this cannot be undone"
echo ""
echo "Kept as-is: .env, and the decurix_ollama volume (your pulled models)."
echo ""

if [ -z "$SKIP_CONFIRM" ]; then
    if [ ! -t 0 ]; then
        echo "ERROR: not running interactively. Re-run with --yes to confirm." >&2
        exit 1
    fi

    read -r -p "Type 'yes' to continue: " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted. Nothing was changed."
        exit 1
    fi
fi

# However far the rebuild got, an interrupted one should not leave a stack
# half old, half fresh - tear down whatever is currently there rather than
# leave it running detached.
cleanup() {
    echo ""
    stop_lan_tunnel
    stop_keep_awake
    echo "=== Interrupted: stopping Decurix and its containers ==="

    docker compose down || true

    docker ps -a \
        --filter "name=desktop_" \
        --format "{{.ID}}" \
        | xargs -r docker rm -f

    docker rm -f decurix-ollama >/dev/null 2>&1 || true
}

trap 'cleanup; exit 130' INT TERM

# A fresh clone has no .env at all, and "docker compose up --build" fails
# without one. Start from the example so OLLAMA_MODEL and friends are there.
# An existing .env is left exactly as it is - a rebuild is not a reset of
# your configuration.
if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

echo ""
echo "=== Stopping Decurix ==="

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
echo "=== Wiping the app database (decurix-data) ==="

docker volume rm decurix-data >/dev/null 2>&1 \
    || echo "(nothing to remove)"

echo ""
echo "=== Removing the bundled Ollama container (keeping its model volume) ==="

docker rm -f decurix-ollama >/dev/null 2>&1 \
    || echo "(nothing to remove)"

export DOCKER_BUILDKIT=1

echo ""
echo "=== Enabling Linux Kernel Samepage Merging (KSM) for RAM deduplication ==="
if [ -w /sys/kernel/mm/ksm/run ]; then
    echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null && echo "KSM enabled (run=1)" || true
    if [ -w /sys/kernel/mm/ksm/use_zero_pages ]; then
        echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null && echo "KSM zero pages deduplication enabled" || true
    fi
else
    # Try enabling via docker with mounted sysfs if direct write is not permitted
    docker run --rm --privileged -v /sys/kernel/mm/ksm:/sys/kernel/mm/ksm alpine sh -c "echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null; echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null" >/dev/null 2>&1 || true
fi

echo ""
./scripts/linux/build-images.sh

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Configuring host filesystem integration ==="
if [ -d /mnt/c ]; then
    export DECURIX_HOST_MNT_C="/mnt/c"
    echo "Discovered Windows filesystem at /mnt/c (WSL environment)"
    # Pre-cache Windows applications list to /tmp/decurix_host_apps.json
    if [ -x "/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe" ]; then
        /mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null && echo "Pre-cached Windows applications via PowerShell." || true
    elif [ -x "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe" ]; then
        /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null && echo "Pre-cached Windows applications via PowerShell." || true
    elif command -v powershell.exe >/dev/null 2>&1; then
        powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null && echo "Pre-cached Windows applications via PowerShell." || true
    fi
else
    export DECURIX_HOST_MNT_C="/"
fi
export DECURIX_HOST_HOME="${HOME:-/home}"

echo ""
echo "=== Starting Decurix ==="

docker compose up -d --build

echo ""
echo "=== Waiting for dashboard ==="

for i in {1..60}; do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "Decurix dashboard is running."
        break
    fi

    echo "Waiting for Decurix... ($i/60)"
    sleep 2
done

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    echo "ERROR: Decurix dashboard is not responding on port 8080."
    docker compose logs --tail=100
    exit 1
fi

echo ""
echo "========================================"
echo " Decurix is running (clean slate)"
echo "========================================"
echo ""
echo "Idle desktops stop automatically after 5 minutes (set DECURIX_IDLE_STOP_MINUTES=0 in .env to disable, or to another number of minutes)."

# DECURIX_LAN_PORT in .env changes which port the access-code-gated listener
# binds; read_env_var comes from lib/ollama-setup.sh, already sourced above.
lan_port="$(read_env_var DECURIX_LAN_PORT)"
lan_port="${lan_port:-8090}"

# The machine's own address on whatever network it's on, so both the admin
# (unauthenticated) and LAN (access-code-gated) URLs below are something
# another device can actually use - not "localhost", which only means
# anything on this machine. "ip route get" asks the kernel which source
# address it would use to reach the internet without sending a single
# packet; hostname -I is the fallback for a machine without `ip` (rare, but
# cheap to guard against a script erroring out over a cosmetic feature).
lan_ip="$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
if [ -z "$lan_ip" ]; then
    lan_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
fi

if [ -n "$lan_ip" ]; then
    lan_url="http://$lan_ip:$lan_port"

    # Reported once here, separately from the tunnel URL below (which is
    # reported every time cloudflared prints one) - see httpapi.tunnelHandler,
    # which never lets one blank out the other.
    curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
        -H "Content-Type: application/json" \
        -d "{\"lan_url\":\"$lan_url\"}" \
        >/dev/null 2>&1 || true

    echo ""
    echo "LAN access URL (needs the code below, works without the tunnel):  $lan_url"
fi

# Printed here as a convenience - it also always lives in the dashboard's
# Mobile menu (both URLs above alongside it), which is the one place it
# stays current after a rotate.
if command -v jq >/dev/null 2>&1; then
    lan_code="$(curl -fsS http://127.0.0.1:8080/api/lan-access/status 2>/dev/null | jq -r '.code // empty')"

    if [ -n "$lan_code" ]; then
        echo ""
        echo "Access code: $lan_code"
        echo "(Needed once by anyone using the LAN URL above. Also in the dashboard's Mobile menu, which can generate a new one.)"

        # A one-click link for whoever is sitting at this terminal right now -
        # they already have the code above, so this authenticates on click
        # instead of asking them to type it into the gate's form.
        if [ -n "$lan_url" ]; then
            echo ""
            echo "Open it yourself without typing the code: $lan_url/__decurix_access/authenticate?code=$lan_code"
        fi
    fi
fi

# The rebuild itself is done at this point (fresh images, fresh desktops,
# empty database, dashboard responding) - clear the interrupt trap before
# handing off to the CLI so leaving it (however that happens) just leaves
# Decurix running, the same as leaving build.sh's CLI does, rather than
# tearing down the stack this script just finished rebuilding.
# Nothing runs while this machine sleeps - no email trigger, no schedule -
# and the control plane, in its container, cannot stop the host sleeping.
# Held only while the dashboard has something that needs it (lib/keep-awake.sh).
echo ""
start_keep_awake

if [ -n "$CLI_MODE" ]; then
    trap - INT TERM

    echo ""
    echo "=== Opening the Decurix CLI ==="
    echo "(Decurix keeps running after you exit; 'exit' or Ctrl+D to leave.)"
    echo ""

    docker compose exec desktop-manager ./decurix --cli
    cli_status=$?
    stop_keep_awake
    exit $cli_status
fi

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "ERROR: cloudflared is not installed."
    exit 1
fi

# A second tunnel, to the access-code-gated listener - so the code-protected
# URL works from anywhere, not just $lan_url above on this machine's own
# network. Backgrounded (see lib/lan-tunnel.sh); the loop below only keeps
# this script in the foreground.
start_lan_tunnel "$lan_port"

echo ""

# The primary listener (:8080) is deliberately never tunnelled: it has no
# login and can create desktops and run commands on this machine. Only the
# access-code-gated listener above is reachable from off this machine. The
# tunnel to it runs in the background (lib/lan-tunnel.sh, which restarts it
# if it drops); this just keeps the script - and so Ctrl+C handling - in the
# foreground. "sleep & wait" so the trap runs at once, not after the sleep.
while true; do
    sleep 5 &
    wait $!
done
