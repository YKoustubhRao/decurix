package hostshell

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// alive reports whether pid is a running process (a zombie waiting to be
// reaped is not running).
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z" && fields[0] != "X"
}

// When the context ends, every process the command started is killed, not
// only the shell: a build's compiler left running held the job for hours.
func TestCommandContextKillsTheWholeGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := CommandContext(ctx, "sleep 60 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := out.Read(buf)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		t.Fatalf("no child pid: %q", buf[:n])
	}
	cancel()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			p, _ := os.FindProcess(pid)
			_ = p.Kill()
			t.Fatalf("child %d still running after the context ended", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
