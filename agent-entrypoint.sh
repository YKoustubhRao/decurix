#!/bin/bash

# Starts the Flowera engine + vm_server.py in the background at container
# boot, independent of XFCE session autostart (~/.config/autostart) -
# found unreliable in this base image: even the pre-existing demo-desktop
# container has vm_server.py running with no autostart-log evidence and a
# process tree that doesn't match the xfce4-terminal-wrapped autostart
# path, meaning it was almost certainly started by hand during
# development, not by the autostart entry that's supposed to do it. Task
# submission must work the moment the container is up, not only if/when a
# desktop session happens to process autostart - so this is now the real
# startup path, and start_agent_terminal.sh (still wired into
# ~/.config/autostart for the visible terminal UX) just tails the log
# these processes already write to instead of launching a second copy.

set -e

LOG=/tmp/flowera-startup.log
echo "[$(date)] agent-entrypoint starting" >> "$LOG"

(
  cd /opt/flowera-engine
  echo "[$(date)] starting Flowera engine" >> "$LOG"
  node server.js >> /tmp/flowera-engine.log 2>&1
) &

(
  # Give the engine a moment to bind its port before vm_server.py's own
  # wait_for_engine() polling starts.
  sleep 2
  echo "[$(date)] starting vm_server.py" >> "$LOG"
  python3 /opt/vm_server.py >> /tmp/vm_server.log 2>&1
) &

exec /usr/bin/tini -- /dockerstartup/startup.sh
