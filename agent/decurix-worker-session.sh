#!/bin/sh
# The worker's session in broker mode (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "Next: the input broker").
# Started by agent/decurix-entrypoint.sh as the desktop user (headless), in the background, with the container's
# environment less the screen's password. In the default mode none of this runs: the base image's startup starts the
# X server and the XFCE session as this user, and XFCE's autostart starts the agent.
#
# Here the X server and the window manager are the screen user's (agent/decurix-screen-session.sh). This user's own X
# clients - the panel, the desktop, the settings daemon, the file manager's daemon, and every app the worker or a
# person starts from them - open the display with an untrusted cookie: they draw and take input in their own windows,
# and cannot send input to others (no XTEST, no events to the screen user's windows) or read them. The agent is started
# directly, not by an autostart of a session this user does not run.

set -u
LOG=/tmp/decurix-startup.log
exec >> "$LOG" 2>&1 < /dev/null
echo "[$(date)] worker session starting (uid $(id -u))"

SHARED=${DECURIX_SCREEN_DIR:-/run/decurix-screen}
i=0
while [ ! -s "$SHARED/worker.xauth" ]; do
    if [ $i -ge 600 ]; then
        echo "[$(date)] no X cookie for the worker after 60 s; its apps cannot open the display"
        break
    fi
    i=$((i + 1))
    sleep 0.1
done
umask 077
# ~/.Xauthority: where a `docker exec` as this user, and session.ensure, find it.
if cp "$SHARED/worker.xauth" "$HOME/.Xauthority.new" 2>/dev/null; then
    mv -f "$HOME/.Xauthority.new" "$HOME/.Xauthority"
fi
umask 022

DISPLAY=:1
XAUTHORITY="$HOME/.Xauthority"
XDG_CURRENT_DESKTOP=XFCE
XDG_SESSION_DESKTOP=xfce
DESKTOP_SESSION=xfce
XDG_SESSION_TYPE=x11
export DISPLAY XAUTHORITY XDG_CURRENT_DESKTOP XDG_SESSION_DESKTOP DESKTOP_SESSION XDG_SESSION_TYPE
if [ -z "${XDG_RUNTIME_DIR:-}" ]; then
    XDG_RUNTIME_DIR=/tmp/runtime-headless
    mkdir -p "$XDG_RUNTIME_DIR" && chmod 700 "$XDG_RUNTIME_DIR"
    export XDG_RUNTIME_DIR
fi

# This user's own session bus: its apps' settings (xfconf), its accessibility bus, the panel's plugins.
eval "$(dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS

which xdg-user-dirs-update >/dev/null 2>&1 && xdg-user-dirs-update
for part in xfsettingsd xfce4-panel xfdesktop "Thunar --daemon"; do
    # shellcheck disable=SC2086 # a part is a program and its arguments
    setsid $part < /dev/null >> "$LOG" 2>&1 &
    echo "[$(date)] started $part (pid $!)"
done

# The agent, as XFCE's autostart (~/.config/autostart/decurix-agent.desktop) starts it in the default mode.
setsid /opt/start_agent_terminal.sh < /dev/null >> "$LOG" 2>&1 &
echo "[$(date)] agent start script started (pid $!)"
