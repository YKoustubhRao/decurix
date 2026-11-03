#!/bin/sh
# Maintain only Decurix resources in a shared Docker Engine. Never stop the
# distro or daemon, unregister the distro, or prune unrelated Docker data.
#
#   maintain-existing.sh stop|reset|delete [name-to-keep ...]
#   maintain-existing.sh inventory
#
# Decurix's resources are found by the labels Decurix puts on them
# (decurix=true; desktop-manager=true on desktops; the hop, practice and
# qualification labels; the compose project "decurix") and by the names of its
# volumes and images. A name to keep is never removed: the app passes what
# `inventory` listed before it first used this engine (saved in
# windows-runtime.json), so a checkout's own stack, workers or data that were
# here first survive an uninstall.
#
# A step that fails is counted, not fatal: the rest still runs, and the script
# ends non-zero naming what is left, so a caller never reports a clean slate
# that is not one.
#
# Every listing must answer. `inventory` prints nothing and exits 1 if any one
# fails: a partial list saved as "what was here first" would turn into removing
# another project's containers or volumes at uninstall (A-04). A name is
# decided from one full listing, never from an inspect that fails, since a
# failed inspect cannot tell "absent" from "the engine did not answer".
set -u
mode=${1:-}
[ $# -gt 0 ] && shift
case "$mode" in stop|reset|delete|inventory) ;; *) echo "usage: maintain-existing.sh stop|reset|delete|inventory [name-to-keep ...]" >&2; exit 2;; esac
for name in "$@"; do
    case "$name" in ''|*[!A-Za-z0-9_.:/-]*) echo "not a resource name: $name" >&2; exit 2;; esac
done
keep=" $* "
kept() { case "$keep" in *" $1 "*) return 0;; esac; return 1; }
docker() { command docker --host unix:///var/run/docker.sock "$@"; }
docker version >/dev/null || { echo 'the Docker Engine did not answer' >&2; exit 1; }

failed=''
fail() { failed="$failed $1"; }

# Each listing returns non-zero when a docker call in it fails (no pipeline
# hides a failure: POSIX sh has no pipefail).
containers() {
    for filter in label=decurix=true label=desktop-manager=true label=decurix.hop=1 \
        label=decurix-practice=true label=decurix-qualification=true label=com.docker.compose.project=decurix; do
        docker ps -a --filter "$filter" --format '{{.Names}}' || return 1
    done
}

# Volumes the workers mount (desktop_<id>_home and their other per-desktop
# volumes), read before the workers are removed.
worker_volumes() {
    workers=$(docker ps -a --filter label=desktop-manager=true --format '{{.Names}}') || return 1
    for container in $workers; do
        mounts=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' "$container") || return 1
        for name in $mounts; do
            case "$name" in desktop_*) echo "$name";; esac
        done
    done
}

# pick prints each of the wanted names (after the list) that appears in list.
pick() {
    list=$1
    shift
    for name in "$@"; do
        case "
$list
" in *"
$name
"*) echo "$name";; esac
    done
}

named_volumes() {
    all=$(docker volume ls -q) || return 1
    pick "$all" "$@"
}

labelled_volumes() { docker volume ls -q --filter label=decurix=true; }

images() {
    all=$(docker image ls --format '{{.Repository}}:{{.Tag}}') || return 1
    pick "$all" decurix-desktop:basic decurix-desktop:developer decurix-desktop:ai decurix-tools:latest
}

unique() { printf '%s\n' $1 | sort -u; }

listing_failed=''
all_containers=$(containers) || listing_failed=1
all_containers=$(unique "$all_containers")
homes=$(worker_volumes) || listing_failed=1
homes=$(unique "$homes")

if [ "$mode" = inventory ]; then
    labelled=$(labelled_volumes) || listing_failed=1
    named=$(named_volumes decurix-data decurix-tools decurix-cache decurix_ollama) || listing_failed=1
    ours=$(images) || listing_failed=1
    if [ -n "$listing_failed" ]; then
        echo 'the Docker Engine did not answer every listing, so nothing is listed' >&2
        exit 1
    fi
    for name in $(unique "$all_containers $homes $labelled $named $ours"); do
        echo "decurix-resource $name"
    done
    exit 0
fi
[ -n "$listing_failed" ] && fail 'a listing (some resources may be left)'

if [ "$mode" = stop ]; then
    for container in $all_containers; do docker stop "$container" >/dev/null || fail "$container"; done
else
    for container in $all_containers; do
        kept "$container" && continue
        docker rm -f "$container" >/dev/null || fail "$container"
    done
    data=$(named_volumes decurix-data) || fail 'the volume listing'
    volumes="$homes $data"
    if [ "$mode" = delete ]; then
        labelled=$(labelled_volumes) || fail 'the labelled volume listing'
        named=$(named_volumes decurix-tools decurix-cache decurix_ollama) || fail 'the volume listing'
        volumes="$volumes $labelled $named"
    fi
    for name in $(printf '%s\n' $volumes | sort -u); do
        kept "$name" && continue
        docker volume rm "$name" >/dev/null || fail "$name"
    done
    if [ "$mode" = delete ]; then
        # Only our own image tags; never prune a shared daemon.
        ours=$(images) || fail 'the image listing'
        for image in $ours; do
            kept "$image" && continue
            docker image rm "$image" >/dev/null || fail "$image"
        done
        for net in $( (docker network ls --filter label=decurix=true --format '{{.Name}}'; \
            docker network ls --filter label=decurix-practice=true --format '{{.Name}}') | sort -u); do
            kept "$net" && continue
            docker network rm "$net" >/dev/null || fail "$net"
        done
    fi
fi

if [ -n "$failed" ]; then
    echo "could not $mode:$failed" >&2
    exit 1
fi
