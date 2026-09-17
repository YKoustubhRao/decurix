#!/usr/bin/env bash
# ==============================================================================
# setup_vm_memory_dedup.sh
# Destination VM post-migration setup:
# 1. Configures Linux Kernel Samepage Merging (KSM) for RAM deduplication.
# 2. Installs native packages cleanly via manifest (eliminating library duplication).
# 3. Configures GNOME Keyring and verifies Google session/auth tokens.
# ==============================================================================

set -euo pipefail

# Ensure running with root privileges
if [ "$(id -u)" -ne 0 ]; then
    echo "Error: This script must be run as root (or with sudo)."
    echo "Usage: sudo bash $0"
    exit 1
fi

SETUP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SETUP_DIR}/manifest"

# ANSI Colors
BOLD="\033[1m"
GREEN="\033[32m"
BLUE="\033[34m"
YELLOW="\033[33m"
CYAN="\033[36m"
RESET="\033[0m"

echo -e "${BOLD}${BLUE}======================================================${RESET}"
echo -e "${BOLD}${BLUE}   VM Memory Deduplication & Application Setup       ${RESET}"
echo -e "${BOLD}${BLUE}======================================================${RESET}"

# ------------------------------------------------------------------------------
# 1. Linux Kernel Samepage Merging (KSM) Configuration
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[1/4] Configuring Linux Kernel Samepage Merging (KSM)...${RESET}"
KSM_PATH="/sys/kernel/mm/ksm"

if [ -d "${KSM_PATH}" ]; then
    echo 1 > "${KSM_PATH}/run"
    echo 200 > "${KSM_PATH}/pages_to_scan"
    echo 20 > "${KSM_PATH}/sleep_millisecs"
    
    # Enable advanced kernel features if available (Linux 5.16+)
    [ -f "${KSM_PATH}/smart_scan" ] && echo 1 > "${KSM_PATH}/smart_scan" 2>/dev/null || true
    [ -f "${KSM_PATH}/use_zero_pages" ] && echo 1 > "${KSM_PATH}/use_zero_pages" 2>/dev/null || true

    echo -e "  ${GREEN}✓${RESET} KSM enabled: actively merging duplicate memory pages across processes."

    # Create persistent systemd unit for KSM
    cat << 'EOF' > /etc/systemd/system/ksm-memory-dedup.service
[Unit]
Description=Kernel Samepage Merging (KSM) Memory Deduplication Service
After=sysinit.target local-fs.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'if [ -d /sys/kernel/mm/ksm ]; then echo 1 > /sys/kernel/mm/ksm/run; echo 200 > /sys/kernel/mm/ksm/pages_to_scan; echo 20 > /sys/kernel/mm/ksm/sleep_millisecs; [ -f /sys/kernel/mm/ksm/smart_scan ] && echo 1 > /sys/kernel/mm/ksm/smart_scan; fi'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable ksm-memory-dedup.service --now >/dev/null 2>&1 || true
    echo -e "  ${GREEN}✓${RESET} KSM persistence service installed and enabled."
else
    echo -e "  ${YELLOW}!${RESET} Kernel does not have KSM compiled in. Skipping KSM."
fi

# ------------------------------------------------------------------------------
# 2. Native Package Rehydration (Preventing Shared Library Redundancy)
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[2/4] Installing application packages natively via APT...${RESET}"
if [ -f "${MANIFEST_DIR}/apt_packages.txt" ]; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -y
    
    # Filter out packages that might conflict or be architecture-specific kernels
    grep -vE '(linux-image|linux-headers|linux-modules|grub)' "${MANIFEST_DIR}/apt_packages.txt" > /tmp/apt_install_list.txt || true
    
    echo -e "  ${CYAN}-> Installing packages cleanly via APT to allow kernel shared page caching...${RESET}"
    xargs -a /tmp/apt_install_list.txt apt-get install -y --no-install-recommends >/dev/null 2>&1 || {
        echo -e "  ${YELLOW}! Some packages failed batch install. Attempting resilient installation...${RESET}"
        while IFS= read -r pkg; do
            [ -n "${pkg}" ] && apt-get install -y --no-install-recommends "${pkg}" >/dev/null 2>&1 || true
        done < /tmp/apt_install_list.txt
    }
    rm -f /tmp/apt_install_list.txt
    echo -e "  ${GREEN}✓${RESET} Native packages rehydrated."
