#!/bin/bash

# Adaptively wait for X11 display :1 to be ready (up to 15s max), proceeding immediately once available
for i in $(seq 1 150); do
    if [ -S /tmp/.X11-unix/X1 ] || (which xdpyinfo >/dev/null 2>&1 && xdpyinfo -display :1 >/dev/null 2>&1); then
        break
    fi
    sleep 0.1
done

LOG=/tmp/decurix-startup.log

echo "[$(date)] Starting Decurix agent" >> "$LOG"

export DISPLAY=:1

# Only this desktop's own user may open the display. "xhost +" turned X access control off, so anything that could
# reach the X server - another container on the Docker network, if Xvnc listens on TCP - could drive this screen
# around the approval gate (review 2026-11-03, N-F2). Every process that drives the screen (the agent, the gui
# command, soul, the control plane's docker exec) runs as the desktop's user, headless.
which xhost >/dev/null 2>&1 && { xhost - >/dev/null 2>&1; xhost +SI:localuser:headless >/dev/null 2>&1; } || true

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
if [ -f /opt/decurix-chromium.init ]; then
    cp /opt/decurix-chromium.init "$HOME/.chromium-browser.init" 2>> "$LOG" \
        || echo "[$(date)] could not install Chromium launch flags" >> "$LOG"
fi

# The browser is not started at boot: idle with no window it was 224 MB of an
# idle desktop's 510 (measured 2026-10-30), the largest part of it. A request
# warms it as it starts (computer/browser.warm), overlapping the cold start with
# the model's first call, and closes it again if nothing opened a page. A person
# clicking a web app's icon first waits the cold start once (a few seconds).
# DECURIX_PREWARM_CHROMIUM=1 still starts it at boot.
if [ "${DECURIX_PREWARM_CHROMIUM:-}" = "1" ] && which chromium-browser >/dev/null 2>&1; then
    setsid chromium-browser --no-startup-window >> /tmp/chromium-prewarm.log 2>&1 < /dev/null &
    echo "[$(date)] Chromium pre-warmed in background (pid $!)" >> "$LOG"
fi

# Clear out core dumps a crashing Chromium already left behind, before core
# dumps were switched off (see agent/chromium-browser.init). Only files that
# are named core.<pid> AND are ELF core dumps - a file that merely shares
# the name is left alone.
if ls "$HOME"/core.* "$HOME/Desktop"/core.* >/dev/null 2>&1; then
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
fi

# The base image puts its own README and "version sticker" on the desktop.
# They are about the base image, not this product, and sat among the icons a
# person uses. Only those exact two: the README link into the base image's
# readme.md and the sticker launcher; anything a person put there stays.
if [ -L "$HOME/Desktop/README" ] && [ "$(readlink "$HOME/Desktop/README")" = "$HOME/readme.md" ]; then
    rm -f "$HOME/Desktop/README"
fi
if [ -f "$HOME/Desktop/versionsticker.desktop" ] && grep -q "version_sticker" "$HOME/Desktop/versionsticker.desktop" 2>/dev/null; then
    rm -f "$HOME/Desktop/versionsticker.desktop"
fi

# LibreOffice's list of documents to recover. A desktop's restart ends LibreOffice without ceremony, and the next
# start - the worker's, or a person's from the menu - opens on a "Document Recovery" dialog that blocks everything
# until answered: found live (2026-10-15), it stood in front of task after task. A worker's half-made documents are
# not something to recover; the files it saved are on disk. (The worker's own launches also pass --norestore.)
LO_REG="$HOME/.config/libreoffice/4/user/registrymodifications.xcu"
if [ -f "$LO_REG" ] && grep -q 'Office.Recovery/RecoveryList' "$LO_REG" 2>/dev/null; then
    sed -i '/org.openoffice.Office.Recovery\/RecoveryList/d' "$LO_REG"
fi

# The base image's "Chromium Web Browser" icon, on a desktop created before the
# browser became Google Chrome (agent/install-browser.sh): it still starts the
# browser, but its picture went with the old package. Replaced by the image's
# own icon - only that exact file; an icon a person made stays.
browser_icon="$HOME/Desktop/chromium-browser.desktop"
if [ -f /opt/decurix-desktop-browser.desktop ] && [ -f "$browser_icon" ] \
    && grep -q "^Name=Chromium Web Browser$" "$browser_icon" 2>/dev/null; then
    cp /opt/decurix-desktop-browser.desktop "$browser_icon" && chmod 755 "$browser_icon"
fi

