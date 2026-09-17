#!/usr/bin/env bash
# ==============================================================================
# transfer_to_vm.sh
# Performs selective, memory-optimized transfer of local desktop data, Google
# sessions, developer tokens, and manifests to a remote VM via rsync.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SCRIPT_DIR}/manifest"

# ANSI Colors
BOLD="\033[1m"
GREEN="\033[32m"
BLUE="\033[34m"
YELLOW="\033[33m"
RED="\033[31m"
CYAN="\033[36m"
RESET="\033[0m"

# Default configuration
VM_HOST=""
VM_PORT="22"
DRY_RUN=false
CHOSEN_MODULES=""
CUSTOM_CHROME_PATH=""

usage() {
    cat << EOF
Usage: $0 [OPTIONS]

Options:
  --host <user@ip>      Remote VM SSH host (e.g. ubuntu@192.168.1.100)
  --port <port>         SSH port (default: 22)
  --modules <list>      Comma-separated list of modules to transfer:
                        google,auth,manifest,workspaces,dotfiles (or 'all')
  --chrome-path <path>  Custom Chrome/Chromium profile path to transfer
  --dry-run             Preview files that would be transferred without changing anything
  -h, --help            Show this help message
EOF
    exit 0
}

# Parse CLI arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --host)
            VM_HOST="$2"; shift 2;;
        --port)
            VM_PORT="$2"; shift 2;;
        --modules)
            CHOSEN_MODULES="$2"; shift 2;;
        --chrome-path)
            CUSTOM_CHROME_PATH="$2"; shift 2;;
        --dry-run)
            DRY_RUN=true; shift 1;;
        -h|--help)
            usage;;
        *)
            echo -e "${RED}Unknown argument: $1${RESET}"
            usage;;
    esac
done

echo -e "${BOLD}${BLUE}======================================================${RESET}"
echo -e "${BOLD}${BLUE}   VM Migration & Synchronization Engine             ${RESET}"
echo -e "${BOLD}${BLUE}======================================================${RESET}"

# Prompt for VM Host if not specified
if [ -z "${VM_HOST}" ]; then
    echo -e "${CYAN}Please enter the target VM destination (e.g. user@1.2.3.4):${RESET}"
    read -rp "Target VM [user@ip]: " VM_HOST
    if [ -z "${VM_HOST}" ]; then
        echo -e "${RED}Error: VM host cannot be empty.${RESET}"
        exit 1
    fi
fi

# Modular Selection Menu (if not specified via CLI)
if [ -z "${CHOSEN_MODULES}" ]; then
    echo -e "\n${BOLD}Select which components to transfer to ${VM_HOST}:${RESET}"
    echo -e "  ${CYAN}[1] Google Sessions & Tokens${RESET} (Chrome cookies/logins without cache bloat, Keyring, gcloud)"
    echo -e "  ${CYAN}[2] Developer Auth & Tokens${RESET} (GitHub CLI, Git config, SSH client keys, Docker)"
    echo -e "  ${CYAN}[3] App Manifests & Setup Scripts${RESET} (Clean rehydration on VM without duplicate libs)"
    echo -e "  ${CYAN}[4] Shell Dotfiles${RESET} (~/.bashrc, ~/.profile, ~/.config/fish)"
    echo -e "  ${BOLD}[A] All of the above (Sessions, Tokens, Manifests, Dotfiles)${RESET}"
    echo ""
    read -rp "Enter choices (e.g. 1,2,3 or A) [A]: " USER_CHOICE
    USER_CHOICE="${USER_CHOICE:-A}"

    case "${USER_CHOICE^^}" in
        A|ALL)
            CHOSEN_MODULES="google,auth,manifest,dotfiles";;
        *1*|*2*|*3*|*4*)
            MODS=()
            [[ "${USER_CHOICE}" =~ 1 ]] && MODS+=("google")
            [[ "${USER_CHOICE}" =~ 2 ]] && MODS+=("auth")
            [[ "${USER_CHOICE}" =~ 3 ]] && MODS+=("manifest")
            [[ "${USER_CHOICE}" =~ 4 ]] && MODS+=("dotfiles")
            CHOSEN_MODULES=$(IFS=,; echo "${MODS[*]}")
            ;;
        *)
            echo -e "${YELLOW}Invalid choice. Defaulting to all modules.${RESET}"
            CHOSEN_MODULES="google,auth,manifest,dotfiles";;
    esac
fi

# Use user's SSH config if present, otherwise ignore bad /etc/ssh permissions
SSH_CFG_OPT="-F /dev/null"
if [ -f "${HOME}/.ssh/config" ]; then
    SSH_CFG_OPT="-F ${HOME}/.ssh/config"
fi

