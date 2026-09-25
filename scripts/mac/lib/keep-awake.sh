#!/bin/bash
#
# Shared by scripts/mac/build.sh and scripts/mac/rebuild.sh. Meant to be
# sourced, not executed: it only defines start_keep_awake/stop_keep_awake.
#
# The control plane runs in a container (in Docker Desktop's VM), which
# cannot stop the Mac from going to sleep - and nothing runs on a sleeping
# machine: no email trigger, no schedule, no desktop. So while this script
# runs, a background loop asks the control plane (GET /api/keep-awake)
# whether anything needs the Mac awake (a watched mailbox, a waiting
# schedule) and holds a caffeinate assertion only while it does - see
# internal/keepawake, which does the same itself when the binary runs
# directly on a host.
#
# caffeinate -i stops idle sleep; -s also stops system sleep on mains power.
# Closing the lid still sleeps a MacBook (unless it is in clamshell mode with
# a display attached); email that arrived meanwhile is picked up on waking.
# Off with DECURIX_KEEP_AWAKE=0 in .env.
#
# Requires: read_env_var (lib/ollama-setup.sh), sourced by the caller first.
# Written for macOS's bash 3.2: no ${x,,}, no $BASHPID.

KEEP_AWAKE_PID=""

# keep_awake_wanted asks the control plane whether the Mac should stay awake
# right now.
keep_awake_wanted() {
    curl -fsS --max-time 5 "${KEEP_AWAKE_URL:-http://127.0.0.1:8080/api/keep-awake?holder=script}" 2>/dev/null | grep -q '"wanted":true'
}

# start_keep_awake starts the background loop. Sets KEEP_AWAKE_PID for
# stop_keep_awake.
start_keep_awake() {
    local setting
    setting="$(read_env_var DECURIX_KEEP_AWAKE | tr '[:upper:]' '[:lower:]')"

    case "$setting" in
        0|false|off|no)
            echo "Keep-awake is off (DECURIX_KEEP_AWAKE=0): this Mac may sleep, and nothing runs while it does."
            return 0
            ;;
    esac

    if ! command -v caffeinate >/dev/null 2>&1; then
        echo "Keep-awake: caffeinate is not available, so this Mac may still go to sleep."
        return 0
    fi

    local script_pid=$$

    (
        # The caller's set -e must not end this loop over one failed probe.
        set +e

        local caffeinate_pid=""

        # -w ends caffeinate with this script, however the script ends.
        while kill -0 "$script_pid" 2>/dev/null; do
            if keep_awake_wanted; then
                if [ -z "$caffeinate_pid" ] || ! kill -0 "$caffeinate_pid" 2>/dev/null; then
                    caffeinate -i -s -w "$script_pid" &
                    caffeinate_pid=$!
                fi
            elif [ -n "$caffeinate_pid" ]; then
                kill "$caffeinate_pid" 2>/dev/null
                caffeinate_pid=""
            fi

            sleep 30
        done

        [ -n "$caffeinate_pid" ] && kill "$caffeinate_pid" 2>/dev/null
    ) &
    KEEP_AWAKE_PID=$!

    echo "Keep-awake: while mailboxes or schedules are active, this Mac won't go to sleep on its own"
    echo "(for as long as this script runs; closing the lid still sleeps it). DECURIX_KEEP_AWAKE=0 in .env turns this off."
}

# stop_keep_awake ends the loop and the caffeinate assertion it holds.
stop_keep_awake() {
    if [ -n "$KEEP_AWAKE_PID" ]; then
        pkill -P "$KEEP_AWAKE_PID" caffeinate 2>/dev/null || true
        kill "$KEEP_AWAKE_PID" 2>/dev/null || true
        KEEP_AWAKE_PID=""
    fi
}
