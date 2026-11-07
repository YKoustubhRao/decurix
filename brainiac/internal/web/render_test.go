package web

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for a headless browser: started
// with "--headless=new", it prints as many bytes of page as the page URL's
// "n" asks for. The URL carries it because the browser gets a minimal
// environment (browserCommand), which a test variable would not pass.
func TestMain(m *testing.M) {
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
