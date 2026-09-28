#!/bin/bash
#
# Shared by scripts/mac/build.sh and scripts/mac/rebuild.sh. Meant to be
# sourced, not executed: it defines start_lan_tunnel and stop_lan_tunnel for
# the access-code-gated Cloudflare quick tunnel.

# named_tunnel_url prints the fixed link on the person's own domain when
# one is set up in the dashboard (Mobile menu > Your own link), or nothing.
# Decurix runs that tunnel's connector itself (internal/cftunnel), so a
# quick tunnel would only add a second, changing address.
named_tunnel_url() {
    curl -fsS --max-time 5 http://127.0.0.1:8080/api/cftunnel 2>/dev/null \
        | grep -q '"configured":true' || return 0
    curl -fsS --max-time 5 http://127.0.0.1:8080/api/cftunnel 2>/dev/null \
        | grep -oE '"url":"https://[^"]+"' | head -n1 | cut -d'"' -f4
}

# A quick tunnel's address is hyphenated words (word-word-word.trycloudflare.com).
# The pattern below insists on that shape: cloudflared's own errors mention
# https://api.trycloudflare.com, which is Cloudflare's API, not a tunnel -
# it was once reported as the link, and opening it says "method not allowed".
start_lan_tunnel() {
    local lan_port="$1"

    local named_url
    named_url="$(named_tunnel_url)"
    if [ -n "$named_url" ]; then
        echo ""
        echo "=== Your own link is set up ==="
        echo "Gated URL (needs the access code, works from anywhere): $named_url"
        echo "(Decurix keeps its tunnel running itself - no quick tunnel is opened.)"
        return 0
    fi

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
            current_url=$(grep -oE 'https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com' "$LAN_TUNNEL_LOG" 2>/dev/null | tail -n1 || true)
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
        lan_tunnel_url=$(grep -oE 'https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com' "$LAN_TUNNEL_LOG" 2>/dev/null | tail -n1 || true)
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

        # One link that signs a device in and remembers it - what to open on
        # a phone or another computer (the dashboard's Mobile menu shows it too).
        local lan_code
        lan_code=$(curl -fsS --max-time 5 http://127.0.0.1:8080/api/lan-access/status 2>/dev/null \
            | grep -oE '"code":"[^"]+"' | head -n1 | cut -d'"' -f4)
        if [ -n "$lan_code" ]; then
            echo "Open this on your phone or other computer (signs in, nothing to type):"
            echo "  $lan_tunnel_url/__decurix_access/authenticate?code=$lan_code&remember=1&next=%2F"
        fi
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

