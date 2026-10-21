package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/docker/docker/api/types/mount"
)

// A shared folder is the one folder of the person's computer a worker may read and write, chosen when the worker is
// made. A worker cannot see the person's files, so without it everything has to go through the host-app viewer a key
// at a time; with it the worker computes with its own tools and the person's apps open the same files.
//
// It is a bind mount of that folder - nothing else on the computer is reachable, no new HTTP route is added - and it is
// read-write. The mount is at SharedMountPoint, outside $HOME: the base image's startup runs `chown -R $UID $HOME` on
// every start, which would walk into a folder of the person's own files. The agent links ~/PC to the mount.

const (
	// SharedMountPoint is where the shared folder is mounted in the desktop.
	SharedMountPoint = "/mnt/pc"

	// SharedLinkPath is where the worker and the person's request see it: the agent links it to SharedMountPoint.
	SharedLinkPath = "/home/headless/PC"

	// SharedEnvFolder and SharedEnvMount tell the agent which folder this is (as the person writes it) and where it is.
	SharedEnvFolder = "DECURIX_SHARED_FOLDER"
	SharedEnvMount  = "DECURIX_SHARED_MOUNT"

	sharedLabel = "desktop-shared-folder"

	// hostMountSourceEnv is where the Docker daemon sees the host's C: drive when it is not /mnt/c (the control plane
	// itself sees it at DECURIX_HOST_MNT_C, read-only; the daemon resolves a bind's source on its own host).
	hostMountSourceEnv = "DECURIX_HOST_MNT_C_SOURCE"
)

// SharedFolder is a validated shared folder.
type SharedFolder struct {
	// Host is the folder as the person writes it (C:\Users\me\Documents\Decurix demo): shown on the desktop's card,
	// kept as its label, and told to the agent.
	Host string `json:"host"`
	// Source is the same folder as the Docker daemon resolves it for the bind (/mnt/c/Users/me/Documents/Decurix demo).
	Source string `json:"-"`
}

// sharedFS is what validation asks of the disk; tests replace it.
var sharedFS = struct {
	Stat         func(string) (os.FileInfo, error)
	EvalSymlinks func(string) (string, error)
	CDrives      func() []string
	Home         func() (string, error)
}{os.Stat, filepath.EvalSymlinks, getWindowsCDriveCandidates, os.UserHomeDir}

var (
	sharedWindowsPath = regexp.MustCompile(`^([A-Za-z]):(?:[\\/]|$)`)
	sharedWSLPath     = regexp.MustCompile(`^/mnt/([a-zA-Z])(?:/|$)`)
	sharedBadChar     = regexp.MustCompile("[\x00-\x1f\x7f\"<>|*?]")
)

// Folders no worker is given: system folders of each platform, and inside a home folder the places that hold
// credentials and sessions.
var (
	sharedWindowsSystem = map[string]bool{
		"windows": true, "program files": true, "program files (x86)": true, "programdata": true,
		"$recycle.bin": true, "system volume information": true, "recovery": true, "boot": true, "perflogs": true,
		"msocache": true, "config.msi": true, "documents and settings": true,
	}
	sharedPosixSystem = map[string]bool{
		"bin": true, "boot": true, "dev": true, "etc": true, "lib": true, "lib32": true, "lib64": true, "libx32": true,
		"proc": true, "root": true, "run": true, "sbin": true, "sys": true, "usr": true, "var": true, "system": true,
		"library": true, "applications": true, "private": true,
	}
	sharedSensitive = map[string]bool{
		"appdata": true, "application data": true, "local settings": true, ".ssh": true, ".aws": true, ".azure": true,
		".gnupg": true, ".kube": true, ".docker": true, ".config": true, ".claude": true, ".gcloud": true,
		".mozilla": true, ".password-store": true,
	}
)

// ParseSharedFolder validates the folder a person asks to share and says how it is mounted. "" is no shared folder
// (nil, nil). Anything else must be an absolute folder that exists on the C: drive of the person's computer (as C:\...
// or its WSL spelling /mnt/c/...) or, on a Linux or macOS host, an absolute folder - and not a drive root, a home
// folder, a system folder or a place that holds credentials, not a link, not a network path. The error is a plain
// sentence for the person.
func ParseSharedFolder(raw string) (*SharedFolder, error) {
	raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"'`))
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 400 || sharedBadChar.MatchString(raw) {
		return nil, fmt.Errorf("the shared folder is not a usable folder path")
	}
	if strings.HasPrefix(raw, `\\`) || (strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, "///")) {
		return nil, fmt.Errorf("a network folder cannot be shared: choose a folder on this computer")
	}

	switch {
	case sharedWindowsPath.MatchString(raw):
		return parseSharedWindows(raw, strings.ToUpper(raw[:1]), raw[2:])
	case sharedWSLPath.MatchString(raw):
		m := sharedWSLPath.FindStringSubmatch(raw)
		return parseSharedWindows(raw, strings.ToUpper(m[1]), raw[len(m[0]):])
	case strings.HasPrefix(raw, "/"):
		return parseSharedPosix(raw)
	}
	return nil, fmt.Errorf("the shared folder must be a full path, like C:\\Users\\you\\Documents\\Shared")
}