RSYNC_OPTS=(-avzP --hard-links -e "ssh ${SSH_CFG_OPT} -p ${VM_PORT}")
if [ "${DRY_RUN}" = true ]; then
    RSYNC_OPTS+=(--dry-run)
    echo -e "\n${YELLOW}*** DRY RUN MODE ENABLED: No files will be modified on the remote VM. ***${RESET}"
fi

# Test SSH connection
echo -e "\n${CYAN}Testing SSH connectivity to ${VM_HOST}:${VM_PORT}...${RESET}"
if [ "${DRY_RUN}" = false ]; then
    if ! ssh ${SSH_CFG_OPT} -p "${VM_PORT}" -o BatchMode=yes -o ConnectTimeout=5 "${VM_HOST}" "echo 'SSH Connection successful'" >/dev/null 2>&1; then
        echo -e "${YELLOW}Note: Automatic passwordless SSH check failed. Will proceed; SSH may prompt for credentials/password.${RESET}"
    else
        echo -e "  ${GREEN}✓${RESET} SSH connection established successfully."
    fi
    ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ~/.config ~/.local/share ~/vm-setup" 2>/dev/null || true
fi

# Ensure manifest exists
if [ ! -d "${MANIFEST_DIR}" ] || [ ! -f "${MANIFEST_DIR}/apt_packages.txt" ]; then
    echo -e "\n${CYAN}Generating fresh system manifest first...${RESET}"
    "${SCRIPT_DIR}/export_manifest.sh"
fi

# ------------------------------------------------------------------------------
# Module: App Manifests & VM Setup Tool
# ------------------------------------------------------------------------------
if [[ ",${CHOSEN_MODULES}," =~ ,manifest, ]]; then
    echo -e "\n${BOLD}${BLUE}>>> Syncing Package Manifests & Setup Scripts...${RESET}"
    rsync "${RSYNC_OPTS[@]}" \
        "${MANIFEST_DIR}" \
        "${SCRIPT_DIR}/setup_vm_memory_dedup.sh" \
        "${SCRIPT_DIR}/check_memory_savings.sh" \
        "${VM_HOST}:~/vm-setup/"
    echo -e "  ${GREEN}✓${RESET} Manifests and setup scripts synced to ~/vm-setup/ on target VM."
fi

