# apk_safe: apk that never waits forever and never depends on one mirror. Sourced by setup.sh; the Dockerfiles that
# install with apk inline the same loop over the same list (internal/desktop/dockerfile_downloads_test.go keeps the
# lists equal).
#
# Why: on the owner's network, dl-cdn.alpinelinux.org (Fastly) served main/APKINDEX in a second while its
# community/APKINDEX opened and then sent nothing, every time, and apk has no timeout of its own; a first start sat
# at `apk update` for good (2026-11-04). The same index from mirrors.edge.kernel.org came in 15 s.
#
#   apk_safe SECONDS apk-arguments...
#
# Runs apk under `timeout SECONDS` against each mirror in turn, rewriting /etc/apk/repositories to it (same branch,
# same repositories), and keeps the first mirror that works first for the next call. While apk runs it prints a line
# every 30 s, so the app's watchdog sees a bounded wait as alive. It fails with a clear message once every mirror has
# failed.

# Alpine's official mirrors, in the order tried.
APK_MIRRORS="https://dl-cdn.alpinelinux.org/alpine https://mirrors.edge.kernel.org/alpine https://uk.alpinelinux.org/alpine"
APK_REPOSITORIES=${APK_REPOSITORIES:-/etc/apk/repositories}
APK_HEARTBEAT=${APK_HEARTBEAT:-30}

# apk_use_mirror points every repository line at mirror, keeping its branch and repository (v3.24/main, edge/community).
apk_use_mirror() {
    sed -i -E "s#^[a-z]+://[^ ]*(/(v[0-9]+\.[0-9]+|edge)/)#$1\1#" "$APK_REPOSITORIES"
}

# apk_bounded runs apk "$@" under timeout $1, saying every APK_HEARTBEAT seconds that it is still running.
apk_bounded() {
    apk_secs=$1
    shift
    timeout "$apk_secs" apk "$@" &
    apk_pid=$!
    apk_waited=0
    while kill -0 "$apk_pid" 2>/dev/null; do
        sleep 1
        apk_waited=$((apk_waited + 1))
        if [ $((apk_waited % APK_HEARTBEAT)) -eq 0 ] && kill -0 "$apk_pid" 2>/dev/null; then
            echo "apk-safe: apk $1 still running (${apk_waited} s of at most ${apk_secs} s)"
        fi
    done
    wait "$apk_pid"
}

apk_safe() {
    apk_limit=$1
    shift
    for apk_mirror in $APK_MIRRORS; do
        apk_use_mirror "$apk_mirror"
        if apk_bounded "$apk_limit" "$@"; then
            # The mirror that worked is tried first next time.
            apk_rest=""
            for m in $APK_MIRRORS; do [ "$m" = "$apk_mirror" ] || apk_rest="$apk_rest $m"; done
            APK_MIRRORS="$apk_mirror$apk_rest"
            return 0
        fi
        echo "apk-safe: apk $* failed or did not finish within ${apk_limit} s from $apk_mirror; trying the next mirror"
    done
    echo "apk-safe: apk $* failed from every Alpine mirror ($APK_MIRRORS). Check this computer's internet connection, then Retry." >&2
    return 1
}
