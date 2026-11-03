#!/bin/sh
# Live check of the approval gate's boundary on a desktop image (docs/gate-boundary.md). Run it on every new image:
# each row is an attack from a security review, and a FAIL means the worker can approve its own actions.
#
#   sh bench/security/boundary.sh [--image decurix-desktop:basic] [--layer] [--keep]
#
# --layer builds a throwaway image from --image with this checkout's entrypoint and agent code on top, to check a
# change before a full image build. Needs a Linux Docker (in the Decurix WSL distro: run as root there). Starts one
# throwaway desktop (decurix-boundary-*, 1.5 GB) the way the control plane does - the worker's PATH, no NET_RAW, the
# approval key dropped in as a root-only file - and removes it after. No other container is touched, and the key (a
# random one made here) is never printed.
#
# What it checks, as the worker (uid 1001) unless said:
#   key       the key is in no environment: not the container's, not a `docker exec`'s, not any process the worker
#             can read; it sits in the gate's own directory (decurixgate, 0400) and the drop file is gone
#   gate      the gate runs as decurixgate, python3 -I, cwd its own directory, and the worker cannot read its files
#   session   the desktop's processes have no capabilities and no-new-privs
#   socket    no call on the gate's socket grants; a check must describe the action; a forged reply is refused
#   flow      the gate words and signs its question; a reply signed for it grants that one action once; a reply
#             signed for another request, or read against other words, grants nothing
#   restart   after planting a fake `id` (and every other command the entrypoint runs) in ~/.local/bin, hard links to
#             the entrypoint, the gate's code and /etc/passwd in folders root used to chown, and a ~/tempfile.py, two
#             restarts later nothing ran as root or as the gate, and those files are still root's and unwritable
#   no key    a second desktop started without a key has the gate's directory, and a gated click is not allowed

set -u
IMAGE=decurix-desktop:basic
LAYER=0
KEEP=0
while [ $# -gt 0 ]; do
    case "$1" in
        --image) IMAGE=$2; shift 2 ;;
        --layer) LAYER=1; shift ;;
        --keep) KEEP=1; shift ;;
        *) echo "usage: $0 [--image IMAGE] [--layer] [--keep]" >&2; exit 2 ;;
    esac
done

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
RUN=decurix-boundary-$$
VOL=$RUN-home
WORK=$(mktemp -d)
FAILS=0
# The worker's PATH, exactly as the control plane sets it (internal/desktop/docker.go buildDesktopEnv).
WPATH=/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:/home/headless/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILS=$((FAILS + 1)); }
check() { # check NAME OUTPUT: a line "ok" passes, anything else fails with that text
    if [ "$2" = ok ]; then pass "$1"; else fail "$1: $2"; fi
}
# Never a bare `sh` in a docker exec: the exec gets the container's PATH, which puts the worker's ~/.local/bin first,
# so `docker exec -u 0 <desktop> sh` runs whatever the worker put there, as root (this bench caught itself doing it).
as_worker() { docker exec -i -u 1001:1001 "$RUN" /usr/bin/env -i PATH=/usr/bin:/bin HOME=/home/headless /bin/sh -s; }
as_root() { docker exec -i -u 0 "$RUN" /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /bin/sh -s; }

cleanup() {
    if [ "$KEEP" = 0 ]; then
        docker rm -f -v "$RUN" "$RUN-nokey" >/dev/null 2>&1
        docker volume rm "$VOL" "$VOL-nokey" >/dev/null 2>&1
        [ "$LAYER" = 1 ] && docker rmi "$RUN-image" >/dev/null 2>&1
    else
        echo "kept: container $RUN, volume $VOL"
    fi
    rm -rf "$WORK"
}
trap cleanup EXIT

