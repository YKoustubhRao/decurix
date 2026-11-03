#!/bin/sh
# Stage only Decurix's own files in an existing WSL distro. Never install
# packages or change this distro's WSL settings or Docker daemon settings.
set -eu

stack_src=$1
env_file=$2
stack=/var/lib/decurix/stack

test -S /var/run/docker.sock
test -d /mnt/c
command -v bash >/dev/null
command -v wslpath >/dev/null
docker --host unix:///var/run/docker.sock version >/dev/null
docker --host unix:///var/run/docker.sock compose version >/dev/null
os=$(docker --host unix:///var/run/docker.sock info --format '{{.OperatingSystem}}')
case "$os" in *[Dd]ocker" "[Dd]esktop*) echo 'Docker Desktop is not a local WSL Docker Engine' >&2; exit 1;; esac

mkdir -p "$stack"
if [ "$(cat "$stack/VERSION" 2>/dev/null || true)" != "$(cat "$stack_src/VERSION")" ]; then
    for name in decurix decurix-android-arm64 Dockerfile docker-compose.yaml; do
        cp -f "$stack_src/$name" "$stack/$name"
    done
    cp -f "$stack_src/VERSION" "$stack/VERSION"
fi
# .env holds the person's keys: readable by root only, from the moment it exists.
(umask 077 && tr -d '\r' < "$env_file" > "$stack/.env")
chmod 600 "$stack/.env"
echo 'decurix-setup: existing WSL runtime ready'
