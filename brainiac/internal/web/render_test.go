package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestMain lets the test binary stand in for a headless browser: started
// with "--headless=new", it prints BRAINIAC_FAKE_DOM_BYTES bytes of page.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--headless=new" {
		n, _ := strconv.Atoi(os.Getenv("BRAINIAC_FAKE_DOM_BYTES"))
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
	t.Setenv("BRAINIAC_FAKE_DOM_BYTES", "1000000")
	if _, err := c.Render(context.Background(), "http://127.0.0.1:1/"); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("a 1 MB page with a 4 KB limit: %v", err)
	}
	t.Setenv("BRAINIAC_FAKE_DOM_BYTES", "100")
	p, err := c.Render(context.Background(), "http://127.0.0.1:1/")
	if err != nil || !p.Rendered || !strings.Contains(p.Text, "xxx") {
		t.Fatalf("a small page: %+v %v", p, err)
	}
}