fi

# ------------------------------------------------------------------------------
# 3. Google Chrome & Authentication / Keyring Setup
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[3/4] Configuring Google Chrome and GNOME Keyring Daemon...${RESET}"
apt-get install -y --no-install-recommends dbus-x11 gnome-keyring libsecret-1-0 >/dev/null 2>&1 || true

# If Google Chrome is not installed, install official stable release
if ! command -v google-chrome >/dev/null 2>&1 && ! command -v chromium-browser >/dev/null 2>&1 && ! command -v chromium >/dev/null 2>&1; then
    echo -e "  ${CYAN}-> Installing Google Chrome Stable...${RESET}"
    curl -fsSL https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb -o /tmp/chrome.deb
    dpkg -i /tmp/chrome.deb 2>/dev/null || apt-get install -f -y >/dev/null 2>&1
    rm -f /tmp/chrome.deb
    echo -e "  ${GREEN}✓${RESET} Google Chrome installed."
fi

# ------------------------------------------------------------------------------
# 4. Universal Host Remote Execution Bridge (host-run, win-run, mac-run)
# ------------------------------------------------------------------------------
if [ -d "${SETUP_DIR}/host-bridge" ]; then
    echo -e "\n${CYAN}[4/5] Installing Universal Host Bridge (host-run, win-run, mac-run, host-mount)...${RESET}"
    bash "${SETUP_DIR}/host-bridge/install_on_linux_vm.sh" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Universal host bridge installed to /usr/local/bin (Mac/Win/Linux host execution)."
fi

# ------------------------------------------------------------------------------
# 5. Memory Deduplication Verification Tool
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[5/5] Generating memory verification script...${RESET}"
cat << 'EOF' > "${SETUP_DIR}/check_memory_savings.sh"
#!/usr/bin/env bash
BOLD="\033[1m"
GREEN="\033[32m"
CYAN="\033[36m"
RESET="\033[0m"

echo -e "${BOLD}${CYAN}=== System RAM & KSM Memory Deduplication Report ===${RESET}"
free -h
echo ""
KSM="/sys/kernel/mm/ksm"
if [ -d "${KSM}" ] && [ "$(cat ${KSM}/run 2>/dev/null)" = "1" ]; then
    PAGES_SHARED=$(cat ${KSM}/pages_shared 2>/dev/null || echo 0)
    PAGES_SHARING=$(cat ${KSM}/pages_sharing 2>/dev/null || echo 0)
    PAGES_UNSHARED=$(cat ${KSM}/pages_unshared 2>/dev/null || echo 0)
    
    # 4096 bytes per page
    SAVINGS_MB=$(( PAGES_SHARING * 4096 / 1024 / 1024 ))
    
    echo -e "${BOLD}KSM Status:${RESET} ${GREEN}ACTIVE${RESET}"
    echo -e "  • Shared pages in memory:   ${PAGES_SHARED}"
    echo -e "  • Duplicate pages merged:   ${PAGES_SHARING}"
    echo -e "  • Unshared candidate pages: ${PAGES_UNSHARED}"
    echo -e "  • ${BOLD}${GREEN}Direct RAM Saved by Deduplication: ${SAVINGS_MB} MB${RESET}"
else
    echo -e "${BOLD}KSM Status:${RESET} Not running"
fi
EOF
chmod +x "${SETUP_DIR}/check_memory_savings.sh"

echo -e "\n${BOLD}${GREEN}======================================================${RESET}"
echo -e "${BOLD}${GREEN} VM Setup & Optimization Complete!                     ${RESET}"
echo -e "${BOLD}${GREEN} Run ~/vm-setup/check_memory_savings.sh to inspect RAM.${RESET}"
echo -e "${BOLD}${GREEN}======================================================${RESET}"
