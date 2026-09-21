#!/usr/bin/env bash
# ==============================================================================
# choice_migration.sh
# Choice-by-Choice Itemized Migration Engine
# Scans all apps, session tokens, toolchains, and files, allowing granular
# selection of what to copy to the remote VM with zero duplicate memory.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST_DIR="${SCRIPT_DIR}/manifest"
mkdir -p "${MANIFEST_DIR}"

# ANSI Colors
BOLD="\033[1m"
GREEN="\033[32m"
BLUE="\033[34m"
YELLOW="\033[33m"
CYAN="\033[36m"
MAGENTA="\033[35m"
RED="\033[31m"
DIM="\033[2m"
RESET="\033[0m"

# Ensure manifest exists
if [ ! -f "${MANIFEST_DIR}/apt_packages.txt" ]; then
    "${SCRIPT_DIR}/export_manifest.sh" >/dev/null 2>&1 || true
fi

# ------------------------------------------------------------------------------
# Item Definitions & Auto-Detection
# ------------------------------------------------------------------------------
declare -A ITEM_NAMES
declare -A ITEM_CATS
declare -A ITEM_PATHS
declare -A ITEM_EXISTS
declare -A ITEM_DESCS
declare -A ITEM_EXCLUDES

register_item() {
    local id="$1"
    local cat="$2"
    local name="$3"
    local path="$4"
    local desc="$5"
    local excludes="${6:-}"

    ITEM_CATS["$id"]="$cat"
    ITEM_NAMES["$id"]="$name"
    ITEM_PATHS["$id"]="$path"
    ITEM_DESCS["$id"]="$desc"
    ITEM_EXCLUDES["$id"]="$excludes"

    # Check existence
    if [ -n "$path" ] && [ -e "$path" ]; then
        ITEM_EXISTS["$id"]="1"
    elif [ "$path" == "MANIFEST" ]; then
        ITEM_EXISTS["$id"]="1"
    else
        ITEM_EXISTS["$id"]="0"
    fi
}

