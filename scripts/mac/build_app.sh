#!/bin/bash
#
# Builds the Decurix app for this Mac from scratch: Decurix.app, which you
# open with a double click like any other app.
#
# The app is a window on the dashboard plus everything it needs to start
# Decurix by itself: the Linux control plane, already compiled, with the
# Dockerfile and compose file it runs under (desktop/electron/main.js,
# startStack). On a Mac a worker is a Linux desktop in a container, so the
# one thing the Mac must have is Docker Desktop - the app starts it when it is
# installed and not running, builds what is missing the first time (the worker
# desktop image takes a long while, once) and opens the dashboard.
#
# It leaves everything that is running alone: your desktops, their volumes
# and the records (the decurix-data volume) are the same ones
# scripts/mac/build.sh uses, and the app takes over from a control plane that
# script started. For a clean slate, use scripts/mac/rebuild.sh.
#
# Needs, to build: Go, Node.js with npm, and Xcode's command line tools
# (codesign). Docker is not needed to build, only to run the app.
#
# Flags:
#   --no-open   do not show the finished app in the Finder
#
# Written for macOS's bash 3.2. scripts/linux/build_app.sh is its twin.

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

SHOW_APP="1"

for arg in "$@"; do
    case "$arg" in
        --no-open) SHOW_APP="" ;;
        *)
            echo "ERROR: unknown flag: $arg" >&2
            exit 1
            ;;
    esac
done

echo "========================================"
echo " Building the Decurix app (macOS)"
echo "========================================"

for tool in go node npm codesign ditto; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "ERROR: $tool is required to build the app." >&2
        case "$tool" in
            go)        echo "Install it with: brew install go" >&2 ;;
            node|npm)  echo "Install it with: brew install node" >&2 ;;
            *)         echo "Install Xcode's command line tools: xcode-select --install" >&2 ;;
        esac
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
rm -rf "$stage" "$out"/mac "$out"/mac-* "$out"/Decurix-*-mac-*.zip
mkdir -p "$stage"

echo ""
echo "=== Building the control plane ==="

# TheBot's sources, packed for go:embed (embed.go): without them the control
# plane could not build the worker desktop image. Run before GOOS is set -
# the generator is a program of this Mac's.
go generate .

# linux/amd64 is what docker-compose.yaml runs the control plane as, on
# every Mac (Docker Desktop emulates it on Apple silicon).
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
    npm run dist:mac
)

app="$(ls -d "$out"/mac*/Decurix.app 2>/dev/null | head -n 1)"
if [ -z "$app" ]; then
    echo "ERROR: the build finished without a Decurix.app under $out." >&2
    exit 1
fi

# Not signed with a developer certificate (package.json: "identity": null),
# so it is signed ad hoc here: an Apple silicon Mac refuses to run an app with
# no signature at all. Built on this Mac it opens with a double click; sent
# to another Mac, that one asks its owner to allow it the first time
# (right-click, Open).
codesign --force --deep --sign - "$app"

archive="$out/Decurix-$version-mac-$(uname -m).zip"
ditto -c -k --keepParent "$app" "$archive"

echo ""
echo "========================================"
echo " The app is built"
echo "========================================"
echo ""
echo "App:      $(pwd)/$app"
echo "Archive:  $(pwd)/$archive"
echo ""
echo "Double-click Decurix.app to run it, or drag it into Applications first."
echo "It needs Docker Desktop; the first start builds the worker desktop, which takes a long while."

if [ -n "$SHOW_APP" ]; then
    open -R "$app"
fi
