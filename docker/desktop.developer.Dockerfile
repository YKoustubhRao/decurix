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

# Productivity + developer tools
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
        # Shell / CLI
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
        # Build tools
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

# Node.js 24 LTS/current line
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

# Flowera agent
COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh
# TheBot harness's soul binary - built above from real source, started
# conditionally by start_agent_terminal.sh only when AGENT_HARNESS=thebot.
COPY --from=thebot-builder /soul /opt/soul
# python3-pip is already installed on this image (see the apt block
# above) - docling itself isn't yet, so office-doc RAG ingestion
# (PDF/DOCX/etc) fails per-file, honestly, without breaking anything
# else; plain text/code and every other skill don't need Python at all.
COPY agent/thebot/scripts /opt/thebot-scripts

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh /opt/soul

RUN mkdir -p /home/headless/.config/autostart \
    && chown -R headless:headless /home/headless/.config \
    && chown headless:headless /home/headless

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    && chown headless:headless /home/headless/.config/autostart/flowera-agent.desktop

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