if [ "$LAYER" = 1 ]; then
    mkdir -p "$WORK/ctx"
    cp "$REPO/agent/decurix-entrypoint.sh" "$WORK/ctx/"
    cp -r "$REPO/agent/decurix_agent" "$WORK/ctx/decurix_agent"
    find "$WORK/ctx" -name __pycache__ -prune -exec rm -rf {} +
    cat > "$WORK/ctx/Dockerfile" <<EOF
FROM $IMAGE
USER root
COPY decurix-entrypoint.sh /opt/decurix-entrypoint.sh
COPY decurix_agent /opt/decurix_agent
RUN chmod 755 /opt/decurix-entrypoint.sh && chown -R root:root /opt/decurix_agent /opt/decurix-entrypoint.sh \
    && chmod -R go-w /opt/decurix_agent && chown -R 1001:1001 /dockerstartup
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/decurix-entrypoint.sh"]
EOF
    docker build -q -t "$RUN-image" "$WORK/ctx" >/dev/null || { echo "could not build the layered image" >&2; exit 2; }
    IMAGE=$RUN-image
fi

# A random key for this run, never printed; only its digest goes into the worker's checks.
KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
KEYHASH=$(printf '%s' "$KEY" | sha256sum | cut -d' ' -f1)

docker volume create "$VOL" >/dev/null
docker create --name "$RUN" --cap-drop NET_RAW \
    --tmpfs /run:rw,nosuid,size=64m,mode=755 --tmpfs /tmp:rw,nosuid,size=256m,mode=1777 \
    -v "$VOL:/home/headless" -e PATH="$WPATH" -e VNC_PW=benchpw1 -m 1500m "$IMAGE" >/dev/null \
    || { echo "could not create the desktop" >&2; exit 2; }
# The key goes in the way the control plane puts it (desktop.putGateKey): root-owned, 0400, in a 0700 directory.
mkdir -p "$WORK/key/etc/decurix-gate"
printf '%s' "$KEY" > "$WORK/key/etc/decurix-gate/key"
chmod 700 "$WORK/key/etc/decurix-gate"
chmod 400 "$WORK/key/etc/decurix-gate/key"
tar --numeric-owner -C "$WORK/key" -cf - etc | docker cp - "$RUN:/" \
    || { echo "could not put the key in place" >&2; exit 2; }
rm -rf "$WORK/key"

wait_gate() {
    i=0
    while [ $i -lt 60 ]; do
        docker exec -u 0 "$RUN" /usr/bin/test -S /run/decurix-gate/gate.sock 2>/dev/null && return 0
        sleep 2
        i=$((i + 1))
    done
    return 1
}

docker start "$RUN" >/dev/null
if wait_gate; then pass "gate up after the first start"; else fail "gate never came up"; exit 1; fi

# ---- key ----
if docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$RUN" | grep -q '^DECURIX_GATE_KEY='; then
    fail "key: in the container's environment"
else pass "key: not in the container's environment"; fi
if docker exec -u 1001 "$RUN" /usr/bin/env | grep -q "$KEY"; then fail "key: in a docker exec's environment"
else pass "key: not in a docker exec's environment"; fi
check "key: in no process environment the worker can read" "$(as_worker <<EOF
python3 -I - <<'PY'
import hashlib, os
want = "$KEYHASH"
hits = []
for pid in os.listdir("/proc"):
    if not pid.isdigit():
        continue
    try:
        env = open(f"/proc/{pid}/environ", "rb").read().split(b"\0")
    except OSError:
        continue
    for kv in env:
        k, _, v = kv.partition(b"=")
        if v and hashlib.sha256(v).hexdigest() == want:
            hits.append(open(f"/proc/{pid}/comm").read().strip())
print("ok" if not hits else "found in " + ",".join(hits))
PY
EOF
)"
check "key: only in the gate's own directory, decurixgate 0400" "$(as_root <<'EOF'
if [ -e /etc/decurix-gate/key ]; then echo "drop file still there"; exit; fi
s=$(stat -c '%U %a' /var/lib/decurix-gate/key 2>&1); [ "$s" = "decurixgate 400" ] && echo ok || echo "key file is: $s"
EOF
)"

