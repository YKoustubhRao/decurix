#!/bin/bash
#
# One-shot script: takes a fresh macOS checkout all the way to a running Flowera
# dashboard. Runs scripts/mac/setup.sh (if prerequisites are missing) and then
# scripts/mac/build.sh.

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "========================================"
echo " Flowera macOS quickstart"
echo "========================================"
echo ""

needs_setup=false

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    needs_setup=true
fi

if ! command -v ollama >/dev/null 2>&1; then
    needs_setup=true
fi

if [ "$needs_setup" = true ]; then
    echo "Running macOS host setup..."
    ./scripts/mac/setup.sh
else
    echo "Docker and Ollama found and running - skipping host setup."
fi

echo ""
./scripts/mac/build.sh
