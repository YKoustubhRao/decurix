// Tests that the readiness page is never offered Simulate's "enable" while nothing consumes a practice grant: the
// routes the page reads say so (enable_offered false, with a note why), and the page shows the note, not the button.
package httpapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulateSaysEnableIsNotOfferedWhileGrantsAreUnused(t *testing.T) {
	if practiceGrantsUsed {
		t.Skip("practice grants have a consumer now")
	}
	_, owner, _, _ := simulationServer(t, 1<<30)
	w := owner.do("GET", "/api/readiness/simulation/list?worker=worker", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var list struct {
		Offered *bool  `json:"enable_offered"`
		Note    string `json:"enable_note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.Offered == nil || *list.Offered || !strings.Contains(list.Note, "not offered yet") {
		t.Fatalf("list: %s", w.Body)
	}
	// The evidence of a run (what the page's results read) says the same.
	if note := practiceEnableNote(); note == "" || !strings.Contains(note, "nothing to enable") {
		t.Fatalf("note %q", note)
	}
}

// The page offers Simulate's enable only when the route says it is offered, and otherwise shows the route's note.
func TestTheReadinessPageHidesSimulateEnableBehindTheRoutesWord(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "readiness.html"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	i := strings.Index(s, "async function simResults(")
	if i < 0 {
		t.Fatal("the page has no simResults")
	}
	body := s[i:]
	if j := strings.Index(body, "\n"); j > 0 {
		body = body[:j]
	}
	offered := strings.Index(body, "if(d.enable_offered)")
	call := strings.Index(body, "simulation/authorize")
	if offered < 0 || call < offered || !strings.Contains(body, "d.enable_note") {
		t.Fatalf("Simulate's enable is not behind enable_offered, or the note is not shown: %s", body)
	}
	if strings.Count(s, "simulation/authorize") != 1 {
		t.Fatal("the page calls Simulate's authorize somewhere enable_offered does not guard")
	}
}
