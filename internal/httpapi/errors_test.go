package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
)

func TestJSONErrorMapsCallerMistakesToTheirStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"missing desktop", fmt.Errorf("find desktop x: %w", desktop.ErrNotFound), http.StatusNotFound},
		{"rejected memory entry", fmt.Errorf("%w: title is required", memory.ErrInvalid), http.StatusBadRequest},
		{"anything else", fmt.Errorf("disk on fire"), http.StatusInternalServerError},
	}

	for _, c := range cases {
		rec := httptest.NewRecorder()
		jsonError(rec, c.err)

		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
		if strings.Contains(rec.Body.String(), "disk on fire") {
			t.Errorf("%s: internal error text reached the client: %s", c.name, rec.Body.String())
		}
	}
}

func TestMemoryHandlerAnswersBadInputWith400(t *testing.T) {
	db := testDB(t)

	store, err := memory.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Memory: store}

	for name, form := range map[string]map[string]string{
		"nothing":      {},
		"no desktop":   {"title": "t", "summary": "s"},
		"no title":     {"desktop_id": "d", "summary": "s"},
		"huge summary": {"desktop_id": "d", "title": "t", "summary": strings.Repeat("x", 100000)},
		"huge title":   {"desktop_id": "d", "title": strings.Repeat("t", 1000), "summary": "s"},
		"huge desktop": {"desktop_id": strings.Repeat("d", 1000), "title": "t", "summary": "s"},
	} {
		vals := url.Values{}
		for k, v := range form {
			vals.Set(k, v)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/desktops/memory", strings.NewReader(vals.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()

		s.desktopMemoryHandler(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}
