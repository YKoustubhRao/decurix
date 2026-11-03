package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
)

// fakeInfo is a directory (or file) entry for the fake disk.
type fakeInfo struct {
	name string
	dir  bool
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

// fakeDisk replaces the disk validation looks at: dirs and files by slash path, links by slash path to where they lead.
func fakeDisk(t *testing.T, dirs, files []string, links map[string]string) {
	t.Helper()
	key := func(p string) string { return strings.ToLower(filepath.ToSlash(p)) }
	have := map[string]fakeInfo{}
	for _, d := range dirs {
		have[key(d)] = fakeInfo{d, true}
	}
	for _, f := range files {
		have[key(f)] = fakeInfo{f, false}
	}
	saved := sharedFS
	sharedFS.Stat = func(p string) (os.FileInfo, error) {
		if info, ok := have[key(p)]; ok {
			return info, nil
		}
		return nil, errors.New("not found")
	}
	sharedFS.EvalSymlinks = func(p string) (string, error) {
		if to, ok := links[key(p)]; ok {
			return filepath.FromSlash(to), nil
		}
		return filepath.Clean(p), nil
	}
	sharedFS.CDrives = func() []string { return []string{"/mnt/c"} }
	sharedFS.Home = func() (string, error) { return "/home/boss", nil }
	t.Cleanup(func() { sharedFS = saved })
}

func TestASharedFolderIsValidatedAndMappedToWhereTheDaemonSeesIt(t *testing.T) {
	fakeDisk(t, []string{
		"/mnt/c/Users/Administrator/Documents/Decurix demo", "/mnt/c/Work", "/mnt/c/Users/Public/Docs",
		"/srv-data/shared", "/home/boss/Documents/Shared", "/Users/ann/Projects/Shared",
	}, nil, nil)

	for _, raw := range []string{
		`C:\Users\Administrator\Documents\Decurix demo`,
		`c:/users/administrator/documents/decurix demo/`,
		`"C:\Users\Administrator\Documents\Decurix demo"`,
		`  C:\Users\Administrator\Documents\Decurix demo\\  `,
		`/mnt/c/Users/Administrator/Documents/Decurix demo`,
	} {
		sf, err := ParseSharedFolder(raw)
		if err != nil || sf == nil {
			t.Fatalf("%q: %v", raw, err)
		}
		// Shown as the person writes a Windows path (the case they typed for the names), mounted from the daemon's /mnt/c.
		if !strings.EqualFold(sf.Host, `C:\Users\Administrator\Documents\Decurix demo`) {
			t.Errorf("%q: Host = %q", raw, sf.Host)
		}
		if !strings.EqualFold(sf.Source, "/mnt/c/Users/Administrator/Documents/Decurix demo") {
			t.Errorf("%q: Source = %q", raw, sf.Source)
		}
	}

	t.Setenv(hostMountSourceEnv, "/host/drive-c/")
	sf, err := ParseSharedFolder(`C:\Work`)
	if err != nil || sf.Source != "/host/drive-c/Work" || sf.Host != `C:\Work` {
		t.Fatalf("a daemon that sees the drive elsewhere: %+v %v", sf, err)
	}
	if sf, err := ParseSharedFolder("   "); sf != nil || err != nil {
		t.Fatalf("an empty field is no shared folder: %+v %v", sf, err)
	}
	if sf, err := ParseSharedFolder("/home/boss/Documents/Shared"); err != nil || sf.Source != "/home/boss/Documents/Shared" {
		t.Fatalf("a Linux folder: %+v %v", sf, err)
	}
	if sf, err := ParseSharedFolder("/Users/ann/Projects/Shared"); err != nil || sf.Host != "/Users/ann/Projects/Shared" {
		t.Fatalf("a macOS folder: %+v %v", sf, err)
	}
}

func TestAFolderThatIsNotSafeToShareIsRefusedInPlainWords(t *testing.T) {
	fakeDisk(t, []string{
		"/mnt/c/Windows", "/mnt/c/Windows/System32", "/mnt/c/Program Files", "/mnt/c/ProgramData", "/mnt/c/Users",
		"/mnt/c/Users/Administrator", "/mnt/c/Users/Administrator/AppData/Local", "/mnt/c/Users/Administrator/.ssh",
		"/mnt/c/Work", "/home/boss", "/home/boss/.aws", "/home/ann", "/etc", "/usr/share", "/Users/ann", "/mnt/d/Data",
		"/mnt/wsl", "/mnt/c/Link",
	}, []string{"/mnt/c/Work/report.xlsx"}, map[string]string{"/mnt/c/link": "/mnt/c/Windows"})

	cases := map[string]string{
		`Documents\Shared`:                     "full path",
		`C:`:                                   "whole drive",
		`C:\`:                                  "whole drive",
		`D:\Data`:                              "only a folder on the C: drive",
		`/mnt/d/Data`:                          "only a folder on the C: drive",
		`\\server\share\docs`:                  "network folder",
		`//server/share/docs`:                  "network folder",
		`C:\Windows`:                           "system folder",
		`C:\Windows\System32`:                  "system folder",
		`c:\program files`:                     "system folder",
		`C:\ProgramData`:                       "system folder",
		`C:\Users`:                             "Users folder",
		`C:\Users\Administrator`:               "whole home folder",
		`C:\Users\Administrator\AppData\Local`: "settings or credentials",
		`C:\Users\Administrator\.ssh`:          "settings or credentials",
		`C:\Work\..\Windows`:                   "may not go up",
		`C:\Work\report.xlsx`:                  "is a file",
		`C:\Nowhere\Folder`:                    "does not exist",
		`C:\Link`:                              "link to another folder",
		"C:\\Work\x00\\evil":                   "not a usable folder path",
		"C:\\Work\nevil":                       "not a usable folder path",
		`/`:                                    "whole disk",
		`/etc`:                                 "system folder",
		`/usr/share`:                           "system folder",
		`/home/boss`:                           "whole home folder",
		`/home/ann`:                            "whole home folder",
		`/Users/ann`:                           "whole home folder",
		`/home/boss/.aws`:                      "settings or credentials",
		`/mnt/wsl`:                             "mount point",
		`/home/boss/missing`:                   "does not exist",
		`/mnt/c`:                               "whole drive",
		strings.Repeat(`C:\Work\`, 80):         "not a usable folder path",
		`C:\Users\Administrator\Documents\..\..\..\Windows`: "may not go up",
	}
	for raw, want := range cases {
		sf, err := ParseSharedFolder(raw)
		if err == nil || sf != nil {
			t.Errorf("%q was accepted: %+v", raw, sf)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %q, want it to say %q", raw, err.Error(), want)
		}
	}
}

func TestTheSharedFolderIsABindMountOutsideTheHomeFolderAndIsReadWrite(t *testing.T) {
	sf := &SharedFolder{Host: `C:\Users\Administrator\Documents\Decurix demo`, Source: "/mnt/c/Users/Administrator/Documents/Decurix demo"}

	config, host, _ := buildContainerConfigs("img", "desktop_x", "basic", "thebot", 1, 2048, buildDesktopEnv("x", "thebot", "", "", Brain{}, sf), "vol_x", nil, sf)

	if len(host.Mounts) != 1 {
		t.Fatalf("mounts = %+v", host.Mounts)
	}
	m := host.Mounts[0]
	if m.Type != mount.TypeBind || m.Source != sf.Source || m.Target != SharedMountPoint || m.ReadOnly {
		t.Errorf("mount = %+v", m)
	}
	if m.BindOptions == nil || m.BindOptions.Propagation != mount.PropagationRPrivate {
		t.Errorf("a bind must not propagate: %+v", m.BindOptions)
	}
	// Not under $HOME: the base image runs chown -R $UID $HOME on every start.
	if strings.HasPrefix(SharedMountPoint, "/home/headless") {
		t.Errorf("the mount is inside the home folder the startup script chowns recursively: %s", SharedMountPoint)
	}
	// Not a Binds string either: a bind would make a missing folder on the host.
	for _, b := range host.Binds {
		if strings.Contains(b, "/mnt/c") || strings.Contains(b, SharedMountPoint) {
			t.Errorf("the shared folder is in Binds: %s", b)
		}
	}
	if len(host.Binds) != 4 { // home, tools, its own cache, the read-only model cache
		t.Errorf("the desktop's own volumes changed: %v", host.Binds)
	}
	if config.Labels[sharedLabel] != sf.Host {
		t.Errorf("label = %q", config.Labels[sharedLabel])
	}
	if envValue(config.Env, SharedEnvFolder) != sf.Host || envValue(config.Env, SharedEnvMount) != SharedMountPoint {
		t.Errorf("env = %v", config.Env)
	}
}

func TestADesktopWithNoSharedFolderMountsNothingOfThePC(t *testing.T) {
	for _, shared := range [][]*SharedFolder{nil, {nil}} {
		config, host, _ := buildContainerConfigs("img", "desktop_x", "basic", "thebot", 1, 2048, buildDesktopEnv("x", "thebot", "", "", Brain{}, shared...), "vol_x", nil, shared...)
		if len(host.Mounts) != 0 || config.Labels[sharedLabel] != "" || envValue(config.Env, SharedEnvFolder) != "" {
			t.Errorf("a desktop without a shared folder has one: %+v %v", host.Mounts, config.Labels)
		}
	}
}
