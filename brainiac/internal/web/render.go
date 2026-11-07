package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
