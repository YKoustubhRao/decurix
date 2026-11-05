package webui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dashboard asks for a thumbnail with the ETag of the one it shows, at one URL per card and width: an unchanged
// screen is a 304 (resource plan P1 #4). A cache-buster in the URL made every sweep a full download and decode.
func TestTheDashboardAsksForAThumbnailConditionally(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	start := strings.Index(page, "function shotURL(img)")
	end := strings.Index(page, "async function sweepShots()")
	if start < 0 || end < start {
		t.Fatal("the thumbnail code is not where it was")
	}
	code := page[start:end]
	if strings.Contains(code, "Date.now") || strings.Contains(code, "&t=") {
		t.Error("the thumbnail URL still carries a cache-buster")
	}
	for _, want := range []string{`"If-None-Match"`, `res.status === 304`, `cache: "no-store"`, `URL.revokeObjectURL`} {
		if !strings.Contains(code, want) {
			t.Errorf("the thumbnail fetch lacks %s", want)
		}
	}
}
