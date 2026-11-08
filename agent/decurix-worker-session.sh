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

# This user's own session bus: its apps' settings (xfconf), its accessibility bus, the panel's plugins. Started
# without DISPLAY, and the accessibility bus started on it before DISPLAY is given to what the bus starts: the
# accessibility bus's launcher names its bus on the display's root window when it has a display, which an untrusted
# client may not (BadAccess), and it died there - so no app of this user's had an accessibility bus at all (found
# live 2026-11-07). The apps ask the session bus for its address instead (org.a11y.Bus). And with the launch helper's
# folder for its runtime folder (this user's, 0711, launch_helper.bind makes it the same way), where the launcher puts
# its socket (at-spi/bus): the screen broker can pass through to it, as it cannot into this user's own runtime folder
# or ~/.cache (0700).
LAUNCH_DIR=${DECURIX_LAUNCH_DIR:-/tmp/decurix-launch-$(id -u)}
mkdir -p "$LAUNCH_DIR/at-spi" && chmod 711 "$LAUNCH_DIR" "$LAUNCH_DIR/at-spi"
eval "$(env -u DISPLAY -u XAUTHORITY XDG_RUNTIME_DIR="$LAUNCH_DIR" dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS
a11y=$(env -u DISPLAY dbus-send --session --print-reply=literal --dest=org.a11y.Bus /org/a11y/bus \
    org.a11y.Bus.GetAddress 2>/dev/null | tr -d ' \n')
dbus-update-activation-environment DISPLAY XAUTHORITY XDG_RUNTIME_DIR XDG_CURRENT_DESKTOP XDG_SESSION_TYPE \
    >/dev/null 2>&1

# The accessibility bus's address, for the screen broker: it reads this user's apps' widgets there (`gui observe` of a
# LibreOffice window; computer/atspi.py, broker.worker_a11y_bus). In the launch helper's folder (this user's, which
# others may only pass through; launch_helper.bind makes it the same way). The bus admits the screen user (the image's
# accessibility.conf); the screen user's own bus never admits this one.
case "$a11y" in
    unix:*)
        printf '%s\n' "$a11y" > "$LAUNCH_DIR/a11y-bus.new" && chmod 644 "$LAUNCH_DIR/a11y-bus.new" \
            && mv -f "$LAUNCH_DIR/a11y-bus.new" "$LAUNCH_DIR/a11y-bus"
        echo "[$(date)] accessibility bus at $a11y"
        ;;
    *) echo "[$(date)] no accessibility bus: the screen broker sees no widget of this user's apps" ;;
esac

# Not xfdesktop: an untrusted client may not set the root window's properties or see its events, and it exits at
# its first (BadAccess). The screen user draws the wallpaper (computer/wallpaper.py); there are no desktop icons in
# this mode - run as the screen user, xfdesktop's menus would start programs as that user, past the gate.
which xdg-user-dirs-update >/dev/null 2>&1 && xdg-user-dirs-update

# The panel without its window list: as an untrusted client it cannot see the windows, so the list stayed empty. The
# screen user's window list (decurix_agent/computer/windowlist.py, started by the screen session) sits between this
# layout's two bars instead. Put in place of the image's layout while that one still holds a window list (a panel a
# person changed otherwise is left alone), before anything starts the settings daemon that reads it.
PANEL_XML="$HOME/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml"
if [ -f /opt/decurix-panel-broker.xml ] && { [ ! -e "$PANEL_XML" ] || grep -q 'value="tasklist"' "$PANEL_XML"; }; then
    mkdir -p "$(dirname "$PANEL_XML")" \
        && cp /opt/decurix-panel-broker.xml "$PANEL_XML.new" && mv -f "$PANEL_XML.new" "$PANEL_XML" \
        && echo "[$(date)] the panel's layout is broker mode's (no window list)"
fi

for part in xfsettingsd xfce4-panel "Thunar --daemon"; do
    # shellcheck disable=SC2086 # a part is a program and its arguments
    setsid $part < /dev/null >> "$LOG" 2>&1 &
    echo "[$(date)] started $part (pid $!)"
done

# The agent, as XFCE's autostart (~/.config/autostart/decurix-agent.desktop) starts it in the default mode.
setsid /opt/start_agent_terminal.sh < /dev/null >> "$LOG" 2>&1 &
echo "[$(date)] agent start script started (pid $!)"
