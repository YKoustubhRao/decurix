#!/usr/bin/env bash
# ==============================================================================
# interactive_discover.sh
# Step-by-Step Discovery & Migration Wizard (Default Empty VM)
# Scans your system and walks through each discovered app, session, toolchain,
# and file one by one, asking if you want to copy it to the empty VM.
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

clear 2>/dev/null || true
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${BOLD}${BLUE}   Step-by-Step Discovery & Migration (Default Clean / Empty VM)      ${RESET}"
echo -e "${BOLD}${BLUE}======================================================================${RESET}"
echo -e "${DIM}Your VM starts 100% empty. We will scan your system and present each${RESET}"
echo -e "${DIM}discovered app, account session, toolchain, and file one by one.${RESET}"
echo -e "${DIM}You decide what gets copied over. No memory or storage duplicates.${RESET}\n"

# Destination VM details
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

SSH_CFG_OPT="-F /dev/null"
[ -f "${HOME}/.ssh/config" ] && SSH_CFG_OPT="-F ${HOME}/.ssh/config"

RSYNC_BASE=(-avzP --hard-links -e "ssh ${SSH_CFG_OPT} -p ${VM_PORT}" \
    --exclude=.flowera \
    --exclude=flowera \
    --exclude=migration-toolkit \
    --exclude=*.qcow2 \
    --exclude=*.vmdk \
    --exclude=*.iso \
    --exclude=.git)
[ -n "$DRY_OPT" ] && RSYNC_BASE+=(--dry-run)

# Verify connectivity & create base folders on empty VM
echo -e "\n${CYAN}Connecting to ${VM_HOST}...${RESET}"
if [ -z "$DRY_OPT" ]; then
    ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ~/.config ~/.local/share ~/vm-setup" 2>/dev/null || true
fi

# Always sync the optimization scripts and host bridge
rsync "${RSYNC_BASE[@]}" \
    "${SCRIPT_DIR}/setup_vm_memory_dedup.sh" \
    "${SCRIPT_DIR}/check_memory_savings.sh" \
    "${SCRIPT_DIR}/host-bridge" \
    "${VM_HOST}:~/vm-setup/" >/dev/null 2>&1 || true

# ------------------------------------------------------------------------------
# Itemized Discovery List
# ------------------------------------------------------------------------------
declare -a DISCOVERED_ITEMS=()
declare -A ITEM_PATHS
declare -A ITEM_DESCS
declare -A ITEM_EXCLUDES
declare -A ITEM_SIZES

add_candidate() {
    local key="$1"
    local path="$2"
    local desc="$3"
    local excludes="${4:-}"

    if [ -e "$path" ] || [ "$path" == "MANIFEST" ] || [[ "$path" =~ ^BRIDGE: ]]; then
        DISCOVERED_ITEMS+=("$key")
        ITEM_PATHS["$key"]="$path"
        ITEM_DESCS["$key"]="$desc"
        ITEM_EXCLUDES["$key"]="$excludes"

        if [[ "$path" =~ ^BRIDGE: ]]; then
            ITEM_SIZES["$key"]="0 MB (Zero VM RAM)"
        elif [ "$path" != "MANIFEST" ] && [ -e "$path" ]; then
            # Estimate size (first 10MB or du)
            local sz
            sz=$(du -sh "$path" 2>/dev/null | cut -f1 || echo "unknown")
            ITEM_SIZES["$key"]="$sz"
        else
            ITEM_SIZES["$key"]="< 1 MB"
        fi
    fi
}

# 1. Browsers & Sessions
add_candidate "Google Chrome (Testing Profile)" "${HOME}/.config/google-chrome-for-testing" \
    "Cookies, active Google logins, storage (Volatile cache stripped to save RAM)" \
    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad --exclude=BrowserMetrics*"

add_candidate "Google Chrome (Default Profile)" "${HOME}/.config/google-chrome" \
    "Main Chrome profile, cookies, extensions, saved credentials" \
    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad --exclude=BrowserMetrics*"

