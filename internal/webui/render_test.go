package webui

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/helpnotes"
	"github.com/YKoustubhRao/flowera/internal/memory"
)

// Every page is parsed out of the embedded FS and executed here so a broken
// template fails the build rather than the first request that reaches it: the
// renderers use template.Must, so a bad action panics at parse time, and a
// field that no longer exists errors at execute time. Neither shows up in a
// plain `go build`, since templates are data to the compiler.
func TestRenderEveryPage(t *testing.T) {
	renderer := NewRenderer()

	running := desktop.Desktop{
		ID:        "research-box",
		Name:      "research-box",
		Profile:   "developer",
		Harness:   desktop.HarnessTheBot,
		Status:    "running",
		URL:       "/desktop/research-box/",
		CreatedAt: time.Now().Format(time.RFC3339),
		CPUs:      2,
		MemoryMB:  4096,
	}

	stopped := desktop.Desktop{
		ID:      "spare-box",
		Name:    "spare-box",
		Profile: "basic",
		Harness: desktop.HarnessOllama,
		Status:  "stopped",
		URL:     "/desktop/spare-box/",
	}

	entry := memory.WorkerMemoryEntry{
		ID:        "memory-1",
		DesktopID: running.ID,
		Title:     "Lead follow-up",
		Summary:   "Check the CRM before replying.",
		Category:  "workflow",
		CreatedAt: time.Now().UTC(),
	}

	page := PageData{
		Desktops:     []desktop.Desktop{running, stopped},
		WorkerMemory: []memory.WorkerMemoryEntry{entry},
		MemoryByDesktop: map[string][]memory.WorkerMemoryEntry{
			running.ID: {entry},
		},
		TunnelURL: "https://example.trycloudflare.com",
	}

	var index bytes.Buffer

	if err := renderer.RenderIndex(&index, page); err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}

	// The dashboard's open-in-a-new-tab button is the only thing left that
	// emits ?ui=touch, now that /view/<id> is the chat alone and embeds no
	// screen. That marker is what splices the touch keyboard and pinch-zoom
	// into noVNC (httpapi.injectMobileEnhancements), and losing it fails
	// silently - nothing errors, the affordances just stop being injected -
	// so it is asserted here rather than trusted.
	if !strings.Contains(index.String(), "ui=touch") {
		t.Fatal("the dashboard no longer opens a desktop screen with ?ui=touch; mobile keyboard and pinch-zoom will not be injected")
	}

	if err := renderer.RenderMessages(io.Discard, page); err != nil {
		t.Fatalf("RenderMessages: %v", err)
	}

	notes := HelpNotesPageData{
		Notes: []helpnotes.HelpNote{{
			ID:        "1",
			DesktopID: running.ID,
			VMName:    running.Name,
			Filename:  "11092026120000.txt",
			Content:   "FLOWERA HELP REQUEST",
			CreatedAt: time.Now(),
		}},
	}

	if err := renderer.RenderHelpNotesDashboard(io.Discard, notes); err != nil {
		t.Fatalf("RenderHelpNotesDashboard: %v", err)
	}

	// Both branches of the desktop view: a running desktop renders the chat
	// and its screen, a stopped one renders only the "start it first" state.
	for _, d := range []desktop.Desktop{running, stopped} {
		if err := renderer.RenderDesktopView(io.Discard, DesktopViewPageData{Desktop: d}); err != nil {
			t.Fatalf("RenderDesktopView(%s): %v", d.Status, err)
		}
	}
}
