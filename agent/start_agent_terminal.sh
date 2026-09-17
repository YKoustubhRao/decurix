#!/bin/bash

# Adaptively wait for X11 display :1 to be ready (up to 15s max), proceeding immediately once available
for i in $(seq 1 30); do
    if [ -S /tmp/.X11-unix/X1 ] || (which xdpyinfo >/dev/null 2>&1 && xdpyinfo -display :1 >/dev/null 2>&1); then
        break
    fi
    sleep 0.5
done

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

if [ -f /opt/tools/env.sh ]; then
    . /opt/tools/env.sh
fi

# Chromium's launch flags live in $HOME, which is this desktop's persistent
# volume - so an image rebuild alone never replaces the base image's copy
# with its hardcoded --no-sandbox. Reinstall ours on every start.
if [ -f /opt/flowera-chromium.init ]; then
    cp /opt/flowera-chromium.init "$HOME/.chromium-browser.init" 2>> "$LOG" \
        || echo "[$(date)] could not install Chromium launch flags" >> "$LOG"
fi

# Pre-warm Chromium in the background with --no-startup-window so that
# clicking the browser icon or navigating via CDP is instantaneous.
if which chromium-browser >/dev/null 2>&1; then
    setsid chromium-browser --no-startup-window >> /tmp/chromium-prewarm.log 2>&1 < /dev/null &
    echo "[$(date)] Chromium pre-warmed in background (pid $!)" >> "$LOG"
fi

# Clear out core dumps a crashing Chromium already left behind, before core
# dumps were switched off (see agent/chromium-browser.init). Only files that
# are named core.<pid> AND are ELF core dumps - a file that merely shares
# the name is left alone.
python3 - "$HOME" "$HOME/Desktop" >> "$LOG" 2>&1 <<'PY'
import os, re, sys

removed = 0
for folder in sys.argv[1:]:
    try:
        names = os.listdir(folder)
    except OSError:
        continue
    for name in names:
        path = os.path.join(folder, name)
        if not re.fullmatch(r"core\.\d+", name) or not os.path.isfile(path):
            continue
        try:
            with open(path, "rb") as f:
                header = f.read(18)
        except OSError:
            continue
        # ELF magic, then e_type == ET_CORE (4) at offset 16.
        if len(header) == 18 and header[:4] == b"\x7fELF" and int.from_bytes(header[16:18], "little") == 4:
            os.remove(path)
            removed += 1
if removed:
    print(f"removed {removed} leftover core dump(s)")
PY

# The agent's own directories live under ~/.flowera, so ~/work is the only
# one it adds to the home a human browses. A volume from before that still
# has them at the top level: move them in before anything that uses them
# starts, so worker memory, the audit trail and TheBot's stores carry over.
# Where both exist (the control plane saved a memory entry first), entries
# move one at a time and nothing already there is overwritten. ~/ai is only
# TheBot's on a TheBot desktop; anywhere else it could be a human's folder.
legacy_dirs="command helptext final memory audit"
[ "$AGENT_HARNESS" = "thebot" ] && legacy_dirs="$legacy_dirs ai"
python3 - "$HOME" $legacy_dirs >> "$LOG" 2>&1 <<'PY'
import os, sys

home, names = sys.argv[1], sys.argv[2:]
flowera = os.path.join(home, ".flowera")
for name in names:
    old, new = os.path.join(home, name), os.path.join(flowera, name)
    if os.path.islink(old) or not os.path.isdir(old):
        continue
    try:
        os.makedirs(flowera, exist_ok=True)
        if not os.path.exists(new):
            os.rename(old, new)
        else:
            for entry in os.listdir(old):
                if not os.path.lexists(os.path.join(new, entry)):
                    os.rename(os.path.join(old, entry), os.path.join(new, entry))
            os.rmdir(old)
        print(f"moved ~/{name} to ~/.flowera/{name}")
    except OSError as e:
        print(f"could not move ~/{name} to ~/.flowera/{name}: {e}")
