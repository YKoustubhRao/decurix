#!/bin/bash
#
# Checks the restart loop of the public link's quick tunnel
# (scripts/linux/lib/lan-tunnel.sh, which the Mac scripts source too; B-29):
# a tunnel that exits is started again after 1 s, then 2 s, up to the cap;
# the loop stops when the public link is turned off or the script that
# started it is gone, and never starts the tunnel while the port is not the
# access-code page. A stub stands in for cloudflared, and the loop's waits
# are the real ones (a few seconds). Run from anywhere:
#
#   bash scripts/testing/lan-tunnel.test.sh

set -u
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
failures=0

check() {
    if [ "$2" = "$3" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1: got [$2], want [$3]"
        failures=$((failures + 1))
    fi
}

has() { grep -qF -- "$2" "$1" && echo yes || echo no; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work" || exit 1

# shellcheck source=../linux/lib/lan-tunnel.sh
source "$repo/scripts/linux/lib/lan-tunnel.sh"

# The checks the loop repeats before a start, answered from files the test controls.
public_link_enabled() { [ -f "$work/on" ]; }
gated_port_ok() { [ -f "$work/gated" ]; }
named_tunnel_url() { :; }

printf '#!/bin/bash\necho run >> "%s/runs"\nexit 3\n' "$work" > "$work/stub"
chmod +x "$work/stub"
LAN_TUNNEL_COMMAND="$work/stub"
LAN_TUNNEL_FIRST_DELAY=1
LAN_TUNNEL_MAX_DELAY=2

runs() { [ -f "$work/runs" ] && wc -l < "$work/runs" | tr -d ' ' || echo 0; }

# waits up to $2 seconds for pid $1 to end; says whether it did.
ended_within() {
    local i
    for i in $(seq 1 $(( $2 * 10 ))); do
        kill -0 "$1" 2>/dev/null || { echo yes; return; }
        sleep 0.1
    done
    echo no
}

# 1. Exits are restarted with a doubling, capped wait; turning the link off stops it.
touch "$work/on" "$work/gated"
supervise_lan_tunnel 8090 "$work/log" "$$" > "$work/out" 2>&1 &
sup=$!
sleep 4.5 # starts at 0 s, 1 s and 3 s; the next would be at 5 s
rm -f "$work/on"
check "loop stops when the public link is turned off" "$(ended_within "$sup" 5)" yes
check "three starts before it was turned off" "$(runs)" 3
check "first wait is 1 s, with the exit code" "$(has "$work/out" "exited with code 3 after 0s; starting it again in 1s.")" yes
check "the wait doubles" "$(has "$work/out" "starting it again in 2s.")" yes
check "the wait is capped" "$(has "$work/out" "in 4s")" no
check "says it is turned off" "$(has "$work/out" "turned off - the tunnel is not started again.")" yes

# 2. A port that is not the access-code page is never tunnelled; the loop waits and checks again.
rm -f "$work/runs" "$work/gated"
touch "$work/on"
supervise_lan_tunnel 8090 "$work/log" "$$" > "$work/out" 2>&1 &
sup=$!
sleep 2
rm -f "$work/on"
check "loop stops (gate closed)" "$(ended_within "$sup" 5)" yes
check "never started again while the gate is closed" "$(runs)" 1
check "says the port is not the gate" "$(has "$work/out" "is not serving the access-code page; trying again in 2s.")" yes

# 3. The loop ends with the script that started it.
rm -f "$work/runs"
touch "$work/on" "$work/gated"
sleep 0.5 &
owner=$!
supervise_lan_tunnel 8090 "$work/log" "$owner" > "$work/out" 2>&1 &
sup=$!
check "loop stops when its script is gone" "$(ended_within "$sup" 5)" yes
check "not started again after its script is gone" "$(runs)" 1

if [ "$failures" -gt 0 ]; then
    echo "$failures check(s) failed"
    exit 1
fi
echo "all checks passed"
