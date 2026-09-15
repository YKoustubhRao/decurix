#!/bin/bash
#
# Shared by scripts/linux/build.sh and scripts/linux/rebuild.sh. Meant to be
# sourced, not executed: it only defines start_lan_tunnel/stop_lan_tunnel, a
# second Cloudflare quick tunnel pointed at the access-code-gated listener
# (rather than the primary, unauthenticated one both scripts already tunnel
# in their own foreground loop) - so the gated URL is reachable from
# anywhere, not only this machine's own network like the plain LAN address
# is. Both scripts already print and report that LAN address; this gives the
# same gated listener a link that works off-network too, still behind the
# code.
#
# Requires: cloudflared already confirmed installed by the caller - this
# file has no error message of its own for a missing binary, since the
# caller's own primary-tunnel section already has one and exits before this
# would ever be reached without it.

# start_lan_tunnel opens a Cloudflare tunnel to 127.0.0.1:$1 in the
# background (so the caller's own primary tunnel below can still run in the
# foreground) and reports its trycloudflare.com URL to the control plane as
# lan_tunnel_url once cloudflared prints one - waiting up to 30 seconds, the
# same order of magnitude the primary tunnel usually takes to come up.
# Prints the URL, or a one-line note if it didn't show up in time (the
# dashboard's Mobile menu will still pick it up whenever it does). Sets
# LAN_TUNNEL_PID/LAN_TUNNEL_LOG for stop_lan_tunnel to clean up.
start_lan_tunnel() {
    local lan_port="$1"

    echo ""
    echo "=== Starting a second tunnel for the access-code-gated URL ==="

    LAN_TUNNEL_LOG="$(mktemp)"
    cloudflared tunnel --protocol http2 --url "http://127.0.0.1:$lan_port" >"$LAN_TUNNEL_LOG" 2>&1 &
    LAN_TUNNEL_PID=$!

    local lan_tunnel_url=""
    for _ in $(seq 1 30); do
        lan_tunnel_url=$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$LAN_TUNNEL_LOG" 2>/dev/null | head -n1 || true)
        if [ -n "$lan_tunnel_url" ]; then
            break
        fi
        sleep 1
    done

    if [ -n "$lan_tunnel_url" ]; then
        curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
            -H "Content-Type: application/json" \
            -d "{\"lan_tunnel_url\":\"$lan_tunnel_url\"}" \
            >/dev/null 2>&1 || true

        echo "Gated tunnel URL (needs the access code, works from anywhere): $lan_tunnel_url"
    else
        echo "(It didn't come up within 30s - it'll still appear in the dashboard's Mobile menu once it does.)"
    fi
}

# stop_lan_tunnel kills the background cloudflared process start_lan_tunnel
# started, if any, and removes its log file - called from each script's own
# cleanup on exit/interrupt so it never outlives the script as an orphaned
# process.
stop_lan_tunnel() {
    if [ -n "${LAN_TUNNEL_PID:-}" ]; then
        kill "$LAN_TUNNEL_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${LAN_TUNNEL_LOG:-}" ]; then
        rm -f "$LAN_TUNNEL_LOG" 2>/dev/null || true
    fi
}