# ---- gate ----
# (Its working directory cannot be read even by root: a process that changed uid is not dumpable. The planted
# ~/tempfile.py below is the check that nothing is imported from the worker's folders.)
check "gate: decurixgate, python3 -I" "$(as_root <<'EOF'
pid=$(pgrep -f gate_server | head -1)
[ -n "$pid" ] || { echo "no gate process"; exit; }
u=$(stat -c %U /proc/$pid); a=$(tr '\0' ' ' < /proc/$pid/cmdline)
case "$a" in *" -I "*) i=1 ;; *) i=0 ;; esac
[ "$u" = decurixgate ] && [ $i = 1 ] && echo ok || echo "user $u cmd $a"
EOF
)"
check "gate: the worker cannot read its directory, key or environment" "$(as_worker <<'EOF'
pid=$(pgrep -f gate_server | head -1)
out=""
ls /var/lib/decurix-gate >/dev/null 2>&1 && out="$out dir"
cat /var/lib/decurix-gate/key >/dev/null 2>&1 && out="$out key"
cat /proc/$pid/environ >/dev/null 2>&1 && out="$out environ"
[ -z "$out" ] && echo ok || echo "readable:$out"
EOF
)"

# ---- session ----
check "session: no capabilities, no-new-privs" "$(as_root <<'EOF'
bad=""
for d in /proc/[0-9]*; do
    p=${d#/proc/}
    [ "$(stat -c %u $d 2>/dev/null)" = 1001 ] || continue
    # The desktop's own processes descend from PID 1; a docker exec's have parent 0.
    q=$p; root=""
    while [ -n "$q" ] && [ "$q" != 0 ] && [ "$q" != 1 ]; do q=$(awk '/^PPid:/{print $2}' /proc/$q/status 2>/dev/null); done
    [ "$q" = 1 ] || continue
    eff=$(awk '/^CapEff:/{print $2}' $d/status); bnd=$(awk '/^CapBnd:/{print $2}' $d/status); nnp=$(awk '/^NoNewPrivs:/{print $2}' $d/status)
    [ "$eff" = 0000000000000000 ] && [ "$bnd" = 0000000000000000 ] && [ "$nnp" = 1 ] || bad="$bad $(cat $d/comm)"
done
[ -z "$bad" ] && echo ok || echo "has privileges:$bad"
EOF
)"

# ---- socket and flow (the bench stands in for the control plane: it holds the key) ----
gate_flow() {
    { echo "$KEY"; cat "$HERE/gate_flow.py"; } | docker exec -i -u 0 "$RUN" /usr/bin/env -i PATH=/usr/bin:/bin \
        /usr/bin/python3 -I -c 'import sys; key = sys.stdin.readline().strip(); exec(sys.stdin.read())'
}
gate_flow > "$WORK/flow.log" 2>&1
while IFS= read -r line; do
    case "$line" in
        "ok "*) pass "${line#ok }" ;;
        "bad "*) fail "${line#bad }" ;;
        *) echo "      $line" ;;
    esac
done < "$WORK/flow.log"
grep -q '^ok ' "$WORK/flow.log" || fail "flow: the gate's checks did not run"

# ---- plant, restart, try, restart ----
as_worker <<'EOF' >"$WORK/plant.log" 2>&1
mkdir -p ~/.local/bin
for c in id stat find chown chmod getent install rm setsid setpriv env python3 printf cat sh cp mv mkdir touch pgrep tini; do
    cat > ~/.local/bin/$c <<X
#!/bin/sh
u=\$(/usr/bin/id -u); [ "\$u" != 1001 ] && /usr/bin/touch /var/tmp/.bench-proof-path-$c-\$u
exec /usr/bin/$c "\$@"
X
    chmod +x ~/.local/bin/$c
done
cat > ~/tempfile.py <<'X'
import os
if os.getuid() != 1001:
    open("/var/tmp/.bench-proof-py-%d" % os.getuid(), "w").close()
