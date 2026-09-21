#!/bin/bash
#
# Builds the unified desktop base image and tools runtime from the
# docker/ build contexts using --platform linux/amd64 for seamless
# execution under macOS (Apple Silicon / Intel).
#
# Flags:
#   --only-missing   skip building an image if it already exists locally

set -e

# Find the repo root by walking up from this script until the desktop
# Dockerfiles come into view.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while [ "$root" != "/" ] && [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; do
    root="$(dirname "$root")"
done

if [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; then
    echo "ERROR: no docker/desktop.*.Dockerfile found above $(dirname "${BASH_SOURCE[0]}")." >&2
    exit 1
fi

cd "$root"

ONLY_MISSING=""

for arg in "$@"; do
    case "$arg" in
        --only-missing) ONLY_MISSING="1" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

echo "========================================"
echo " Building Decurix desktop images (macOS)"
echo "========================================"

export DOCKER_BUILDKIT=1

image_exists() {
    docker image inspect "$1" >/dev/null 2>&1
}

build_one() {
    local tag="$1" dockerfile="$2" label="$3"

    if [ -n "$ONLY_MISSING" ] && image_exists "$tag"; then
        echo "=== $label image already exists, skipping ($tag) ==="
        return
    fi

    echo ""
    echo "=== Building $label image (linux/amd64) ==="
    docker build \
        --platform linux/amd64 \
        -f "$dockerfile" \
        -t "$tag" \
        .
}

# 1. Build Base and Tools
build_one "decurix-desktop:basic" "docker/desktop.basic.Dockerfile" "Unified Desktop Base"
build_one "decurix-tools:latest" "docker/tools.Dockerfile" "Shared Tools Runtime"

# 2. Tag Developer & AI profiles directly to basic (0 disk, 0 build time)
echo ""
echo "=== Aliasing desktop profiles (developer / ai) to unified base ==="
docker tag "decurix-desktop:basic" "decurix-desktop:developer"
docker tag "decurix-desktop:basic" "decurix-desktop:ai"

# 3. Populate shared tools volume
echo ""
echo "=== Populating shared tools volume (decurix-tools) ==="
docker volume create decurix-tools >/dev/null
docker run --rm -v decurix-tools:/target decurix-tools:latest
docker volume create decurix-cache >/dev/null

# 4. Prune dangling build layers
echo ""
echo "=== Pruning dangling build layers to reclaim disk space ==="
docker image prune -f >/dev/null 2>&1 || true

echo ""
echo "=== Desktop and tools images ready ==="
docker images --filter "reference=decurix-*"
