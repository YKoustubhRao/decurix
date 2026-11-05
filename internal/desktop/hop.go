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
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
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
	// hopInstanceLabel names the control plane that started a hop (sqlitedb.InstanceID): a sweep removes only its own
	// (review RW1-C-02). A hop holds a host-app helper's stdin, so removing another instance's hop ends its helper.
	hopInstanceLabel = "decurix.hop.instance"
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
		"--label", hopInstanceLabel+"="+hopInstance(), "--privileged", "--pid=host", "alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i", "bash", "-c", script)
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
	startedHops.add(name)
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

// hopInstance is the tag a hop is labelled with: this control plane's instance ID (a test seam).
var hopInstance = sqlitedb.InstanceID

// startedHops are the hops this run started: a sweep that runs after the listeners are up (Docker came late) must not
// remove one a page already warmed.
var startedHops = &nameSet{names: map[string]bool{}}

type nameSet struct {
	mu    sync.Mutex
	names map[string]bool
}

func (s *nameSet) add(name string) { s.mu.Lock(); s.names[name] = true; s.mu.Unlock() }

func (s *nameSet) has(name string) bool { s.mu.Lock(); defer s.mu.Unlock(); return s.names[name] }

// SweepHops removes the hop containers an earlier run of this control plane left (running or not), and nothing else:
// only containers labelled HopLabel=1, named with the hop prefix, carrying this instance's tag, and not started by
// this run. Another instance's hops are left to it; a hop with no instance tag (an older build's) is left and logged,
// since it may be another instance's live one. Returns how many it removed.
func SweepHops(ctx context.Context, dockerBin string) int {
	if dockerBin == "" {
		return 0
	}
	out, err := hopExec(ctx, dockerBin, "ps", "-a", "--filter", "label="+HopLabel+"=1",
		"--format", "{{.Names}}\t{{.Label \""+hopInstanceLabel+"\"}}")
	if err != nil {
		log.Printf("hop: could not list hop containers: %v", err)
		return 0
	}
	mine := hopInstance()
	var names []string
	untagged := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		name := strings.TrimSpace(fields[0])
		if !strings.HasPrefix(name, hopNamePrefix) || startedHops.has(name) {
			continue
		}
		tag := ""
		if len(fields) == 2 {
			tag = strings.TrimSpace(fields[1])
		}
		switch tag {
		case mine:
			names = append(names, name)
		case "":
			untagged++
		}
	}
	if untagged > 0 {
		log.Printf("hop: left %d hop container(s) with no instance tag alone (an older build's, maybe another control plane's live ones)", untagged)
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

// SweepUnnamedHops reports the hop containers older control planes left before hops were named: an alpine container
// running `nsenter -t 1 ...` in the host's PID namespace, with no hop label. It no longer removes them: nothing says
// which control plane's they are, and one may be another instance's live helper (review RW1-C-02). Returns how many
// it found.
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
	if len(ids) > 0 {
		log.Printf("hop: left %d unnamed hop container(s) of an earlier version alone; remove them by hand once no other control plane uses them: docker rm -f %s",
			len(ids), strings.Join(ids, " "))
	}
	return len(ids)
}

// SweepHopsAtStart removes the hop containers an earlier run of this control plane left (SweepHops), and reports the
// unnamed ones older versions left (SweepUnnamedHops). Returns how many it removed.
func SweepHopsAtStart() int {
	dockerBin := findDockerBinary()
	if dockerBin == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	SweepUnnamedHops(ctx, dockerBin)
	return SweepHops(ctx, dockerBin)
}