# Auto-detect browser paths across Linux, macOS, and Windows (WSL/Git Bash)
CHROME_PATH="${HOME}/.config/google-chrome-for-testing"
[ ! -d "$CHROME_PATH" ] && CHROME_PATH="${HOME}/.config/google-chrome"
[ ! -d "$CHROME_PATH" ] && CHROME_PATH="${HOME}/Library/Application Support/Google/Chrome"
if [ ! -d "$CHROME_PATH" ]; then
    for c_drive in "/mnt/c" "/c"; do
        if [ -d "${c_drive}/Users" ]; then
            for win_user_dir in "${c_drive}/Users"/*; do
                if [ -d "${win_user_dir}/AppData/Local/Google/Chrome/User Data" ]; then
                    CHROME_PATH="${win_user_dir}/AppData/Local/Google/Chrome/User Data"
                    break 2
                fi
            done
        fi
    done
fi

FIREFOX_PATH="${HOME}/.mozilla/firefox"
[ ! -d "$FIREFOX_PATH" ] && FIREFOX_PATH="${HOME}/Library/Application Support/Firefox"
if [ ! -d "$FIREFOX_PATH" ]; then
    for c_drive in "/mnt/c" "/c"; do
        if [ -d "${c_drive}/Users" ]; then
            for win_user_dir in "${c_drive}/Users"/*; do
                if [ -d "${win_user_dir}/AppData/Roaming/Mozilla/Firefox/Profiles" ]; then
                    FIREFOX_PATH="${win_user_dir}/AppData/Roaming/Mozilla/Firefox/Profiles"
                    break 2
                fi
            done
        fi
    done
fi

# Group 1: Apps & Account Sessions (Tokens, Cookies & Logins)
register_item "1"  "APPS & SESSIONS" "Google Chrome / Chromium" "${CHROME_PATH}" \
    "Profile, active cookies, sessions, login data (Cache stripped to save RAM)" \
    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad --exclude=BrowserMetrics*"

register_item "2"  "APPS & SESSIONS" "Mozilla Firefox" "${FIREFOX_PATH}" \
    "Profiles, saved logins, cookies, tabs (Cache stripped)" \
    "--exclude=cache2* --exclude=jumpListCache*"

register_item "3"  "APPS & SESSIONS" "GitHub CLI & Git Config" "${HOME}/.config/gh" \
    "Logged-in GitHub token, credentials, and ~/.gitconfig" ""

register_item "4"  "APPS & SESSIONS" "Docker Hub & Engine Config" "${HOME}/.docker" \
    "Docker credentials, contexts, and client config" ""

register_item "5"  "APPS & SESSIONS" "OpenCode / VS Code Editor" "${HOME}/.config/opencode" \
    "Editor state, settings, keybindings, and extensions" "--exclude=node_modules"

register_item "6"  "APPS & SESSIONS" "SSH Client Keys & Config" "${HOME}/.ssh" \
    "SSH identity keys, configs, and known hosts (strict 600 permissions)" ""

register_item "7"  "APPS & SESSIONS" "Google Cloud SDK (gcloud)" "${HOME}/.config/gcloud" \
    "Active GCP accounts, credentials, and application default tokens" \
    "--exclude=logs --exclude=cache"

register_item "8"  "APPS & SESSIONS" "Azure CLI (az)" "${HOME}/.azure" \
    "Active Azure subscriptions, tokens, and tenant configurations" ""

register_item "9"  "APPS & SESSIONS" "GNOME Keyring Secrets" "${HOME}/.local/share/keyrings" \
    "OS encrypted secrets database for browser/app passwords" ""

# Group 2: Toolchains & Native App Rehydration (No Duplicate Memory)
register_item "10" "TOOLCHAINS" "103 Manual APT Packages" "MANIFEST" \
    "Declarative reinstall via APT on VM (Shared dynamic .so libraries in RAM)" ""

register_item "11" "TOOLCHAINS" "Rust & Cargo Toolchain" "${HOME}/.cargo" \
    "Cargo binaries, crates manifest, and toolchain configurations" \
    "--exclude=registry/cache"

register_item "12" "TOOLCHAINS" "Bun JavaScript Runtime" "${HOME}/.bun" \
    "Bun runtime binary and global modules" "--exclude=install/cache"

register_item "13" "TOOLCHAINS" "Go Toolchain & Binaries" "${HOME}/go" \
    "Go packages, libraries, and binaries" "--exclude=pkg/mod/cache"

register_item "14" "TOOLCHAINS" ".NET Runtime & CoreFX" "${HOME}/.dotnet" \
    "Dotnet SDK tools and global packages" ""

register_item "15" "TOOLCHAINS" "Python & UV Tools" "${HOME}/.cargo/bin/uv" \
    "UV fast package installer and registered global tools" ""

# Group 3: Shell & Environment Dotfiles
register_item "16" "SHELL & DOTFILES" "Fish Shell Config" "${HOME}/.config/fish" \
    "Custom shell functions, themes, history, and completions" ""

register_item "17" "SHELL & DOTFILES" "Bash / Zsh Dotfiles" "${HOME}/.bashrc" \
    "Shell startup scripts (~/.bashrc, ~/.profile, ~/.zshrc)" ""

# Fallback check for Chrome paths if standard for-testing wasn't the only one
if [ ! -d "${HOME}/.config/google-chrome-for-testing" ] && [ -d "${HOME}/.config/google-chrome" ]; then
    ITEM_PATHS["1"]="${HOME}/.config/google-chrome"
    ITEM_EXISTS["1"]="1"
fi

# ------------------------------------------------------------------------------
# Display Choice Menu
# ------------------------------------------------------------------------------
clear 2>/dev/null || true
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${BOLD}${BLUE}       Choice-by-Choice VM Migration & Deduplication Selector         ${RESET}"
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${DIM}Select the exact applications, sessions, toolchains, and files you want to copy.${RESET}"
echo -e "${DIM}All selections use memory deduplication (KSM + shared dynamic libraries + cache stripping).${RESET}\n"

CURRENT_CAT=""
for i in $(seq 1 18); do
    cat="${ITEM_CATS[$i]}"
    if [ "$cat" != "$CURRENT_CAT" ]; then
        CURRENT_CAT="$cat"
        echo -e "\n${BOLD}${MAGENTA}--- ${CURRENT_CAT} ---${RESET}"
    fi

    name="${ITEM_NAMES[$i]}"
    desc="${ITEM_DESCS[$i]}"
    exists="${ITEM_EXISTS[$i]}"

    if [ "$exists" == "1" ]; then
        status="${GREEN}[DETECTED]${RESET}"
    else
        status="${DIM}[Not found]${RESET}"
    fi

    printf "  ${BOLD}%2d)${RESET} %-28s %b\n" "$i" "$name" "$status"
    printf "      ${DIM}%s${RESET}\n" "$desc"
done

echo -e "\n${BOLD}${MAGENTA}--- CUSTOM FILES & DIRECTORIES ---${RESET}"
echo -e "  ${BOLD} C)${RESET} Add any custom file or folder path to the copy list"
echo -e "\n${BOLD}Selection Shortcuts:${RESET}"
echo -e "  ${CYAN}[A]${RESET} Copy ALL detected items"
echo -e "  ${CYAN}[S]${RESET} Copy ALL App Sessions & Tokens (items 1-9)"
echo -e "  ${CYAN}[T]${RESET} Copy ALL Toolchains & Apps (items 10-15)"
echo -e "  ${CYAN}[W]${RESET} Copy ALL Workspaces & Dotfiles (items 16-18)"
echo -e "  Or enter specific numbers separated by commas (e.g. ${BOLD}1,3,5,10,16${RESET})"
echo ""

read -rp "$(echo -e "${CYAN}Enter your choices: ${RESET}")" USER_SELECTION
USER_SELECTION="${USER_SELECTION:-A}"

# ------------------------------------------------------------------------------
# Parse Selection
# ------------------------------------------------------------------------------
SELECTED_IDS=()

if [[ "${USER_SELECTION^^}" == "A" ]]; then
    for i in $(seq 1 17); do
        [ "${ITEM_EXISTS[$i]}" == "1" ] && SELECTED_IDS+=("$i")
    done
elif [[ "${USER_SELECTION^^}" == "S" ]]; then
    for i in $(seq 1 9); do
        [ "${ITEM_EXISTS[$i]}" == "1" ] && SELECTED_IDS+=("$i")
    done
elif [[ "${USER_SELECTION^^}" == "T" ]]; then
    for i in $(seq 10 15); do
        [ "${ITEM_EXISTS[$i]}" == "1" ] && SELECTED_IDS+=("$i")
    done
elif [[ "${USER_SELECTION^^}" == "W" ]] || [[ "${USER_SELECTION^^}" == "D" ]]; then
    for i in $(seq 16 17); do
        [ "${ITEM_EXISTS[$i]}" == "1" ] && SELECTED_IDS+=("$i")
    done
else
    IFS=',' read -ra ADDR <<< "$USER_SELECTION"
    for item in "${ADDR[@]}"; do
        item=$(echo "$item" | tr -d ' ')
        if [[ "$item" =~ ^[0-9]+$ ]] && [ "$item" -ge 1 ] && [ "$item" -le 17 ]; then
            SELECTED_IDS+=("$item")
        elif [[ "${item^^}" == "C" ]]; then
            continue
        fi
    done
fi

# Handle Custom Files if requested
CUSTOM_PATHS=()
if [[ "${USER_SELECTION^^}" =~ C ]]; then
    echo -e "\n${CYAN}Enter additional file or directory paths to copy (empty line to finish):${RESET}"
    while true; do
        read -rp "  Path: " cpath
        [ -z "$cpath" ] && break
        # Expand ~
        cpath="${cpath/#\~/$HOME}"
        real_cpath=$(realpath "$cpath" 2>/dev/null || echo "$cpath")
        app_dir=$(realpath "${SCRIPT_DIR}/.." 2>/dev/null || echo "")

        # Anti-recursion guard: Never copy our app or migration-toolkit again
        if [ "$real_cpath" == "$SCRIPT_DIR" ] || [ "$real_cpath" == "$app_dir" ] || [[ "$real_cpath" =~ decurix ]] || [[ "$real_cpath" =~ migration-toolkit ]]; then
            echo -e "  ${YELLOW}! Anti-recursion guard: Skipping our app/migration-toolkit to avoid recursive duplicate copies.${RESET}"
            continue
        fi

        if [ -e "$cpath" ]; then
            CUSTOM_PATHS+=("$cpath")
            echo -e "  ${GREEN}✓${RESET} Added: $cpath"
        else
            echo -e "  ${YELLOW}! Warning: Path does not exist: $cpath${RESET}"
        fi
    done
fi

echo -e "\n${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${BOLD}${BLUE} Selected Items for Transfer                                          ${RESET}"
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
for id in "${SELECTED_IDS[@]}"; do
    echo -e "  ${GREEN}✓${RESET} ${BOLD}${ITEM_NAMES[$id]}${RESET} (${ITEM_PATHS[$id]})"
done
for cp in "${CUSTOM_PATHS[@]}"; do
    echo -e "  ${GREEN}✓${RESET} ${BOLD}Custom Path:${RESET} $cp"
done

if [ ${#SELECTED_IDS[@]} -eq 0 ] && [ ${#CUSTOM_PATHS[@]} -eq 0 ]; then
    echo -e "${YELLOW}No items selected. Exiting.${RESET}"
    exit 0
fi

# ------------------------------------------------------------------------------
# Target VM Details
# ------------------------------------------------------------------------------
echo ""
read -rp "$(echo -e "${CYAN}Target VM destination [user@ip]: ${RESET}")" VM_HOST
if [ -z "$VM_HOST" ]; then
    echo -e "${RED}Error: VM destination cannot be empty.${RESET}"
    exit 1
fi
read -rp "$(echo -e "${CYAN}SSH port [default: 22]: ${RESET}")" VM_PORT
VM_PORT="${VM_PORT:-22}"

read -rp "$(echo -e "${CYAN}Dry-run preview first? (y/N): ${RESET}")" DRY_CHOICE
DRY_OPT=""
if [[ "$DRY_CHOICE" =~ ^[yY] ]]; then
    DRY_OPT="--dry-run"
fi

# ------------------------------------------------------------------------------
# Execute Itemized Transfer
# ------------------------------------------------------------------------------
SSH_CFG_OPT="-F /dev/null"
[ -f "${HOME}/.ssh/config" ] && SSH_CFG_OPT="-F ${HOME}/.ssh/config"

RSYNC_BASE=(-avzP --hard-links -e "ssh ${SSH_CFG_OPT} -p ${VM_PORT}")
[ -n "$DRY_OPT" ] && RSYNC_BASE+=(--dry-run)

echo -e "\n${CYAN}Connecting to ${VM_HOST}...${RESET}"
if [ -z "$DRY_OPT" ]; then
    ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ~/.config ~/.local/share ~/vm-setup" 2>/dev/null || true
fi

# Always sync the setup & deduplication scripts so the VM is ready
rsync "${RSYNC_BASE[@]}" \
    "${SCRIPT_DIR}/setup_vm_memory_dedup.sh" \
    "${SCRIPT_DIR}/check_memory_savings.sh" \
    "${SCRIPT_DIR}/host-bridge" \
    "${MANIFEST_DIR}" \
    "${VM_HOST}:~/vm-setup/" >/dev/null 2>&1 || true

for id in "${SELECTED_IDS[@]}"; do
    name="${ITEM_NAMES[$id]}"
    path="${ITEM_PATHS[$id]}"
    excludes="${ITEM_EXCLUDES[$id]}"

    echo -e "\n${BOLD}${BLUE}>>> Transferring: ${name}...${RESET}"

    if [ "$path" == "MANIFEST" ]; then
        rsync "${RSYNC_BASE[@]}" "${MANIFEST_DIR}/" "${VM_HOST}:~/vm-setup/manifest/"
        echo -e "  ${GREEN}✓${RESET} Package manifest synchronized."
        continue
    fi

    if [ ! -e "$path" ]; then
        echo -e "  ${YELLOW}! Path not found locally, skipping: $path${RESET}"
        continue
    fi

    if [[ "$path" =~ ^/mnt/c/ || "$path" =~ ^/c/ ]]; then
        if [ "$id" == "1" ]; then
            remote_dest="${VM_HOST}:~/.config/google-chrome"
        elif [ "$id" == "2" ]; then
            remote_dest="${VM_HOST}:~/.mozilla/firefox"
        else
            remote_dest="${VM_HOST}:~/host-files/$(basename "$path")"
        fi
        echo -e "  ${GREEN}>>> Rehydrating Windows host profile into ${remote_dest}...${RESET}"
        [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ~/.config/google-chrome ~/.mozilla/firefox" 2>/dev/null || true
        CMD=("${RSYNC_BASE[@]}")
        if [ -n "$excludes" ]; then
            for exc in $excludes; do
                CMD+=("$exc")
            done
        fi
        "${CMD[@]}" "${path}/" "${remote_dest}/"
        echo -e "  ${GREEN}✓ Done.${RESET}"
        continue
    fi

    # Determine remote destination path relative to HOME
    rel_path="${path#$HOME/}"
    remote_dest="${VM_HOST}:~/${rel_path}"

    # Build rsync command with excludes
    CMD=("${RSYNC_BASE[@]}")
    if [ -n "$excludes" ]; then
        for exc in $excludes; do
            CMD+=("$exc")
        done
    fi

    if [ -d "$path" ]; then
        # Ensure remote parent directory exists
        remote_parent=$(dirname "~/${rel_path}")
        [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ${remote_parent}" 2>/dev/null || true
        "${CMD[@]}" "${path}/" "${VM_HOST}:~/${rel_path}/"
    else
        remote_parent=$(dirname "~/${rel_path}")
        [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ${remote_parent}" 2>/dev/null || true
        "${CMD[@]}" "${path}" "${VM_HOST}:~/${rel_path}"
    fi

    # Extra security enforcement for SSH or Keyring
    if [ "$id" == "6" ] && [ -z "$DRY_OPT" ]; then
        ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_* ~/.ssh/config 2>/dev/null || true"
    fi
    if [ "$id" == "9" ] && [ -z "$DRY_OPT" ]; then
        ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.local/share/keyrings && chmod 600 ~/.local/share/keyrings/* 2>/dev/null || true"
    fi
    if [ "$id" == "3" ] && [ -z "$DRY_OPT" ] && [ -f "${HOME}/.gitconfig" ]; then
        "${RSYNC_BASE[@]}" "${HOME}/.gitconfig" "${VM_HOST}:~/.gitconfig"
    fi
done

# Transfer Custom Paths
for cp in "${CUSTOM_PATHS[@]}"; do
    echo -e "\n${BOLD}${BLUE}>>> Transferring Custom Path: ${cp}...${RESET}"
    if [ -d "$cp" ]; then
        base=$(basename "$cp")
        "${RSYNC_BASE[@]}" "$cp/" "${VM_HOST}:~/${base}/"
    else
        "${RSYNC_BASE[@]}" "$cp" "${VM_HOST}:~/"
    fi
done

echo -e "\n${BOLD}${GREEN}======================================================================${RESET}"
echo -e "${BOLD}${GREEN} Selected items successfully migrated!                                ${RESET}"
echo -e "${BOLD}${GREEN}======================================================================${RESET}"

if [ -z "$DRY_OPT" ]; then
    cat << EOF

${BOLD}Next Step: Run the VM Memory Deduplication & Setup Tool${RESET}
Connect to your VM to activate Kernel Samepage Merging (KSM) and native app sharing:

  ${CYAN}ssh -p ${VM_PORT} ${VM_HOST}${RESET}
  ${CYAN}sudo bash ~/vm-setup/setup_vm_memory_dedup.sh${RESET}

To monitor active RAM savings:
  ${CYAN}~/vm-setup/check_memory_savings.sh${RESET}
EOF
fi
