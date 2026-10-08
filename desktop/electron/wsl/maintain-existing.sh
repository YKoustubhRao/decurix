#!/bin/sh
# Maintain only Decurix resources in a shared Docker Engine. Never stop the
# distro or daemon, unregister the distro, or prune unrelated Docker data.
set -eu
mode=$1
case "$mode" in stop|reset|delete) ;; *) exit 2;; esac
docker() { command docker --host unix:///var/run/docker.sock "$@"; }
docker version >/dev/null
workers=$(docker ps -aq --filter label=desktop-manager=true)
stack=$(docker ps -aq --filter label=com.docker.compose.project=decurix)
containers="$workers $stack"
if [ "$mode" = stop ]; then
    for container in $containers; do docker stop "$container" >/dev/null; done
    exit 0
fi
# Remember only worker home volumes actually mounted by our workers.
homes=''
for container in $workers; do
    volumes=$(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{println .Name}}{{end}}{{end}}' "$container")
    for name in $volumes; do
        case "$name" in desktop_*_home) homes="$homes $name";; esac
    done
done
for container in $containers; do docker rm -f "$container" >/dev/null; done
volumes="$homes decurix-data"
if [ "$mode" = delete ]; then volumes="$volumes decurix-tools decurix-cache decurix_ollama"; fi
for name in $volumes; do
    if docker volume inspect "$name" >/dev/null 2>&1; then docker volume rm "$name" >/dev/null; fi
done
if [ "$mode" = delete ]; then
    # Remove only our image tags; never prune a shared daemon.
    for image in decurix-desktop:basic decurix-desktop:developer decurix-desktop:ai decurix-tools:latest; do
        docker image rm "$image" >/dev/null 2>&1 || true
    done
fi
