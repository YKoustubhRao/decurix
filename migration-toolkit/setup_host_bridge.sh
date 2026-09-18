#!/usr/bin/env bash
# ==============================================================================
# setup_host_bridge.sh
# Universal Host <-> Linux VM Remote Execution Bridge (macOS / Windows / Linux)
# Enables the Linux VM to invoke apps and access files directly on your local
# host machine with ZERO file copying and ZERO duplicate memory.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_DIR="${SCRIPT_DIR}/host-bridge"

# ANSI Colors
BOLD="\033[1m"
GREEN="\033[32m"
BLUE="\033[34m"
YELLOW="\033[33m"
CYAN="\033[36m"
MAGENTA="\033[35m"
DIM="\033[2m"
RESET="\033[0m"

clear 2>/dev/null || true
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${BOLD}${BLUE}   Universal Host <-> Linux VM Execution Bridge (Mac/Win/Linux)       ${RESET}"
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${DIM}Enables your Linux VM to run apps directly on your local host machine.${RESET}"
echo -e "${DIM}• The binary stays 100% on the local host (no copying executables)${RESET}"
echo -e "${DIM}• Zero duplicate RAM on the Linux VM (process runs in local host memory)${RESET}"
echo -e "${DIM}• Output, files, clipboard, and GUI windows bridge across machines${RESET}\n"

# Ensure all bridge scripts are executable
chmod +x "${BRIDGE_DIR}"/* 2>/dev/null || true

echo -e "${BOLD}${MAGENTA}--- Step 1: Enable OpenSSH Server on your Local Host Machine ---${RESET}"
echo -e "Choose your local host operating system:"
echo -e "  ${CYAN}[1] macOS${RESET}:"
echo -e "      Run in Terminal: ${BOLD}bash ${BRIDGE_DIR}/enable_mac_remote_login.sh${RESET}"
echo -e "      (Or toggle System Settings > General > Sharing > Remote Login: ON)"
echo ""
echo -e "  ${CYAN}[2] Windows (10/11)${RESET}:"
echo -e "      Run in PowerShell as Administrator:"
echo -e "      ${BOLD}powershell -ExecutionPolicy Bypass -File ${BRIDGE_DIR}/enable_windows_ssh_server.ps1${RESET}"
echo ""
echo -e "  ${CYAN}[3] Linux Desktop${RESET}:"
echo -e "      Run in Terminal: ${BOLD}bash ${BRIDGE_DIR}/enable_linux_sshd.sh${RESET}"

echo -e "\n${BOLD}${MAGENTA}--- Step 2: Connect to your Linux VM with the Bridge Tunnel ---${RESET}"
echo -e "From your local machine (Mac / Windows WSL / Linux), run:"
echo -e "  ${BOLD}${CYAN}bash ${BRIDGE_DIR}/connect-to-vm-with-bridge.sh user@<linux-vm-ip>${RESET}"
echo -e "  ${DIM}(Or run: ssh -R 2222:localhost:22 user@<linux-vm-ip>)${RESET}"

echo -e "\n${BOLD}${MAGENTA}--- Step 3: Run Local Host Apps from the Linux VM ---${RESET}"
echo -e "Once connected to the Linux VM, use any of these commands:"
echo -e "  ${GREEN}host-run <command>${RESET}   - Run any local host command (brew, pbcopy, powershell, notepad, etc.)"
echo -e "  ${GREEN}host-open <url/file>${RESET} - Opens a link or file directly on your local host screen"
echo -e "  ${GREEN}host-clip${RESET}            - Pipes VM output to your local host clipboard"
echo -e "  ${GREEN}host-mount${RESET}           - Mounts host filesystem to /mnt/host with zero file duplication"
echo -e "  ${DIM}(Aliases: win-run, mac-run, win-mount work identically)${RESET}"

echo -e "\n${BOLD}${GREEN}======================================================================${RESET}"
echo -e "${BOLD}${GREEN} Setup guide and files ready in: ${BRIDGE_DIR}${RESET}"
echo -e "${BOLD}${GREEN}======================================================================${RESET}"

