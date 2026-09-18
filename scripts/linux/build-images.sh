#!/bin/bash
#
# Builds the three desktop images (basic / developer / ai) from the
# docker/desktop.*.Dockerfile build contexts. Called by scripts/linux/build.sh
# and scripts/linux/rebuild.sh.
#
# This is the repo-checkout path. A downloaded release binary does not need it:
# it embeds the same build context and builds the images itself via
# `flowera --build-images`.
#
# Flags:
#   --only-missing  skip an image whose tag already exists (build.sh's "just
#                    open the app" - don't pay for a rebuild nobody asked for)

set -e

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

# Find the repo root by walking up from this script until the desktop
# Dockerfiles come into view, so it runs correctly from any directory.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while [ "$root" != "/" ] && [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; do
    root="$(dirname "$root")"
done

if [ ! -f "$root/docker/desktop.basic.Dockerfile" ]; then
    echo "ERROR: no docker/desktop.*.Dockerfile found above $(dirname "${BASH_SOURCE[0]}")." >&2
    exit 1
fi

cd "$root"

echo "========================================"
echo " Building Flowera desktop images"
echo "========================================"

if [ ! -f ~/.docker/cli-plugins/docker-buildx ] && [ -f /usr/libexec/docker/cli-plugins/docker-buildx ]; then
    mkdir -p ~/.docker/cli-plugins
    ln -sf /usr/libexec/docker/cli-plugins/docker-buildx ~/.docker/cli-plugins/docker-buildx 2>/dev/null || true
fi

# Ensure broken Windows desktop credential helper does not fail Linux builds
if [ -f ~/.docker/config.json ] && grep -q '"desktop\.exe"' ~/.docker/config.json 2>/dev/null; then
    if ! command -v docker-credential-desktop.exe >/dev/null 2>&1; then
        sed -i '/desktop\.exe/d' ~/.docker/config.json 2>/dev/null || true
    fi
fi

export DOCKER_BUILDKIT=1

build_one() {
    local tag="$1" dockerfile="$2" label="$3"

    if [ -n "$ONLY_MISSING" ] && docker image inspect "$tag" >/dev/null 2>&1; then
        echo ""
        echo "=== $label image already exists, skipping ($tag) ==="
        return 0
    fi

    echo ""
    echo "=== Building $label image ==="

    docker build \
        -f "$dockerfile" \
        -t "$tag" \
        .
}

build_one flowera-desktop:basic docker/desktop.basic.Dockerfile "Unified Desktop Base"
build_one flowera-tools:latest docker/tools.Dockerfile "Shared Tools Runtime"

echo ""
echo "=== Aliasing desktop profiles (developer / ai) to unified base ==="
docker tag flowera-desktop:basic flowera-desktop:developer
docker tag flowera-desktop:basic flowera-desktop:ai

echo ""
echo "=== Populating shared tools volume (flowera-tools) ==="
docker volume create flowera-tools >/dev/null
docker run --rm -v flowera-tools:/target flowera-tools:latest
docker volume create flowera-cache >/dev/null

echo ""
echo "=== Pruning dangling build layers to reclaim disk space ==="
docker image prune -f >/dev/null 2>&1 || true

echo ""
echo "=== Desktop and tools images ready ==="

docker images --filter "reference=flowera-*"

