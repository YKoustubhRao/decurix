#!/bin/sh
# The screen user's session in broker mode (DECURIX_SCREEN_USER=1; docs/gate-boundary.md, "Next: the input broker").
# Started by agent/decurix-entrypoint.sh as decurixscreen, with a cleared environment, in its own folder
# ($HOME, /var/lib/decurix-screen, 0700), where the entrypoint has already put the VNC password file. The container
# waits on this script, and it waits on the X server.
#
# What runs here, as the screen user: the X server with its VNC port (Xtigervnc), the window manager (xfwm4), a
# session bus of this user's own, and the screen broker (the gui daemon as the broker, computer/daemon.py), which
# starts the browser when a command opens a page. Nothing else: the panel, the desktop, the terminal and the office
# apps are the worker's (agent/decurix-worker-session.sh), as untrusted X clients.
#
# The X server is Ubuntu's Xtigervnc, not the base image's /usr/bin/Xvnc: that one is built without the SECURITY
# extension, so it cannot give the worker an untrusted cookie (found 2026-11-07: `xauth generate` says "couldn't
# query Security extension"). Ubuntu's has it, and an untrusted client of it sees only BIG-REQUESTS and XC-MISC: no
# XTEST, no RECORD, no XInput.

set -u
PATH=/usr/local/bin:/usr/bin:/bin
export PATH
SCREEN_HOME=${HOME:?}
SHARED=/run/decurix-screen
LOG="$SCREEN_HOME/session.log"
DISPLAY=:1
XAUTHORITY="$SCREEN_HOME/Xauthority"
XDG_RUNTIME_DIR="$SCREEN_HOME/runtime"
# The window manager's look (decurix-xfwm4.xml) for this user, who has no XFCE configuration of its own.
XDG_CONFIG_DIRS=/etc/xdg/decurix-screen:/etc/xdg
export DISPLAY XAUTHORITY XDG_RUNTIME_DIR XDG_CONFIG_DIRS
# What a command the broker runs keeps (its references for the next action, the audit, the evidence, recordings) and
# its logs: in this user's own folder, never the worker's ~/.decurix, which this user cannot write and must not.
# A caller cannot change them (daemon.caller_may_set: a *_DIR or *_FILE is the screen side's). What of it is the
# worker's (its recordings and evidence) is handed back to the caller and written by it (broker.mirror), and what the
# command must know of the worker's state - its task, the task's secrets, its evidence so far - comes with each
# request and is put here (broker.take_context): the task file's parts in a folder of this user's on tmpfs, like
# the worker's own in /tmp.
DECURIX_DIR="$SCREEN_HOME/.decurix"
DECURIX_WORK_DIR="$SCREEN_HOME/work"
DECURIX_LOG_DIR="$SCREEN_HOME/logs"
DECURIX_TASK_FILE="$SHARED/caller/task"
export DECURIX_DIR DECURIX_WORK_DIR DECURIX_LOG_DIR DECURIX_TASK_FILE
umask 077
cd "$SCREEN_HOME" || exit 1
mkdir -p "$XDG_RUNTIME_DIR" "$DECURIX_DIR" "$DECURIX_WORK_DIR" "$DECURIX_LOG_DIR" "$SHARED/caller"
chmod 700 "$SHARED/caller"
exec >> "$LOG" 2>&1 < /dev/null
echo "[$(date)] screen session starting (uid $(id -u))"

# The display's cookie: new on every start, in this user's folder only. Given to xauth on stdin, never as an argument
# (a process's arguments are anyone's to read).
cookie=$(/usr/bin/mcookie) || exit 1
rm -f "$XAUTHORITY"
printf 'add :1 . %s\nadd %s/unix:1 . %s\n' "$cookie" "$(/usr/bin/hostname)" "$cookie" \
    | /usr/bin/xauth -q -f "$XAUTHORITY" source - || exit 1
cookie=

# No TCP for X (as vncserver-config-mandatory says for the default mode); the VNC port stays on the desktop's own
# network for the control plane (desktop.captureFrame, the screen bridge), guarded by the password file.
/usr/bin/Xtigervnc :1 \
    -auth "$XAUTHORITY" \
    -rfbauth "$SCREEN_HOME/vnc.passwd" \
    -rfbport "${VNC_PORT:-5901}" \
    -geometry "${VNC_RESOLUTION:-1360x768}" \
    -depth "${VNC_COL_DEPTH:-24}" \
    -desktop "$(/usr/bin/hostname):1" \
    -SecurityTypes VncAuth \
    -nolisten tcp \
    -pn &
xvnc=$!

i=0
while [ ! -S /tmp/.X11-unix/X1 ]; do
    if ! kill -0 "$xvnc" 2>/dev/null || [ $i -ge 300 ]; then
        echo "[$(date)] the X server did not start"
        kill "$xvnc" 2>/dev/null
        exit 1
    fi
    i=$((i + 1))
    sleep 0.1
done
echo "[$(date)] X server up (pid $xvnc)"

# The worker's cookie: untrusted (the SECURITY extension), and never timing out while the display lives. Readable by
# the worker in the shared folder; it opens the display as an untrusted client and nothing more.
rm -f "$SHARED/worker.xauth.new"
if /usr/bin/xauth -q -f "$SHARED/worker.xauth.new" generate :1 . untrusted timeout 0; then
    chmod 644 "$SHARED/worker.xauth.new" && mv -f "$SHARED/worker.xauth.new" "$SHARED/worker.xauth"
    rm -f "$SHARED/worker.xauth.new-c" "$SHARED/worker.xauth.new-l"
    echo "[$(date)] the worker's untrusted cookie is in $SHARED/worker.xauth"
else
    echo "[$(date)] could not make the worker's untrusted cookie: the worker's apps cannot open the display"
fi

# This user's own session bus: the window manager's settings (xfconf) and the browser's own D-Bus use it.
eval "$(/usr/bin/dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS

# Without the accessibility bridge: a GTK client of this user's would start this user's accessibility bus and name it on
# the display (AT_SPI_BUS), and the worker's apps would then try that bus - which they may not open - instead of
# their own (found live 2026-11-07: "Couldn't connect to accessibility bus ... Permission denied").
NO_AT_BRIDGE=1 /usr/bin/xfwm4 &
echo "[$(date)] window manager started (pid $!)"

# The wallpaper, drawn on the root window by this user: xfdesktop cannot run as the worker's untrusted client, and run
# as this user its menus would start programs past the gate (computer/wallpaper.py). A picture only - no icons.
(cd / && PYTHONPATH=/opt /usr/bin/python3 -m decurix_agent.computer.wallpaper /opt/decurix-wallpaper.svg) &

# The screen broker, run again when it ends (its code changed on disk, a dev loop's docker cp) and not when it says
# it cannot serve here (3). In /, the agent's code from /opt, nothing of anyone else's on sys.path.
(
    cd / || exit 0
    while :; do
        PYTHONPATH=/opt /usr/bin/python3 -m decurix_agent.computer.daemon
        [ $? -eq 3 ] && break
        sleep 1
    done
) &
echo "[$(date)] screen broker started (loop pid $!)"

wait "$xvnc"
code=$?
echo "[$(date)] X server ended ($code)"
exit "$code"