add_candidate "Google Chrome (macOS Profile)" "${HOME}/Library/Application Support/Google/Chrome" \
    "Cookies, active Google logins, storage (Volatile cache stripped to save RAM)" \
    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad --exclude=BrowserMetrics*"

# Windows Chrome & Edge Profiles (WSL / Git Bash)
for c_drive in "/mnt/c" "/c" "C:"; do
    if [ -d "${c_drive}/Users" ]; then
        for win_user_dir in "${c_drive}/Users"/*; do
            win_chrome="${win_user_dir}/AppData/Local/Google/Chrome/User Data"
            if [ -d "${win_chrome}" ]; then
                add_candidate "Google Chrome (Windows Host Profile)" "${win_chrome}" \
                    "Active Google account tokens, cookies, sessions from Windows host" \
                    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad --exclude=BrowserMetrics*"
            fi
            win_edge="${win_user_dir}/AppData/Local/Microsoft/Edge/User Data"
            if [ -d "${win_edge}" ]; then
                add_candidate "Microsoft Edge (Windows Host Profile)" "${win_edge}" \
                    "Active Edge cookies, tokens, and sessions from Windows host" \
                    "--exclude=Cache* --exclude=*cache* --exclude=Crashpad"
            fi
        done
    fi
done

add_candidate "Mozilla Firefox Profile" "${HOME}/.mozilla/firefox" \
    "Firefox profiles, cookies, saved passwords, tabs (Cache stripped)" \
    "--exclude=cache2* --exclude=jumpListCache*"

add_candidate "Mozilla Firefox (macOS Profile)" "${HOME}/Library/Application Support/Firefox" \
    "Firefox profiles, cookies, saved passwords, tabs (Cache stripped)" \
    "--exclude=cache2* --exclude=jumpListCache*"

# Discovered Host Applications (Zero VM RAM, Host Remote Bridge)
for c_drive in "/mnt/c" "/c" "C:"; do
    if [ -d "${c_drive}/Program Files" ] || [ -d "${c_drive}/Program Files (x86)" ]; then
        for app_check in \
            "Notepad++/notepad++.exe:Notepad++:notepad++.exe:win-notepad" \
            "Microsoft VS Code/Code.exe:VS Code (Windows Host):code.cmd:win-code" \
            "Google/Chrome/Application/chrome.exe:Google Chrome (Windows Host App):chrome.exe:win-chrome" \
            "Mozilla Firefox/firefox.exe:Firefox (Windows Host App):firefox.exe:win-firefox" \
            "Git/git-bash.exe:Git Bash:git-bash.exe:win-gitbash"; do

            subpath="${app_check%%:*}"
            rest="${app_check#*:}"
            app_name="${rest%%:*}"
            rest2="${rest#*:}"
            app_cmd="${rest2%%:*}"
            app_slug="${rest2#*:}"

            for pf in "${c_drive}/Program Files" "${c_drive}/Program Files (x86)"; do
                if [ -f "${pf}/${subpath}" ]; then
                    add_candidate "Host App Bridge: ${app_name}" "BRIDGE:${app_slug}:${app_name}:${app_cmd}" \
                        "Zero VM RAM: Runs on Windows host via win-run / host-run bridge" ""
                    break
                fi
            done
        done
    fi
done

if [ -d "/Applications" ]; then
    for mac_app in "/Applications/Visual Studio Code.app" "/Applications/Google Chrome.app" "/Applications/Slack.app"; do
        if [ -d "$mac_app" ]; then
            app_base=$(basename "$mac_app" .app)
            slug="mac-$(echo "$app_base" | tr '[:upper:]' '[:lower:]' | tr -s ' ' '-')"
            add_candidate "Host App Bridge: ${app_base} (macOS)" "BRIDGE:${slug}:${app_base}:open -a '${app_base}'" \
                "Zero VM RAM: Runs on macOS host via host-run bridge" ""
        fi
    done
fi

add_candidate "Google Cloud SDK (gcloud)" "${HOME}/.config/gcloud" \
    "Active GCP accounts, credentials, application default tokens" \
    "--exclude=logs --exclude=cache"

add_candidate "GNOME Keyring Secrets" "${HOME}/.local/share/keyrings" \
    "System secret service database (decrypts saved browser & app passwords)" ""

# 2. Developer Auth & Configs
add_candidate "GitHub CLI (gh) & Git Config" "${HOME}/.config/gh" \
    "Active GitHub authentication token and ~/.gitconfig" ""

add_candidate "Docker Hub & Engine Config" "${HOME}/.docker" \
    "Docker client authentication, contexts, daemon configs" ""

add_candidate "Azure CLI (az)" "${HOME}/.azure" \
    "Azure tenant tokens, active subscription configurations" ""

add_candidate "SSH Client Keys & Configs" "${HOME}/.ssh" \
    "Private & public SSH keys, host configs (transferred with strict 600 permissions)" ""

# 3. Editors & IDEs
add_candidate "OpenCode / VS Code Editor" "${HOME}/.config/opencode" \
    "Editor settings, keybindings, state, extensions" "--exclude=node_modules"

add_candidate "VS Code Config (~/.vscode)" "${HOME}/.vscode" \
    "VS Code extensions and settings" ""

# 4. Toolchains (Native Rehydration - Zero Duplicate Memory)
add_candidate "103 Manual System APT Packages" "MANIFEST" \
    "Declarative list of 103 packages installed cleanly via APT (dynamic .so shared in RAM)" ""

add_candidate "Rust & Cargo Toolchain" "${HOME}/.cargo" \
    "Cargo binaries, installed crates, toolchain configs" "--exclude=registry/cache"

add_candidate "Bun JavaScript Runtime" "${HOME}/.bun" \
    "Bun runtime and global modules" "--exclude=install/cache"

add_candidate "Go Toolchain & Workspaces" "${HOME}/go" \
    "Go binaries, packages, and environment" "--exclude=pkg/mod/cache"

add_candidate ".NET CoreFX & SDK" "${HOME}/.dotnet" \
    "Dotnet CLI tools and corefx runtime configs" ""

add_candidate "Python & UV Fast Package Tool" "${HOME}/.cargo/bin/uv" \
    "UV binary and global tools" ""

# 5. Shell & Environment Dotfiles
add_candidate "Fish Shell Config" "${HOME}/.config/fish" \
    "Fish shell functions, themes, history, completions" ""

add_candidate "Bash Dotfiles (~/.bashrc)" "${HOME}/.bashrc" \
    "Bash startup files (~/.bashrc, ~/.profile)" ""

add_candidate "Zsh Dotfiles (~/.zshrc)" "${HOME}/.zshrc" \
    "Zsh shell configuration and environment (~/.zshrc)" ""

# ------------------------------------------------------------------------------
# One-by-One Interactive Prompting Loop
# ------------------------------------------------------------------------------
COPIED_COUNT=0
TOTAL_CANDIDATES=${#DISCOVERED_ITEMS[@]}

echo -e "\n${BOLD}${GREEN}Discovery Complete: Found ${TOTAL_CANDIDATES} candidate items.${RESET}"
echo -e "${DIM}Beginning one-by-one review. Press [Enter] or 'n' to skip, 'y' to copy.${RESET}\n"

for i in "${!DISCOVERED_ITEMS[@]}"; do
    num=$((i + 1))
    item="${DISCOVERED_ITEMS[$i]}"
    path="${ITEM_PATHS[$item]}"
    desc="${ITEM_DESCS[$item]}"
    sz="${ITEM_SIZES[$item]}"
    excludes="${ITEM_EXCLUDES[$item]}"

    echo -e "${BOLD}${BLUE}----------------------------------------------------------------------${RESET}"
    echo -e " [${num}/${TOTAL_CANDIDATES}] ${BOLD}${CYAN}${item}${RESET}  (Disk Size: ${BOLD}${sz}${RESET})"
    echo -e " Location: ${DIM}${path}${RESET}"
    echo -e " Info:     ${desc}"
    echo -e "${BOLD}${BLUE}----------------------------------------------------------------------${RESET}"

    read -rp "$(echo -e "Copy this to the VM? [y/N/q to stop review]: ")" USER_RESP
    USER_RESP="${USER_RESP:-N}"

    if [[ "${USER_RESP^^}" == "Q" ]]; then
        echo -e "${YELLOW}Stopping discovery review.${RESET}"
        break
    fi

    if [[ "${USER_RESP^^}" =~ ^[Y] ]]; then
        echo -e "  ${GREEN}>>> Copying ${item} to ${VM_HOST}...${RESET}"

        if [ "$path" == "MANIFEST" ]; then
            "${SCRIPT_DIR}/export_manifest.sh" >/dev/null 2>&1 || true
            rsync "${RSYNC_BASE[@]}" "${MANIFEST_DIR}/" "${VM_HOST}:~/vm-setup/manifest/"
            echo -e "  ${GREEN}✓${RESET} Package manifest synchronized."
            COPIED_COUNT=$((COPIED_COUNT + 1))
            continue
        fi

        if [[ "$path" =~ ^BRIDGE: ]]; then
            IFS=':' read -r _ b_slug b_name b_cmd <<< "$path"
            echo -e "  ${GREEN}>>> Configuring host bridge launcher for ${b_name}...${RESET}"
            if [ -z "$DRY_OPT" ]; then
                ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" \
                    "bash ~/vm-setup/host-bridge/create_app_bridge.sh '${b_slug}' '${b_name}' '${b_cmd}'" 2>/dev/null || true
            fi
            echo -e "  ${GREEN}✓${RESET} Bridge launcher configured (Runs on host, Zero VM RAM)."
            COPIED_COUNT=$((COPIED_COUNT + 1))
            continue
        fi

        if [[ "$path" =~ ^/mnt/c/ || "$path" =~ ^/c/ ]]; then
            echo -e "  ${GREEN}>>> Rehydrating Windows host browser profile into VM Chrome...${RESET}"
            remote_dest="${VM_HOST}:~/.config/google-chrome"
            [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ~/.config/google-chrome" 2>/dev/null || true
            CMD=("${RSYNC_BASE[@]}")
            if [ -n "$excludes" ]; then
                for exc in $excludes; do
                    CMD+=("$exc")
                done
            fi
            "${CMD[@]}" "${path}/" "${remote_dest}/"
            echo -e "  ${GREEN}✓${RESET} Windows host tokens & sessions synced to Linux Chrome."
            COPIED_COUNT=$((COPIED_COUNT + 1))
            continue
        fi

        rel_path="${path#$HOME/}"
        remote_dest="${VM_HOST}:~/${rel_path}"

        CMD=("${RSYNC_BASE[@]}")
        if [ -n "$excludes" ]; then
            for exc in $excludes; do
                CMD+=("$exc")
            done
        fi

        if [ -d "$path" ]; then
            remote_parent=$(dirname "~/${rel_path}")
            [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ${remote_parent}" 2>/dev/null || true
            "${CMD[@]}" "${path}/" "${VM_HOST}:~/${rel_path}/"
        else
            remote_parent=$(dirname "~/${rel_path}")
            [ -z "$DRY_OPT" ] && ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "mkdir -p ${remote_parent}" 2>/dev/null || true
            "${CMD[@]}" "${path}" "${VM_HOST}:~/${rel_path}"
        fi

        # Permission enforcement for sensitive keys
        if [ "$item" == "SSH Client Keys & Configs" ] && [ -z "$DRY_OPT" ]; then
            ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.ssh && chmod 600 ~/.ssh/id_* ~/.ssh/config 2>/dev/null || true"
        fi
        if [ "$item" == "GNOME Keyring Secrets" ] && [ -z "$DRY_OPT" ]; then
            ssh ${SSH_CFG_OPT} -p "${VM_PORT}" "${VM_HOST}" "chmod 700 ~/.local/share/keyrings && chmod 600 ~/.local/share/keyrings/* 2>/dev/null || true"
        fi
        if [ "$item" == "GitHub CLI (gh) & Git Config" ] && [ -z "$DRY_OPT" ] && [ -f "${HOME}/.gitconfig" ]; then
            "${RSYNC_BASE[@]}" "${HOME}/.gitconfig" "${VM_HOST}:~/.gitconfig"
        fi

        echo -e "  ${GREEN}✓ Done.${RESET}\n"
        COPIED_COUNT=$((COPIED_COUNT + 1))
    else
        echo -e "  ${DIM}Skipped.${RESET}\n"
    fi
done

# Optional Custom Files Prompt
echo -e "\n${BOLD}${MAGENTA}Would you like to copy any custom files or folders to the VM?${RESET}"
read -rp "$(echo -e "Add custom files? (y/N): ")" WANT_CUSTOM
if [[ "${WANT_CUSTOM^^}" =~ ^[Y] ]]; then
    while true; do
        read -rp "Enter file or directory path (leave blank to finish): " cpath
        [ -z "$cpath" ] && break
        cpath="${cpath/#\~/$HOME}"
        real_cpath=$(realpath "$cpath" 2>/dev/null || echo "$cpath")
        app_dir=$(realpath "${SCRIPT_DIR}/.." 2>/dev/null || echo "")

        # Anti-recursion: Never copy our app or the migration-toolkit again
        if [ "$real_cpath" == "$SCRIPT_DIR" ] || [ "$real_cpath" == "$app_dir" ] || [[ "$real_cpath" =~ flowera ]] || [[ "$real_cpath" =~ migration-toolkit ]]; then
            echo -e "  ${YELLOW}! Anti-recursion guard: Skipping our app/migration-toolkit to prevent duplicate or recursive copying.${RESET}"
            continue
        fi

        if [ -e "$cpath" ]; then
            echo -e "  ${GREEN}>>> Copying $cpath...${RESET}"
            if [ -d "$cpath" ]; then
                base=$(basename "$cpath")
                "${RSYNC_BASE[@]}" --exclude='migration-toolkit' --exclude='.git' --exclude='node_modules' --exclude='target' "$cpath/" "${VM_HOST}:~/${base}/"
            else
                "${RSYNC_BASE[@]}" "$cpath" "${VM_HOST}:~/"
            fi
            echo -e "  ${GREEN}✓ Done.${RESET}"
            COPIED_COUNT=$((COPIED_COUNT + 1))
        else
            echo -e "  ${YELLOW}! Warning: Path not found: $cpath${RESET}"
        fi
    done
fi

echo -e "\n${BOLD}${GREEN}======================================================================${RESET}"
echo -e "${BOLD}${GREEN} Step-by-step migration complete!                                     ${RESET}"
echo -e "${BOLD}${GREEN} Total items copied to ${VM_HOST}: ${COPIED_COUNT}${RESET}"
echo -e "${BOLD}${GREEN}======================================================================${RESET}"

if [ -z "$DRY_OPT" ] && [ $COPIED_COUNT -gt 0 ]; then
    cat << EOF

${BOLD}Next Step: Run the VM Memory Deduplication & Setup Tool${RESET}
Connect to your VM to activate Kernel Samepage Merging (KSM) and native app sharing:

  ${CYAN}ssh -p ${VM_PORT} ${VM_HOST}${RESET}
  ${CYAN}sudo bash ~/vm-setup/setup_vm_memory_dedup.sh${RESET}

To monitor active RAM savings:
  ${CYAN}~/vm-setup/check_memory_savings.sh${RESET}
EOF
fi
