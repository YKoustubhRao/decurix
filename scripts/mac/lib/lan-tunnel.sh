#!/bin/bash
#
# Shared by scripts/mac/build.sh and scripts/mac/rebuild.sh. Meant to be
# sourced, not executed: it defines start_lan_tunnel and stop_lan_tunnel for
# the access-code-gated Cloudflare quick tunnel.

start_lan_tunnel() {
    local lan_port="$1"

    echo ""
    echo "=== Starting a second tunnel for the access-code-gated URL ==="

    LAN_TUNNEL_LOG="$(mktemp)"

    # Run in a background supervisor loop so if cloudflared terminates or drops, it auto-reconnects
    (
        while true; do
            cloudflared tunnel --protocol http2 --url "http://127.0.0.1:$lan_port" >>"$LAN_TUNNEL_LOG" 2>&1
            sleep 2
        done
    ) &
    LAN_TUNNEL_SUPERVISOR_PID=$!
    LAN_TUNNEL_PID=$LAN_TUNNEL_SUPERVISOR_PID

    # Background monitor to post URL updates if the tunnel reconnects with a fresh URL
    (
        local last_url=""
        while kill -0 "$LAN_TUNNEL_SUPERVISOR_PID" 2>/dev/null; do
            local current_url
            current_url=$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$LAN_TUNNEL_LOG" 2>/dev/null | tail -n1 || true)
            if [ -n "$current_url" ] && [ "$current_url" != "$last_url" ]; then
                last_url="$current_url"
                curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
                    -H "Content-Type: application/json" \
                    -d "{\"lan_tunnel_url\":\"$current_url\"}" \
                    >/dev/null 2>&1 || true
            fi
            sleep 2
        done
    ) &
    LAN_TUNNEL_WATCHER_PID=$!

    local lan_tunnel_url=""
    for _ in $(seq 1 30); do
        lan_tunnel_url=$(grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' "$LAN_TUNNEL_LOG" 2>/dev/null | tail -n1 || true)
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

stop_lan_tunnel() {
    if [ -n "${LAN_TUNNEL_WATCHER_PID:-}" ]; then
        kill "$LAN_TUNNEL_WATCHER_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${LAN_TUNNEL_SUPERVISOR_PID:-}" ]; then
        pkill -P "$LAN_TUNNEL_SUPERVISOR_PID" >/dev/null 2>&1 || true
        kill "$LAN_TUNNEL_SUPERVISOR_PID" >/dev/null 2>&1 || true
    elif [ -n "${LAN_TUNNEL_PID:-}" ]; then
        kill "$LAN_TUNNEL_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${LAN_TUNNEL_LOG:-}" ]; then
        rm -f "$LAN_TUNNEL_LOG" 2>/dev/null || true
    fi
}

