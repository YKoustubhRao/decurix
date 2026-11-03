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

containers() {
    for filter in label=decurix=true label=desktop-manager=true label=decurix.hop=1 \
        label=decurix-practice=true label=decurix-qualification=true label=com.docker.compose.project=decurix; do
        docker ps -a --filter "$filter" --format '{{.Names}}'
    done | sort -u
}

# Volumes the workers mount (desktop_<id>_home and their other per-desktop
# volumes), read before the workers are removed.
worker_volumes() {
    for container in $(docker ps -a --filter label=desktop-manager=true --format '{{.Names}}'); do
        docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' "$container"
    done | while read -r name; do
        case "$name" in desktop_*) echo "$name";; esac
    done | sort -u
}

named_volumes() {
    for name in "$@"; do
        if docker volume inspect "$name" >/dev/null 2>&1; then echo "$name"; fi
    done
}

images() {
    for image in decurix-desktop:basic decurix-desktop:developer decurix-desktop:ai decurix-tools:latest; do
        if docker image inspect "$image" >/dev/null 2>&1; then echo "$image"; fi
    done
}

all_containers=$(containers)
homes=$(worker_volumes)

if [ "$mode" = inventory ]; then
    {
        echo "$all_containers"
        echo "$homes"
        docker volume ls -q --filter label=decurix=true
        named_volumes decurix-data decurix-tools decurix-cache decurix_ollama
        images
    } | sort -u | while read -r name; do
        [ -n "$name" ] && echo "decurix-resource $name"
    done
    exit 0
fi

if [ "$mode" = stop ]; then
    for container in $all_containers; do docker stop "$container" >/dev/null || fail "$container"; done
else
    for container in $all_containers; do
        kept "$container" && continue
        docker rm -f "$container" >/dev/null || fail "$container"
    done
    volumes="$homes $(named_volumes decurix-data)"
    if [ "$mode" = delete ]; then
        volumes="$volumes $(docker volume ls -q --filter label=decurix=true) $(named_volumes decurix-tools decurix-cache decurix_ollama)"
    fi
    for name in $(printf '%s\n' $volumes | sort -u); do
        kept "$name" && continue
        docker volume rm "$name" >/dev/null || fail "$name"
    done
    if [ "$mode" = delete ]; then
        # Only our own image tags; never prune a shared daemon.
        for image in $(images); do
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
