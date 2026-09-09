#!/bin/bash

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

./scripts/linux/build-images.sh

echo ""
echo "=== Stopping Flowera ==="

docker compose down

echo ""
echo "=== Removing old desktop containers ==="

docker ps -a \
    --filter "name=desktop_" \
    --format "{{.ID}}" \
    | xargs -r docker rm -f

echo ""
echo "=== Removing old desktop volumes ==="

docker volume ls \
    --filter "name=desktop_" \
    --format "{{.Name}}" \
    | xargs -r docker volume rm

echo ""
echo "=== Starting Flowera ==="

docker compose up --build -d

echo ""
echo "=== Waiting for dashboard ==="

for i in {1..60}; do
    if curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
        echo "Flowera dashboard is running."
        break
    fi

    echo "Waiting for Flowera... ($i/60)"
    sleep 2
done

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null 2>&1; then
    echo "ERROR: Flowera dashboard is not responding on port 8080."
    docker compose logs --tail=100
    exit 1
fi

echo ""
echo "========================================"
echo " Flowera is running"
echo "========================================"

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "ERROR: cloudflared is not installed."
    exit 1
fi

echo ""

# Start Cloudflare tunnel, echoing its logs while watching for the
# trycloudflare.com URL so it can be reported to the control plane for
# display in the dashboard's Mobile dropdown.
cloudflared tunnel --url http://127.0.0.1:8080 2>&1 | while IFS= read -r line; do
    echo "$line"

    url=$(echo "$line" | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' || true)

    if [ -n "$url" ]; then
        curl -fsS -X POST http://127.0.0.1:8080/api/tunnel \
            -H "Content-Type: application/json" \
            -d "{\"url\":\"$url\"}" \
            >/dev/null 2>&1 || true
    fi
done