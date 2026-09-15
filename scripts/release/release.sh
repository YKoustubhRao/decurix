#!/bin/bash
#
# Cross-compiles the Flowera control plane into standalone executables and
# packages each one as a downloadable archive.
#
# The web templates (web/embed.go) and the desktop image build context
# (embed.go) are compiled into the binary, so each executable is fully
# self-contained: it needs only Docker on the host, and builds its own desktop
# images via `flowera --build-images`. The archives add nothing it depends on,
# just a README and an example config.
#
# Usage:
#   ./scripts/release/release.sh                  # every default target
#   ./scripts/release/release.sh linux/amd64      # just one
#   ./scripts/release/release.sh --version v0.2.0
#   ./scripts/release/release.sh --output /tmp/out
#   ./scripts/release/release.sh --binaries-only  # skip the archives

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

DEFAULT_TARGETS=(
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "windows/arm64"
    "darwin/amd64"
    "darwin/arm64"
)

VERSION=""
OUTPUT_DIR="dist"
BINARIES_ONLY=0
TARGETS=()

while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="$2"
            shift 2
            ;;
        --output)
            OUTPUT_DIR="$2"
            shift 2
            ;;
        --binaries-only)
            BINARIES_ONLY=1
            shift
            ;;
        -h|--help)
            sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        -*)
            echo "ERROR: unknown option $1" >&2
            exit 1
            ;;
        *)
            TARGETS+=("$1")
            shift
            ;;
    esac
done

