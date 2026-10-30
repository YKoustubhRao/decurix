package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/concierge"
	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

func conciergeServer(t *testing.T) (*Server, *[]string) {
	t.Helper()
	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := concierge.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	var sent []string
	c := concierge.New(store, concierge.Deps{
		Desktops: func(context.Context) ([]desktop.Desktop, error) {
			return []desktop.Desktop{{ID: "alpha", Name: "alpha", Status: "running"}}, nil
		},
		Busy: func(string) (string, error) { return "", nil },
		Send: func(_ context.Context, id, text string) (string, string, error) {
			sent = append(sent, id+": "+text)
			return "0123456789abcdef", "new", nil
		},
		Transcript: func(id string) conversation.Thread { return conversation.Thread{DesktopID: id} },
	})

	s := &Server{Concierge: c, Web: webui.NewRenderer()}
	s.nets.subnets = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	s.nets.gateways = []netip.Addr{netip.MustParseAddr("172.18.0.1")}
	s.nets.fetched = time.Now()
	return s, &sent
}

func TestConciergeSendStartsAJobAndShowsIt(t *testing.T) {
	s, sent := conciergeServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	page := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	mux.ServeHTTP(page, req)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="compose"`) {
		t.Fatalf("/chat: %d", page.Code)
	}

	rec := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/concierge/send",
		strings.NewReader(url.Values{"text": {"check the gold price"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:5000"
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if len(*sent) != 1 || (*sent)[0] != "alpha: check the gold price" {
		t.Fatalf("sent %v", *sent)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/concierge?after=0", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	mux.ServeHTTP(rec, req)
	var state struct {
		Messages []concierge.Message `json:"messages"`
		Sessions []concierge.Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 2 || state.Messages[0].Role != concierge.RoleUser ||
		!strings.Contains(state.Messages[1].Text, "#1 started on alpha") {
		t.Fatalf("messages %+v", state.Messages)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].TaskID != "0123456789abcdef" {
		t.Fatalf("sessions %+v", state.Sessions)
	}
}

// The concierge can start work on every desktop and answer their approvals,
// so an agent inside a desktop must not be able to reach it.
func TestTheConciergeIsNotAvailableToDesktops(t *testing.T) {
	s, sent := conciergeServer(t)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	for _, path := range []string{"/chat", "/api/concierge", "/api/concierge/send"} {
		method := http.MethodGet
		var body *strings.Reader = strings.NewReader("")
		if strings.HasSuffix(path, "/send") {
			method = http.MethodPost
			body = strings.NewReader("text=yes")
		}
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "172.18.0.5:40000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from a desktop: %d, want 403", path, rec.Code)
		}
	}
	if len(*sent) != 0 {
		t.Fatalf("a desktop's request was sent: %v", *sent)
	}
}
