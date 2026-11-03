#!/bin/sh
# The desktop's entrypoint. It starts as root for one reason: to do, once and with nothing the worker can influence,
# the setup the base image used to do through `sudo` as the worker (hand /dockerstartup and $HOME to the desktop user,
# lock /etc/passwd), and to prepare the approval gate's own directory. It then drops to the desktop user with no
# capabilities and with no way to gain any, and runs the base image's startup as that user.
#
# Why this exists (docs/gate-boundary.md): the base image gave the desktop user full `sudo` with a password that is
# public (the image's default), and /etc/passwd world-writable. A worker that reads an email or a web page is
# untrusted; with root it can write the approval gate's state and approve its own payments. Nothing below root's
# reach is a boundary, so the worker must not be able to become root.
#
# Everything this script does as root touches only paths it names. It never runs, sources or reads for a decision
# anything under the worker's home.

set -u

UID_WORKER=1001
GID_WORKER=1001
HOME_DIR=/home/headless

if [ "$(id -u)" != "0" ]; then
    # Started as the desktop user (an old setup, a test): there is no gate boundary, but the desktop still starts.
    exec /dockerstartup/startup.sh "$@"
fi

# The volume under $HOME is root-owned when new. Only the top is checked on later starts: a recursive pass over a
# worker's whole home on every boot is slow, and a recursive chown does not follow links.
if [ "$(stat -c %u "$HOME_DIR" 2>/dev/null)" != "$UID_WORKER" ]; then
    chown -R "$UID_WORKER:$GID_WORKER" "$HOME_DIR"
fi
chown -R "$UID_WORKER:$GID_WORKER" /dockerstartup

# The account files are the base image's; the worker may not edit them (a uid 0 line added there is root).
chmod 644 /etc/passwd /etc/group

# The approval gate (agent/decurix_agent/gate_server.py), as its own user: its state is in a directory only that user
# can read, the key that makes a person's reply count is in its environment alone, and it listens for the control
# plane's signed replies. /run/decurix-gate exists from here on - the agent reads that as "this desktop has a gate"
# and, with the gate down, allows nothing rather than falling back to a store the worker could write.
if getent passwd decurixgate >/dev/null 2>&1 && [ -n "${DECURIX_GATE_KEY:-}" ]; then
    install -d -m 700 -o decurixgate -g decurixgate /var/lib/decurix-gate
    install -d -m 755 -o decurixgate -g decurixgate /run/decurix-gate
    GATE_UID=$(id -u decurixgate)
    GATE_GID=$(id -g decurixgate)
    setsid setpriv --reuid="$GATE_UID" --regid="$GATE_GID" --clear-groups \
        --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
        env HOME=/var/lib/decurix-gate DECURIX_DIR=/var/lib/decurix-gate DECURIX_WORK_DIR=/var/lib/decurix-gate/work \
        PYTHONPATH=/opt PYTHONDONTWRITEBYTECODE=1 \
        python3 -m decurix_agent.gate_server >> /var/lib/decurix-gate/gate.log 2>&1 < /dev/null &
fi
# No other process gets the key.
unset DECURIX_GATE_KEY

# From here on: the desktop user, no capabilities (in any set, and none can be gained), no setuid effect.
exec setpriv --reuid="$UID_WORKER" --regid="$GID_WORKER" --init-groups \
    --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
    env HOME="$HOME_DIR" USER=headless LOGNAME=headless \
    /dockerstartup/startup.sh "$@"
