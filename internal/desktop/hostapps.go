package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// HostAppItem describes an application, session profile, or toolchain on the host system.
type HostAppItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Category        string   `json:"category"`
	Description     string   `json:"description"`
	Detected        bool     `json:"detected"`
	DetectedPaths   []string `json:"detected_paths,omitempty"`
	DefaultSelected bool     `json:"default_selected"`
}

// knownAppDef defines how to probe and migrate a specific application or session.
type knownAppDef struct {
	id              string
	name            string
	category        string
	description     string
	defaultSelected bool
	probeFn         func() []string
	packFn          func(tw *tar.Writer, paths []string) error
}

// DetectHostApps scans the host machine for installed applications, session tokens,
// and configurations across Linux, macOS, and Windows WSL.
func DetectHostApps() []HostAppItem {
	defs := getKnownAppDefs()
	items := make([]HostAppItem, 0, len(defs))

	for _, def := range defs {
		paths := def.probeFn()
		detected := len(paths) > 0 || def.id == "host_bridge"
		items = append(items, HostAppItem{
			ID:              def.id,
			Name:            def.name,
			Category:        def.category,
			Description:     def.description,
			Detected:        detected,
			DetectedPaths:   paths,
			DefaultSelected: detected && def.defaultSelected,
		})
	}

	return items
}

// MigrateHostApps packages selected host application sessions, tokens, and configurations,
// strips cache bloat to maintain zero redundant memory, streams them into the container's
// /home/headless volume, and installs the host execution bridge launchers.
func (m *Manager) MigrateHostApps(ctx context.Context, desktopID string, selectedIDs []string) error {
	if len(selectedIDs) == 0 {
		return nil
	}

	sanitizedID := sanitizeName(desktopID)
	containerName := "desktop_" + sanitizedID

	// Inspect container to ensure it exists
	inspect, err := m.client.ContainerInspect(ctx, containerName)
	if err != nil {
		return fmt.Errorf("inspect %s for migration: %w", containerName, err)
	}

	selectedMap := make(map[string]bool)
	for _, id := range selectedIDs {
		selectedMap[strings.ToLower(strings.TrimSpace(id))] = true
	}

	// Always include host_bridge when apps are being migrated
	selectedMap["host_bridge"] = true

	// Build tar archive buffer
	tarBuf := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuf)

	defs := getKnownAppDefs()
	migratedCount := 0

	for _, def := range defs {
		if !selectedMap[def.id] {
			continue
		}

		paths := def.probeFn()
		if len(paths) == 0 && def.id != "host_bridge" {
			continue
		}

		if err := def.packFn(tw, paths); err != nil {
			log.Printf("hostapps: packing %s for %s: %v", def.id, desktopID, err)
			continue
		}
		migratedCount++
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar archive: %w", err)
	}

	if tarBuf.Len() == 0 {
		return nil
	}

	// Stream archive directly into container at /home/headless
	copyOpts := container.CopyToContainerOptions{
		AllowOverwriteDirWithFile: false,
		CopyUIDGID:                false,
	}

	if err := m.client.CopyToContainer(ctx, inspect.ID, "/home/headless", tarBuf, copyOpts); err != nil {
		return fmt.Errorf("copy host apps to %s: %w", containerName, err)
	}

	// Fix permissions inside container:
	// - Set ownership to headless:headless (1000:1000)
	// - Secure .ssh and keyring directories
	fixCmd := `
chown -R 1000:1000 /home/headless/.local /home/headless/.config /home/headless/Desktop 2>/dev/null || true
if [ -d /home/headless/.ssh ]; then
    chown -R 1000:1000 /home/headless/.ssh
    chmod 700 /home/headless/.ssh
    chmod 600 /home/headless/.ssh/id_* /home/headless/.ssh/config /home/headless/.ssh/known_hosts 2>/dev/null || true
fi
if [ -d /home/headless/.local/share/keyrings ]; then
    chown -R 1000:1000 /home/headless/.local/share/keyrings
    chmod 700 /home/headless/.local/share/keyrings
    chmod 600 /home/headless/.local/share/keyrings/* 2>/dev/null || true
fi
if [ -d /home/headless/.local/bin ]; then
    chmod +x /home/headless/.local/bin/* 2>/dev/null || true
fi
if [ -d /home/headless/Desktop ]; then
    chmod +x /home/headless/Desktop/*.desktop 2>/dev/null || true
fi
if [ -d /home/headless/.local/share/applications ]; then
    chmod +x /home/headless/.local/share/applications/*.desktop 2>/dev/null || true
fi
`
	execCfg := container.ExecOptions{
		Cmd:          []string{"bash", "-c", fixCmd},
		AttachStdout: true,
		AttachStderr: true,
	}
	if execResp, err := m.client.ContainerExecCreate(ctx, inspect.ID, execCfg); err == nil {
		_ = m.client.ContainerExecStart(ctx, execResp.ID, container.ExecStartOptions{})
		_ = waitExec(ctx, m.client, execResp.ID, fmt.Sprintf("permissions fix for %s", containerName))
	}

	log.Printf("hostapps: successfully migrated %d component(s) into %s", migratedCount, containerName)
	return nil
}

