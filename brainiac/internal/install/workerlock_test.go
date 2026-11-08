package install

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"brainiac/internal/worker"
)

// workerDir is the repository's worker directory.
const workerDir = "../../workers/python"

// G-09: the shipped locks cover every supported Python on every supported
// system, for both worker groups, and each is fully hash-locked.
func TestTheShippedWorkerLocksCoverEverySupportedTarget(t *testing.T) {
	for _, group := range []string{"asr", "ocr"} {
		for _, py := range []string{"3.11.9", "3.12.4", "3.13.1", "3.14.0"} {
			for _, sys := range [][2]string{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "arm64"}} {
				f, err := WorkerLock(workerDir, group, py, sys[0], sys[1])
				if err != nil {
					t.Errorf("%s %s %s/%s: %v", group, py, sys[0], sys[1], err)
					continue
				}
				if err := worker.CheckLocked(f); err != nil {
					t.Errorf("%s: %v", f, err)
				}
			}
		}
	}
}

// Every package a group's requirement file names is in each of its locks,
// at the exact version the file pins (numpy included: a pin, not a range).
func TestTheLocksHoldTheRequirementsPins(t *testing.T) {
	pin := regexp.MustCompile(`^([A-Za-z0-9._-]+)==([^\s;]+)\s*(;\s*python_version\s*(<|>=)\s*"3\.(\d+)")?`)
	for _, group := range []string{"asr", "ocr"} {
		data, err := os.ReadFile(filepath.Join(workerDir, "requirements-"+group+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		locks, _ := filepath.Glob(filepath.Join(workerDir, "locks", group+"-cp3*-*.txt"))
		if len(locks) != 16 {
			t.Fatalf("%s: %d locks, want 4 Pythons x 4 systems", group, len(locks))
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			if line == "" {
				continue
			}
			m := pin.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("%s: %q is not an exact pin", group, line)
			}
			name := strings.ToLower(strings.ReplaceAll(m[1], "_", "-"))
			for _, lock := range locks {
				minor := regexp.MustCompile(`-cp3(\d+)-`).FindStringSubmatch(filepath.Base(lock))[1]
				if m[3] != "" && !versionHolds(minor, m[4], m[5]) {
					continue
				}
				if got := lockedVersion(t, lock, name); got != m[2] {
					t.Errorf("%s pins %s %s, the lock holds %q", filepath.Base(lock), name, m[2], got)
				}
			}
		}
	}
}

func versionHolds(minor, op, bound string) bool {
	a, b := atoi(minor), atoi(bound)
	if op == "<" {
		return a < b
	}
	return a >= b
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func lockedVersion(t *testing.T, lock, name string) string {
	t.Helper()
	f, err := os.Open(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if n, v, ok := strings.Cut(strings.Fields(sc.Text() + " x")[0], "=="); ok && n == name {
			return v
		}
	}
	return ""
}

// With no lock for this Python or system the install is refused, never
// done from the unlocked file.
func TestNoLockRefusesTheInstall(t *testing.T) {
	for _, c := range []struct{ py, goos, goarch string }{
		{"3.10.12", "linux", "amd64"}, // no lock for 3.10
		{"3.12.1", "freebsd", "amd64"},
		{"3.12.1", "darwin", "amd64"},
		{"", "linux", "amd64"},
		{"2.7.18", "linux", "amd64"},
	} {
		if f, err := WorkerLock(workerDir, "ocr", c.py, c.goos, c.goarch); err == nil {
			t.Errorf("%+v: installed from %s", c, f)
		}
	}
	if _, err := WorkerLock(workerDir, "nope", "3.12.1", "linux", "amd64"); err == nil {
		t.Error("a group with no requirement file was accepted")
	}
}
