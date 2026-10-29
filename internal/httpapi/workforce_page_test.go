package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/knowledge"
	"github.com/YKoustubhRao/decurix/internal/skillhub"
	"github.com/YKoustubhRao/decurix/internal/webui"
	"github.com/YKoustubhRao/decurix/internal/workqueue"
)

// TestWorkforcePageHidesPrimaryOnlySectionsWhenGated checks GET /workforce
// renders every wired section on the primary listener, and leaves out access,
// queue and handoffs (registered on the primary listener only) when the
// request came through the gated one.
func TestWorkforcePageHidesPrimaryOnlySectionsWhenGated(t *testing.T) {
	s, _ := accessServer(t)
	s.Web = webui.NewRenderer()
	db := testDB(t)
	var err error
	if s.SkillHub, err = skillhub.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if s.Knowledge, err = knowledge.NewStore(db); err != nil {
		t.Fatal(err)
	}
	if s.Queue, err = workqueue.NewStore(db); err != nil {
		t.Fatal(err)
	}

	get := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/workforce", nil))
		return rec
	}

	page := http.HandlerFunc(s.workforcePageHandler)

	primary := get(page)
	if primary.Code != http.StatusOK {
		t.Fatalf("primary: %d %s", primary.Code, primary.Body)
	}
	for _, want := range []string{`id="sec-workers"`, `id="sec-skills"`, `id="sec-knowledge"`, `id="sec-queue"`, `id="sec-access"`, "/api/access/users"} {
		if !strings.Contains(primary.Body.String(), want) {
			t.Errorf("primary page lacks %q", want)
		}
	}

	gated := get(MarkGated(page))
	if gated.Code != http.StatusOK {
		t.Fatalf("gated: %d %s", gated.Code, gated.Body)
	}
	body := gated.Body.String()
	for _, gone := range []string{`id="sec-access"`, "/api/access/", `id="sec-queue"`, "/api/queue"} {
		if strings.Contains(body, gone) {
			t.Errorf("gated page still has %q", gone)
		}
	}
	if !strings.Contains(body, `id="sec-skills"`) || !strings.Contains(body, `id="sec-knowledge"`) {
		t.Error("gated page lost the sections both listeners serve")
	}

	rec := httptest.NewRecorder()
	page.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/workforce/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/workforce/x: %d", rec.Code)
	}
}
