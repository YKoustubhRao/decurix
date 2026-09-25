#!/bin/bash
#
# Shared by scripts/linux/build.sh and scripts/linux/rebuild.sh. Meant to be
# sourced, not executed: it only defines start_keep_awake/stop_keep_awake.
#
# The control plane runs in a container, which cannot stop the host from
# going to sleep - and nothing runs on a sleeping machine: no email trigger,
# no schedule, no desktop. So while this script runs, a background loop asks
# the control plane (GET /api/keep-awake) whether anything needs the machine
# awake (a watched mailbox, a waiting schedule) and holds the host's own
# "don't idle-sleep" request only while it does - see internal/keepawake,
# which does the same thing itself when the binary runs directly on a host.
#
# It stops *idle* sleep only. Closing a laptop's lid or choosing Sleep still
# sleeps it; email that arrived meanwhile is picked up on waking.
#
# Under WSL it is Windows that sleeps (taking the Linux VM with it), so the
# request is made through powershell.exe; on other Linux, with a
# systemd-inhibit lock. Off with DECURIX_KEEP_AWAKE=0 in .env.
#
# Requires: read_env_var (lib/ollama-setup.sh), sourced by the caller first.

KEEP_AWAKE_PID=""

# keep_awake_wanted asks the control plane whether the machine should stay
# awake right now.
keep_awake_wanted() {
    curl -fsS --max-time 5 "${KEEP_AWAKE_URL:-http://127.0.0.1:8080/api/keep-awake?holder=script}" 2>/dev/null | grep -q '"wanted":true'
}

# windows_powershell prints the path of Windows PowerShell, when this is WSL.
windows_powershell() {
    if command -v powershell.exe >/dev/null 2>&1; then
        command -v powershell.exe
        return
    fi

    local p
    for p in /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe \
             /mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe; do
        if [ -x "$p" ]; then
            echo "$p"
            return
        fi
    done
}

# The Windows half: holds ES_CONTINUOUS|ES_SYSTEM_REQUIRED while told "on",
# clears it on "off", and exits (dropping the request) when its stdin closes.
# Kept identical to internal/keepawake's powerShellHelper.
KEEP_AWAKE_PS='$ErrorActionPreference = '"'"'Stop'"'"'
Add-Type -Namespace Decurix -Name Power -MemberDefinition '"'"'[DllImport("kernel32.dll")] public static extern uint SetThreadExecutionState(uint esFlags);'"'"'
while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  if ($line -eq '"'"'on'"'"') { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483649) }
  elseif ($line -eq '"'"'off'"'"') { [void][Decurix.Power]::SetThreadExecutionState([uint32]2147483648) }
}
'

# start_keep_awake starts the background loop. Sets KEEP_AWAKE_PID for
# stop_keep_awake.
start_keep_awake() {
    local setting
    setting="$(read_env_var DECURIX_KEEP_AWAKE | tr '[:upper:]' '[:lower:]')"

    case "$setting" in
        0|false|off|no)
            echo "Keep-awake is off (DECURIX_KEEP_AWAKE=0): this machine may sleep, and nothing runs while it does."
            return 0
            ;;
    esac

    local ps=""
    if grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null; then
        ps="$(windows_powershell)"

        if [ -z "$ps" ]; then
            echo "Keep-awake: powershell.exe not found, so Windows may still go to sleep."
            return 0
        fi
    elif ! command -v systemd-inhibit >/dev/null 2>&1; then
        echo "Keep-awake: systemd-inhibit is not available, so this machine may still go to sleep."
        return 0
    fi

    (
        # The caller's set -e must not end this loop over one failed probe.
        set +e

        # The loop ends with this script ($$), and whatever it holds with it.
        local held="" lock_pid=""

        if [ -n "$ps" ]; then
            local encoded
            encoded="$(printf '%s' "$KEEP_AWAKE_PS" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)"
            exec 3> >("$ps" -NoProfile -NonInteractive -EncodedCommand "$encoded" >/dev/null 2>&1)
        fi

        while kill -0 $$ 2>/dev/null; do
            # 3>&- on every child: a child holding the helper's pipe open
            # would keep the request alive after this loop is gone.
            if keep_awake_wanted 3>&-; then
                if [ -n "$ps" ]; then
                    echo on >&3 2>/dev/null
                elif [ -z "$lock_pid" ] || ! kill -0 "$lock_pid" 2>/dev/null; then
                    # tail --pid ends with this loop, so the lock cannot outlive it.
                    systemd-inhibit --what=idle:sleep --who=Decurix \
                        --why="Watching mailboxes and running schedules" --mode=block \
                        tail --pid="$BASHPID" -f /dev/null >/dev/null 2>&1 3>&- &
                    lock_pid=$!
                fi
                held=1
            elif [ -n "$held" ]; then
                if [ -n "$ps" ]; then
                    echo off >&3 2>/dev/null
                elif [ -n "$lock_pid" ]; then
                    kill "$lock_pid" 2>/dev/null
                    lock_pid=""
                fi
                held=""
            fi

            sleep 30 3>&-
        done
    ) &
    KEEP_AWAKE_PID=$!

    echo "Keep-awake: while mailboxes or schedules are active, this machine won't go to sleep on its own"
    echo "(for as long as this script runs; closing the lid still sleeps it). DECURIX_KEEP_AWAKE=0 in .env turns this off."
}

# stop_keep_awake ends the loop, which releases whatever it held.
stop_keep_awake() {
    if [ -n "$KEEP_AWAKE_PID" ]; then
        kill "$KEEP_AWAKE_PID" 2>/dev/null || true
        KEEP_AWAKE_PID=""
    fi
}
