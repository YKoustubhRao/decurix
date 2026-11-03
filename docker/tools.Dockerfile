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
RUN curl -fsSL https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz -o /tmp/node.tar.gz && \
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
     rm -rf /tmp/gh*)

# 5. Developer CLIs: ripgrep, fd-find, fzf, jq
ARG RG_VERSION=14.1.1
RUN (curl -fsSL https://github.com/BurntSushi/ripgrep/releases/download/${RG_VERSION}/ripgrep-${RG_VERSION}-x86_64-unknown-linux-musl.tar.gz -o /tmp/rg.tar.gz && \
     tar -xzf /tmp/rg.tar.gz -C /tmp && \
     mv /tmp/ripgrep-${RG_VERSION}-x86_64-unknown-linux-musl/rg /opt/tools/bin/rg && \
     rm -rf /tmp/rg*)

ARG FD_VERSION=10.2.0
RUN (curl -fsSL https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-x86_64-unknown-linux-musl.tar.gz -o /tmp/fd.tar.gz && \
     tar -xzf /tmp/fd.tar.gz -C /tmp && \
     mv /tmp/fd-v${FD_VERSION}-x86_64-unknown-linux-musl/fd /opt/tools/bin/fd && \
     rm -rf /tmp/fd*)

ARG FZF_VERSION=0.60.2
RUN (curl -fsSL https://github.com/junegunn/fzf/releases/download/v${FZF_VERSION}/fzf-${FZF_VERSION}-linux_amd64.tar.gz -o /tmp/fzf.tar.gz && \
     tar -xzf /tmp/fzf.tar.gz -C /opt/tools/bin && \
     rm -f /tmp/fzf.tar.gz)

RUN (curl -fsSL https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64 -o /opt/tools/bin/jq && \
     chmod +x /opt/tools/bin/jq)

# 6. Claude Code CLI
RUN (HOME=/tmp/claude-installer bash -c 'curl -fsSL https://claude.ai/install.sh | bash' && \
     mv /tmp/claude-installer/.local/bin/claude /opt/tools/bin/claude && \
     rm -rf /tmp/claude-installer)

# 7. AI CLIs: @openai/codex
RUN (PATH="/opt/tools/bin:$PATH" npm install -g --prefix /opt/tools/node @openai/codex && \
     ln -s /opt/tools/node/bin/codex /opt/tools/bin/codex)

# 8. Environment setup script & sentinel marker. The sentinel holds a hash of the whole tree, so a rebuilt tools
# image (a new toolchain) replaces what an existing volume holds instead of being skipped for good (B-24).
RUN echo '#!/bin/sh' > /opt/tools/env.sh && \
    echo 'export PATH="/opt/tools/bin:/opt/tools/go/bin:/opt/tools/node/bin:/opt/tools/rust/bin:$PATH"' >> /opt/tools/env.sh && \
    echo 'export GOROOT="/opt/tools/go"' >> /opt/tools/env.sh && \
    echo 'export RUSTUP_HOME="/opt/tools/rust"' >> /opt/tools/env.sh && \
    echo 'export CARGO_HOME="${HOME:-/home/headless}/.cargo"' >> /opt/tools/env.sh && \
    echo 'export NPM_CONFIG_CACHE="${HOME:-/home/headless}/.npm"' >> /opt/tools/env.sh && \
    echo 'export GOPATH="${HOME:-/home/headless}/go"' >> /opt/tools/env.sh && \
    chmod 755 /opt/tools/env.sh && \
    chmod -R 755 /opt/tools && \
    tar -c --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C /opt tools | sha256sum | cut -d' ' -f1 \
        > /tmp/decurix-tools-ready && mv /tmp/decurix-tools-ready /opt/tools/.decurix-tools-ready
# Final stage: minimal populator, pinned like docker/Dockerfile (the digest is the owner's to add once pulled).
# The control plane runs this CMD as is (internal/desktop/tools.go): the one place the copy rule lives. A volume
# whose sentinel differs from this image's is emptied and filled again; one that matches is left alone.
FROM alpine:3.24
COPY --from=builder /opt/tools /opt/tools
# The ready marker is taken away first and written back last, after everything else is copied and synced, so a copy
# that fails partway (a full disk, a kill) leaves no marker and the next run fills the volume again - never a matching
# marker over half a tree (review RW1-D-02). A refresh (a marker that differs) waits while desktops are running
# (DECURIX_DESKTOPS_RUNNING=1, set by the control plane): their /opt/tools would vanish mid-task.
CMD ["sh", "-c", "set -e; R=.decurix-tools-ready; if cmp -s /opt/tools/$R /target/$R; then exit 0; fi; if [ -e /target/$R ] && [ \"$DECURIX_DESKTOPS_RUNNING\" = 1 ]; then echo 'tools update waits for idle desktops'; exit 0; fi; rm -f /target/$R; sync; find /target -mindepth 1 -maxdepth 1 -exec rm -rf {} +; for f in /opt/tools/* /opt/tools/.[!.]*; do [ -e \"$f\" ] || continue; [ \"${f##*/}\" = $R ] && continue; cp -a \"$f\" /target/; done; sync; cp /opt/tools/$R /target/$R.new; sync; mv /target/$R.new /target/$R; sync"]

