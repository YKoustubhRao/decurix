#!/bin/bash
#
# Shared by build.sh and rebuild.sh in scripts/linux and scripts/mac. Meant to be
# sourced, not executed: it defines report_lan_access (the LAN URL and access
# code), and start_lan_tunnel/stop_lan_tunnel, a
# second Cloudflare quick tunnel pointed at the access-code-gated listener
# (the only listener either script tunnels: the primary, unauthenticated
# one is never exposed) - so the gated URL is reachable from
# anywhere, not only this machine's own network like the plain LAN address
# is. Both scripts already print and report that LAN address; this gives the
# same gated listener a link that works off-network too, still behind the
# code.
#
# Requires: cloudflared already confirmed installed by the caller - this
# file has no error message of its own for a missing binary, since the
# caller's own tunnel section already has one and exits before this
# would ever be reached without it.

# start_lan_tunnel opens a Cloudflare tunnel to 127.0.0.1:$1 in the
# background (so the caller stays in the foreground) and reports its trycloudflare.com URL to the control plane as
# lan_tunnel_url once cloudflared prints one - waiting up to 30 seconds, the
# same order of magnitude a quick tunnel usually takes to come up.
# Prints the URL, or a one-line note if it didn't show up in time (the
# dashboard's Mobile menu will still pick it up whenever it does). Sets
# LAN_TUNNEL_PID/LAN_TUNNEL_LOG for stop_lan_tunnel to clean up.
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
# public_link_enabled says whether the person turned the public link on: DECURIX_PUBLIC_LINK=1 in the environment or
# in .env. It is off by default (owner decision O-5), as in the app.
public_link_enabled() {
    [ "${DECURIX_PUBLIC_LINK:-}" = "1" ] && return 0
    [ -f .env ] && grep -Eq '^[[:space:]]*DECURIX_PUBLIC_LINK[[:space:]]*=[[:space:]]*"?1"?[[:space:]]*$' .env
}

# gated_port_ok says whether port is the gated listener's: never the primary one (8080), and serving the access-code
# page right now (the same check the app makes before it exposes anything, B-07).
gated_port_ok() {
    local port="$1"
    [ "$port" != "8080" ] || return 1
    curl -fsS --max-time 5 "http://127.0.0.1:$port/" 2>/dev/null | grep -q "Enter the access code"
}

