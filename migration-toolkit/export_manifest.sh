#!/usr/bin/env bash
# ==============================================================================
# export_manifest.sh
# Scans and exports local application packages, configurations, Google tokens,
# and toolchain manifests without copying bloated binaries or duplicate libraries.
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
RESET="\033[0m"

echo -e "${BOLD}${BLUE}======================================================${RESET}"
echo -e "${BOLD}${BLUE}   Local Desktop & System Manifest Exporter           ${RESET}"
echo -e "${BOLD}${BLUE}======================================================${RESET}"

# 1. System Package Manifest (APT / Homebrew)
echo -e "\n${CYAN}[1/6] Scanning manually installed system packages...${RESET}"
if command -v apt-mark >/dev/null 2>&1; then
    apt-mark showmanual | sort > "${MANIFEST_DIR}/apt_packages.txt"
    echo -e "  ${GREEN}✓${RESET} Exported $(wc -l < "${MANIFEST_DIR}/apt_packages.txt") APT packages to apt_packages.txt"
elif command -v brew >/dev/null 2>&1; then
    brew list --formula | sort > "${MANIFEST_DIR}/brew_formulas.txt" 2>/dev/null || true
    brew list --cask | sort > "${MANIFEST_DIR}/brew_casks.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported macOS Homebrew formulas and casks to manifest/"
else
    echo -e "  ${YELLOW}!${RESET} Neither apt-mark nor brew found. Skipping system package export."
fi

# 2. Developer Toolchains & Language Runtimes
echo -e "\n${CYAN}[2/6] Scanning developer toolchains and global packages...${RESET}"

# Rust / Cargo
if command -v cargo >/dev/null 2>&1; then
    cargo install --list > "${MANIFEST_DIR}/cargo_packages.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported Cargo installed crates to cargo_packages.txt"
fi

# Bun / NPM
if command -v bun >/dev/null 2>&1; then
    bun pm ls -g > "${MANIFEST_DIR}/bun_global.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported Bun global packages to bun_global.txt"
fi
if command -v npm >/dev/null 2>&1; then
    npm list -g --depth=0 > "${MANIFEST_DIR}/npm_global.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported NPM global packages to npm_global.txt"
fi

# Python / UV / Pip
if command -v uv >/dev/null 2>&1; then
    uv tool list > "${MANIFEST_DIR}/uv_tools.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported UV tools to uv_tools.txt"
fi
if command -v pip3 >/dev/null 2>&1; then
    pip3 list --format=freeze > "${MANIFEST_DIR}/pip_packages.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported Pip packages to pip_packages.txt"
fi

# Go Binaries
if [ -d "${HOME}/go/bin" ]; then
    ls -1 "${HOME}/go/bin" > "${MANIFEST_DIR}/go_binaries.txt" 2>/dev/null || true
    echo -e "  ${GREEN}✓${RESET} Exported Go binaries list to go_binaries.txt"
fi

# 3. Google Browser Sessions & Profiles
echo -e "\n${CYAN}[3/7] Detecting Google Chrome and Chromium browser profiles...${RESET}"
CHROME_DETECTED=0
> "${MANIFEST_DIR}/chrome_paths.txt"

# Standard Linux & macOS Chrome paths
for path in \
    "${HOME}/.config/google-chrome" \
    "${HOME}/.config/google-chrome-for-testing" \
    "${HOME}/.config/google-chrome-beta" \
    "${HOME}/.config/chromium" \
    "${HOME}/Library/Application Support/Google/Chrome"; do
    if [ -d "${path}" ]; then
        echo "${path}" >> "${MANIFEST_DIR}/chrome_paths.txt"
        echo -e "  ${GREEN}✓${RESET} Found browser profile: ${BOLD}${path}${RESET}"
        CHROME_DETECTED=1
    fi
done

