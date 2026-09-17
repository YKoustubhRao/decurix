#!/usr/bin/env bash
# ==============================================================================
# create_app_bridge.sh
# Creates a zero-RAM desktop launcher and CLI wrapper on this Linux VM that
# executes an application residing on the local host (Windows / macOS / Linux).
# ==============================================================================

set -euo pipefail

if [ $# -lt 3 ]; then
    echo "Usage: create_app_bridge.sh <app_slug> <app_display_name> <host_command>"
    echo "Example:"
    echo "  create_app_bridge.sh win-code \"Visual Studio Code\" \"code.cmd\""
    echo "  create_app_bridge.sh win-chrome \"Google Chrome\" \"chrome.exe\""
    echo "  create_app_bridge.sh mac-chrome \"Google Chrome\" \"open -a 'Google Chrome'\""
    exit 1
fi

APP_SLUG="$1"
APP_NAME="$2"
HOST_CMD="$3"

BIN_PATH="/usr/local/bin/${APP_SLUG}"

# 1. Create executable wrapper in /usr/local/bin
sudo tee "${BIN_PATH}" > /dev/null << EOF
#!/usr/bin/env bash
# Wrapper to run ${APP_NAME} on local host from Linux VM (Zero VM RAM)
exec host-run ${HOST_CMD} "\$@"
EOF
sudo chmod +x "${BIN_PATH}"

# 2. Create XDG .desktop launcher
DESKTOP_DIR="${HOME}/.local/share/applications"
mkdir -p "${DESKTOP_DIR}"
DESKTOP_FILE="${DESKTOP_DIR}/${APP_SLUG}.desktop"

cat << EOF > "${DESKTOP_FILE}"
[Desktop Entry]
Version=1.0
Type=Application
Name=${APP_NAME} (Host)
Comment=Run ${APP_NAME} on local host with zero duplicate VM RAM
Exec=${BIN_PATH} %U
Icon=application-x-executable
Terminal=false
Categories=Utility;Development;Network;
EOF
chmod +x "${DESKTOP_FILE}"

# 3. Create desktop icon on Desktop if folder exists
if [ -d "${HOME}/Desktop" ]; then
    cp -f "${DESKTOP_FILE}" "${HOME}/Desktop/${APP_SLUG}.desktop"
    chmod +x "${HOME}/Desktop/${APP_SLUG}.desktop"
fi

echo "✓ Created host application bridge launcher: ${APP_NAME} -> ${BIN_PATH}"

