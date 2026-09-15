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

build_one() {
    local tag="$1" dockerfile="$2" label="$3"

    if [ -n "$ONLY_MISSING" ] && docker image inspect "$tag" >/dev/null 2>&1; then
        echo ""
        echo "=== $label desktop image already exists, skipping ($tag) ==="
        return 0
    fi

    echo ""
    echo "=== Building $label desktop image ==="

    docker build \
        -f "$dockerfile" \
        -t "$tag" \
        .
}

build_one flowera-desktop:basic docker/desktop.basic.Dockerfile "Basic"
build_one flowera-desktop:developer docker/desktop.developer.Dockerfile "Developer"
build_one flowera-desktop:ai docker/desktop.ai.Dockerfile "AI Developer"

echo ""
echo "=== Desktop images built ==="

docker images "flowera-desktop"
