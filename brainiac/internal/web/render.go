package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
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
// time budget; the address is checked before it is opened (the browser
// itself then loads what the page asks for, so Render is for public pages).
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
	cmd := exec.CommandContext(ctx, bin, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-sync", "--mute-audio", "--hide-scrollbars",
		"--user-data-dir="+profile, "--virtual-time-budget=8000", "--user-agent="+browserUA, "--dump-dom", u.String())
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return Page{}, fmt.Errorf("web: the headless browser could not read the page: %w", err)
	}
	if int64(len(out)) > c.maxBytes() {
		out = out[:c.maxBytes()]
	}
	p := pageFrom(u.String(), "text/html", out)
	p.Rendered = true
	return p, nil
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
