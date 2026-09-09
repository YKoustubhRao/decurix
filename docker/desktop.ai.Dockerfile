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

# Flowera agent
COPY agent/vm_server.py /opt/vm_server.py
COPY agent/flowera_agent /opt/flowera_agent
COPY agent/start_agent_terminal.sh /opt/start_agent_terminal.sh

RUN chmod +x /opt/vm_server.py /opt/start_agent_terminal.sh

RUN mkdir -p /home/headless/.config/autostart \
    && chown -R headless:headless /home/headless/.config \
    && chown headless:headless /home/headless

COPY agent/flowera-agent.desktop /home/headless/.config/autostart/flowera-agent.desktop

RUN chmod 644 /home/headless/.config/autostart/flowera-agent.desktop \
    && chown headless:headless /home/headless/.config/autostart/flowera-agent.desktop

USER headless

# Claude Code CLI (native binary; used by the "claude" agent harness)
RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/home/headless/.local/bin:${PATH}"

ENTRYPOINT ["/usr/bin/tini", "--", "/dockerstartup/startup.sh"]