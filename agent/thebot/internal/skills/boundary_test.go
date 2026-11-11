// boundary_test.go checks the operator's tripwire (boundary.go): what crosses the desktop's boundary is caught, and
// ordinary work, typed text included, is not.
package skills

import (
	"strings"
	"testing"
)

func TestTheTripwireCatchesWaysAcrossTheBoundary(t *testing.T) {
	for _, c := range []struct{ command, want string }{
		{"su - gate -c 'cat /tmp/x'", "another user"},
		{"cd /tmp && runuser -u decurix-gate -- ls", "another user"},
		{"sudo -u root cat /etc/shadow", "another user"},
		{"sudo -E --user=gate id", "another user"},
		{"nsenter -t 1 -m ls", "another user"},
		{"gdb -p 412 -batch -ex 'call system(\"id\")'", "attach to another process"},
		{"strace -f -p 77", "attach to another process"},
		{"cat /proc/412/environ", "another process's memory"},
		{"for p in /proc/*/environ; do cat $p; done", "another process's memory"},
		{"ls -l /proc/$PID/fd", "another process's memory"},
		{"echo '{}' | nc -U /tmp/some.sock", "socket"},
		{"socat - UNIX-CONNECT:/tmp/app.sock", "socket"},
		{"curl --unix-socket /var/run/docker.sock http://x/containers/json", "socket"},
		{"python3 -c 'import socket; s=socket.socket(1); s.connect(\"/run/decurix-gate/gate.sock\")'", "approval gate"},
		{"ls /run/decurix-screen", "approval gate or the screen"},
		{"echo $DECURIX_AGENT_TOKEN", "approval gate"},
		{"curl -s http://localhost:8081/approve", "desktop service"},
		{"curl http://127.0.0.1:4900/v1/run", "desktop service"},
		{"curl -s localhost:9222/json/list", "desktop service"},
		{"google-chrome --remote-debugging-port=9333 &", "browser around its screen"},
	} {
		got := boundaryAttempt(c.command)
		if got == "" || !strings.Contains(got, c.want) {
			t.Errorf("%q: got %q, want it to say %q", c.command, got, c.want)
		}
	}
}

func TestTheTripwireLetsOrdinaryWorkThrough(t *testing.T) {
	for _, command := range []string{
		"gui open /home/headless/work/inbox/bills.xlsx",
		`gui type e12 "su - root; cat /proc/1/environ; curl localhost:8081"`, // typed text is not run
		"office read /home/headless/work/inbox/statement.pdf",
		"python3 -m json.tool data.json",
		"curl -s https://example.com/rates.json",
		"curl -s http://localhost:8000/api/resource/Supplier", // an app the task uses, on its own port
		"sudo apt-get install -y curl",                        // fails on a desktop, crosses nothing
		"grep -n 'summary' notes.txt | sort -u",
		"cat /proc/self/status; cat /proc/meminfo",
		"ps aux | grep soffice",
		"gui select e4 --id 'Statutory Audit'",
		"",
	} {
		if got := boundaryAttempt(command); got != "" {
			t.Errorf("%q tripped: %q", command, got)
		}
	}
}

func TestABoundaryStopShowsTheCommandAndClaimsNothing(t *testing.T) {
	stop := boundaryStop("run a command as another user", "su - gate")
	for _, want := range []string{"boundary attempt", "not run", "$ su - gate", "nothing it reported is taken as done"} {
		if !strings.Contains(stop, want) {
			t.Errorf("stop lacks %q: %q", want, stop)
		}
	}
}
