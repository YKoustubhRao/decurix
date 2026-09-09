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