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

echo "=== Done ==="

docker ps

echo ""
echo "=== Visit main dashboard at http://localhost:8080/ ==="