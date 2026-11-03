//go:build linux

package isolate

import (
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeInfo struct{ dir bool }

func (f fakeInfo) Name() string { return "" }
func (f fakeInfo) Size() int64  { return 0 }
func (f fakeInfo) Mode() fs.FileMode {
	return map[bool]fs.FileMode{true: fs.ModeDir, false: fs.ModeCharDevice}[f.dir]
}
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

// The view's /dev binds exactly the plain devices and the GPU nodes this
// machine has (a WSL2 /dev/dxg, NVIDIA's, AMD's, DRI), plus its own
// /dev/shm; nothing that is absent, and nothing else.
func TestTheViewBindsTheGPUNodesThatExistAndNothingElse(t *testing.T) {
	present := map[string]bool{"/dev/null": false, "/dev/urandom": false, "/dev/dxg": false, "/dev/dri": true, "/dev/nvidia0": false}
	stat := func(p string) (os.FileInfo, error) {
		if dir, ok := present[p]; ok {
			return fakeInfo{dir}, nil
		}
		return nil, os.ErrNotExist
	}
	glob := func(string) ([]string, error) { return []string{"/dev/nvidia0"}, nil }
	var got []string
	for _, s := range devStepsWith(stat, glob) {
		got = append(got, s.path)
		if s.path == "/dev/dxg" && (!s.write || !s.isFile) {
			t.Errorf("/dev/dxg is bound as %+v", s)
		}
	}
	sort.Strings(got)
	want := "/dev/dri /dev/dxg /dev/null /dev/nvidia0 /dev/shm /dev/urandom"
	if strings.Join(got, " ") != want {
		t.Fatalf("bound %v, want %s", got, want)
	}
	// NVIDIA's driver directory is read-only, where it exists.
	pol := ForProgram("/bin/true", nil, nil)
	if !contains(pol.Read, "/proc/driver/nvidia") || contains(pol.Write, "/proc/driver/nvidia") {
		t.Fatalf("read %v write %v", pol.Read, pol.Write)
	}
	// On this machine: whatever of these exists is in the plan, read-only.
	for _, s := range planView(Policy{Read: pol.Read, Write: pol.Write, Mount: true}) {
		if strings.HasPrefix(s.path, "/proc/driver/nvidia") && s.write {
			t.Errorf("%s is writable in the view", s.path)
		}
	}
	if _, err := os.Stat("/dev/dxg"); err == nil {
		found := false
		for _, s := range devSteps() {
			found = found || s.path == "/dev/dxg"
		}
		if !found {
			t.Error("this WSL2 host has /dev/dxg, and the view leaves it out")
		}
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
