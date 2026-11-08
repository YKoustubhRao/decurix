#!/bin/sh
# The screen rows of bench/security/boundary.sh (sourced by it; it defines RUN, BROKER, HERE, pass, fail, check,
# as_root). Each row is read-only and runs as the worker (screen_probes.py): nothing clicks, types or sends a key. The
# one thing done to the desktop first is `gui open` of a page of the bench's own, so the browser runs (it is not
# started at boot) and has a profile to look for.
#
# In broker mode (--broker) a row that is not closed is a FAIL; in the default mode, where these paths are open by
# design, it is OPEN and not counted (docs/gate-boundary.md, "Next: the input broker").

screen_row() { # screen_row NAME ROW [ARGS...]: one probe as the worker
    name=$1
    shift
    out=$(docker exec -i -u 1001:1001 "$RUN" /usr/bin/env -i PATH=/usr/bin:/bin HOME=/home/headless \
        /usr/bin/python3 -I - "$@" < "$HERE/screen_probes.py" 2>/dev/null | tail -1)
    if [ "$out" = ok ]; then pass "screen: $name"
    elif [ "$BROKER" = 1 ]; then fail "screen: $name: ${out:-no answer}"
    else echo "OPEN  screen: $name: ${out:-no answer} (default mode: open by design)"; fi
}

screen_probes() {
    # The desktop's display and the worker's session: up before anything is asked of them.
    i=0
    while [ $i -lt 60 ] && ! docker exec -u 0 "$RUN" /usr/bin/test -S /tmp/.X11-unix/X1 2>/dev/null; do
        sleep 2
        i=$((i + 1))
    done
    sleep 5
    # A page of the bench's own, opened as an engine opens one: through `gui`, as the worker (no input is sent).
    docker exec -i -u 1001:1001 "$RUN" /usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/home/headless \
        DISPLAY=:1 DECURIX_SCREEN_USER="$([ "$BROKER" = 1 ] && echo 1)" /bin/sh -c '
        printf "<html><head><title>Boundary page</title></head><body>boundary</body></html>" > /tmp/boundary-page.html
        chmod 644 /tmp/boundary-page.html
        gui open file:///tmp/boundary-page.html' > /dev/null 2>&1
    # The profile's cookie store, wherever the browser keeps it (found as root: the worker may not list it).
    i=0
    stores=""
    while [ $i -lt 15 ] && [ -z "$stores" ]; do
        stores=$(as_root <<'EOF'
find /home/headless /var/lib/decurix-screen -xdev -name Cookies -path '*Default*' 2>/dev/null | tr '\n' ' '
EOF
)
        [ -n "$stores" ] || sleep 2
        i=$((i + 1))
    done
    obfuscated=$(as_root <<'EOF'
printf 'benchpw1\n' | /usr/bin/vncpasswd -f | od -An -tx1 | tr -d ' \n'
EOF
)

    screen_row "the worker's X client has no XTEST on the display" xtest
    screen_row "the screen's VNC password is in no file or environment the worker can read" vnc "$obfuscated" benchpw1
    screen_row "no DevTools endpoint of the browser answers the worker" devtools
    # shellcheck disable=SC2086 # stores is a list of paths with no spaces
    screen_row "the worker cannot read the browser profile's cookie store" cookies $stores
    screen_row "the screen side's processes are not the worker's, and their environments are unreadable" environ
}