# Check Windows Chrome in WSL or Git Bash (if mounted)
for c_drive in "/mnt/c" "/c" "C:"; do
    if [ -d "${c_drive}/Users" ]; then
        for win_user_dir in "${c_drive}/Users"/*; do
            win_chrome="${win_user_dir}/AppData/Local/Google/Chrome/User Data"
            if [ -d "${win_chrome}" ]; then
                echo "${win_chrome}" >> "${MANIFEST_DIR}/chrome_paths.txt"
                echo -e "  ${GREEN}✓${RESET} Found Windows host Chrome profile: ${BOLD}${win_chrome}${RESET}"
                CHROME_DETECTED=1
            fi
        done
    fi
done

if [ "${CHROME_DETECTED}" -eq 0 ]; then
    echo -e "  ${YELLOW}!${RESET} No active Google Chrome/Chromium directory detected in standard paths."
fi

# 4. Host Desktop Applications (Windows & macOS) for Zero-RAM Remote Bridging
echo -e "\n${CYAN}[4/7] Scanning Host Desktop Applications for Zero-RAM Remote Bridging...${RESET}"
> "${MANIFEST_DIR}/host_apps.txt"

# Windows Host Apps (WSL / Git Bash)
for c_drive in "/mnt/c" "/c" "C:"; do
    if [ -d "${c_drive}/Program Files" ] || [ -d "${c_drive}/Program Files (x86)" ]; then
        for app_check in \
            "Google/Chrome/Application/chrome.exe:Google Chrome:chrome.exe" \
            "Microsoft/Edge/Application/msedge.exe:Microsoft Edge:msedge.exe" \
            "Mozilla Firefox/firefox.exe:Mozilla Firefox:firefox.exe" \
            "Notepad++/notepad++.exe:Notepad++:notepad++.exe" \
            "Microsoft VS Code/Code.exe:Visual Studio Code:code.cmd" \
            "Git/git-bash.exe:Git Bash:git-bash.exe"; do

            subpath="${app_check%%:*}"
            rest="${app_check#*:}"
            app_name="${rest%%:*}"
            app_cmd="${rest#*:}"

            for pf in "${c_drive}/Program Files" "${c_drive}/Program Files (x86)"; do
                if [ -f "${pf}/${subpath}" ]; then
                    echo "windows|${app_name}|${app_cmd}|${pf}/${subpath}" >> "${MANIFEST_DIR}/host_apps.txt"
                    echo -e "  ${GREEN}✓${RESET} Found Windows Host App: ${BOLD}${app_name}${RESET}"
                    break
                fi
            done
        done

        if [ -d "${c_drive}/Users" ]; then
            for win_user_dir in "${c_drive}/Users"/*; do
                local_prog="${win_user_dir}/AppData/Local/Programs"
                if [ -d "${local_prog}/Microsoft VS Code" ]; then
                    echo "windows|Visual Studio Code|code.cmd|${local_prog}/Microsoft VS Code/Code.exe" >> "${MANIFEST_DIR}/host_apps.txt"
                    echo -e "  ${GREEN}✓${RESET} Found Windows Host App: ${BOLD}Visual Studio Code (User Install)${RESET}"
                fi
                if [ -d "${win_user_dir}/AppData/Local/slack" ]; then
                    echo "windows|Slack|slack.exe|${win_user_dir}/AppData/Local/slack/slack.exe" >> "${MANIFEST_DIR}/host_apps.txt"
                    echo -e "  ${GREEN}✓${RESET} Found Windows Host App: ${BOLD}Slack${RESET}"
                fi
            done
        fi
    fi
done

# macOS Host Apps
if [ -d "/Applications" ]; then
    for app in /Applications/*.app; do
        if [ -d "$app" ]; then
            app_base=$(basename "$app" .app)
            echo "macos|${app_base}|open -a '${app_base}'|${app}" >> "${MANIFEST_DIR}/host_apps.txt"
            echo -e "  ${GREEN}✓${RESET} Found macOS Host App: ${BOLD}${app_base}${RESET}"
        fi
    done
fi

# 5. GNOME Keyring & Secret Service (for decrypting stored Google cookies/tokens)
echo -e "\n${CYAN}[5/7] Detecting OS Keyring and Secret Service DB...${RESET}"
> "${MANIFEST_DIR}/keyring_paths.txt"
if [ -d "${HOME}/.local/share/keyrings" ]; then
    echo "${HOME}/.local/share/keyrings" >> "${MANIFEST_DIR}/keyring_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found GNOME Keyring store: ${BOLD}${HOME}/.local/share/keyrings${RESET}"
else
    echo -e "  ${YELLOW}!${RESET} No standard ~/.local/share/keyrings directory found."
fi

# 6. Cloud & Developer Tokens
echo -e "\n${CYAN}[6/7] Detecting Cloud & Developer auth tokens...${RESET}"
> "${MANIFEST_DIR}/token_paths.txt"

if [ -d "${HOME}/.config/gcloud" ]; then
    echo "${HOME}/.config/gcloud" >> "${MANIFEST_DIR}/token_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found Google Cloud SDK credentials: ${BOLD}${HOME}/.config/gcloud${RESET}"
fi

if [ -d "${HOME}/.config/gh" ]; then
    echo "${HOME}/.config/gh" >> "${MANIFEST_DIR}/token_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found GitHub CLI authentication: ${BOLD}${HOME}/.config/gh${RESET}"
fi

if [ -f "${HOME}/.gitconfig" ]; then
    echo "${HOME}/.gitconfig" >> "${MANIFEST_DIR}/token_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found Git configuration: ${BOLD}${HOME}/.gitconfig${RESET}"
fi

if [ -d "${HOME}/.docker" ]; then
    echo "${HOME}/.docker" >> "${MANIFEST_DIR}/token_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found Docker client config: ${BOLD}${HOME}/.docker${RESET}"
fi

if [ -d "${HOME}/.ssh" ]; then
    echo "${HOME}/.ssh" >> "${MANIFEST_DIR}/token_paths.txt"
    echo -e "  ${GREEN}✓${RESET} Found SSH keys & config: ${BOLD}${HOME}/.ssh${RESET}"
fi

# 7. Generate Summary
echo -e "\n${CYAN}[7/7] Generating summary report...${RESET}"
cat << EOF > "${MANIFEST_DIR}/summary.json"
{
  "timestamp": "$(date -u +"%Y-%m-%dT%H:%M:%SZ")",
  "hostname": "$(hostname)",
  "user": "${USER}",
  "arch": "$(uname -m)",
  "kernel": "$(uname -r)",
  "manifest_files": [
$(find "${MANIFEST_DIR}" -type f -not -name "summary.json" -exec basename {} \; | sed 's/^/    "/;s/$/",/' | sed '$ s/,$//')
  ]
}
EOF

echo -e "${BOLD}${GREEN}======================================================${RESET}"
echo -e "${BOLD}${GREEN} Manifest export complete!                             ${RESET}"
echo -e "${BOLD}${GREEN} Files written to: ${MANIFEST_DIR}${RESET}"
echo -e "${BOLD}${GREEN}======================================================${RESET}"
