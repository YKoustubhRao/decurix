FROM ubuntu:22.04 AS builder

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    tar \
    xz-utils \
    unzip \
    git \
    build-essential \
    pkg-config \
    python3 \
    python3-pip \
    python3-venv \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /opt/tools
RUN mkdir -p /opt/tools/bin /opt/tools/go /opt/tools/node /opt/tools/rust

# 1. Go toolchain (official linux-amd64)
ARG GO_VERSION=1.26.0
RUN wget -q https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz -O /tmp/go.tar.gz && \
    tar -C /opt/tools -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz && \
    ln -s /opt/tools/go/bin/go /opt/tools/bin/go && \
    ln -s /opt/tools/go/bin/gofmt /opt/tools/bin/gofmt

# 2. Node.js LTS (official linux-x64 binary distribution)
ARG NODE_VERSION=24.0.0
RUN (curl -fsSL https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz -o /tmp/node.tar.gz || \
     curl -fsSL https://nodejs.org/dist/v22.14.0/node-v22.14.0-linux-x64.tar.gz -o /tmp/node.tar.gz) && \
    tar -C /opt/tools/node --strip-components=1 -xzf /tmp/node.tar.gz && \
    rm /tmp/node.tar.gz && \
    ln -s /opt/tools/node/bin/node /opt/tools/bin/node && \
    ln -s /opt/tools/node/bin/npm /opt/tools/bin/npm && \
    ln -s /opt/tools/node/bin/npx /opt/tools/bin/npx

# 3. Rust toolchain (minimal cargo + rustc)
ENV RUSTUP_HOME=/opt/tools/rust
ENV CARGO_HOME=/opt/tools/rust
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --no-modify-path && \
    ln -s /opt/tools/rust/bin/rustc /opt/tools/bin/rustc && \
    ln -s /opt/tools/rust/bin/cargo /opt/tools/bin/cargo

# 4. GitHub CLI (gh)
ARG GH_VERSION=2.67.0
RUN (curl -fsSL https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz -o /tmp/gh.tar.gz && \
     tar -xzf /tmp/gh.tar.gz -C /tmp && \
     mv /tmp/gh_${GH_VERSION}_linux_amd64/bin/gh /opt/tools/bin/gh && \
     rm -rf /tmp/gh*) || true

# 5. Developer CLIs: ripgrep, fd-find, fzf, jq
ARG RG_VERSION=14.1.1
RUN (curl -fsSL https://github.com/BurntSushi/ripgrep/releases/download/${RG_VERSION}/ripgrep-${RG_VERSION}-x86_64-unknown-linux-musl.tar.gz -o /tmp/rg.tar.gz && \
     tar -xzf /tmp/rg.tar.gz -C /tmp && \
     mv /tmp/ripgrep-${RG_VERSION}-x86_64-unknown-linux-musl/rg /opt/tools/bin/rg && \
     rm -rf /tmp/rg*) || true

ARG FD_VERSION=10.2.0
RUN (curl -fsSL https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-x86_64-unknown-linux-musl.tar.gz -o /tmp/fd.tar.gz && \
     tar -xzf /tmp/fd.tar.gz -C /tmp && \
     mv /tmp/fd-v${FD_VERSION}-x86_64-unknown-linux-musl/fd /opt/tools/bin/fd && \
     rm -rf /tmp/fd*) || true

ARG FZF_VERSION=0.60.2
RUN (curl -fsSL https://github.com/junegunn/fzf/releases/download/v${FZF_VERSION}/fzf-${FZF_VERSION}-linux_amd64.tar.gz -o /tmp/fzf.tar.gz && \
     tar -xzf /tmp/fzf.tar.gz -C /opt/tools/bin && \
     rm -f /tmp/fzf.tar.gz) || true

RUN (curl -fsSL https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64 -o /opt/tools/bin/jq && \
     chmod +x /opt/tools/bin/jq) || true

# 6. Claude Code CLI
RUN (HOME=/tmp/claude-installer bash -c 'curl -fsSL https://claude.ai/install.sh | bash' && \
     if [ -f /tmp/claude-installer/.local/bin/claude ]; then \
         mv /tmp/claude-installer/.local/bin/claude /opt/tools/bin/claude; \
     fi && \
     rm -rf /tmp/claude-installer) || true

# 7. AI CLIs: @openai/codex
RUN (PATH="/opt/tools/bin:$PATH" npm install -g --prefix /opt/tools/node @openai/codex && \
     if [ -f /opt/tools/node/bin/codex ]; then \
         ln -s /opt/tools/node/bin/codex /opt/tools/bin/codex; \
     fi) || true

# 8. Environment setup script & sentinel marker
RUN echo '#!/bin/sh' > /opt/tools/env.sh && \
    echo 'export PATH="/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:$PATH"' >> /opt/tools/env.sh && \
    echo 'export GOROOT="/opt/tools/go"' >> /opt/tools/env.sh && \
    echo 'export RUSTUP_HOME="/opt/tools/rust"' >> /opt/tools/env.sh && \
    echo 'export CARGO_HOME="${HOME:-/home/headless}/.cargo"' >> /opt/tools/env.sh && \
    echo 'export NPM_CONFIG_CACHE="${HOME:-/home/headless}/.npm"' >> /opt/tools/env.sh && \
    echo 'export GOPATH="${HOME:-/home/headless}/go"' >> /opt/tools/env.sh && \
    chmod 755 /opt/tools/env.sh && \
    touch /opt/tools/.flowera-tools-ready && \
    chmod -R 755 /opt/tools
# Final stage: minimal populator
FROM alpine:latest
COPY --from=builder /opt/tools /opt/tools
CMD ["sh", "-c", "if [ ! -f /target/.flowera-tools-ready ]; then cp -a /opt/tools/. /target/ && sync; fi"]

