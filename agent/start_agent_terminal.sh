#!/bin/bash

sleep 15

LOG=/tmp/flowera-startup.log

echo "[$(date)] Starting Flowera agent" >> "$LOG"

export DISPLAY=:1

if [ -z "$XDG_RUNTIME_DIR" ]; then
    export XDG_RUNTIME_DIR="/tmp/runtime-headless"
    mkdir -p "$XDG_RUNTIME_DIR"
    chmod 700 "$XDG_RUNTIME_DIR"
fi

echo "DISPLAY=$DISPLAY" >> "$LOG"
echo "USER=$(whoami)" >> "$LOG"

setsid python3 /opt/vm_server.py >> "$LOG" 2>&1 < /dev/null &

echo "[$(date)] vm_server.py started as background daemon (pid $!)" >> "$LOG"

# TheBot harness only - its soul binary is a persistent HTTP server
# thebot_harness.py talks to (see flowera_agent/thebot_harness.py), not a
# per-task CLI like claude, so it needs to already be running before any
# task arrives. Harmless to skip entirely for the other two harnesses -
# no point paying its startup cost when nothing will ever talk to it.
if [ "$AGENT_HARNESS" = "thebot" ] && [ -x /opt/soul ]; then
    # /home/headless/ai is TheBot's fixed default AI workdirectory - every
    # agent run cds here and works here, regardless of which real folder
    # a task happens to be scoped to; L1/L2/the Data RAG stay global.
    export WORKSPACE_ROOT="/home/headless/ai"
    mkdir -p "$WORKSPACE_ROOT"
    export LLM_PROVIDER="${LLM_PROVIDER:-ollama}"
    # flowera-ollama is a stub with only a 0.6b model - real testing needs
    # a real model. Defaults to the Cloudflare-tunneled real Ollama server
    # for now (see the project notes); override OLLAMA_URL per-VM once
    # flowera has its own real Ollama available.
    export OLLAMA_HOST="${OLLAMA_URL:-https://art-sized-symposium-instructor.trycloudflare.com}"
    export CHAT_MODEL="${OLLAMA_MODEL:-devstral-small-2:24b}"
    export SOUL_PORT="4900"
    export DOCLING_SCRIPT="/opt/thebot-scripts/docling_convert.py"
    setsid /opt/soul >> /tmp/thebot-soul.log 2>&1 < /dev/null &
    echo "[$(date)] soul (TheBot) started as background daemon (pid $!)" >> "$LOG"
fi