// -----------------------------------------------------------------------------
// Known Application Definitions & Packaging
// -----------------------------------------------------------------------------

func getKnownAppDefs() []knownAppDef {
	home, _ := os.UserHomeDir()

	return []knownAppDef{
		{
			id:              "chrome",
			name:            "Google Chrome / Chromium",
			category:        "Apps & Sessions",
			description:     "Profile, active cookies, sessions, login data (Cache stripped to save RAM)",
			defaultSelected: true,
			probeFn: func() []string {
				candidates := []string{
					filepath.Join(home, ".config", "google-chrome-for-testing"),
					filepath.Join(home, ".config", "google-chrome"),
					filepath.Join(home, ".config", "chromium"),
					filepath.Join(home, "Library", "Application Support", "Google", "Chrome"),
				}
				// Windows WSL candidate paths
				for _, cDrive := range []string{"/mnt/c", "/c"} {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Local", "Google", "Chrome", "User Data")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						candidates = append(candidates, matches...)
					}
				}
				var found []string
				for _, p := range candidates {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				// Use first valid profile found
				src := paths[0]
				// Copy to .config/google-chrome
				if err := writeDirToTar(tw, src, ".config/google-chrome", isCacheOrTransient); err != nil {
					return err
				}
				// Also add symlink .config/chromium -> google-chrome so Chromium in the VM picks up the profile
				header := &tar.Header{
					Name:     ".config/chromium",
					Linkname: "google-chrome",
					Typeflag: tar.TypeSymlink,
					Mode:     0755,
					ModTime:  time.Now(),
					Uid:      1000,
					Gid:      1000,
				}
				return tw.WriteHeader(header)
			},
		},
		{
			id:              "firefox",
			name:            "Mozilla Firefox",
			category:        "Apps & Sessions",
			description:     "Profiles, saved logins, cookies, tabs (Cache stripped)",
			defaultSelected: true,
			probeFn: func() []string {
				candidates := []string{
					filepath.Join(home, ".mozilla", "firefox"),
					filepath.Join(home, "Library", "Application Support", "Firefox"),
				}
				for _, cDrive := range []string{"/mnt/c", "/c"} {
					pattern := filepath.Join(cDrive, "Users", "*", "AppData", "Roaming", "Mozilla", "Firefox", "Profiles")
					if matches, err := filepath.Glob(pattern); err == nil && len(matches) > 0 {
						candidates = append(candidates, matches...)
					}
				}
				var found []string
				for _, p := range candidates {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".mozilla/firefox", isCacheOrTransient)
			},
		},
		{
			id:              "gh",
			name:            "GitHub CLI & Git Config",
			category:        "Apps & Sessions",
			description:     "Logged-in GitHub token, credentials, and ~/.gitconfig",
			defaultSelected: true,
			probeFn: func() []string {
				var found []string
				ghConfig := filepath.Join(home, ".config", "gh")
				if fi, err := os.Stat(ghConfig); err == nil && fi.IsDir() {
					found = append(found, ghConfig)
				}
				gitConfig := filepath.Join(home, ".gitconfig")
				if fi, err := os.Stat(gitConfig); err == nil && !fi.IsDir() {
					found = append(found, gitConfig)
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					if strings.HasSuffix(p, ".gitconfig") {
						if err := writeFileToTar(tw, p, ".gitconfig", 0644); err != nil {
							return err
						}
					} else {
						if err := writeDirToTar(tw, p, ".config/gh", nil); err != nil {
							return err
						}
					}
				}
				return nil
			},
		},
		{
			id:              "docker",
			name:            "Docker Hub & Engine Config",
			category:        "Apps & Sessions",
			description:     "Docker credentials, contexts, and client config",
			defaultSelected: true,
			probeFn: func() []string {
				dockerDir := filepath.Join(home, ".docker")
				if fi, err := os.Stat(dockerDir); err == nil && fi.IsDir() {
					return []string{dockerDir}
				}
				return nil
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".docker", func(name, fullPath string, isDir bool) bool {
					// exclude docker.sock or socket files
					return strings.HasSuffix(name, ".sock")
				})
			},
		},
		{
			id:              "vscode",
			name:            "OpenCode / VS Code Editor",
			category:        "Apps & Sessions",
			description:     "Editor state, settings, keybindings, and extensions",
			defaultSelected: true,
			probeFn: func() []string {
				candidates := []string{
					filepath.Join(home, ".config", "Code"),
					filepath.Join(home, ".config", "opencode"),
					filepath.Join(home, ".vscode"),
				}
				var found []string
				for _, p := range candidates {
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						found = append(found, p)
					}
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					base := filepath.Base(p)
					destDir := ".config/" + base
					if base == ".vscode" {
						destDir = ".vscode"
					}
					if err := writeDirToTar(tw, p, destDir, isCacheOrTransient); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			id:              "ssh",
			name:            "SSH Client Keys & Config",
			category:        "Apps & Sessions",
			description:     "SSH identity keys, configs, and known hosts (strict 600 permissions)",
			defaultSelected: true,
			probeFn: func() []string {
				sshDir := filepath.Join(home, ".ssh")
				if fi, err := os.Stat(sshDir); err == nil && fi.IsDir() {
					return []string{sshDir}
				}
				return nil
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".ssh", nil)
			},
		},
		{
			id:              "gcloud",
			name:            "Google Cloud SDK (gcloud)",
			category:        "Apps & Sessions",
			description:     "Active GCP accounts, credentials, and application default tokens",
			defaultSelected: true,
			probeFn: func() []string {
				gcloudDir := filepath.Join(home, ".config", "gcloud")
				if fi, err := os.Stat(gcloudDir); err == nil && fi.IsDir() {
					return []string{gcloudDir}
				}
				return nil
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".config/gcloud", isCacheOrTransient)
			},
		},
		{
			id:              "azure",
			name:            "Azure CLI (az)",
			category:        "Apps & Sessions",
			description:     "Active Azure subscriptions, tokens, and tenant configurations",
			defaultSelected: false,
			probeFn: func() []string {
				azDir := filepath.Join(home, ".azure")
				if fi, err := os.Stat(azDir); err == nil && fi.IsDir() {
					return []string{azDir}
				}
				return nil
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".azure", isCacheOrTransient)
			},
		},
		{
			id:              "keyring",
			name:            "GNOME Keyring Secrets",
			category:        "Apps & Sessions",
			description:     "OS encrypted secrets database for browser/app passwords",
			defaultSelected: true,
			probeFn: func() []string {
				keyringDir := filepath.Join(home, ".local", "share", "keyrings")
				if fi, err := os.Stat(keyringDir); err == nil && fi.IsDir() {
					return []string{keyringDir}
				}
				return nil
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				if len(paths) == 0 {
					return nil
				}
				return writeDirToTar(tw, paths[0], ".local/share/keyrings", nil)
			},
		},
		{
			id:              "dotfiles",
			name:            "Shell & Environment Dotfiles",
			category:        "Environment",
			description:     "Shell startup scripts (~/.bashrc, ~/.profile, ~/.zshrc, fish)",
			defaultSelected: true,
			probeFn: func() []string {
				var found []string
				for _, name := range []string{".bashrc", ".profile", ".bash_aliases", ".zshrc"} {
					p := filepath.Join(home, name)
					if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
						found = append(found, p)
					}
				}
				fishDir := filepath.Join(home, ".config", "fish")
				if fi, err := os.Stat(fishDir); err == nil && fi.IsDir() {
					found = append(found, fishDir)
				}
				return found
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				for _, p := range paths {
					base := filepath.Base(p)
					if fi, err := os.Stat(p); err == nil && fi.IsDir() {
						if err := writeDirToTar(tw, p, ".config/fish", nil); err != nil {
							return err
						}
					} else {
						if err := writeFileToTar(tw, p, base, 0644); err != nil {
							return err
						}
					}
				}
				return nil
			},
		},
		{
			id:              "host_bridge",
			name:            "Universal Host Bridge (Zero-RAM Emulation)",
			category:        "Execution Bridge",
			description:     "Runs host apps directly on your local host with zero duplicate RAM, lightly emulated on VM",
			defaultSelected: true,
			probeFn: func() []string {
				// Always available
				return []string{"host_bridge"}
			},
			packFn: func(tw *tar.Writer, paths []string) error {
				return packHostBridgeLaunchers(tw)
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Host Bridge Launchers & CLI Wrappers
// -----------------------------------------------------------------------------

func packHostBridgeLaunchers(tw *tar.Writer) error {
	// 1. host-run wrapper
	hostRunScript := `#!/usr/bin/env bash
# ==============================================================================
# host-run: Universal Host Execution Bridge
# Executes commands on your local host machine from inside the VM with zero duplicate RAM.
# ==============================================================================
set -euo pipefail

CP_URL="${CONTROL_PLANE_URL:-http://host.docker.internal:8080}"
TUNNEL_PORT="${HOST_TUNNEL_PORT:-2222}"

usage() {
    cat << EOF
Usage: host-run <command> [args...]
       host-run open <file-or-url>
       host-run clip <text or pipe from stdin>

Runs apps directly on the host machine with zero duplicate VM RAM.
EOF
    exit 0
}

[ $# -eq 0 ] && usage

subcmd="$1"

case "$subcmd" in
    open)
        shift
        target="${1:-.}"
        # Try HTTP bridge to control plane first
        if curl -s -f -X POST "${CP_URL}/api/host-bridge/open" -H "Content-Type: application/json" -d "{\"target\": \"${target}\"}" >/dev/null 2>&1; then
            echo "Opened on host desktop: ${target}"
            exit 0
        fi
        # Fallback to SSH tunnel if active
        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "xdg-open '${target}' 2>/dev/null || open '${target}' 2>/dev/null || cmd.exe /c start '' '${target}'" 2>/dev/null || true
            exit 0
        fi
        echo "Error: Host bridge not reachable at ${CP_URL} or tunnel port ${TUNNEL_PORT}."
        exit 1
        ;;
    clip)
        shift
        if [ $# -gt 0 ]; then
            text="$*"
        else
            text=$(cat)
        fi
        if curl -s -f -X POST "${CP_URL}/api/host-bridge/clip" -H "Content-Type: application/json" -d "{\"text\": $(printf '%s' "$text" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}" >/dev/null 2>&1; then
            echo "Copied to host clipboard!"
            exit 0
        fi
        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            echo -n "$text" | ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "pbcopy 2>/dev/null || clip.exe 2>/dev/null || xclip -selection clipboard 2>/dev/null || wl-copy 2>/dev/null" 2>/dev/null || true
            echo "Copied to host clipboard via tunnel!"
            exit 0
        fi
        echo "Error: Host bridge not reachable."
        exit 1
        ;;
    -h|--help)
        usage
        ;;
    *)
        # Generic command execution on host desktop
        cmd="$1"
        shift
        args_json="[]"
        if [ $# -gt 0 ]; then
            args_json=$(python3 -c "import json,sys; print(json.dumps(sys.argv[1:]))" "$@")
        fi
        payload=$(python3 -c "import json,sys; print(json.dumps({'cmd': sys.argv[1], 'args': json.loads(sys.argv[2])}))" "$cmd" "$args_json")

        response=$(curl -s -f -X POST "${CP_URL}/api/host-bridge/exec" -H "Content-Type: application/json" -d "$payload" 2>/dev/null || true)
        if [ -n "$response" ]; then
            python3 -c "import json,sys; res=json.loads(sys.argv[1]); sys.stdout.write(res.get('stdout','')); sys.stderr.write(res.get('stderr','')); sys.exit(res.get('exit_code',0))" "$response"
            exit $?
        fi

        # Fallback to SSH tunnel if active
        if timeout 1 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/${TUNNEL_PORT}" 2>/dev/null; then
            ssh -F /dev/null -p "${TUNNEL_PORT}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null 127.0.0.1 "$cmd" "$@"
            exit $?
        fi
        echo "Error: Could not reach host bridge at ${CP_URL} to execute '${cmd}'."
        exit 1
        ;;
esac
`

	if err := writeStringToTar(tw, ".local/bin/host-run", hostRunScript, 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/host-open", "#!/usr/bin/env bash\nexec host-run open \"$@\"\n", 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/host-clip", "#!/usr/bin/env bash\nexec host-run clip \"$@\"\n", 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/win-run", "#!/usr/bin/env bash\nexec host-run \"$@\"\n", 0755); err != nil {
		return err
	}
	if err := writeStringToTar(tw, ".local/bin/mac-run", "#!/usr/bin/env bash\nexec host-run \"$@\"\n", 0755); err != nil {
		return err
	}

	// 2. Host app wrappers in .local/bin
	chromeWrapper := "#!/usr/bin/env bash\nexec host-run google-chrome \"$@\"\n"
	if err := writeStringToTar(tw, ".local/bin/chrome-host", chromeWrapper, 0755); err != nil {
		return err
	}
	codeWrapper := "#!/usr/bin/env bash\nexec host-run code \"$@\"\n"
	if err := writeStringToTar(tw, ".local/bin/code-host", codeWrapper, 0755); err != nil {
		return err
	}
	termWrapper := "#!/usr/bin/env bash\nexec host-run x-terminal-emulator \"$@\"\n"
	if err := writeStringToTar(tw, ".local/bin/terminal-host", termWrapper, 0755); err != nil {
		return err
	}

	// 3. Desktop launchers in Desktop/ and .local/share/applications/
	chromeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=Google Chrome (Host)
Comment=Run Google Chrome on host desktop with zero duplicate VM RAM
Exec=/home/headless/.local/bin/chrome-host %U
Icon=google-chrome
Terminal=false
Categories=Network;WebBrowser;
`
	codeDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=VS Code (Host)
Comment=Run Visual Studio Code on host desktop with zero duplicate VM RAM
Exec=/home/headless/.local/bin/code-host %F
Icon=code
Terminal=false
Categories=Development;IDE;
`
	termDesktop := `[Desktop Entry]
Version=1.0
Type=Application
Name=Host Terminal (Host)
Comment=Open terminal on host desktop
Exec=/home/headless/.local/bin/terminal-host
Icon=utilities-terminal
Terminal=false
Categories=Utility;TerminalEmulator;
`

	entries := []struct {
		filename string
		content  string
	}{
		{"chrome-host.desktop", chromeDesktop},
		{"code-host.desktop", codeDesktop},
		{"terminal-host.desktop", termDesktop},
	}

	for _, entry := range entries {
		if err := writeStringToTar(tw, "Desktop/"+entry.filename, entry.content, 0755); err != nil {
			return err
		}
		if err := writeStringToTar(tw, ".local/share/applications/"+entry.filename, entry.content, 0755); err != nil {
			return err
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// Tar Archiving Helpers with Cache Filtering
// -----------------------------------------------------------------------------

// isCacheOrTransient returns true if a directory or file represents disposable caches,
// metrics, or transient crash dumps that cause RAM redundancy and page cache bloat.
func isCacheOrTransient(name, fullPath string, isDir bool) bool {
	lower := strings.ToLower(name)
	lowerPath := strings.ToLower(fullPath)

	// Strictly exclude self / app repository to prevent recursive nesting
	if strings.Contains(lowerPath, "flowera") || strings.Contains(lowerPath, "migration-toolkit") {
		return true
	}

	// Browser and toolchain caches
	if strings.Contains(lower, "cache") {
		return true
	}
	if strings.HasPrefix(lower, "cached") {
		return true
	}
	if lower == "gpu-cache" || lower == "gpucache" || lower == "code cache" || lower == "codecache" || lower == "dawncache" {
		return true
	}
	if lower == "crashpad" || lower == "crash reports" || strings.HasPrefix(lower, "browsermetrics") {
		return true
	}
	if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".dmp") {
		return true
	}
	if isDir && (lower == "logs" || lower == "node_modules" || lower == "pkg" || lower == "temp") {
		return true
	}

	return false
}

// writeDirToTar recursively walks a source directory on the host and writes its
// contents to the tar archive under destPrefix, filtering through excludeFilter.
func writeDirToTar(tw *tar.Writer, srcDir, destPrefix string, excludeFilter func(name, fullPath string, isDir bool) bool) error {
	srcClean := filepath.Clean(srcDir)

	return filepath.Walk(srcClean, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable files gracefully
			return nil
		}

		rel, err := filepath.Rel(srcClean, path)
		if err != nil || rel == "." {
			return nil
		}

		isDir := info.IsDir()
		if excludeFilter != nil && excludeFilter(info.Name(), path, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}

		// Enforce size safety limit (skip individual files > 100MB to avoid OOM)
		if !isDir && info.Size() > 100*1024*1024 {
			return nil
		}

		tarPath := filepath.ToSlash(filepath.Join(destPrefix, rel))

		var linkTarget string
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				linkTarget = target
			}
		}

		header, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return nil
		}

		header.Name = tarPath
		header.Uid = 1000
		header.Gid = 1000
		header.Uname = "headless"
		header.Gname = "headless"

		// Normalize permissions
		if isDir {
			header.Mode = 0755
		} else if info.Mode().IsRegular() {
			if info.Mode()&0111 != 0 {
				header.Mode = 0755
			} else {
				header.Mode = 0644
			}
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}

		return nil
	})
}

// writeFileToTar writes a single host file into the tar archive at tarPath.
func writeFileToTar(tw *tar.Writer, srcPath, tarPath string, mode int64) error {
	info, err := os.Stat(srcPath)
	if err != nil {
		return err
	}

	header := &tar.Header{
		Name:     tarPath,
		Mode:     mode,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Typeflag: tar.TypeReg,
		Uid:      1000,
		Gid:      1000,
		Uname:    "headless",
		Gname:    "headless",
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tw, f)
	return err
}

// writeStringToTar writes a virtual string into the tar archive at tarPath with given mode.
func writeStringToTar(tw *tar.Writer, tarPath, content string, mode int64) error {
	data := []byte(content)
	header := &tar.Header{
		Name:     tarPath,
		Mode:     mode,
		Size:     int64(len(data)),
		ModTime:  time.Now(),
		Typeflag: tar.TypeReg,
		Uid:      1000,
		Gid:      1000,
		Uname:    "headless",
		Gname:    "headless",
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err := tw.Write(data)
	return err
}

// HostOS returns the host operating system ("linux", "darwin", "windows").
func HostOS() string {
	return runtime.GOOS
}
