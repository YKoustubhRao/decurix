package webui

// The chat page and the recordings page of a desktop that is not running offer Wake (paused) or Start (stopped) in
// place, so the card's Chat button and the Recordings link never lead to a page that only says "go back".

import (
	"bytes"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

func TestANotRunningDesktopsPagesOfferWakeOrStart(t *testing.T) {
	renderer := NewRenderer()
	for status, want := range map[string]string{
		"paused":       `data-wake="unpause"`,
		"stopped":      `data-wake="start"`,
		"crashed":      `data-wake="start"`,
		"provisioning": "",
	} {
		d := desktop.Desktop{ID: "box", Name: "box", Status: status, URL: "/desktop/box/"}

		var chat, recordings bytes.Buffer
		if err := renderer.RenderDesktopView(&chat, DesktopViewPageData{Desktop: d}); err != nil {
			t.Fatalf("RenderDesktopView(%s): %v", status, err)
		}
		if err := renderer.RenderRecordings(&recordings, RecordingsPageData{Desktop: d}); err != nil {
			t.Fatalf("RenderRecordings(%s): %v", status, err)
		}
		for page, html := range map[string]string{"chat": chat.String(), "recordings": recordings.String()} {
			if !strings.Contains(html, `id="desktopAsleep"`) {
				t.Errorf("%s page of a %s desktop has no not-running notice", page, status)
			}
			if want == "" {
				if strings.Contains(html, `data-wake="`) {
					t.Errorf("%s page of a desktop being made offers to start it", page)
				}
				continue
			}
			if !strings.Contains(html, want) {
				t.Errorf("%s page of a %s desktop lacks %s", page, status, want)
			}
		}
	}

	running := desktop.Desktop{ID: "box", Name: "box", Status: "running", URL: "/desktop/box/"}
	var chat bytes.Buffer
	if err := renderer.RenderDesktopView(&chat, DesktopViewPageData{Desktop: running}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(chat.String(), `data-wake="`) {
		t.Error("a running desktop's chat offers to wake it")
	}
}
