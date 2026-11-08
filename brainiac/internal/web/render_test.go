package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMain lets the test binary stand in for a headless browser: started
// with "--headless=new", it prints as many bytes of page as the page URL's
// "n" asks for. The URL carries it because the browser gets a minimal
// environment (browserCommand), which a test variable would not pass.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == landlockProbeArg {
		landlockProbe(os.Args[2:])
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--headless=new" {
		var n int
		if u, err := url.Parse(os.Args[len(os.Args)-1]); err == nil {
			n, _ = strconv.Atoi(u.Query().Get("n"))
		}
		os.Stdout.WriteString("<html><body><p>" + strings.Repeat("x", n) + "</p></body></html>")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A rendered page larger than MaxBytes is refused, read through a bounded
// pipe; one within it is read.
func TestARenderedPageIsBounded(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := New()
	c.Browser, c.AllowPrivate, c.MaxBytes = self, true, 4096
	if _, err := c.Render(context.Background(), "http://127.0.0.1:1/?n=1000000"); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("a 1 MB page with a 4 KB limit: %v", err)
	}
	p, err := c.Render(context.Background(), "http://127.0.0.1:1/?n=100")
	if err != nil || !p.Rendered || !strings.Contains(p.Text, "xxx") {
		t.Fatalf("a small page: %+v %v", p, err)
	}
}

// The headless browser runs a stranger's JavaScript, so it gets the minimal
// environment of a model process, not Brainiac's: a key in Brainiac's
// environment does not reach it. Built, never started.
func TestTheBrowserGetsAMinimalEnvironment(t *testing.T) {
	t.Setenv("BRAINIAC_TEST_REMOTE_KEY", "not-for-the-browser")
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+"brainiac-test")
	cmd := browserCommand(context.Background(), "browser", "127.0.0.1:1", t.TempDir(), "http://example.com/")
	if cmd.Env == nil {
		t.Fatal("the browser inherits Brainiac's whole environment (cmd.Env is nil)")
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, "BRAINIAC_TEST_REMOTE_KEY") || strings.Contains(env, "not-for-the-browser") {
		t.Fatalf("a key reached the browser: %q", cmd.Env)
	}
	if !strings.Contains(env, "brainiac-test") {
		t.Fatalf("PATH did not reach the browser: %q", cmd.Env)
	}
}

// landlockProbeArg makes the test binary a stand-in browser that tries the
// paths it is given and prints, one line each, "ok" or "denied" and the
// path. Write paths start with "w:".
const landlockProbeArg = "--landlock-probe"

func landlockProbe(paths []string) {
	for _, p := range paths {
		var err error
		if w, ok := strings.CutPrefix(p, "w:"); ok {
			p = "w:" + w
			err = os.WriteFile(w, []byte("x"), 0o600)
		} else {
			var f *os.File
			if f, err = os.Open(p); err == nil {
				_, err = f.Read(make([]byte, 1))
				f.Close()
			}
		}
		switch {
		case err == nil || errors.Is(err, io.EOF):
			fmt.Println("ok", p)
		case errors.Is(err, syscall.EACCES):
			fmt.Println("denied", p) // EACCES: what Landlock answers
		case errors.Is(err, os.ErrPermission):
			fmt.Println("refused", p, err)
		default:
			fmt.Println("error", p, err)
		}
	}
}

// The browser's file policy, built on every platform (applied on Linux):
// its profile read-write, the system read set, nothing of the operator's,
// nothing of /proc beyond its own entry and the machine's facts (N-W4-8).
func TestTheBrowsersFilePolicy(t *testing.T) {
	p := browserPolicy("/home/op/.cache/ms-playwright/chromium-1/chrome-linux/chrome", "/tmp/brainiac-browser-123")
	if len(p.Write) == 0 || p.Write[0] != "/tmp/brainiac-browser-123" {
		t.Fatalf("the profile is not the first writable folder: %v", p.Write)
	}
	for _, w := range p.Write[1:] {
		if !strings.HasPrefix(w, "/dev/") {
			t.Errorf("writable beyond the profile and devices: %s", w)
		}
	}
	for _, want := range []string{"/usr", "/etc/fonts", "/etc/ssl", "/proc/self", "/home/op/.cache/ms-playwright/chromium-1/chrome-linux"} {
		if !covered(p.Read, want) {
			t.Errorf("%s is not readable: %v", want, p.Read)
		}
	}
	for _, never := range []string{"/", "/etc", "/home", "/home/op", "/root", "/tmp", "/proc", "/proc/1", "/var", "/run", "/home/op/.brainiac"} {
		if covered(p.Read, never) || covered(p.Write, never) {
			t.Errorf("%s is reachable", never)
		}
	}
	if p.BlockTCP || p.Mount {
		t.Errorf("the network must be kept and no mount view asked: %+v", p)
	}
	// A browser under /usr or /opt adds nothing: its folder is covered.
	if q := browserPolicy("/opt/google/chrome/chrome", "/tmp/p"); len(q.Read) != len(browserSystemRead) {
		t.Errorf("a browser in /opt added %v", q.Read[len(browserSystemRead):])
	}
}
