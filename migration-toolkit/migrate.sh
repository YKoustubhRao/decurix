#!/usr/bin/env bash
# ==============================================================================
# migrate.sh
# Main interactive wizard to migrate local desktop sessions, Google tokens,
# toolchains, and apps to a VM with zero duplicate memory footprint.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ANSI Colors
BOLD="\033[1m"
GREEN="\033[32m"
BLUE="\033[34m"
YELLOW="\033[33m"
CYAN="\033[36m"
RESET="\033[0m"

clear 2>/dev/null || true
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${BOLD}${BLUE}   Local Desktop -> VM Migration Wizard (Zero-Duplicate Memory)       ${RESET}"
echo -e "${BOLD}${BLUE}======================================================================${RESET}"

echo -e "\n${BOLD}This wizard will help you:${RESET}"
echo -e "  1. Export local app manifests and inspect session stores."
echo -e "  2. Selectively copy Google Chrome logins, tokens, keys, and workspace."
echo -e "  3. Prevent duplicate memory on the VM using kernel page cache + KSM deduplication."
echo ""

# Make all scripts executable
chmod +x "${SCRIPT_DIR}"/*.sh

# Step 1: Mode Selection
echo -e "${BOLD}Select Migration Mode:${RESET}"
echo -e "  ${CYAN}[1] Step-by-Step Discovery (Recommended)${RESET} - Default Clean/Empty VM: scans & asks for each app, token, and file one by one"
echo -e "  ${CYAN}[2] Itemized Table Selector${RESET} - View full table of detected apps, sessions, and toolchains; select by number"
echo -e "  ${CYAN}[3] Universal Host Remote Bridge${RESET} - Use Mac / Windows / Linux host apps from Linux VM (Zero copy, zero duplicate RAM)"
echo -e "  ${CYAN}[4] Quick Category Mode${RESET} - Bulk transfer by high-level category"
echo ""
read -rp "$(echo -e "${CYAN}Choose mode [1]: ${RESET}")" MODE_CHOICE
MODE_CHOICE="${MODE_CHOICE:-1}"

case "${MODE_CHOICE}" in
    1)
        exec "${SCRIPT_DIR}/interactive_discover.sh"
        ;;
    2)
        exec "${SCRIPT_DIR}/choice_migration.sh"
        ;;
    3)
        exec "${SCRIPT_DIR}/setup_host_bridge.sh"
        ;;
    4)
        # Step 2: Destination VM details
        read -rp "$(echo -e "${CYAN}Enter target VM destination (e.g. user@1.2.3.4 or hostname): ${RESET}")" VM_HOST
        if [ -z "${VM_HOST}" ]; then
            echo -e "${YELLOW}No host provided. Exiting.${RESET}"
            exit 1
        fi

        read -rp "$(echo -e "${CYAN}Enter SSH port [default: 22]: ${RESET}")" VM_PORT
        VM_PORT="${VM_PORT:-22}"

        # Dry Run option
        echo -e "\n${BOLD}Do you want to run in DRY-RUN mode first?${RESET}"
        echo -e "  (Dry-run shows exact files to be transferred without making any changes)"
        read -rp "$(echo -e "${CYAN}Perform dry-run first? (y/N): ${RESET}")" DRY_CHOICE
        DRY_OPT=""
        if [[ "${DRY_CHOICE}" =~ ^[yY] ]]; then
            DRY_OPT="--dry-run"
        fi

        echo -e "\n${BOLD}${BLUE}>>> Running Manifest Exporter...${RESET}"
        "${SCRIPT_DIR}/export_manifest.sh"

        echo -e "\n${BOLD}${BLUE}>>> Launching Modular Transfer...${RESET}"
        "${SCRIPT_DIR}/transfer_to_vm.sh" --host "${VM_HOST}" --port "${VM_PORT}" ${DRY_OPT}
        ;;
    *)
        echo -e "${YELLOW}Invalid option. Defaulting to Step-by-Step Discovery.${RESET}"
        exec "${SCRIPT_DIR}/interactive_discover.sh"
        ;;
esac
