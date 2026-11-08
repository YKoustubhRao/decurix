package httpapi

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A finished task's row says whether the desktop checked it, and a request
// that starts like a formula is kept as text for the spreadsheet reading it.
func TestTaskExportSaysWhatWasCheckedAndStaysText(t *testing.T) {
	s := chatServer(t)

	checked := deliver(t, s, "box", `=HYPERLINK("http://x","invoice")`)
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"box","task_id":"`+checked.TaskID+`","content":"DONE","kind":"final","reply":"Filed INV-7.","engine":"thebot","checked":true,`+
			`"verdict":{"schema":"decurix.verdict/v1","verdict":"done","by":"verify","why":"","basis":["code"],`+
			`"checks":[{"id":"files.read_back","result":"pass","basis":"code","detail":"","refs":[]}],"evidence":[]}}`)

	unsaid := deliver(t, s, "old-image", "what is the kernel version?")
	postJSON(t, s.queryTextHandler, "/api/querytext",
		`{"desktop_id":"old-image","task_id":"`+unsaid.TaskID+`","content":"DONE","kind":"final","reply":"6.6","engine":"ollama"}`)

	rec := httptest.NewRecorder()
	s.taskExportHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	byID := map[string][]string{}
	for _, r := range rows[1:] {
		byID[r[col["task_id"]]] = r
	}

	row := byID[checked.TaskID]
	if row == nil {
		t.Fatalf("no row for %s in %v", checked.TaskID, rows)
	}
	if row[col["checked"]] != "yes" {
		t.Errorf("checked = %q, want yes", row[col["checked"]])
	}
	if row[col["request"]] != `'=HYPERLINK("http://x","invoice")` {
		t.Errorf("request = %q: a formula must stay text", row[col["request"]])
	}
	if row[col["final_reply"]] != "Filed INV-7." {
		t.Errorf("final_reply = %q", row[col["final_reply"]])
	}

	lesson := deliver(t, s, "box", "/record file the invoice")
	rec = httptest.NewRecorder()
	s.taskExportHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/export", nil))
	rows, _ = csv.NewReader(rec.Body).ReadAll()
	for _, r := range rows[1:] {
		byID[r[col["task_id"]]] = r
	}
	if got := byID[lesson.TaskID][col["autonomous"]]; got != "not counted" {
		t.Errorf("a demonstration: autonomous = %q, want not counted", got)
	}

	if got := byID[unsaid.TaskID][col["checked"]]; got != "unknown" {
		t.Errorf("a desktop that did not say: checked = %q, want unknown", got)
	}

	rec = httptest.NewRecorder()
	s.taskExportHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/export?format=json", nil))
	var full struct {
		Tasks []struct {
			ID         string `json:"id"`
			Executions []struct {
				Checked *bool `json:"checked"`
			} `json:"executions"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil || len(full.Tasks) != 3 {
		t.Fatalf("json export: %v, %s", err, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.taskExportHandler(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/export?since=yesterday", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a bad date: status %d", rec.Code)
	}
}