# The agent's own directories live under ~/.decurix, so ~/work is the only
# one it adds to the home a human browses. A volume from before that still
# has them at the top level: move them in before anything that uses them
# starts, so worker memory, the audit trail and TheBot's stores carry over.
legacy_dirs="command helptext final memory audit"
[ "$AGENT_HARNESS" = "thebot" ] && legacy_dirs="$legacy_dirs ai"
has_legacy=0
for d in $legacy_dirs; do
    if [ -d "$HOME/$d" ] && [ ! -L "$HOME/$d" ]; then
        has_legacy=1
        break
    fi
done
if [ "$has_legacy" -eq 1 ]; then
python3 - "$HOME" $legacy_dirs >> "$LOG" 2>&1 <<'PY'
import os, sys

home, names = sys.argv[1], sys.argv[2:]
decurix = os.path.join(home, ".decurix")
for name in names:
    old, new = os.path.join(home, name), os.path.join(decurix, name)
    if os.path.islink(old) or not os.path.isdir(old):
        continue
    try:
        os.makedirs(decurix, exist_ok=True)
        if not os.path.exists(new):
            os.rename(old, new)
        else:
            for entry in os.listdir(old):
                if not os.path.lexists(os.path.join(new, entry)):
                    os.rename(os.path.join(old, entry), os.path.join(new, entry))
            os.rmdir(old)
        print(f"moved ~/{name} to ~/.decurix/{name}")
    except OSError as e:
        print(f"could not move ~/{name} to ~/.decurix/{name}: {e}")
PY
fi

# The help-notes directory was renamed to querytext when the "help"
# terminology became "query" throughout the app. A volume that still has
# the old name - including one that just landed there from the legacy move
# above - gets it renamed in place, merged the same non-destructive way.
if [ -d "$HOME/.decurix/helptext" ] && [ ! -L "$HOME/.decurix/helptext" ]; then
python3 - "$HOME/.decurix" >> "$LOG" 2>&1 <<'PY'
import os, sys

decurix = sys.argv[1]
old, new = os.path.join(decurix, "helptext"), os.path.join(decurix, "querytext")
if os.path.isdir(old) and not os.path.islink(old):
    try:
        if not os.path.exists(new):
            os.rename(old, new)
        else:
            for entry in os.listdir(old):
                if not os.path.lexists(os.path.join(new, entry)):
                    os.rename(os.path.join(old, entry), os.path.join(new, entry))
            os.rmdir(old)
        print("moved ~/.decurix/helptext to ~/.decurix/querytext")
    except OSError as e:
        print(f"could not move ~/.decurix/helptext to ~/.decurix/querytext: {e}")
PY
fi

# Run background agent daemon with nice -n 10 so interactive GUI (Xvnc, Xorg,
# xfce4, terminal) always has scheduling priority over background agent reasoning.
setsid nice -n 10 python3 /opt/vm_server.py >> "$LOG" 2>&1 < /dev/null &

echo "[$(date)] vm_server.py started as background daemon (pid $!)" >> "$LOG"

# TheBot harness only - its soul binary is a persistent HTTP server
# thebot_harness.py talks to (see decurix_agent/thebot_harness.py), not a
# per-task CLI like claude, so it needs to already be running before any
# task arrives. Harmless to skip entirely for the other two harnesses -
# no point paying its startup cost when nothing will ever talk to it.
if [ "$AGENT_HARNESS" = "thebot" ] && [ -x /opt/soul ]; then
    # /home/headless/.decurix/ai is TheBot's fixed default AI workdirectory -
    # every agent run cds here and works here, regardless of which real
    # folder a task happens to be scoped to; L1/L2/the Data RAG stay global.
    export WORKSPACE_ROOT="/home/headless/.decurix/ai"
    mkdir -p "$WORKSPACE_ROOT"
    export LLM_PROVIDER="${LLM_PROVIDER:-ollama}"
    # Mirrors buildDesktopEnv's defaults in internal/desktop/docker.go -
    # the host running the control plane, reached over the ExtraHosts entry
    # every desktop already gets. OLLAMA_URL is normally set on the
    # container, so this fallback only applies to a desktop started without
    # one; it replaced a hardcoded Cloudflare quick-tunnel URL that stopped
    # resolving as soon as that tunnel was restarted.
    export OLLAMA_HOST="${OLLAMA_URL:-http://host.docker.internal:11434}"
    export CHAT_MODEL="${OLLAMA_MODEL:-devstral-small-2:24b}"
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
    # Embedded in Decurix: soul's acquired skills and coding harness stay off (owner decision O-8, review T-07/T-08).
    export THEBOT_STANDALONE=0
    export DOCLING_SCRIPT="/opt/thebot-scripts/docling_convert.py"
    setsid nice -n 10 /opt/soul >> /tmp/thebot-soul.log 2>&1 < /dev/null &
    echo "[$(date)] soul (TheBot) started as background daemon (pid $!)" >> "$LOG"
fi