#!/bin/bash
# Starts browser-agent and doc-ui as background daemons -- same pattern as
# start_agent_terminal.sh (setsid + redirect to a log, no visible terminal),
# just for these two standalone tools rather than the main flowera_agent.

sleep 20

LOG=/tmp/flowera-tools-startup.log
VENV=/opt/flowera-tools-venv/bin

echo "[$(date)] Starting Flowera tools (browser-agent, doc-ui)" >> "$LOG"

export DISPLAY=:1
# Mirrors start_agent_terminal.sh's OLLAMA_HOST fallback for the same reason:
# OLLAMA_URL is normally set on the container already, this only covers a
# desktop started without one.
export OLLAMA_URL="${OLLAMA_URL:-http://host.docker.internal:11434}"

cd /opt/browser-agent || exit 1
setsid "$VENV/python" server.py >> /tmp/browser-agent.log 2>&1 < /dev/null &
echo "[$(date)] browser-agent started as background daemon (pid $!)" >> "$LOG"

cd /opt/doc-ui || exit 1
setsid "$VENV/python" -m uvicorn server:app --host 127.0.0.1 --port 8000 >> /tmp/doc-ui.log 2>&1 < /dev/null &
echo "[$(date)] doc-ui started as background daemon (pid $!)" >> "$LOG"
