package ollama

import (
	"context"
	"testing"
)

// The control plane preloads whatever DesktopURL names, so it must agree
// with the value desktops are created with.
func TestDesktopURL(t *testing.T) {
	t.Setenv("OLLAMA_URL", "")
	if got := DesktopURL(); got != defaultURL {
		t.Errorf("DesktopURL() with no OLLAMA_URL = %q, want %q", got, defaultURL)
	}

	// A trailing slash must not make the bundled URL look like some other
	// server, or EnsureReady would skip starting the container desktops use.
	t.Setenv("OLLAMA_URL", BundledURL+"/")
	if got := DesktopURL(); got != BundledURL {
		t.Errorf("DesktopURL() = %q, want %q", got, BundledURL)
	}
}

func TestDesktopModel(t *testing.T) {
	t.Setenv("OLLAMA_MODEL", "")
	if got := DesktopModel(); got != Model {
		t.Errorf("DesktopModel() with no OLLAMA_MODEL = %q, want %q", got, Model)
	}

	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")
	if got := DesktopModel(); got != "llama3.2:1b" {
		t.Errorf("DesktopModel() = %q, want llama3.2:1b", got)
	}
}

// Only host.docker.internal is ever rewritten.
func TestDialableURLLeavesOtherHostsAlone(t *testing.T) {
	for _, u := range []string{BundledURL, "http://10.0.0.5:11434", "https://ollama.example.com"} {
		if got := dialableURL(context.Background(), u); got != u {
			t.Errorf("dialableURL(%q) = %q, want it unchanged", u, got)
		}
	}
}

func TestDefaultNumThread(t *testing.T) {
	t.Setenv("OLLAMA_NUM_THREAD", "3")
	if got := DefaultNumThread(); got != 3 {
		t.Errorf("DefaultNumThread() with OLLAMA_NUM_THREAD=3 got %d, want 3", got)
	}

	t.Setenv("OLLAMA_NUM_THREAD", "")
	got := DefaultNumThread()
	if got < 1 {
		t.Errorf("DefaultNumThread() fallback = %d, want >= 1", got)
	}
}
