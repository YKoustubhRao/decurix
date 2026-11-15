// training_test.go checks `decurix --export-training`: it asks the export route with the bound and format given and
// copies the lines through, reporting the trailer's counts on stderr.
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportTrainingCopiesTheLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/runs/training-export" || r.URL.Query().Get("since") != "7d" || r.URL.Query().Get("format") != "toolcall" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Trailer", "X-Decurix-Exported, X-Decurix-Skipped")
		_, _ = w.Write([]byte("{\"run_id\":\"e_1\"}\n"))
		w.Header().Set("X-Decurix-Exported", "1")
	}))
	defer srv.Close()
	var out, errs bytes.Buffer
	if code := runExportTraining(srv.URL, "7d", "toolcall", &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if out.String() != "{\"run_id\":\"e_1\"}\n" || !strings.Contains(errs.String(), "1 runs written") {
		t.Fatalf("out %q, err %q", out.String(), errs.String())
	}
	if code := runExportTraining(srv.URL, "7d", "decurix", &out, &errs); code != 1 {
		t.Fatalf("a refused export exited %d", code)
	}
}
