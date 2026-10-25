#!/usr/bin/env bash
#
# Builds Brainiac and brings it up on this machine: it measures the machine,
# chooses the best open models that fit for each kind of work, shows the
# choice and why, installs it after a yes, and serves one OpenAI-compatible
# endpoint. The same script runs on a laptop, a GPU box or a server; only what
# it measures differs. Twin: scripts/build.ps1 (Windows, keep both in step).
#
#   scripts/build.sh              build, then `brainiac up` (plan, ask, install, serve)
#   scripts/build.sh --yes        the same without the question
#   scripts/build.sh plan         build, then only show the plan
#   scripts/build.sh census       build, then only measure
#
# Anything after the command goes to it (`scripts/build.sh up --licenses permissive,restricted`).
# Models and state live in BRAINIAC_HOME (default ~/.brainiac), never in the repo.

set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Go: the one on PATH, else the usual install path, else a golang container.
GO=""
if command -v go >/dev/null 2>&1; then
    GO="go"
elif [ -x /usr/local/go/bin/go ]; then
    GO="/usr/local/go/bin/go"
fi

mkdir -p bin
VERSION="$(git describe --always --dirty 2>/dev/null || echo dev)"
echo "Building brainiac ($VERSION)..."
if [ -n "$GO" ]; then
    # GOTOOLCHAIN=auto fetches the Go that go.mod names when the installed one is older.
    GOTOOLCHAIN=auto "$GO" build -trimpath -ldflags "-X main.version=$VERSION" -o bin/brainiac ./cmd/brainiac
elif command -v docker >/dev/null 2>&1; then
    docker run --rm -v "$PWD":/src -w /src -e GOTOOLCHAIN=auto golang:1.27 \
        go build -buildvcs=false -trimpath -ldflags "-X main.version=$VERSION" -o bin/brainiac ./cmd/brainiac
else
    echo "brainiac: building needs Go (https://go.dev/dl) or Docker" >&2
    exit 1
fi

CMD="up"
case "${1:-}" in
    census|plan|pull|serve|up|status|version)
        CMD="$1"
        shift
        ;;
esac

exec ./bin/brainiac "$CMD" "$@"
