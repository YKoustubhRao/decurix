package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"time"

	"brainiac/internal/isolate"
)

// FindBrowser returns a Chrome, Chromium or Edge executable, or "".
func FindBrowser() string {
	var tries []string
	switch runtime.GOOS {
	case "windows":
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")} {
			if base == "" {
				continue
			}
			tries = append(tries, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "Chromium", "Application", "chrome.exe"))
		}
	case "darwin":
		tries = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}
	default:
		for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "microsoft-edge"} {
			if p, err := exec.LookPath(name); err == nil {
				tries = append(tries, p)
			}
		}
	}
	for _, p := range tries {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Render reads a page with a real headless browser, so pages that build
// their text with JavaScript (dashboards, leaderboards, single-page apps) are
// read as a person sees them. The browser gets a throwaway profile and a
// time budget, and reaches the web only through a proxy in this process
// that applies Fetch's address guard to every connection it makes, loopback
// included: the page, its redirects and everything it loads. It is used
// only when a client asks for it (/v1/brainiac/fetch with "render": true).
func (c *Client) Render(ctx context.Context, raw string) (Page, error) {
	u, err := checkURL(raw)
	if err != nil {
		return Page{}, err
	}
	if !c.AllowPrivate {
		if err := c.resolvesPublic(ctx, u.Hostname()); err != nil {
			return Page{}, err
		}
	}
	return c.renderNoPrecheck(ctx, raw)
}

func (c *Client) renderNoPrecheck(ctx context.Context, raw string) (Page, error) {
	u, err := checkURL(raw)
	if err != nil {
		return Page{}, err
	}
	bin := c.Browser
	if bin == "" {
		bin = FindBrowser()
	}
	if bin == "" {
		return Page{}, errors.New("web: no Chrome, Chromium or Edge found for pages that need JavaScript")
	}
	profile, err := os.MkdirTemp("", "brainiac-browser-")
	if err != nil {
		return Page{}, err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	proxy, stop, err := c.startProxy()
	if err != nil {
		return Page{}, err
	}
	defer stop()
	cmd := browserCommand(ctx, bin, proxy, profile, u.String())
	if err := confineBrowser(cmd, browserPolicy(bin, profile)); err != nil {
		return Page{}, fmt.Errorf("web: the headless browser could not be confined: %w", err)
	}
	// The page is read through a bounded pipe: a page that builds more than
	// MaxBytes of DOM is refused, and never held in memory whole.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Page{}, err
	}
	if err := isolate.StartLocked(cmd); err != nil {
		return Page{}, fmt.Errorf("web: the headless browser did not start: %w", err)
	}
	out, rerr := io.ReadAll(io.LimitReader(stdout, c.maxBytes()+1))
	tooBig := int64(len(out)) > c.maxBytes()
	if tooBig || rerr != nil {
		cancel() // ends the browser; Wait below reaps it
	}
	werr := cmd.Wait()
	if tooBig {
		return Page{}, fmt.Errorf("web: the rendered page is larger than %d bytes", c.maxBytes())
	}
	if len(out) == 0 {
		return Page{}, fmt.Errorf("web: the headless browser could not read the page: %v", errors.Join(rerr, werr))
	}
	p := pageFrom(u.String(), "text/html", out)
	p.Rendered = true
	return p, nil
}

// browserCommand is the headless browser's command line for one page. The
// browser runs a stranger's JavaScript, so it gets the minimal environment
// Brainiac's model processes get (isolate.Env: what the system needs to start
// a program, none of Brainiac's keys, such as a remote brain's API key), and
// on Linux it is killed when Brainiac exits, however it exits.
func browserCommand(ctx context.Context, bin, proxy, profile, target string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-sync", "--mute-audio", "--hide-scrollbars",
		// Every connection through the guard proxy; "<-loopback>" sends
		// loopback there too instead of letting the browser reach it.
		"--proxy-server=http://"+proxy, "--proxy-bypass-list=<-loopback>",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp", "--disable-background-networking",
		"--user-data-dir="+profile, "--virtual-time-budget=8000", "--user-agent="+browserUA, "--dump-dom", target)
	cmd.Env = isolate.Env()
	isolate.DieWithParent(cmd)
	return cmd
}

// resolvesPublic refuses a host whose addresses include a blocked one.
func (c *Client) resolvesPublic(ctx context.Context, host string) error {
	ips, err := (&netResolver{}).lookup(ctx, host)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}
	for _, ip := range ips {
		if blocked(ip) {
			return ErrBlockedAddress
		}
	}
	return nil
}

// browserSystemRead is what a headless Chrome needs from the system, read
// only: its programs and libraries, fonts, certificates, the loader's cache,
// the few files libc reads, and the machine's own facts. Not /etc as a
// whole, not /home, not /tmp beyond the profile, and of /proc only what is
// the browser's own or the machine's: /proc/<pid>/environ of another
// process would hand over its secrets, Brainiac's first of all. /proc/self
// is bound when the rule is made, to the process that becomes the browser;
// Chrome's own child processes do not get theirs.
var browserSystemRead = []string{"/usr", "/lib", "/lib32", "/lib64", "/bin", "/sbin", "/opt",
	"/etc/fonts", "/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/localtime", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf", "/etc/hosts", "/etc/resolv.conf",
	"/etc/machine-id", "/var/lib/dbus/machine-id", "/var/cache/fontconfig", "/sys",
	"/proc/self", "/proc/cpuinfo", "/proc/meminfo", "/proc/stat", "/proc/filesystems", "/proc/version", "/proc/sys",
	"/nix/store", "/run/opengl-driver"}

// browserDevices are the device nodes it reads and writes, and the shared
// memory its processes talk through.
var browserDevices = []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/shm"}

// browserPolicy is the Landlock file policy the headless browser runs under
// on Linux (N-W4-8): its throwaway profile read-write, the devices it needs,
// the system read set, and the folder its own program lives in (a browser
// installed outside /usr and /opt). Everything else is refused, the
// operator's files, Brainiac's store and other processes' /proc entries
// among them. The network is kept: the browser reaches the web only through
// the guard proxy, which applies the address rules.
func browserPolicy(bin, profile string) isolate.Policy {
	bin, profile = filepath.ToSlash(bin), filepath.ToSlash(profile)
	read := append([]string(nil), browserSystemRead...)
	dirs := []string{path.Dir(bin)}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		dirs = append(dirs, path.Dir(filepath.ToSlash(real)))
	}
	for _, d := range dirs {
		if d != "/" && d != "." && !covered(read, d) {
			read = append(read, d)
		}
	}
	return isolate.Policy{Read: read, Write: append([]string{profile}, browserDevices...)}
}

// covered reports whether p is one of dirs or beneath one.
func covered(dirs []string, p string) bool {
	for _, d := range dirs {
		if p == d || len(p) > len(d) && p[:len(d)] == d && p[len(d)] == '/' {
			return true
		}
	}
	return false
}
