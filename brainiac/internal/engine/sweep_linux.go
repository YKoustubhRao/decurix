//go:build linux

package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// SweepOrphans kills llama-servers of this user left behind by a Brainiac
// that died without stopping them (see orphanServer), so their memory comes
// back before this one plans its loads. It returns how many it killed.
func SweepOrphans(logf func(string, ...any)) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	uid := os.Getuid()
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		var st syscall.Stat_t
		if syscall.Stat(dir, &st) != nil || int(st.Uid) != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if !orphanServer(args, parentName(dir)) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			n++
			if logf != nil {
				logf("stopped a model server (pid %d) a previous Brainiac left running", pid)
			}
		}
	}
	return n
}

// parentName is the command name of a process's parent, from /proc.
func parentName(procDir string) string {
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return ""
	}
	// pid (comm) state ppid ...: comm may hold spaces, so read after ")".
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return ""
	}
	comm, err := os.ReadFile(filepath.Join("/proc", f[1], "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(comm))
}
