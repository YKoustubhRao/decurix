#!/bin/bash

set -e

echo "=== Building Flowera desktop image ==="

docker build --no-cache -f DesktopDockerfile -t flowera-desktop:latest .

echo "=== Stopping Flowera ==="

docker compose down

echo "=== Removing old desktop containers ==="

docker ps -a --filter "name=desktop_" --format "{{.ID}}" | xargs -r docker rm -f

echo "=== Removing old desktop volumes ==="

docker volume ls --filter "name=desktop_" --format "{{.Name}}" | xargs -r docker volume rm

echo "=== Starting Flowera ==="

docker compose up --build -d

echo "=== Waiting for dashboard ==="

sleep 3

echo "=== Checking Flowera dashboard ==="

if ! curl -fsS http://127.0.0.1:8080/ >/dev/null; then
    echo "ERROR: Flowera dashboard is not responding on port 8080."
    exit 1
fi

echo "Flowera dashboard is running."

echo ""
echo "=== Starting Cloudflare Tunnel ==="

if ! command -v cloudflared >/dev/null 2>&1; then
    echo "ERROR: cloudflared is not installed."
    exit 1
fi

echo ""

# Start Cloudflare tunnel
cloudflared tunnel --url http://127.0.0.1:8080