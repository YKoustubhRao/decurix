// Render checks for cron triggers read in a time zone (B-37): the requests,
// triggers and DAG pages offer a zone field beside the cron, no longer call
// the cron UTC, and the DAG list shows a cron's zone and next run.

package webui

import (
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
)

func TestCronFormsOfferAZone(t *testing.T) {
	renderer := NewRenderer()
	page := PageData{Desktops: []desktop.Desktop{{ID: "bordu", Name: "bordu", Status: "running"}}}

	var requests, triggers, dagPage strings.Builder
	if err := renderer.RenderRequests(&requests, page); err != nil {
		t.Fatalf("RenderRequests: %v", err)
	}
	if err := renderer.RenderTriggers(&triggers, page); err != nil {
		t.Fatalf("RenderTriggers: %v", err)
	}
	if err := renderer.RenderDag(&dagPage, DagPageData{DAG: dag.DAG{ID: "dag-1", Name: "nightly", CronExpr: "0 9 * * *", CronZone: "Asia/Kolkata"}}); err != nil {
		t.Fatalf("RenderDag: %v", err)
	}

	for name, html := range map[string]string{"requests": requests.String(), "triggers": triggers.String(), "dag": dagPage.String()} {
		if !strings.Contains(html, `id="cronZone"`) || !strings.Contains(html, `id="zoneList"`) {
			t.Errorf("the %s page has no time zone field for its cron", name)
		}
		if strings.Contains(html, "weekday, UTC") || strings.Contains(html, "09:00 UTC") || strings.Contains(html, "(UTC, optional)") {
			t.Errorf("the %s page still says its cron is UTC", name)
		}
	}

	if !strings.Contains(dagPage.String(), `value="Asia/Kolkata"`) {
		t.Error("the DAG page does not prefill the cron's stored zone")
	}
}

func TestDagListShowsCronZoneAndNextRun(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	next := time.Date(2026, 11, 8, 9, 0, 0, 0, ist)

	var out strings.Builder
	if err := NewRenderer().RenderDags(&out, DagsPageData{Dags: []dag.Summary{{
		DAG: dag.DAG{ID: "dag-1", Name: "nightly", CronExpr: "0 9 * * *", CronZone: "Asia/Kolkata", NextRunAt: next},
	}}}); err != nil {
		t.Fatalf("RenderDags: %v", err)
	}

	html := out.String()
	if !strings.Contains(html, "Asia/Kolkata") || !strings.Contains(html, `data-local="2026-11-08T09:00:00&#43;05:30"`) {
		t.Fatalf("the DAG list does not show the cron's zone and next run:\n%s", html)
	}
}
