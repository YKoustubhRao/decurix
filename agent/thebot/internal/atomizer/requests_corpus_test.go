package atomizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// liveFixture is the path of a fixture made from live runs: outside the repository (it quotes the person's requests,
// demo logins included - never committed), in DECURIX_TEST_FIXTURES, else decurix-tests/fixtures under /mnt/c when that
// exists, else its Windows path. The test skips, naming the path, when the file is not there.
func liveFixture(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("DECURIX_TEST_FIXTURES")
	if dir == "" {
		dir = "/mnt/c/Users/Administrator/decurix-tests/fixtures"
		if _, err := os.Stat(dir); err != nil {
			dir = `C:\Users\Administrator\decurix-tests\fixtures`
		}
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("fixture not here: %s (set DECURIX_TEST_FIXTURES)", path)
	}
	return path
}

// liveRequest is one request of the live corpus (live_requests.json, liveFixture): its text as the person wrote it, the
// apps WorkApps finds in it whole, and the app each deliverable is bound to ("" = none).
type liveRequest struct {
	ID           string   `json:"id"`
	Text         string   `json:"text"`
	WorkApps     []string `json:"work_apps"`
	Deliverables []struct {
		File string `json:"file"`
		App  string `json:"app"`
	} `json:"deliverables"`
}

// loadLiveRequests reads the corpus the soul's and the agent's request tests share.
func loadLiveRequests(t *testing.T, path string) []liveRequest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var corpus struct {
		Requests []liveRequest `json:"requests"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(corpus.Requests) == 0 {
		t.Fatalf("%s holds no requests", path)
	}
	return corpus.Requests
}

// stagePromptFor is a planned stage's prompt as long_task.stage_prompt opens it: the stage, what it hands in, the app
// line the agent decided for it (long_task.stage_app), then the request.
func stagePromptFor(n, of int, file, app, request string) string {
	lines := []string{"Stage " + itoa(n) + " of " + itoa(of) + " of a longer request a person made: make " + file + ".",
		"Hands in: " + file}
	if app != "" {
		lines = append(lines, "Work in app: "+app, "The request names "+app+" for "+file+": open it in "+app+".")
	} else {
		lines = append(lines, "No app is named for what this stage hands in: make it with your own tools (office, python) - "+
			"an app the request names for another file is not this stage's.")
	}
	return strings.Join(lines, "\n") + "\n\nThe request:\n" + request
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Every per-request decision the soul makes, across the six live requests (round 5, 2026-10-20). A named app binds to
// its own deliverable only: found live (CL), "a 5-slide MIS deck in PowerPoint" held P1's workbook stage to
// PowerPoint. A format ("the template (Word)"), an option ("if you use Word there", "if you use the Office apps on my
// PC") is no app to work in. Each request, whole or as one of its stages, is desk work for the operator.
func TestLiveRequestsBindAppsToTheirOwnDeliverables(t *testing.T) {
	for _, r := range loadLiveRequests(t, liveFixture(t, "live_requests.json")) {
		t.Run(r.ID, func(t *testing.T) {
			got := WorkApps(r.Text)
			if len(got) == 0 {
				got = nil
			}
			want := r.WorkApps
			if len(want) == 0 {
				want = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("WorkApps = %v, want %v", got, want)
			}
			if route := Triage(r.Text); route.Kind != RouteOperate {
				t.Errorf("the whole request triaged %s (%s), want the operator", route.Kind, route.Reason)
			}
			for i, d := range r.Deliverables {
				prompt := stagePromptFor(i+1, len(r.Deliverables), d.File, d.App, r.Text)
				var wantApps []string
				if d.App != "" {
					wantApps = []string{strings.ToLower(d.App)}
				}
				if apps := AppsFor(prompt); !reflect.DeepEqual(apps, wantApps) {
					t.Errorf("%s: AppsFor = %v, want %v", d.File, apps, wantApps)
				}
				if route := Triage(prompt); route.Kind != RouteOperate {
					t.Errorf("%s: its stage triaged %s (%s), want the operator", d.File, route.Kind, route.Reason)
				}
			}
		})
	}
}