// sharedParts splits a path into its names, refusing "..": a folder is shared by where it is, not by where it climbs to.
func sharedParts(rest string) ([]string, error) {
	var parts []string
	for _, part := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		part = strings.TrimSpace(part)
		switch part {
		case "", ".":
			continue
		case "..":
			return nil, fmt.Errorf("the shared folder's path may not go up a folder (..)")
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func parseSharedWindows(raw, drive, rest string) (*SharedFolder, error) {
	parts, err := sharedParts(rest)
	if err != nil {
		return nil, err
	}
	if drive != "C" {
		return nil, fmt.Errorf("only a folder on the C: drive can be shared (this is %s:)", drive)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("a whole drive cannot be shared: choose a folder on it")
	}
	first := strings.ToLower(parts[0])
	switch {
	case sharedWindowsSystem[first]:
		return nil, fmt.Errorf("a system folder cannot be shared: %s", parts[0])
	case first == "users" && len(parts) == 1:
		return nil, fmt.Errorf("the Users folder cannot be shared: choose a folder inside your own")
	case first == "users" && len(parts) == 2:
		return nil, fmt.Errorf("a whole home folder cannot be shared: choose a folder inside it, like Documents\\Shared")
	case first == "users" && sharedSensitive[strings.ToLower(parts[2])]:
		return nil, fmt.Errorf("a folder that holds settings or credentials cannot be shared: %s", parts[2])
	}

	host := `C:\` + strings.Join(parts, `\`)
	sourceRoot := strings.TrimRight(os.Getenv(hostMountSourceEnv), `/`)
	if sourceRoot == "" {
		sourceRoot = "/mnt/c"
	}
	source := sourceRoot + "/" + strings.Join(parts, "/")

	roots := sharedFS.CDrives()
	if len(roots) == 0 {
		return nil, fmt.Errorf("the control plane cannot see this computer's C: drive, so it cannot share a folder from it")
	}
	var found string
	for _, root := range roots {
		candidate := filepath.Join(append([]string{root}, parts...)...)
		if info, err := sharedFS.Stat(candidate); err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("%s is a file, not a folder", host)
			}
			found = candidate
			break
		}
	}
	if found == "" {
		return nil, fmt.Errorf("the folder %s does not exist", host)
	}
	if err := sharedNotALink(found, host); err != nil {
		return nil, err
	}
	return &SharedFolder{Host: host, Source: source}, nil
}

func parseSharedPosix(raw string) (*SharedFolder, error) {
	parts, err := sharedParts(raw)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("the whole disk cannot be shared: choose a folder on it")
	}
	first := strings.ToLower(parts[0])
	clean := "/" + strings.Join(parts, "/")
	switch {
	case sharedPosixSystem[first]:
		return nil, fmt.Errorf("a system folder cannot be shared: /%s", parts[0])
	case (first == "mnt" || first == "media" || first == "volumes") && len(parts) <= 2:
		return nil, fmt.Errorf("a mount point cannot be shared: choose a folder inside it")
	case (first == "home" || first == "users") && len(parts) <= 2:
		return nil, fmt.Errorf("a whole home folder cannot be shared: choose a folder inside it")
	case (first == "home" || first == "users") && len(parts) > 2 && sharedSensitive[strings.ToLower(parts[2])]:
		return nil, fmt.Errorf("a folder that holds settings or credentials cannot be shared: %s", parts[2])
	}
	if home, err := sharedFS.Home(); err == nil && home != "" && filepath.ToSlash(filepath.Clean(home)) == clean {
		return nil, fmt.Errorf("your home folder cannot be shared: choose a folder inside it")
	}
	info, err := sharedFS.Stat(filepath.FromSlash(clean))
	if err != nil {
		return nil, fmt.Errorf("the folder %s does not exist", clean)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file, not a folder", clean)
	}
	if err := sharedNotALink(filepath.FromSlash(clean), clean); err != nil {
		return nil, err
	}
	return &SharedFolder{Host: clean, Source: clean}, nil
}

// sharedNotALink refuses a folder that is a link (a junction or symlink): what it points at was never looked at, and
// could be a system folder or a whole home folder.
func sharedNotALink(path, shown string) error {
	real, err := sharedFS.EvalSymlinks(path)
	if err != nil {
		return nil // not being able to resolve it is not a link; the mount itself fails if it cannot be used
	}
	same := real == filepath.Clean(path)
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(real, filepath.Clean(path))
	}
	if !same {
		return fmt.Errorf("%s is a link to another folder: share the real folder", shown)
	}
	return nil
}

// Mount is the bind that gives a desktop its shared folder. It is a Mounts entry, not a Binds string: the daemon refuses
// a source that does not exist instead of making it on the host, and the folder is read-write by the person's choice.
func (s *SharedFolder) Mount() mount.Mount {
	return mount.Mount{
		Type:        mount.TypeBind,
		Source:      s.Source,
		Target:      SharedMountPoint,
		ReadOnly:    false,
		BindOptions: &mount.BindOptions{Propagation: mount.PropagationRPrivate},
	}
}

// Env is what the agent is told: the folder as the person writes it, and where it is mounted.
func (s *SharedFolder) Env() []string {
	return []string{SharedEnvFolder + "=" + s.Host, SharedEnvMount + "=" + SharedMountPoint}
}