PY

# The help-notes directory was renamed to querytext when the "help"
# terminology became "query" throughout the app. A volume that still has
# the old name - including one that just landed there from the legacy move
# above - gets it renamed in place, merged the same non-destructive way.
python3 - "$HOME/.flowera" >> "$LOG" 2>&1 <<'PY'
import os, sys

flowera = sys.argv[1]
old, new = os.path.join(flowera, "helptext"), os.path.join(flowera, "querytext")
if os.path.isdir(old) and not os.path.islink(old):
    try:
        if not os.path.exists(new):
            os.rename(old, new)
        else:
            for entry in os.listdir(old):
                if not os.path.lexists(os.path.join(new, entry)):
                    os.rename(os.path.join(old, entry), os.path.join(new, entry))
            os.rmdir(old)
        print("moved ~/.flowera/helptext to ~/.flowera/querytext")
    except OSError as e:
        print(f"could not move ~/.flowera/helptext to ~/.flowera/querytext: {e}")
PY

setsid python3 /opt/vm_server.py >> "$LOG" 2>&1 < /dev/null &

echo "[$(date)] vm_server.py started as background daemon (pid $!)" >> "$LOG"

# TheBot harness only - its soul binary is a persistent HTTP server
# thebot_harness.py talks to (see flowera_agent/thebot_harness.py), not a
# per-task CLI like claude, so it needs to already be running before any
# task arrives. Harmless to skip entirely for the other two harnesses -
# no point paying its startup cost when nothing will ever talk to it.
if [ "$AGENT_HARNESS" = "thebot" ] && [ -x /opt/soul ]; then
    # /home/headless/.flowera/ai is TheBot's fixed default AI workdirectory -
    # every agent run cds here and works here, regardless of which real
    # folder a task happens to be scoped to; L1/L2/the Data RAG stay global.
    export WORKSPACE_ROOT="/home/headless/.flowera/ai"
    mkdir -p "$WORKSPACE_ROOT"
    export LLM_PROVIDER="${LLM_PROVIDER:-ollama}"
    # Mirrors buildDesktopEnv's defaults in internal/desktop/docker.go -
    # the host running the control plane, reached over the ExtraHosts entry
    # every desktop already gets. OLLAMA_URL is normally set on the
    # container, so this fallback only applies to a desktop started without
    # one; it replaced a hardcoded Cloudflare quick-tunnel URL that stopped
    # resolving as soon as that tunnel was restarted.
    export OLLAMA_HOST="${OLLAMA_URL:-http://host.docker.internal:11434}"
    export CHAT_MODEL="${OLLAMA_MODEL:-qwen2.5-coder:3b}"
    # Coding builds can run on their own model (see TheBot's internal/llm
    # Models.ForTaskType): a build sends the longest prompts the agent ever
    # produces and wants the largest context window, while chat wants a good
    # conversationalist. Unset means "use CHAT_MODEL for coding too", so a
    # single-model VM behaves exactly as before.
    export CODING_MODEL="${CODING_MODEL:-}"
    # Web research reads its pages into one prompt, and prompt reading is
    # what a CPU-only Ollama is slowest at (see TheBot's internal/skills/
    # web.go). Two pages of 2500 characters keeps a research step to a
    # couple of minutes on a laptop CPU; raise both on a GPU host.
    export THEBOT_WEB_PAGES="${THEBOT_WEB_PAGES:-2}"
    export THEBOT_WEB_PAGE_CHARS="${THEBOT_WEB_PAGE_CHARS:-2500}"
    export SOUL_PORT="4900"
    export DOCLING_SCRIPT="/opt/thebot-scripts/docling_convert.py"
    setsid /opt/soul >> /tmp/thebot-soul.log 2>&1 < /dev/null &
    echo "[$(date)] soul (TheBot) started as background daemon (pid $!)" >> "$LOG"
fi