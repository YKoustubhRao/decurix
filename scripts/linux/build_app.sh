#!/bin/bash
#
# Builds the Decurix app for this Linux computer from scratch: one AppImage,
# which you run with a double click (or from a terminal).
#
# The app is a window on the dashboard plus everything it needs to start
# Decurix by itself: the control plane, already compiled, with the Dockerfile
# and compose file it runs under (desktop/electron/main.js, startStack). A
# worker is a Linux desktop in a container, so the one thing the computer must
# have is Docker, usable by you without sudo. The app builds what is missing
# the first time (the worker desktop image takes a long while, once) and opens
# the dashboard.
#
# It leaves everything that is running alone: your desktops, their volumes
# and the records (the decurix-data volume) are the same ones
# scripts/linux/build.sh uses, and the app takes over from a control plane
# that script started. For a clean slate, use scripts/linux/rebuild.sh.
#
# Needs, to build: Go and Node.js with npm. Docker is not needed to build,
# only to run the app.
#
# scripts/mac/build_app.sh is its twin.

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for arg in "$@"; do
    echo "ERROR: unknown flag: $arg" >&2
    exit 1
done

echo "========================================"
echo " Building the Decurix app (Linux)"
echo "========================================"

for tool in go node npm; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: $tool is required to build the app." >&2
        exit 1
    fi
done

version="dev"
if command -v git >/dev/null 2>&1; then
    version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

electron="desktop/electron"
stage="$electron/build/stack"
out="dist/electron"

echo ""
echo "=== Checking the launcher ==="
node --test "$electron/main.test.js" "$electron/mobile-access.test.js"

# From scratch: nothing an earlier build staged or packaged is reused.
echo ""
echo "=== Clearing the last build ==="
rm -rf "$stage" "$out"/linux-unpacked "$out"/*.AppImage
mkdir -p "$stage"

echo ""
echo "=== Building the control plane ==="

# TheBot's sources, packed for go:embed (embed.go): without them the control
# plane could not build the worker desktop image. Run before GOOS is set -
# the generator is a program of this computer's.
go generate .

# linux/amd64 is what docker-compose.yaml runs the control plane as.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/decurix" ./cmd/decurix

# The phone build a phone downloads from the control plane when it links.
CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/decurix-android-arm64" ./cmd/decurix

# What the control plane runs under: the repo's own compose file, building
# from the Dockerfile beside it instead of from the sources.
cp "$electron/stack/Dockerfile" "$stage/Dockerfile"
sed 's#dockerfile: docker/Dockerfile#dockerfile: Dockerfile#' docker-compose.yaml > "$stage/docker-compose.yaml"
if ! grep -q 'dockerfile: Dockerfile' "$stage/docker-compose.yaml"; then
    echo "ERROR: docker-compose.yaml no longer names docker/Dockerfile; update this script." >&2
    exit 1
fi

# How the app tells this build from the one whose control plane is running.
printf '%s %s\n' "$version" "$(date -u +%Y%m%dT%H%M%SZ)" > "$stage/VERSION"

echo ""
echo "=== Packaging the app ==="

(
    cd "$electron"
    npm ci
    npm run dist:linux
)

app="$(ls "$out"/*.AppImage 2>/dev/null | head -n 1)"
if [ -z "$app" ]; then
    echo "ERROR: the build finished without an AppImage under $out." >&2
    exit 1
fi

# A file manager runs an AppImage on a double click only when it may execute.
chmod +x "$app"

echo ""
echo "========================================"
echo " The app is built"
echo "========================================"
echo ""
echo "App:  $(pwd)/$app"
echo ""
echo "Double-click it to run it (it needs FUSE, which most desktops have;"
echo "without it: \"$(pwd)/$app\" --appimage-extract-and-run)."
echo "It needs Docker; the first start builds the worker desktop, which takes a long while."
