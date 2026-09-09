#!/bin/bash

sleep 15

LOG=/tmp/flowera-startup.log

echo "[$(date)] Opening visible agent terminal (autostart)" >> "$LOG"

export DISPLAY=:1

if [ -z "$XDG_RUNTIME_DIR" ]; then
    export XDG_RUNTIME_DIR="/tmp/runtime-headless"
    mkdir -p "$XDG_RUNTIME_DIR"
    chmod 700 "$XDG_RUNTIME_DIR"
fi

echo "DISPLAY=$DISPLAY" >> "$LOG"
echo "USER=$(whoami)" >> "$LOG"

# The Flowera engine and vm_server.py are both already started by
# agent-entrypoint.sh at container boot - that's the real startup path
# now (this autostart entry was found unreliable on its own; see
# agent-entrypoint.sh's comment). This just tails vm_server.py's log in a
# visible terminal for anyone watching the desktop over VNC - it does NOT
# launch a second copy (which would fail to bind :8081 behind the first).
touch /tmp/vm_server.log

xfce4-terminal \
    --display="$DISPLAY" \
    --title="Flowera Agent" \
    --geometry=120x35 \
    --hold \
    -e "tail -f /tmp/vm_server.log" \
    >> "$LOG" 2>&1
