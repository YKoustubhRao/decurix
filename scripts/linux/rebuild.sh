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
# shellcheck source=lib/lan-address.sh
source ./scripts/linux/lib/lan-address.sh
# shellcheck source=lib/keep-awake.sh
source ./scripts/linux/lib/keep-awake.sh
# shellcheck source=lib/build-resources.sh
source ./scripts/linux/lib/build-resources.sh

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

# Check and start the memory-limited builder before deleting any app data.
configure_build_resources

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
        --filter "label=desktop-manager=true" \
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
    --filter "label=desktop-manager=true" \
    --format "{{.ID}}" \
    | xargs -r docker rm -f

echo ""
echo "=== Removing old desktop volumes ==="

docker volume ls \
    --filter "name=^desktop_" \
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
# KSM merges identical memory pages across processes - here across desktops
# that do not trust each other, which is a known page-sharing side channel -
# and turning it on from here needs a privileged container on every launch.
# So it is off unless asked for: DECURIX_ENABLE_KSM=1 in the environment or in
# .env (internal/desktop reads the same switch).
echo "=== Linux Kernel Samepage Merging (KSM) ==="
if [ "${DECURIX_ENABLE_KSM:-$(read_env_var DECURIX_ENABLE_KSM)}" != "1" ]; then
    echo "KSM stays off (DECURIX_ENABLE_KSM=1 turns it on)."
elif grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null; then
    echo "Skipping KSM changes inside WSL."
elif [ -w /sys/kernel/mm/ksm/run ]; then
    echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null && echo "KSM enabled (run=1)" || true
    if [ -w /sys/kernel/mm/ksm/use_zero_pages ]; then
        echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null && echo "KSM zero pages deduplication enabled" || true
    fi
else
    # Try enabling via docker with mounted sysfs if direct write is not permitted
    docker run --rm --privileged -v /sys/kernel/mm/ksm:/sys/kernel/mm/ksm alpine:3.20 sh -c "echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null; echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null" >/dev/null 2>&1 || true
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
    # No Windows drive here. Never the host's root: the control plane has no login, and its "C:" is what host-file
    # checks read from - "/" put /etc/shadow and every home folder in reach (review 2026-11-02). An empty folder.
    DECURIX_HOST_MNT_C="${XDG_CACHE_HOME:-$HOME/.cache}/decurix/no-windows-drive"
    mkdir -p "$DECURIX_HOST_MNT_C"
    export DECURIX_HOST_MNT_C
fi
export DECURIX_HOST_HOME="${HOME:-/home}"

echo ""
echo "=== Starting Decurix ==="

if [ -n "$DECURIX_BUILD_BUILDER" ]; then
    docker compose build --builder "$DECURIX_BUILD_BUILDER"
else
    docker compose build
fi
docker compose -f docker-compose.yaml ${DECURIX_COMPOSE_OVERRIDE:+-f "$DECURIX_COMPOSE_OVERRIDE"} up -d --no-build

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
# Under WSL2 this is Windows' own address, forwarded into WSL on the gated
# port (lib/lan-address.sh) - WSL's private one is unreachable from a phone.
lan_ip="$(decurix_lan_ip "$lan_port")"

report_lan_access "$lan_ip" "$lan_port"

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

# cloudflared is needed only for the public link, which is off unless DECURIX_PUBLIC_LINK=1 (lib/lan-tunnel.sh).
if public_link_enabled && ! command -v cloudflared >/dev/null 2>&1; then
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