# ------------------------------------------------------------------------------
# Module: Google Sessions & Tokens
# ------------------------------------------------------------------------------
if [[ ",${CHOSEN_MODULES}," =~ ,google, ]]; then
    echo -e "\n${BOLD}${BLUE}>>> Syncing Google Sessions & Authentication Tokens...${RESET}"

    # Determine Chrome source paths
    CHROME_SRCS=()
    if [ -n "${CUSTOM_CHROME_PATH}" ] && [ -d "${CUSTOM_CHROME_PATH}" ]; then
        CHROME_SRCS+=("${CUSTOM_CHROME_PATH}")
    elif [ -f "${MANIFEST_DIR}/chrome_paths.txt" ]; then
        while IFS= read -r p; do
            [ -d "${p}" ] && CHROME_SRCS+=("${p}")
        done < "${MANIFEST_DIR}/chrome_paths.txt"
    fi

    # Rsync Chrome with memory-optimized cache exclusions:
    # Exclude Cache, GPUCache, Code Cache, and transient crashdumps so RAM is not bloated.
    CHROME_EXCLUDES=(
        --exclude='Cache*'
        --exclude='*Cache*'
        --exclude='*cache*'
        --exclude='Crashpad'
        --exclude='Crash Reports'
        --exclude='BrowserMetrics*'
        --exclude='*.tmp'
    )

    if [ ${#CHROME_SRCS[@]} -gt 0 ]; then
        for cpath in "${CHROME_SRCS[@]}"; do
            echo -e "  ${CYAN}-> Syncing Chrome profile:${RESET} ${cpath}"
            cname=$(basename "${cpath}")
            rsync "${RSYNC_OPTS[@]}" "${CHROME_EXCLUDES[@]}" \
                "${cpath}/" \
                "${VM_HOST}:~/.config/${cname}/"
        done
        echo -e "  ${GREEN}✓${RESET} Chrome profile and sessions synced (without heavy cache bloat)."
    else
        echo -e "  ${YELLOW}!${RESET} No active Chrome profile directory found to sync."
    fi

    # Sync GNOME Keyring DB if present (needed to decrypt stored cookies/tokens)
    if [ -d "${HOME}/.local/share/keyrings" ]; then
        echo -e "  ${CYAN}-> Syncing GNOME Keyring credentials...${RESET}"
        rsync "${RSYNC_OPTS[@]}" \
            "${HOME}/.local/share/keyrings/" \
            "${VM_HOST}:~/.local/share/keyrings/"
        [ "${DRY_RUN}" = false ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.local/share/keyrings && chmod 600 ~/.local/share/keyrings/* 2>/dev/null || true"
        echo -e "  ${GREEN}✓${RESET} Keyring store synced."
    fi

    # Sync Google Cloud SDK tokens
    if [ -d "${HOME}/.config/gcloud" ]; then
        echo -e "  ${CYAN}-> Syncing Google Cloud SDK (gcloud) auth & tokens...${RESET}"
        rsync "${RSYNC_OPTS[@]}" \
            --exclude='logs' \
            --exclude='cache' \
            "${HOME}/.config/gcloud/" \
            "${VM_HOST}:~/.config/gcloud/"
        [ "${DRY_RUN}" = false ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.config/gcloud"
        echo -e "  ${GREEN}✓${RESET} Google Cloud SDK config & active tokens synced."
    fi
fi

# ------------------------------------------------------------------------------
# Module: Developer Auth & Tokens
# ------------------------------------------------------------------------------
if [[ ",${CHOSEN_MODULES}," =~ ,auth, ]]; then
    echo -e "\n${BOLD}${BLUE}>>> Syncing Developer Authentication & Keys...${RESET}"

    # GitHub CLI
    if [ -d "${HOME}/.config/gh" ]; then
        echo -e "  ${CYAN}-> Syncing GitHub CLI (gh) auth...${RESET}"
        rsync "${RSYNC_OPTS[@]}" "${HOME}/.config/gh/" "${VM_HOST}:~/.config/gh/"
    fi

    # Git config
    if [ -f "${HOME}/.gitconfig" ]; then
        echo -e "  ${CYAN}-> Syncing Git configuration (~/.gitconfig)...${RESET}"
        rsync "${RSYNC_OPTS[@]}" "${HOME}/.gitconfig" "${VM_HOST}:~/.gitconfig"
    fi

    # Docker config
    if [ -d "${HOME}/.docker" ]; then
        echo -e "  ${CYAN}-> Syncing Docker client credentials...${RESET}"
        rsync "${RSYNC_OPTS[@]}" "${HOME}/.docker/" "${VM_HOST}:~/.docker/"
    fi

    # SSH Keys (Strict permissions)
    if [ -d "${HOME}/.ssh" ]; then
        echo -e "  ${CYAN}-> Syncing SSH client keys & config...${RESET}"
        rsync "${RSYNC_OPTS[@]}" "${HOME}/.ssh/" "${VM_HOST}:~/.ssh/"
        if [ "${DRY_RUN}" = false ]; then
            ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_* ~/.ssh/config 2>/dev/null || true"
        fi
        echo -e "  ${GREEN}✓${RESET} SSH keys transferred with secure permissions."
    fi
fi

# ------------------------------------------------------------------------------
# Module: Shell Dotfiles
# ------------------------------------------------------------------------------
if [[ ",${CHOSEN_MODULES}," =~ ,dotfiles, ]]; then
    echo -e "\n${BOLD}${BLUE}>>> Syncing Shell Environment Dotfiles...${RESET}"
    DOTFILES=(".bashrc" ".profile" ".bash_aliases" ".zshrc")
    for df in "${DOTFILES[@]}"; do
        if [ -f "${HOME}/${df}" ]; then
            rsync "${RSYNC_OPTS[@]}" "${HOME}/${df}" "${VM_HOST}:~/${df}"
            echo -e "  ${GREEN}✓${RESET} Synced ${df}"
        fi
    done

    if [ -d "${HOME}/.config/fish" ]; then
        rsync "${RSYNC_OPTS[@]}" "${HOME}/.config/fish/" "${VM_HOST}:~/.config/fish/"
        echo -e "  ${GREEN}✓${RESET} Synced Fish shell configuration"
    fi
fi

# ------------------------------------------------------------------------------
# Module: Project Workspaces (Guarded against recursion)
# ------------------------------------------------------------------------------
if [[ ",${CHOSEN_MODULES}," =~ ,workspaces, ]]; then
    echo -e "\n${BOLD}${YELLOW}>>> Anti-Recursion Check: Skipping our own app repository (~/flowera).${RESET}"
    echo -e "  To prevent duplicate copies and recursion, our app and migration-toolkit are not copied."
fi

echo -e "\n${BOLD}${GREEN}======================================================${RESET}"
echo -e "${BOLD}${GREEN} Transfer process completed!                           ${RESET}"
echo -e "${BOLD}${GREEN}======================================================${RESET}"

if [ "${DRY_RUN}" = false ]; then
    cat << EOF

${BOLD}Next Step: Run the VM Memory Deduplication & Setup Script${RESET}
Connect to your VM and execute the optimization script:

  ${CYAN}ssh -p ${VM_PORT} ${VM_HOST}${RESET}
  ${CYAN}sudo bash ~/vm-setup/setup_vm_memory_dedup.sh${RESET}

This will:
  1. Rehydrate apps natively via APT/Cargo/NPM (sharing memory pages across apps).
  2. Activate Linux Kernel Samepage Merging (KSM) to de-duplicate identical memory pages.
  3. Validate active Google session tokens and credentials.
EOF
fi
