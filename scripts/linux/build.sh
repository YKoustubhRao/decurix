#!/bin/bash
#
# Opens Decurix: builds whichever desktop images don't exist yet, makes sure
# a desktop can reach Ollama, starts the control plane if it isn't already
# running, and opens a Cloudflare tunnel to it. Existing desktops, their
# volumes and the app database are left exactly as they are - this is meant
# to feel like reopening an app, not reinstalling it.
#
# Want a clean slate instead (fresh images, fresh desktops, empty database)?
# Use scripts/linux/rebuild.sh.
#
# Flags:
#   --cli   skip the tunnel and open the terminal CLI instead (see
#           `docker compose exec desktop-manager ./decurix --cli`, below)

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

CLI_MODE=""

for arg in "$@"; do
    case "$arg" in
        --cli) CLI_MODE="1" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

# Ctrl+C lands here while the script is tailing the tunnel's logs, which run
# forever until interrupted. Unlike rebuild.sh, this script's whole point is
# to leave Decurix running - so interrupting it just stops watching the
# tunnel, it does not tear anything down.
cleanup() {
    echo ""
    stop_lan_tunnel
    stop_keep_awake
    echo "=== Tunnel stopped. Decurix is still running: http://127.0.0.1:8080 ==="
    echo "(Run this script again for a new tunnel, or 'docker compose down' to stop Decurix.)"
}

trap 'cleanup; exit 130' INT TERM

echo "========================================"
echo " Opening Decurix"
echo "========================================"

# A fresh clone has no .env at all, and "docker compose up" fails without
# one. Start from the example so OLLAMA_MODEL and friends are there.
if [ ! -f .env ]; then
    if [ -f .env.example ]; then
        echo "No .env yet - creating it from .env.example."
        cp .env.example .env
    else
        touch .env
    fi
fi

# Enable Linux KSM for RAM deduplication if supported
if [ -d /sys/kernel/mm/ksm ] && ! grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null; then
    if [ -w /sys/kernel/mm/ksm/run ]; then
        echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null || true
        echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null || true
        echo 1000 > /sys/kernel/mm/ksm/pages_to_scan 2>/dev/null || true
        echo 20 > /sys/kernel/mm/ksm/sleep_millisecs 2>/dev/null || true
    else
        docker run --rm --privileged -v /sys/kernel/mm/ksm:/sys/kernel/mm/ksm alpine sh -c "echo 1 > /sys/kernel/mm/ksm/run 2>/dev/null; echo 1 > /sys/kernel/mm/ksm/use_zero_pages 2>/dev/null; echo 1000 > /sys/kernel/mm/ksm/pages_to_scan 2>/dev/null; echo 20 > /sys/kernel/mm/ksm/sleep_millisecs 2>/dev/null" >/dev/null 2>&1 || true
    fi
fi

# Only build an image that doesn't exist yet - that is the entire difference
# between "open the app" and "rebuild the app". A missing image (first run,
# or a profile added since) still gets built.
./scripts/linux/build-images.sh --only-missing

echo ""
echo "=== Ensuring the desktops can reach Ollama ==="

ensure_ollama_ready

echo ""
echo "=== Configuring host filesystem integration ==="
if [ -d /mnt/c ]; then
    export DECURIX_HOST_MNT_C="/mnt/c"
    if [ -x "/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe" ]; then
        /mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null || true
    elif [ -x "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe" ]; then
        /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null || true
    elif command -v powershell.exe >/dev/null 2>&1; then
        powershell.exe -NoProfile -NonInteractive -InputFormat None -Command "Get-StartApps | Select-Object Name, AppID | ConvertTo-Json -Compress" < /dev/null > /tmp/decurix_host_apps.json 2>/dev/null || true
    fi
else
    export DECURIX_HOST_MNT_C="/"
fi
export DECURIX_HOST_HOME="${HOME:-/home}"

echo ""
echo "=== Starting Decurix ==="

# No --build: an already-built control plane image starts as-is. Existing
# desktop_* containers and volumes, and the decurix-data volume (task
# history, chats, worker memory), are untouched.
# A fresh checkout has no control-plane image. Build it with the same WSL
# limits; existing images are left alone on normal launches.
control_plane_image="$(docker compose config --images | head -n 1)"
if ! docker image inspect "$control_plane_image" >/dev/null 2>&1; then
    configure_build_resources
    if [ -n "$DECURIX_BUILD_BUILDER" ]; then
        docker compose build --builder "$DECURIX_BUILD_BUILDER"
    else
        docker compose build
    fi
fi
docker compose up -d --no-build

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
echo " Decurix is running"
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

# --cli is a terminal-native alternative to the tunnel below: it never opens
# a tunnel URL, so the INT/TERM trap's "tunnel stopped" message would not
# make sense here - Ctrl+C to leave the CLI should just leave it.
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
    echo "Decurix is still running locally at http://127.0.0.1:8080."
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
