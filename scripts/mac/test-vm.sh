#!/bin/bash
#
# Spins up ONE throwaway desktop container standalone on macOS, for quickly testing
# a desktop image or the agent harness without running the full control plane.
#
# Usage:
#   ./scripts/mac/test-vm.sh [--profile basic|developer|ai] [--harness thebot|ollama|claude]
#                            [--name test-vm] [--task "do something"] [--down]

set -e

# Every script runs from the repo root, two levels up from scripts/<group>/.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

PROFILE="basic"
HARNESS="thebot"
NAME="test-vm"
TASK=""
TEARDOWN=false

while [ $# -gt 0 ]; do
    case "$1" in
        --profile) PROFILE="$2"; shift 2 ;;
        --harness) HARNESS="$2"; shift 2 ;;
        --name) NAME="$2"; shift 2 ;;
        --task) TASK="$2"; shift 2 ;;
        --down) TEARDOWN=true; shift ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

DESKTOP_CONTAINER="desktop_${NAME}"
DUMMY_CP="flowera-dummy-cp"

OLLAMA_URL="${OLLAMA_URL:-http://host.docker.internal:11434}"
OLLAMA_MODEL="${OLLAMA_MODEL:-qwen2.5-coder:3b}"
CODING_MODEL="${CODING_MODEL:-}"

if [ "$TEARDOWN" = true ]; then
    echo "=== Removing test desktop ${DESKTOP_CONTAINER} ==="
    docker rm -f "$DESKTOP_CONTAINER" 2>/dev/null || true
    echo "=== Removing dummy control plane ==="
    docker rm -f "$DUMMY_CP" 2>/dev/null || true
    exit 0
fi

case "$PROFILE" in
    basic|developer|ai) ;;
    *) echo "ERROR: --profile must be basic, developer, or ai" >&2; exit 1 ;;
esac

case "$HARNESS" in
    thebot|ollama|claude) ;;
    *) echo "ERROR: --harness must be thebot, ollama, or claude" >&2; exit 1 ;;
esac

echo "========================================"
echo " Flowera standalone VM test (macOS)"
echo "========================================"
echo "profile=${PROFILE} harness=${HARNESS} name=${NAME}"
echo "chat model=${OLLAMA_MODEL}${CODING_MODEL:+ | coding model=${CODING_MODEL}}"
echo ""

echo "=== Ensuring flowera-network ==="
docker network inspect flowera-network >/dev/null 2>&1 || docker network create flowera-network >/dev/null

echo ""
echo "=== Ensuring image flowera-desktop:${PROFILE} ==="
if ! docker image inspect "flowera-desktop:${PROFILE}" >/dev/null 2>&1; then
    echo "Image not found locally - building it (this can take a while)..."
    docker build --platform linux/amd64 -f "docker/desktop.${PROFILE}.Dockerfile" -t "flowera-desktop:${PROFILE}" .
fi

echo ""
echo "=== Starting dummy control plane (${DUMMY_CP}) ==="
if ! docker ps --filter "name=^${DUMMY_CP}\$" --format '{{.Names}}' | grep -q "^${DUMMY_CP}\$"; then
    docker rm -f "$DUMMY_CP" >/dev/null 2>&1 || true
    docker run -d \
        --name "$DUMMY_CP" \
        --network flowera-network \
        --network-alias "$DUMMY_CP" \
        -v "$(pwd)/scripts/testing:/srv:ro" \
        -w /srv \
        python:3-alpine \
        python dummy_control_plane.py >/dev/null
    echo "Started."
else
    echo "Already running."
fi

echo ""
echo "=== Starting test desktop (${DESKTOP_CONTAINER}) ==="
docker rm -f "$DESKTOP_CONTAINER" >/dev/null 2>&1 || true

docker run -d \
    --name "$DESKTOP_CONTAINER" \
    --platform linux/amd64 \
    --network flowera-network \
    --shm-size=512m \
    -p 16901:6901 \
    -p 14900:4900 \
    -p 18081:8081 \
    --add-host host.docker.internal:host-gateway \
    -e VNC_PW=admin \
    -e VNC_RESOLUTION=1280x720 \
    -e DESKTOP_ID="$NAME" \
    -e DESKTOP_NAME="$NAME" \
    -e CONTROL_PLANE_URL="http://${DUMMY_CP}:8080" \
    -e OLLAMA_URL="$OLLAMA_URL" \
    -e OLLAMA_MODEL="$OLLAMA_MODEL" \
    -e AGENT_HARNESS="$HARNESS" \
    -e CODING_MODEL="$CODING_MODEL" \
    "flowera-desktop:${PROFILE}" >/dev/null

echo ""
echo "=== Waiting for the agent to come up (vm_server.py:8081) ==="
ready=false
for i in $(seq 1 60); do
    if docker exec "$DESKTOP_CONTAINER" grep -q "VM agent listening" /tmp/flowera-startup.log 2>/dev/null; then
        ready=true
        break
    fi
    echo "Waiting... ($i/60)"
    sleep 2
done
if [ "$ready" != true ]; then
    echo "WARNING: agent did not come up within 2 minutes - check 'docker logs desktop_${NAME}'."
fi

if [ -n "$TASK" ]; then
    echo ""
    echo "=== Sending task ==="
    payload=$(jq -n --arg content "$TASK" '{content:$content}')
    curl -fsS -X POST "http://127.0.0.1:18081/message" \
        -H "Content-Type: application/json" \
        -d "$payload" \
        >/dev/null && echo "Sent." || echo "WARNING: could not deliver task yet."
fi

echo ""
echo "========================================"
echo " Test desktop is up"
echo "========================================"
echo "VNC:              http://localhost:16901/vnc.html"
if [ "$HARNESS" = "thebot" ]; then
echo "Agent chat UI:     http://localhost:14900/"
fi
echo "Send a task:       curl -X POST http://localhost:18081/message -H 'Content-Type: application/json' -d '{\"content\":\"...\"}'"
echo "Watch callbacks:    docker logs -f ${DUMMY_CP}"
echo "Watch agent logs:   docker exec ${DESKTOP_CONTAINER} tail -f /tmp/flowera-startup.log"
echo "Tear down:          ./scripts/mac/test-vm.sh --name ${NAME} --down"
