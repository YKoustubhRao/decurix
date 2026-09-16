# TheBot's soul binary, built from its real source (agent/thebot/) rather
# than a pre-committed binary - genuinely part of this repo, not an
# opaque blob. Pure-Go (modernc.org/sqlite, no CGO), one self-contained
# static binary with its whole UI embedded via go:embed. A dedicated
# builder stage rather than the Go toolchain this image installs further
# down for developers' own use (that one's 1.26, a version behind what
# this module needs, and installed into the final image for a different
# purpose entirely).
FROM golang:1.27 AS thebot-builder
WORKDIR /src
COPY agent/thebot/go.mod agent/thebot/go.sum ./
RUN go mod download
COPY agent/thebot/cmd ./cmd
COPY agent/thebot/internal ./internal
COPY agent/thebot/web ./web
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /soul ./cmd/soul

FROM accetto/ubuntu-vnc-xfce-chromium-g3:latest

USER root

# Productivity + developer + AI tooling
RUN apt-get update && \
    apt-get install -y \
        # Office
        libreoffice \
        libreoffice-calc \
        libreoffice-writer \
        libreoffice-impress \
        \
        # Git
        git \
        git-lfs \
        \
        # CLI
        curl \
        wget \
        unzip \
        zip \
        jq \
        tree \
        htop \
        tmux \
        vim \
        nano \
        ripgrep \
        fd-find \
        fzf \
        \
        # Build
        build-essential \
        pkg-config \
        \
        # Python
        python3 \
        python3-pip \
        python3-venv \
        python3-dev \
        \
        # Databases
        sqlite3 \
        postgresql-client \
        redis-tools \
        \
        # Networking
        net-tools \
        iputils-ping \
        dnsutils \
        \
        # Misc
        ca-certificates \
        gnupg \
        lsb-release \
        && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Node.js
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - && \
    apt-get update && \
    apt-get install -y nodejs && \
    npm install -g npm@latest && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        -o /usr/share/keyrings/githubcli-archive-keyring.gpg && \
    chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list && \
    apt-get update && \
    apt-get install -y gh && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Go
RUN wget -q https://go.dev/dl/go1.26.0.linux-amd64.tar.gz \
        -O /tmp/go.tar.gz && \
    rm -rf /usr/local/go && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz

ENV PATH="/usr/local/go/bin:${PATH}"

# Rust
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
        | sh -s -- -y --profile minimal

ENV PATH="/home/headless/.cargo/bin:${PATH}"

# AI CLIs
#
# Install these globally so they are already available
# when the VM starts. (Claude Code is installed separately below via its
# native binary installer, shared with the developer profile.)
RUN npm install -g \
        @openai/codex

# Computer use: what flowera_agent/computer needs to see and work the screen.
# xdotool sends real input (XTEST); python3-xlib reads the framebuffer and the
# window list; at-spi2-core, libatk-adaptor and the Atspi bindings expose GTK,
# XFCE and LibreOffice widgets by role and name. ~3 MB in all. Chromium is read
# over its DevTools port instead (agent/chromium-browser.init), which needs
# nothing installed.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        xdotool \
        python3-xlib \
        python3-gi \
        gir1.2-atspi-2.0 \
        at-spi2-core \
        libatk-adaptor && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# The base image sets NO_AT_BRIDGE=1, which stops every GTK app - the XFCE
# panel, Thunar, mousepad, LibreOffice's GTK front end - from publishing its
# widgets over AT-SPI at all. Without this the accessibility tree is empty.
ENV NO_AT_BRIDGE=0

# A modern look for the desktop a human actually opens: Ubuntu's own Yaru
# theme (GTK + xfwm4, in a red accent to match Flowera's own palette),
# Papirus icons (colorful, actively maintained, much clearer at a glance
# than the base image's flat default set) and the Breeze cursor. Inter is
# a screen-tuned UI font, used in place of the base image's plain "Sans".
# Verified live over VNC: this alone takes the desktop from stock
# Xubuntu-grey to something that looks intentionally designed - see
# flowera-xsettings.xml/flowera-xfwm4.xml below for where it's applied.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        yaru-theme-gtk \
        papirus-icon-theme \
        breeze-cursor-theme \
        fonts-inter \
        xfce4-whiskermenu-plugin && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Flowera agent
COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh
# The `gui` command: the screen as a shell command, for the engines that
# work through shell commands (flowera_agent/computer/cli.py).
COPY agent/gui /usr/local/bin/gui
# TheBot harness's soul binary - built above from real source, started
# conditionally by start_agent_terminal.sh only when AGENT_HARNESS=thebot.
COPY --from=thebot-builder /soul /opt/soul
# python3-pip is already installed on this image (see the apt block
# above) - docling itself isn't yet, so office-doc RAG ingestion
# (PDF/DOCX/etc) fails per-file, honestly, without breaking anything
# else; plain text/code and every other skill don't need Python at all.
COPY agent/thebot/scripts /opt/thebot-scripts
# Default desktop wallpaper, replacing the base image's stock XFCE one -
# end users should see Flowera branding, not generic XFCE art, the moment
# a desktop opens.
COPY agent/flowera-wallpaper.svg /opt/flowera-wallpaper.svg
# The taskbar's Start button icon (flowera-panel.xml's whiskermenu plugin) -
# an absolute path rather than an icon-theme name, so it renders regardless
# of which icon theme is active.
COPY agent/flowera-start-icon.svg /opt/flowera-start-icon.svg
# Chromium's launch flags, without the base image's hardcoded --no-sandbox
# (see agent/chromium-browser.init). Seeded into /home/headless for new
# desktops, and kept in /opt for start_agent_terminal.sh to reinstall into
# existing ones, whose home is a volume this layer never reaches.
COPY agent/chromium-browser.init /opt/flowera-chromium.init
COPY agent/chromium-browser.init /home/headless/.chromium-browser.init

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh /opt/soul /usr/local/bin/gui

RUN mkdir -p /home/headless/.config/autostart \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml \
    /home/headless/.config/xfce4/panel/launcher-3 \
    /home/headless/.config/xfce4/panel/launcher-4 \
    /home/headless/.config/xfce4/panel/launcher-5 \
    && chown -R headless:headless /home/headless/.config \
    && chown headless:headless /home/headless

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop
# Pre-seeds four xfconf channels so XFCE starts already looking like
# Flowera instead of generating the base image's own defaults on first
# launch: the wallpaper (flowera-desktop-wallpaper.xml), the GTK
# theme/icons/font/cursor (flowera-xsettings.xml), the matching
# window-manager theme (flowera-xfwm4.xml) and a single bottom taskbar in
# place of the base image's own top-menu-bar-plus-autohide-dock layout
# (flowera-panel.xml) - a Start button (whiskermenu) with pinned launchers
# at the left, running windows centered, and the system tray/clock at the
# right, the layout a Windows user already knows. Each file only sets the
# properties it cares about - everything else still falls back to
# xfwm4/xfsettingsd's own defaults, verified live by diffing a fresh
# session's full property dump against one seeded with just these.
COPY agent/flowera-desktop-wallpaper.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml
COPY agent/flowera-xsettings.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml
COPY agent/flowera-xfwm4.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml
COPY agent/flowera-panel.xml /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml
# The taskbar's three pinned launchers (Files/Browser/Terminal) - each
# xfce4-panel launcher plugin keeps its own desktop-entry under
# launcher-<plugin-id>/, keyed by the plugin ID hardcoded in
# flowera-panel.xml's own plugin-3/4/5, not by the desktop file's name.
COPY agent/flowera-launcher-files.desktop /home/headless/.config/xfce4/panel/launcher-3/flowera-files.desktop
COPY agent/flowera-launcher-browser.desktop /home/headless/.config/xfce4/panel/launcher-4/flowera-browser.desktop
COPY agent/flowera-launcher-terminal.desktop /home/headless/.config/xfce4/panel/launcher-5/flowera-terminal.desktop

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml \
    /home/headless/.config/xfce4/panel/launcher-3/flowera-files.desktop \
    /home/headless/.config/xfce4/panel/launcher-4/flowera-browser.desktop \
    /home/headless/.config/xfce4/panel/launcher-5/flowera-terminal.desktop \
    /home/headless/.chromium-browser.init /opt/flowera-chromium.init /opt/flowera-start-icon.svg \
    && chown headless:headless \
    /home/headless/.config/autostart/flowera-agent.desktop \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml \
    /home/headless/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml \
    /home/headless/.config/xfce4/panel/launcher-3/flowera-files.desktop \
    /home/headless/.config/xfce4/panel/launcher-4/flowera-browser.desktop \
    /home/headless/.config/xfce4/panel/launcher-5/flowera-terminal.desktop \
    /home/headless/.chromium-browser.init /opt/flowera-start-icon.svg

# Restore the base image's own permissions on /etc/passwd and /etc/group.
#
# Real bug this fixes, found starting a fresh developer VM: the container
# exited immediately with
#   /dockerstartup/vnc_startup.rc: line 106: /dockerstartup/vnc.log: Permission denied
#
# The base image ships those files world-writable (666) on purpose. Its
# own startup runs user_generator.rc, whose ENTIRE body is gated on
# `if [[ -w /etc/passwd ]]` - and that body is what does
# `sudo chown -R $UID /dockerstartup $HOME`, which is the only reason the
# unprivileged headless user can write the VNC logs at all.
#
# Installing packages that create a system user (this image installs a
# lot of them) rewrites /etc/passwd with the system default 644/664. The
# generator then silently skips everything, /dockerstartup stays
# root-owned, vncserver cannot open its log, and the container dies with
# no VNC and no agent - looking like a broken image rather than a
# permission bit. Restoring 666 here, after every install and before the
# USER switch, puts the base image's mechanism back exactly as designed.
RUN chmod 666 /etc/passwd /etc/group

USER headless

# Claude Code CLI (native binary; used by the "claude" agent harness)
RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/home/headless/.local/bin:${PATH}"

ENTRYPOINT ["/usr/bin/tini", "--", "/dockerstartup/startup.sh"]