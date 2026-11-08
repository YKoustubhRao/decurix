package desktop

// A normal desktop's browser reaching a local stand-in app under the stand-in's own host names, as a practice
// desktop's does (practiceBrowserFlags): so a person can record a demonstration on the very addresses a practice run
// later uses. Only reserved test names may be mapped (RFC 2606 / RFC 6761), so a real site can never be redirected in
// a person's desktop; and a stand-in started here runs as the worker, in the worker's home.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

// browserInitPath is the image's browser launch flags, root-owned; the start script copies it to the worker's home
// (homeBrowserInit) on every desktop start.
const browserInitPath = "/opt/decurix-chromium.init"

// homeBrowserInit is the copy of browserInitPath the browser launcher sources, in the worker's home.
const homeBrowserInit = ".chromium-browser.init"

// The lines a mapping block is written between, so a new mapping replaces the last and Unmap removes it whole.
const (
	browserHostsStart = "# >>> decurix: host names mapped to a local stand-in app (MapBrowserHosts)"
	browserHostsEnd   = "# <<< decurix: end of mapped host names"
)

// reservedTestTLDs are the top-level names reserved for testing and documentation: no real site is ever under one.
var reservedTestTLDs = []string{"test", "example", "invalid", "localhost"}

// ErrNotTestHost is a host name a normal desktop's browser may not map: only names under a reserved test name may be.
var ErrNotTestHost = errors.New("only host names under .test, .example, .invalid or .localhost may be mapped")

// ErrDesktopNotRunning is a desktop that must be running (not stopped or paused) for what was asked.
var ErrDesktopNotRunning = errors.New("the desktop is not running")

// ReservedTestHost reports whether h is a host name under a reserved test top-level name (app.example.test,
// portal.example): a name no real site has, which a normal desktop's browser may map to a stand-in.
func ReservedTestHost(h string) bool {
	if !practiceHostPattern.MatchString(h) {
		return false
	}
	lower := strings.ToLower(h)
	for _, tld := range reservedTestTLDs {
		if strings.HasSuffix(lower, "."+tld) && len(lower) > len(tld)+1 {
			return true
		}
	}
	return false
}

// browserTargetPattern is where mapped names may lead: the desktop's own loopback, on a port.
var browserTargetPattern = regexp.MustCompile(`^127\.0\.0\.1:([0-9]{2,5})$`)

// browserHostsBlock is the marked launch-flags block that maps every host to target (127.0.0.1:<port>).
func browserHostsBlock(hosts []string, target string) (string, error) {
	m := browserTargetPattern.FindStringSubmatch(target)
	if m == nil {
		return "", fmt.Errorf("not a loopback port: %q", target)
	}
	if port, _ := strconv.Atoi(m[1]); port < 1024 || port > 65535 {
		return "", fmt.Errorf("not a usable port: %q", target)
	}
	if len(hosts) == 0 || len(hosts) > 32 {
		return "", errors.New("between 1 and 32 host names may be mapped")
	}
	rules := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if !ReservedTestHost(h) {
			return "", fmt.Errorf("%q: %w", h, ErrNotTestHost)
		}
		rules = append(rules, "MAP "+strings.ToLower(h)+" "+target)
	}
	return browserHostsStart + "\nset -- '--host-resolver-rules=" + strings.Join(rules, ",") + "' \"$@\"\n" + browserHostsEnd + "\n", nil
}

// withBrowserHosts is a launch-flags file with any earlier mapping block taken out and block (when not "") added at
// the end: a second mapping replaces the first, never stacks on it.
func withBrowserHosts(init []byte, block string) []byte {
	text := string(init)
	for {
		start := strings.Index(text, browserHostsStart)
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], browserHostsEnd)
		if end < 0 {
			text = text[:start]
			break
		}
		end += start + len(browserHostsEnd)
		if end < len(text) && text[end] == '\n' {
			end++
		}
		text = text[:start] + text[end:]
	}
	if block != "" {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += block
	}
	return []byte(text)
}

// runningDesktop is the container id of normal desktop id when it is running and not paused.
func (m *Manager) runningDesktop(ctx context.Context, id string) (string, error) {
	inspect, err := m.client.ContainerInspect(ctx, "desktop_"+sanitizeName(id))
	if err != nil {
		return "", notFound(id, err)
	}
	if inspect.Config != nil && inspect.Config.Labels[PracticeLabel] == "true" {
		// The runner's own desktops are mapped at their creation (practiceBrowserFlags), never here.
		return "", fmt.Errorf("find desktop %s: %w", sanitizeName(id), ErrNotFound)
	}
	if inspect.State == nil || !inspect.State.Running || inspect.State.Paused {
		return "", ErrDesktopNotRunning
	}
	return inspect.ID, nil
}