if [ ${#TARGETS[@]} -eq 0 ]; then
    TARGETS=("${DEFAULT_TARGETS[@]}")
fi

# The output directory gets wiped before each run, so refuse an empty value.
if [ -z "$OUTPUT_DIR" ]; then
    echo "ERROR: --output needs a directory." >&2
    exit 1
fi

if ! command -v go >/dev/null 2>&1; then
    echo "ERROR: Go is not installed (needed to build the executables)." >&2
    exit 1
fi

# Version defaults to the git description, so a binary can always be traced
# back to the commit it came from. Falls back to "dev" outside a checkout.
if [ -z "$VERSION" ]; then
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

echo "========================================"
echo " Building Flowera $VERSION"
echo "========================================"
echo ""
echo "Go:      $(go version)"
echo "Output:  $OUTPUT_DIR/"
echo "Targets: ${TARGETS[*]}"

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

STAGE_ROOT="$(mktemp -d)"
trap 'rm -rf "$STAGE_ROOT"' EXIT

# stageBundle writes the human-facing extras that sit next to the binary in an
# archive: an example config and a README for this OS ($2). The binary needs
# neither — it embeds the web templates and the desktop build context — so
# there is deliberately no docker/, agent/ or scripts/ in a bundle.
stageBundle() {
    local dest="$1"
    local goos="$2"
    local run="./flowera"
    local copyEnv="cp .env.example .env"
    local dataDir="~/.config/flowera"

    if [ "$goos" = "windows" ]; then
        run=".\\flowera.exe"
        copyEnv="copy .env.example .env"
        dataDir="%AppData%\\flowera"
    elif [ "$goos" = "darwin" ]; then
        dataDir="~/Library/Application Support/flowera"
    fi

    cp .env.example "$dest/.env.example"

    cat > "$dest/README.txt" <<EOF
Flowera $VERSION
================

This bundle is the Flowera control plane as a single self-contained
executable. It needs no runtime, no repo checkout and no files beside it --
the dashboard and the desktop image build context are compiled in.

Requirements
------------
  * Docker (running, and reachable by the user running Flowera)
  * ~15 GB of disk for the three desktop images

Setup
-----
  1. Build the desktop images (one time, and again whenever you upgrade):

         $run --build-images

     This builds flowera-desktop:basic, :developer and :ai straight from the
     executable. It takes a while the first time. Add --rebuild to rebuild
     images that already exist.

  2. Configure (optional -- every setting has a working default):

         $copyEnv

     Edit .env -- set DEFAULT_PASSWORD (the VNC password for new desktops).
     Flowera reads .env from the working directory, or from the folder the
     executable lives in. Plain environment variables work too. The Claude
     harness needs no .env entry -- log in from the dashboard instead, next
     to the "Claude Code" option when creating a desktop.

     Workers think with a model served by Ollama (https://ollama.com).
     OLLAMA_URL picks the server (default: the Ollama on this machine) and
     OLLAMA_MODEL the model (default: qwen2.5-coder:3b). Pull it first:

         ollama pull qwen2.5-coder:3b
         ollama pull nomic-embed-text   (only the thebot harness needs this)

     On Linux, Ollama must listen on more than 127.0.0.1 for desktop
     containers to reach it: set OLLAMA_HOST=0.0.0.0 on its service.

  3. Run:

         $run

  4. Open http://localhost:8080

Notes
-----
  * Flowera listens on port 8080 and talks to Docker through the local Docker
    socket / named pipe. On Linux the user must be able to reach
    /var/run/docker.sock (usually: be in the "docker" group).
  * Task records, chats, help requests, worker memory and action screenshots
    are kept in flowera.db under $dataDir, so they survive a
    restart. Set FLOWERA_DATA_DIR to keep them somewhere else.
  * $run --version prints the build this bundle was cut from.
  * Full docs: https://github.com/YKoustubhRao/flowera
EOF
}

# archiveBundle packs the staged directory $2 (named $1) into $OUTPUT_DIR,
# as a .zip for Windows and a .tar.gz everywhere else.
archiveBundle() {
    local name="$1"
    local stage="$2"
    local goos="$3"
    local out
    out="$(cd "$OUTPUT_DIR" && pwd)"

    if [ "$goos" = "windows" ] && command -v zip >/dev/null 2>&1; then
        (cd "$stage/.." && zip -qr "$out/$name.zip" "$name")
        echo "  archive: $OUTPUT_DIR/$name.zip"
        return
    fi

    if [ "$goos" = "windows" ]; then
        echo "  note: 'zip' not installed, falling back to .tar.gz"
    fi

    tar -czf "$out/$name.tar.gz" -C "$stage/.." "$name"
    echo "  archive: $OUTPUT_DIR/$name.tar.gz"
}

for target in "${TARGETS[@]}"; do
    goos="${target%%/*}"
    goarch="${target##*/}"

    if [ "$goos" = "$target" ] || [ -z "$goarch" ]; then
        echo "ERROR: target '$target' is not in os/arch form (e.g. linux/amd64)." >&2
        exit 1
    fi

    name="flowera_${VERSION}_${goos}_${goarch}"
    ext=""

    if [ "$goos" = "windows" ]; then
        ext=".exe"
    fi

    echo ""
    echo "=== $goos/$goarch ==="

    # CGO off keeps the binary static, so it runs on any machine of that
    # os/arch without matching system libraries. -s -w drops the debug
    # symbols; -X stamps main.version.
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build \
        -trimpath \
        -ldflags "-s -w -X main.version=$VERSION" \
        -o "$OUTPUT_DIR/${name}${ext}" \
        ./cmd/flowera

    echo "  binary:  $OUTPUT_DIR/${name}${ext}"

    if [ "$BINARIES_ONLY" -eq 1 ]; then
        continue
    fi

    stage="$STAGE_ROOT/$name"
    mkdir -p "$stage"

    cp "$OUTPUT_DIR/${name}${ext}" "$stage/flowera${ext}"
    chmod +x "$stage/flowera${ext}"
    stageBundle "$stage" "$goos"
    archiveBundle "$name" "$stage" "$goos"
done

echo ""
echo "=== Checksums ==="

(
    cd "$OUTPUT_DIR"

    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum flowera_* > SHA256SUMS
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 flowera_* > SHA256SUMS
    else
        echo "warning: no sha256sum/shasum found, skipping SHA256SUMS" >&2
    fi
)

echo ""
echo "========================================"
echo " Flowera $VERSION built"
echo "========================================"
echo ""

ls -lh "$OUTPUT_DIR"
