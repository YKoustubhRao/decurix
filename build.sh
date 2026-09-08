#!/bin/bash

set -e

echo "========================================"
echo " Building Flowera desktop images"
echo "========================================"

echo ""
echo "=== Building Basic desktop image ==="

docker build \
    -f DesktopDockerfile.basic \
    -t flowera-desktop:basic \
    .

echo ""
echo "=== Building Developer desktop image ==="

docker build \
    -f DesktopDockerfile.developer \
    -t flowera-desktop:developer \
    .

echo ""
echo "=== Building AI Developer desktop image ==="

docker build \
    -f DesktopDockerfile.ai \
    -t flowera-desktop:ai \
    .

echo ""
echo "=== Desktop images built ==="

docker images "flowera-desktop"

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

# Start Cloudflare tunnel
cloudflared tunnel --url http://127.0.0.1:8080