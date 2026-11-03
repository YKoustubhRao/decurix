#!/bin/bash
#
# Decurix macOS Setup: installs prerequisites (Homebrew, Docker Desktop,
# Ollama, cloudflared, QEMU, jq, git) and configures host bindings.

set -e

echo "========================================"
echo "        Decurix macOS Setup"
echo "========================================"
echo ""

# Check for Homebrew
if ! command -v brew >/dev/null 2>&1; then
    echo "Homebrew is not installed. Please install Homebrew first:"
    echo '  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"'
    exit 1
fi

echo "=== Installing base dependencies ==="
brew install git jq qemu || true

echo ""
echo "=== Checking Docker Desktop ==="
if command -v docker >/dev/null 2>&1 && [ -d "/Applications/Docker.app" ]; then
    echo "Docker Desktop is already installed."
else
    echo "Installing Docker Desktop via Homebrew Cask..."
    brew install --cask docker
fi

echo ""
echo "=== Checking Ollama ==="
if command -v ollama >/dev/null 2>&1; then
    echo "Ollama is already installed ($(ollama --version 2>/dev/null || echo 'installed'))."
else
    echo "Installing Ollama via Homebrew..."
    brew install ollama
fi

echo ""
echo "=== Configuring Ollama host binding ==="
# Nothing to change: Docker Desktop forwards host.docker.internal to this
# Mac's 127.0.0.1, where Ollama listens by default. Ollama has no login, so it
# is never put on every address. An earlier version of this script did that;
# it is taken back here.
if [ "$(launchctl getenv OLLAMA_HOST 2>/dev/null)" = "0.0.0.0" ]; then
    launchctl unsetenv OLLAMA_HOST || true
    echo "Removed launchctl's OLLAMA_HOST set to 0.0.0.0 (an earlier Decurix set it); restart the Ollama app."
fi
if [ -f "$HOME/.zshrc" ] && grep -qx 'export OLLAMA_HOST="0[.]0[.]0[.]0"' "$HOME/.zshrc"; then
    zshrc_tmp="$(mktemp "${TMPDIR:-/tmp}/decurix-zshrc.XXXXXX")"
    grep -vx 'export OLLAMA_HOST="0[.]0[.]0[.]0"' "$HOME/.zshrc" > "$zshrc_tmp" && cat "$zshrc_tmp" > "$HOME/.zshrc" \
        && echo "Removed the OLLAMA_HOST line an earlier Decurix added to ~/.zshrc."
    rm -f "$zshrc_tmp"
fi
echo "Ollama stays on 127.0.0.1; desktops reach it through Docker Desktop."

echo ""
echo "=== Checking Cloudflare cloudflared ==="
if command -v cloudflared >/dev/null 2>&1; then
    echo "cloudflared already installed."
else
    echo "Installing cloudflared via Homebrew..."
    brew install cloudflared || echo "Optional: cloudflared installation skipped or failed."
fi

echo ""
echo "=== Ensuring Docker Desktop is running ==="
if ! docker info >/dev/null 2>&1; then
    echo "Starting Docker Desktop..."
    open -a Docker
    echo "Waiting for Docker daemon to initialize..."
    for i in {1..30}; do
        if docker info >/dev/null 2>&1; then
            echo "Docker is ready."
            break
        fi
        sleep 2
    done
else
    echo "Docker is already running."
fi

echo ""
echo "========================================"
echo "        Verifying Installation"
echo "========================================"
echo ""

echo "Git:"
git --version || true

echo ""
echo "Docker:"
docker --version || true

echo ""
echo "Docker Compose:"
docker compose version || true

echo ""
echo "QEMU:"
qemu-system-x86_64 --version 2>/dev/null | head -n 1 || echo "(qemu installed)"

echo ""
echo "Ollama:"
ollama --version 2>/dev/null || echo "(ollama installed)"

echo ""
echo "Cloudflared:"
cloudflared --version 2>/dev/null || echo "(cloudflared installed)"

echo ""
echo "========================================"
echo " macOS Host Setup Complete"
echo "========================================"
echo ""
echo "Next steps:"
echo "  - For a full clean-slate build (clears old desktops & database):"
echo "      ./scripts/mac/rebuild.sh"
echo ""
echo "  - To open Decurix (retaining existing desktops and data):"
echo "      ./scripts/mac/build.sh"
echo ""
echo "========================================"
