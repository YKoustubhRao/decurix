package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// A hop is how a control plane running in a container runs something on the host it sits on: a short-lived, privileged
// alpine container in the host's PID namespace that nsenters PID 1's namespaces and runs a bash script there (on WSL,
// that is how a Windows program is reached). Found live (2026-10-20): a context's cancel and a session's Process.Kill
// both kill the docker CLI, not the container - with -i the container keeps its stdin open, so the Windows bridge reading
// that stdin never saw it close, and ten hop containers per hour piled up on an 8 GB host. Every hop is now named and
// labelled, removed (docker rm -f) when it is cancelled or ends, and swept at start.

const (
	// HopLabel marks a hop container; nothing else carries it.
	HopLabel = "decurix.hop"
	// hopPurposeLabel says what a hop was for, for a person reading `docker ps`.
	hopPurposeLabel = "decurix.hop.purpose"
	// hopNamePrefix starts every hop container's name.
	hopNamePrefix = "decurix-hop-"
)

// hopExec runs a docker command for the hop's own housekeeping (removing one, sweeping). Tests replace it.
var hopExec = func(ctx context.Context, dockerBin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, dockerBin, args...).CombinedOutput()
}

var hopPurposeUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// HopName is a fresh name for a hop container doing `purpose`: decurix-hop-<purpose>-<random>.
func HopName(purpose string) string {
	p := strings.Trim(hopPurposeUnsafe.ReplaceAllString(strings.ToLower(purpose), "-"), "-")
	if p == "" {
		p = "run"
	}
	if len(p) > 24 {
		p = p[:24]
	}
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%s%s-%d", hopNamePrefix, p, time.Now().UnixNano())
	}
	return hopNamePrefix + p + "-" + hex.EncodeToString(raw)
}

// hopArgs are the `docker run` arguments of a hop: named, labelled, removed when it ends, in the host's namespaces.
func hopArgs(name, purpose string, interactive bool, script string) []string {
	args := []string{"run"}
	if interactive {
		args = append(args, "-i")
	}
	args = append(args, "--rm", "--name", name, "--label", HopLabel+"=1", "--label", hopPurposeLabel+"="+purpose,
		"--privileged", "--pid=host", "alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", script)
	return args
}

// HopCommand is the not-yet-started command that runs `script` with bash in the host's namespaces through a hop
// container. `interactive` keeps the container's stdin open for the caller (a bridge driven line by line). When ctx is
// done, or the command's Cancel is called (winhost calls it to end a session), the container is removed - which ends
// bash and everything it started there - and then the docker client is killed. A nil ctx is a hop that ends on its own
// (Cancel still removes it when called).
func HopCommand(ctx context.Context, dockerBin, purpose string, interactive bool, script string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	name := HopName(purpose)
	cmd := exec.CommandContext(ctx, dockerBin, hopArgs(name, purpose, interactive, script)...)
	cmd.Cancel = func() error {
		RemoveHop(dockerBin, name)
		if cmd.Process != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	// The client is given a moment after the container is gone before its pipes are closed under it.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// RemoveHop removes one hop container by name, whatever state it is in; it is already gone when it ended by itself.
func RemoveHop(dockerBin, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if out, err := hopExec(ctx, dockerBin, "rm", "-f", name); err != nil && !strings.Contains(strings.ToLower(string(out)), "no such container") {
		log.Printf("hop: could not remove %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// SweepHops removes the hop containers a previous run of the control plane left (running or not), and nothing else:
// only containers labelled HopLabel=1 whose names start with the hop prefix. Returns how many it removed.
func SweepHops(ctx context.Context, dockerBin string) int {
	if dockerBin == "" {
		return 0
	}
	out, err := hopExec(ctx, dockerBin, "ps", "-a", "--filter", "label="+HopLabel+"=1", "--format", "{{.Names}}")
	if err != nil {
		log.Printf("hop: could not list hop containers: %v", err)
		return 0
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, hopNamePrefix) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return 0
	}
	if out, err := hopExec(ctx, dockerBin, append([]string{"rm", "-f"}, names...)...); err != nil {
		log.Printf("hop: could not remove %d leftover hop containers: %v: %s", len(names), err, strings.TrimSpace(string(out)))
		return 0
	}
	log.Printf("hop: removed %d hop container(s) left from an earlier run", len(names))
	return len(names)
}

// SweepUnnamedHops removes the hop containers older control planes left before hops were named: an alpine container
// running `nsenter -t 1 ...` in the host's PID namespace, with no hop label. Nothing else matches all of that. Returns
// how many it removed.
func SweepUnnamedHops(ctx context.Context, dockerBin string) int {
	if dockerBin == "" {
		return 0
	}
	out, err := hopExec(ctx, dockerBin, "ps", "-a", "--filter", "ancestor=alpine", "--no-trunc",
		"--format", "{{.ID}}\t{{.Command}}\t{{.Label \""+HopLabel+"\"}}")
	if err != nil {
		return 0
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(fields) < 2 || (len(fields) == 3 && fields[2] != "") {
			continue
		}
		command := strings.Trim(fields[1], `"`)
		if strings.HasPrefix(command, "nsenter -t 1 -m -u -n -i bash -c ") {
			ids = append(ids, fields[0])
		}
	}
	if len(ids) == 0 {
		return 0
	}
	if _, err := hopExec(ctx, dockerBin, append([]string{"rm", "-f"}, ids...)...); err != nil {
		return 0
	}
	log.Printf("hop: removed %d unnamed hop container(s) left by an earlier version", len(ids))
	return len(ids)
}

// SweepHopsAtStart removes every hop container an earlier run of the control plane left, named or (from before hops
// were named) unnamed. Called once at start, before anything can start a hop of its own: a hop belongs to the process
// that started it, and that process is gone. Returns how many were removed.
func SweepHopsAtStart() int {
	dockerBin := findDockerBinary()
	if dockerBin == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return SweepHops(ctx, dockerBin) + SweepUnnamedHops(ctx, dockerBin)
}
