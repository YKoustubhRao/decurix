#!/bin/bash
#
# Builds the three desktop images (basic / developer / ai) from the
# docker/desktop.*.Dockerfile build contexts using --platform linux/amd64
# for seamless execution under macOS (Apple Silicon / Intel).

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

echo "========================================"
echo " Building Flowera desktop images (macOS)"
echo "========================================"

echo ""
echo "=== Building Basic desktop image (linux/amd64) ==="
docker build \
    --platform linux/amd64 \
    -f docker/desktop.basic.Dockerfile \
    -t flowera-desktop:basic \
    .

echo ""
echo "=== Building Developer desktop image (linux/amd64) ==="
docker build \
    --platform linux/amd64 \
    -f docker/desktop.developer.Dockerfile \
    -t flowera-desktop:developer \
    .

echo ""
echo "=== Building AI Developer desktop image (linux/amd64) ==="
docker build \
    --platform linux/amd64 \
    -f docker/desktop.ai.Dockerfile \
    -t flowera-desktop:ai \
    .

echo ""
echo "=== Desktop images built ==="
docker images "flowera-desktop"