// MapBrowserHosts makes desktop id's browser reach target (127.0.0.1:<port>) for every one of hosts, each a reserved
// test name (ReservedTestHost), from its next launch: the marked block is written into the image's launch flags
// (root-owned, so a restart keeps it) and into the worker's copy the launcher reads now. A browser already open keeps
// its old flags until it is closed. An earlier mapping is replaced.
func (m *Manager) MapBrowserHosts(ctx context.Context, id string, hosts []string, target string) error {
	block, err := browserHostsBlock(hosts, target)
	if err != nil {
		return err
	}
	return m.setBrowserHosts(ctx, id, block)
}

// UnmapBrowserHosts takes desktop id's mapping block (MapBrowserHosts) out of both launch-flags files.
func (m *Manager) UnmapBrowserHosts(ctx context.Context, id string) error {
	return m.setBrowserHosts(ctx, id, "")
}

// setBrowserHosts rewrites both launch-flags files of a running desktop with block in place of any earlier one.
func (m *Manager) setBrowserHosts(ctx context.Context, id, block string) error {
	cid, err := m.runningDesktop(ctx, id)
	if err != nil {
		return err
	}
	// A fixed root-owned path (copyOut's rule), read and written root-side, as appendBrowserFlags does.
	b, err := m.copyOut(ctx, cid, browserInitPath, 1<<20)
	if err != nil {
		return fmt.Errorf("read the browser flags: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	h, err := tr.Next()
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		return err
	}
	body = withBrowserHosts(body, block)
	if err := m.client.CopyToContainer(ctx, cid, path.Dir(browserInitPath),
		tarOf(map[string][]byte{path.Base(browserInitPath): body}, h.Mode, 0), container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("write the browser flags: %w", err)
	}
	// The worker's copy, as the worker (homefs.go): what the launcher sources from now on, as a start would copy it.
	return m.WriteTar(ctx, id, homeDir, tarOf(map[string][]byte{homeBrowserInit: body}, 0o644, desktopUID))
}

// workerProcessScript, run as the worker, stops the process an earlier start left in the folder (its pid file), then
// starts the command there detached, its output in process.log and its pid in process.pid.
const workerProcessScript = `cd "$1" || exit 1; shift
if [ -f process.pid ]; then kill "$(cat process.pid)" 2>/dev/null; rm -f process.pid; sleep 0.3; fi
setsid "$@" > process.log 2>&1 < /dev/null &
echo $! > process.pid`

// workerStopScript, run as the worker, stops the process whose pid file is in the folder.
const workerStopScript = `cd "$1" 2>/dev/null || exit 0
if [ -f process.pid ]; then kill "$(cat process.pid)" 2>/dev/null; rm -f process.pid; fi; exit 0`

// StartWorkerProcess starts argv as the worker in folder dir of running desktop id (under its home), detached, with
// an empty environment and the system PATH; a process an earlier call started in that folder is stopped first. If
// waitPort > 0 it then waits up to waitSeconds for something to answer HTTP on 127.0.0.1:waitPort in the desktop.
func (m *Manager) StartWorkerProcess(ctx context.Context, id, dir string, argv []string, waitPort, waitSeconds int) error {
	clean, err := homePath(dir, false)
	if err != nil {
		return err
	}
	if len(argv) == 0 {
		return errors.New("nothing to start")
	}
	cid, err := m.runningDesktop(ctx, id)
	if err != nil {
		return err
	}
	cmd := append([]string{"/bin/sh", "-c", workerProcessScript, "sh", clean}, argv...)
	if code, err := m.execWorker(ctx, cid, nil, cmd...); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("the process could not be started (exit %d)", code)
	}
	if waitPort <= 0 {
		return nil
	}
	code, err := m.execWorker(ctx, cid, nil, "/usr/bin/python3", "-I", "-c", practiceMockWaitScript, strconv.Itoa(waitPort), strconv.Itoa(waitSeconds))
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("nothing answered on port %d within %d s", waitPort, waitSeconds)
	}
	return nil
}

// StopWorkerProcess stops the process StartWorkerProcess started in folder dir of running desktop id; nothing when
// none is there.
func (m *Manager) StopWorkerProcess(ctx context.Context, id, dir string) error {
	clean, err := homePath(dir, false)
	if err != nil {
		return err
	}
	cid, err := m.runningDesktop(ctx, id)
	if err != nil {
		return err
	}
	_, err = m.execWorker(ctx, cid, nil, "/bin/sh", "-c", workerStopScript, "sh", clean)
	return err
}