start_lan_tunnel() {
    local lan_port="$1"

    if ! public_link_enabled; then
        echo ""
        echo "Public link: off. Phones reach Decurix on this network only. For a link that works from anywhere (it still needs the access code), set DECURIX_PUBLIC_LINK=1 in .env and run this again."
        return 0
    fi
    if ! gated_port_ok "$lan_port"; then
        echo ""
        echo "Public link: not started - 127.0.0.1:$lan_port is not serving the access-code page, and only that listener is ever tunnelled."
        return 0
    fi

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

    # Run in a background supervisor loop so if cloudflared terminates or drops, it is started again (B-29).
    supervise_lan_tunnel "$lan_port" "$LAN_TUNNEL_LOG" "$$" &
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

# The restart loop's timings (B-29): the first wait after the tunnel exits is LAN_TUNNEL_FIRST_DELAY seconds, doubling
# up to LAN_TUNNEL_MAX_DELAY; a tunnel that stayed up LAN_TUNNEL_RESET_AFTER seconds starts again from the first wait.
# Variables so scripts/testing/lan-tunnel.test.sh can shorten them; LAN_TUNNEL_COMMAND lets it stand in for cloudflared.
LAN_TUNNEL_COMMAND="${LAN_TUNNEL_COMMAND:-cloudflared}"
LAN_TUNNEL_FIRST_DELAY="${LAN_TUNNEL_FIRST_DELAY:-1}"
LAN_TUNNEL_MAX_DELAY="${LAN_TUNNEL_MAX_DELAY:-60}"
LAN_TUNNEL_RESET_AFTER="${LAN_TUNNEL_RESET_AFTER:-600}"

# supervise_lan_tunnel runs the quick tunnel to 127.0.0.1:$1 with its output
# appended to $2, and starts it again whenever it exits, for as long as the
# script with pid $3 runs (start_lan_tunnel runs it in the background). Each
# exit is logged with its code and the wait: 1 s, doubling to a 60 s cap, back
# to 1 s once a run has stayed up 10 minutes. Before each start it checks
# again what start_lan_tunnel checked - the public link is still on
# (DECURIX_PUBLIC_LINK, .env read again), no fixed link has been set up since,
# and the port still serves the access-code page, the only listener ever
# tunnelled. Turned off, or replaced by a fixed link: it stops.
supervise_lan_tunnel() {
    local port="$1" log="$2" owner="$3"
    local delay="$LAN_TUNNEL_FIRST_DELAY" started up code
    while kill -0 "$owner" 2>/dev/null; do
        started=$(date +%s)
        "$LAN_TUNNEL_COMMAND" tunnel --protocol http2 --url "http://127.0.0.1:$port" >>"$log" 2>&1
        code=$?
        up=$(( $(date +%s) - started ))
        if [ "$up" -ge "$LAN_TUNNEL_RESET_AFTER" ]; then
            delay="$LAN_TUNNEL_FIRST_DELAY"
        fi
        echo "Public link: the tunnel exited with code $code after ${up}s; starting it again in ${delay}s."
        while :; do
            sleep "$delay"
            delay=$(( delay * 2 ))
            [ "$delay" -le "$LAN_TUNNEL_MAX_DELAY" ] || delay="$LAN_TUNNEL_MAX_DELAY"
            kill -0 "$owner" 2>/dev/null || return 0
            if ! public_link_enabled; then
                echo "Public link: turned off - the tunnel is not started again."
                return 0
            fi
            local named
            named="$(named_tunnel_url)"
            if [ -n "$named" ]; then
                echo "Public link: your own link is set up ($named) - the quick tunnel is not started again."
                return 0
            fi
            if gated_port_ok "$port"; then
                echo "Public link: tunnel started again."
                break
            fi
            echo "Public link: 127.0.0.1:$port is not serving the access-code page; trying again in ${delay}s."
        done
    done
}

# report_lan_access tells the control plane the LAN URL for address $1 (when
# there is one) on the gated port $2, and prints it with the access code.
report_lan_access() {
    local lan_url="" lan_code

    if [ -n "$1" ]; then
        lan_url="http://$1:$2"

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
}

# stop_lan_tunnel ends the restart loop, kills the background cloudflared
# process start_lan_tunnel started, if any, and removes its log file - called from each script's own
# cleanup on exit/interrupt so it never outlives the script as an orphaned
# process.
stop_lan_tunnel() {
    if [ -n "${LAN_TUNNEL_WATCHER_PID:-}" ]; then
        kill "$LAN_TUNNEL_WATCHER_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${LAN_TUNNEL_SUPERVISOR_PID:-}" ]; then
        # The supervisor first, so it cannot start the tunnel again once cloudflared is gone; its children are listed
        # before, since a child whose parent has gone is no longer found by its parent's pid.
        local children
        children="$(pgrep -P "$LAN_TUNNEL_SUPERVISOR_PID" 2>/dev/null || true)"
        kill "$LAN_TUNNEL_SUPERVISOR_PID" >/dev/null 2>&1 || true
        if [ -n "$children" ]; then
            # shellcheck disable=SC2086
            kill $children >/dev/null 2>&1 || true
        fi
    elif [ -n "${LAN_TUNNEL_PID:-}" ]; then
        kill "$LAN_TUNNEL_PID" >/dev/null 2>&1 || true
    fi
    if [ -n "${LAN_TUNNEL_LOG:-}" ]; then
        rm -f "$LAN_TUNNEL_LOG" 2>/dev/null || true
    fi
}
