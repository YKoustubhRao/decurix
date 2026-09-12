#!/bin/bash
#
# Spins up ONE throwaway desktop container standalone, for quickly testing a
# desktop image or the agent harness without building/running the whole
# flowera control plane. Pairs with scripts/testing/dummy_control_plane.py,
# a stdlib-only stub that accepts the desktop's help/completion callbacks
# and prints them live, so you can watch the agent work.
#
# Usage:
#   ./scripts/linux/test-vm.sh [--profile basic|developer|ai] [--harness thebot|ollama|claude]
#                               [--name test-vm] [--task "do something"] [--down]
#
# --down removes this script's own test containers (desktop_<name> and, if
# nothing else is using it, the dummy control plane) and exits - it never
# touches desktop_* containers created any other way.

set -e

# Under Git Bash on Windows, MSYS rewrites anything that looks like a Unix
# absolute path (e.g. "/srv" in a docker -w/-v argument) into a Windows path
# before Docker ever sees it. No-op on real Linux/macOS.
export MSYS_NO_PATHCONV=1

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

# Same default as internal/desktop/docker.go's buildDesktopEnv - the host
# running the control plane, reached over the ExtraHosts entry every desktop
# gets. Override with your own OLLAMA_URL/OLLAMA_MODEL env vars if needed.
OLLAMA_URL="${OLLAMA_URL:-http://host.docker.internal:11434}"
OLLAMA_MODEL="${OLLAMA_MODEL:-qwen3:latest}"
# Optional second model, used only for coding builds - TheBot routes
# "coding" separately (see its internal/llm Models.ForTaskType), because a
# build sends the longest prompts the agent ever produces and wants the
# largest context window, while chat wants a good conversationalist. Unset
# means the one model above does everything.
CODING_MODEL="${CODING_MODEL:-}"

if [ "$TEARDOWN" = true ]; then
    echo "=== Removing test desktop ${DESKTOP_CONTAINER} ==="
    docker rm -f "$DESKTOP_CONTAINER" 2>/dev/null || true

    # The dummy control plane is only ever used by test-vm's own desktop
    # containers (never by real flowera-managed ones), so it's always safe
    # to remove here - it's a cheap stub, re-created instantly next run.
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
echo " Flowera standalone VM test"
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
    docker build -f "docker/desktop.${PROFILE}.Dockerfile" -t "flowera-desktop:${PROFILE}" .
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
# Checked from inside the container (vm_server.py logs this line itself
# once its HTTPServer starts) rather than by probing the published host
# port - Docker Desktop's host<->container port forwarding has real,
# observed lag/flakiness for rapid successive probes right after a
# container starts, which made a host-side curl loop unreliable here even
# though the container was already serving requests.
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
        >/dev/null && echo "Sent." || echo "WARNING: could not deliver the task yet - the agent may still be starting. Retry with the same --task once it's up."
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
echo "Tear down:          ./scripts/linux/test-vm.sh --name ${NAME} --down"
