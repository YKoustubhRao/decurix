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
    # Mirrors buildDesktopEnv's defaults in internal/desktop/docker.go -
    # the host running the control plane, reached over the ExtraHosts entry
    # every desktop already gets. OLLAMA_URL is normally set on the
    # container, so this fallback only applies to a desktop started without
    # one; it replaced a hardcoded Cloudflare quick-tunnel URL that stopped
    # resolving as soon as that tunnel was restarted.
    export OLLAMA_HOST="${OLLAMA_URL:-http://host.docker.internal:11434}"
    export CHAT_MODEL="${OLLAMA_MODEL:-qwen3:latest}"
    # Coding builds can run on their own model (see TheBot's internal/llm
    # Models.ForTaskType): a build sends the longest prompts the agent ever
    # produces and wants the largest context window, while chat wants a good
    # conversationalist. Unset means "use CHAT_MODEL for coding too", so a
    # single-model VM behaves exactly as before.
    export CODING_MODEL="${CODING_MODEL:-}"
    export SOUL_PORT="4900"
    export DOCLING_SCRIPT="/opt/thebot-scripts/docling_convert.py"
    setsid /opt/soul >> /tmp/thebot-soul.log 2>&1 < /dev/null &
    echo "[$(date)] soul (TheBot) started as background daemon (pid $!)" >> "$LOG"
fi