X
for t in /opt/decurix-entrypoint.sh /opt/decurix_agent/gate_server.py /etc/passwd; do
    n=$(basename $t)
    ln "$t" /dockerstartup/.bench-$n 2>/dev/null && echo "linked $n into /dockerstartup"
    ln "$t" ~/.bench-$n 2>/dev/null && echo "linked $n into home"
done
EOF
sed 's/^/      plant: /' "$WORK/plant.log"

docker restart "$RUN" >/dev/null
if wait_gate; then pass "gate up after a restart (its key survived)"; else fail "gate down after a restart"; fi

check "restart: the entrypoint, the gate's code and /etc/passwd are still root's and unwritable" "$(as_worker <<'EOF'
bad=""
for t in /opt/decurix-entrypoint.sh /opt/decurix_agent/gate_server.py /etc/passwd /etc/group; do
    [ "$(stat -c %u $t)" = 0 ] || bad="$bad $t(owner)"
    [ -w "$t" ] && bad="$bad $t(writable)"
    ( printf '\n# bench\n' >> "$t" ) 2>/dev/null && bad="$bad $t(appended)"
done
[ -z "$bad" ] && echo ok || echo "$bad"
EOF
)"
gate_flow >/dev/null 2>&1   # the gate reads a reply: anything it imports lazily is imported now
docker restart "$RUN" >/dev/null
wait_gate >/dev/null
gate_flow >/dev/null 2>&1
check "restart: nothing the worker planted ran as root or as the gate" "$(as_root <<'EOF'
p=$(ls /var/tmp/.bench-proof-* 2>/dev/null | while read f; do [ "$(stat -c %u "$f")" != 1001 ] && echo "$f"; done)
[ -z "$p" ] && echo ok || echo "ran:" $p
EOF
)"

# ---- no key: a desktop whose gate never started allows nothing (round-4 recheck: it fell back to the worker's store)
docker stop "$RUN" >/dev/null   # one desktop at a time (WSL memory)
docker volume create "$VOL-nokey" >/dev/null
docker create --name "$RUN-nokey" --cap-drop NET_RAW \
    --tmpfs /run:rw,nosuid,size=64m,mode=755 --tmpfs /tmp:rw,nosuid,size=256m,mode=1777 \
    -v "$VOL-nokey:/home/headless" -e PATH="$WPATH" -e VNC_PW=benchpw1 -m 1500m "$IMAGE" >/dev/null \
    && docker start "$RUN-nokey" >/dev/null
i=0
while [ $i -lt 30 ] && ! docker exec -u 0 "$RUN-nokey" /usr/bin/test -d /run/decurix-gate 2>/dev/null; do sleep 2; i=$((i + 1)); done
check "no key: the gate's directory exists, and a gated click is not allowed" "$(docker exec -i -u 1001:1001 "$RUN-nokey" \
    /usr/bin/env -i PATH=/usr/bin:/bin HOME=/home/headless /usr/bin/python3 -I - <<'PY' 2>&1 | tail -1
import os, sys
sys.path.insert(0, "/opt")
from decurix_agent.computer import approval
d = approval.check("click", {"role": "button", "name": "Pay now", "window_title": "Bench bank"},
                   context={"url": "https://bank.invalid/pay"})
gate = os.path.isdir("/run/decurix-gate") and not os.path.exists("/run/decurix-gate/gate.sock")
print("ok" if gate and approval.remote() and not d.allowed else f"dir {gate} remote {approval.remote()} allowed {d.allowed}")
PY
)"
docker rm -f -v "$RUN-nokey" >/dev/null 2>&1
docker volume rm "$VOL-nokey" >/dev/null 2>&1

echo
if [ "$FAILS" = 0 ]; then echo "boundary holds ($IMAGE)"; exit 0; fi
echo "$FAILS check(s) failed ($IMAGE)"
exit 1
