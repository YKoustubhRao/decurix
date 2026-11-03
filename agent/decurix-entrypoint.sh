#!/bin/sh
# The desktop's entrypoint. It starts as root for one reason: to do, once and with nothing the worker can influence,
# the setup the base image used to do through `sudo` as the worker (hand a new $HOME to the desktop user), and to start
# the approval gate as its own user. It then drops to the desktop user with no capabilities and with no way to gain
# any, and runs the base image's startup as that user.
#
# Why this exists (docs/gate-boundary.md): the base image gave the desktop user full `sudo` with a password that is
# public (the image's default), and /etc/passwd world-writable. A worker that reads an email or a web page is
# untrusted; with root it can read the gate's key and approve its own payments. Nothing below root's reach is a
# boundary, so the worker must not be able to become root - and this script runs as root on every start.
#
# Rules for everything below (round-3 recheck: the worker became root twice through this file):
#   - PATH is the system's only, and every command is named by its absolute path: the container's PATH puts the
#     worker's ~/.local/bin first, and a planted `id` there ran as root.
#   - Nothing root does here follows a path the worker can write into: no recursive chown of a tree the worker can
#     hardlink into (this kernel has protected_hardlinks off; a root `chown -R /dockerstartup` handed the worker this
#     very file). /dockerstartup is the worker's from the image build (docker/desktop.basic.Dockerfile).
#   - Nothing under the worker's home is run, sourced or read for a decision.

set -u
WORKER_PATH=${PATH:-/usr/bin:/bin}
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

UID_WORKER=1001
GID_WORKER=1001
HOME_DIR=/home/headless
KEY_DROP=/etc/decurix-gate/key       # where the control plane puts the key before the first start (root, 0400)
GATE_DIR=/var/lib/decurix-gate       # the gate's own directory (decurixgate, 0700)

if [ "$(/usr/bin/id -u)" != "0" ]; then
    # Started as the desktop user (an old setup, a test): there is no gate boundary, but the desktop still starts.
    exec /dockerstartup/startup.sh "$@"
fi

# A new volume under $HOME is root-owned (only then: the worker cannot change its home's owner). Even then nothing
# that has a second hard link is handed over, and links are changed themselves, never followed.
if [ "$(/usr/bin/stat -c %u "$HOME_DIR" 2>/dev/null)" != "$UID_WORKER" ]; then
    /usr/bin/find "$HOME_DIR" -xdev \( -type d -o -type l -o \( -type f -links 1 \) \) \
        -exec /usr/bin/chown -h "$UID_WORKER:$GID_WORKER" {} +
fi

# The account files are the base image's; the worker may not edit them (a uid 0 line added there is root). The image
# sets this already; a path under /etc is not the worker's to swap, so this is safe to repeat.
/usr/bin/chmod 644 /etc/passwd /etc/group

# The approval gate (agent/decurix_agent/gate_server.py), as its own user. Its key reaches it as a file in its own
# directory, never through an environment: every `docker exec` into this container inherits the container's
# environment. A key the control plane dropped is moved there; an older control plane passed it in the environment,
# and that copy is moved the same way and then removed from this process.
if /usr/bin/getent passwd decurixgate >/dev/null 2>&1; then
    /usr/bin/install -d -m 700 -o decurixgate -g decurixgate "$GATE_DIR"
    if [ -f "$KEY_DROP" ]; then
        /usr/bin/install -m 400 -o decurixgate -g decurixgate "$KEY_DROP" "$GATE_DIR/key"
        /usr/bin/rm -f "$KEY_DROP"
    elif [ -n "${DECURIX_GATE_KEY:-}" ]; then
        (umask 077; printf '%s' "$DECURIX_GATE_KEY" > "$GATE_DIR/key")
        /usr/bin/chown decurixgate:decurixgate "$GATE_DIR/key"
        /usr/bin/chmod 400 "$GATE_DIR/key"
    fi
fi
unset DECURIX_GATE_KEY

if [ -s "$GATE_DIR/key" ]; then
    # /run/decurix-gate exists from here on: the agent reads that as "this desktop has a gate" and, with the gate down,
    # allows nothing rather than falling back to a store the worker could write.
    /usr/bin/install -d -m 755 -o decurixgate -g decurixgate /run/decurix-gate
    GATE_UID=$(/usr/bin/id -u decurixgate)
    GATE_GID=$(/usr/bin/id -g decurixgate)
    # cd into its own directory and python3 -I: nothing on sys.path, and no file the gate opens by a relative name,
    # is in a folder the worker can write (its home was the image's working directory, and a planted ~/tempfile.py
    # would have run as the gate).
    cd "$GATE_DIR" || exit 1
    /usr/bin/setsid /usr/bin/setpriv --reuid="$GATE_UID" --regid="$GATE_GID" --clear-groups \
        --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
        /usr/bin/env PATH=/usr/bin:/bin HOME="$GATE_DIR" DECURIX_DIR="$GATE_DIR" DECURIX_WORK_DIR="$GATE_DIR/work" \
        /usr/bin/python3 -I -c 'import sys, runpy; sys.path.insert(0, "/opt"); runpy.run_module("decurix_agent.gate_server", run_name="__main__", alter_sys=True)' \
        >> "$GATE_DIR/gate.log" 2>&1 < /dev/null &
    cd /
fi

# From here on: the desktop user, no capabilities (in any set, and none can be gained), no setuid effect, and the
# container's PATH back for its own tools.
cd "$HOME_DIR" 2>/dev/null || cd /
exec /usr/bin/setpriv --reuid="$UID_WORKER" --regid="$GID_WORKER" --init-groups \
    --inh-caps=-all --ambient-caps=-all --bounding-set=-all --no-new-privs \
    /usr/bin/env PATH="$WORKER_PATH" HOME="$HOME_DIR" USER=headless LOGNAME=headless \
    /dockerstartup/startup.sh "$@"
