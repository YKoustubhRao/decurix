package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/querynotes"
)

// guardedServer is a Server whose only container network is 172.30.0.0/16 (gateway .1), where 172.30.0.5 is the
// desktop "box-a", and a handler that answers 200 with the desktop it was told it acts for.
func guardedServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{}
	s.nets.subnets = []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}
	s.nets.gateways = []netip.Addr{netip.MustParseAddr("172.30.0.1")}
	s.nets.fetched = time.Now().Add(time.Hour)
	s.callerAt = func(r *http.Request) (string, bool) {
		if strings.HasPrefix(r.RemoteAddr, "172.30.0.5:") {
			return "box-a", true
		}
		return "", false
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := actingAs(w, r, r.URL.Query().Get("desktop_id"))
		if !ok {
			return
		}
		_, _ = w.Write([]byte(who))
	})
	return s, s.GuardPrimary(inner)
}

func guardDo(h http.Handler, method, target, remote string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const fromHost = "127.0.0.1:50000"

// The review created a desktop and queued a task from a page on another site: a cross-site write is refused, a page's
// own and a script's (no Origin) are served.
func TestGuardRefusesCrossSiteWrites(t *testing.T) {
	_, h := guardedServer(t)
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"other origin POST", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"null origin POST", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080", "Origin": "null"}, http.StatusForbidden},
		{"cross-site fetch metadata", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site other port", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080", "Origin": "http://127.0.0.1:3000"}, http.StatusForbidden},
		{"cross-site WebSocket", http.MethodGet, map[string]string{"Host": "127.0.0.1:8080", "Origin": "http://evil.example", "Upgrade": "websocket"}, http.StatusForbidden},
		{"own page POST", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080", "Origin": "http://127.0.0.1:8080", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"script POST", http.MethodPost, map[string]string{"Host": "127.0.0.1:8080"}, http.StatusOK},
		{"cross-site GET read", http.MethodGet, map[string]string{"Host": "127.0.0.1:8080", "Sec-Fetch-Site": "cross-site"}, http.StatusOK},
	}
	for _, c := range cases {
		if got := guardDo(h, c.method, "/api/desktops/create", fromHost, c.headers).Code; got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A DNS-rebinding page arrives with its own domain as Host and is refused; the names this listener is reached by are
// served.
func TestGuardRefusesUnknownHostNames(t *testing.T) {
	_, h := guardedServer(t)
	for host, want := range map[string]int{
		"attacker.example:8080":     http.StatusMisdirectedRequest,
		"rebind.attacker.com":       http.StatusMisdirectedRequest,
		"127.0.0.1:8080":            http.StatusOK,
		"localhost:8080":            http.StatusOK,
		"[::1]:8080":                http.StatusOK,
		"host.docker.internal:8080": http.StatusOK,
		"decurix:8080":              http.StatusOK,
		"172.30.0.1:8080":           http.StatusOK,
	} {
		if got := guardDo(h, http.MethodGet, "/", fromHost, map[string]string{"Host": host}).Code; got != want {
			t.Errorf("Host %s: got %d, want %d", host, got, want)
		}
	}
	t.Setenv("DECURIX_ALLOWED_HOSTS", "decurix.office.lan")
	if got := guardDo(h, http.MethodGet, "/", fromHost, map[string]string{"Host": "decurix.office.lan:8080"}).Code; got != http.StatusOK {
		t.Errorf("a name in DECURIX_ALLOWED_HOSTS was refused: %d", got)
	}
}

// A desktop reaches only its callback routes, and on them is who its address says, whatever its body claims.
func TestGuardLimitsDesktopsToTheirCallbacks(t *testing.T) {
	_, h := guardedServer(t)
	desk := "172.30.0.5:40000"
	for _, c := range []struct {
		method, target string
		want           int
	}{
		{http.MethodPost, "/api/desktops/send", http.StatusForbidden},                 // answering its own approval
		{http.MethodPost, "/api/desktops/create", http.StatusForbidden},               // making a desktop with host files
		{http.MethodGet, "/api/lan-access/status", http.StatusForbidden},              // the access code
		{http.MethodPost, "/api/tasks/verdict", http.StatusForbidden},                 // grading its own work
		{http.MethodPost, "/api/dags/cron/set", http.StatusForbidden},                 // jobs aimed at other workers
		{http.MethodPost, "/api/tunnel", http.StatusForbidden},                        // the link people are sent
		{http.MethodPost, "/api/knowledge", http.StatusForbidden},                     // writing org rules
		{http.MethodPost, "/api/agent/events", http.StatusOK},                         // its own progress
		{http.MethodGet, "/api/knowledge", http.StatusOK},                             // reading org rules
		{http.MethodPost, "/api/llm/azure/chat", http.StatusOK},                       // the model proxy
		{http.MethodPost, "/api/agent/events?desktop_id=box-b", http.StatusForbidden}, // as another desktop
		{http.MethodPost, "/api/agent/events?desktop_id=box-a", http.StatusOK},
	} {
		w := guardDo(h, c.method, c.target, desk, map[string]string{"Host": "decurix:8080"})
		if w.Code != c.want {
			t.Errorf("%s %s from a desktop: got %d, want %d (%s)", c.method, c.target, w.Code, c.want, w.Body.String())
		}
		if w.Code == http.StatusOK && w.Body.String() != "box-a" {
			t.Errorf("%s %s: acted as %q, want box-a", c.method, c.target, w.Body.String())
		}
	}
	// An address in the desktop network that is no desktop's (another container) gets nothing.
	if got := guardDo(h, http.MethodPost, "/api/agent/events", "172.30.0.9:40000", map[string]string{"Host": "decurix:8080"}).Code; got != http.StatusForbidden {
		t.Errorf("a container that is no desktop was served: %d", got)
	}
	// The host itself (the network's gateway, as a published port arrives) is a person.
	if got := guardDo(h, http.MethodPost, "/api/desktops/send?desktop_id=box-b", "172.30.0.1:40000", map[string]string{"Host": "127.0.0.1:8080"}); got.Code != http.StatusOK || got.Body.String() != "box-b" {
		t.Errorf("the host was not served as itself: %d %q", got.Code, got.Body.String())
	}
}

// The gated listener refuses cross-site writes too, and every desktop.
func TestGuardGated(t *testing.T) {
	s, _ := guardedServer(t)
	h := s.GuardGated(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if got := guardDo(h, http.MethodPost, "/api/desktops/send", fromHost, map[string]string{"Host": "work.example.com", "Origin": "https://evil.example"}).Code; got != http.StatusForbidden {
		t.Errorf("cross-site POST on the gated listener: %d", got)
	}
	if got := guardDo(h, http.MethodPost, "/api/desktops/send", fromHost, map[string]string{"Host": "localhost:8090", "Origin": "https://work.example.com", "X-Forwarded-Host": "work.example.com"}).Code; got != http.StatusOK {
		t.Errorf("the tunnel's own page was refused: %d", got)
	}
	if got := guardDo(h, http.MethodGet, "/", "172.30.0.5:1234", map[string]string{"Host": "localhost:8090"}).Code; got != http.StatusForbidden {
		t.Errorf("a desktop on the gated listener: %d", got)
	}
}

// Every route a desktop may call is one the control plane serves on the primary listener: a typo here would fail
// an agent's callback closed, silently.
func TestContainerRoutesAreServed(t *testing.T) {
	mux := http.NewServeMux()
	s := &Server{}
	s.RegisterLocalRoutes(mux)
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("RegisterRoutes needs a full server here: %v", r)
		}
	}()
	s.RegisterRoutes(mux)
	for _, route := range containerRoutes {
		path := route.path
		if route.prefix {
			path += "x"
		}
		if strings.HasPrefix(path, "/api/handoffs") || strings.HasPrefix(path, "/api/knowledge") || strings.HasPrefix(path, "/api/skillhub") {
			continue // registered only when their stores are set
		}
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern == "/" {
			t.Errorf("desktop callback %s is not a route of the primary listener", route.path)
		}
	}
}

func TestValidDisplay(t *testing.T) {
	for display, want := range map[string]bool{
		":1": true, ":1.0": true, "172.17.0.2:1": true,
		":1; rm -rf /": false, "$(id):1": false, "`id`": false, ":1 ": false, "a:1": false, "": false,
	} {
		if got := validDisplay(display); got != want {
			t.Errorf("validDisplay(%q) = %v, want %v", display, got, want)
		}
	}
	if got := shellQuote(`a'b`); got != `'a'\''b'` {
		t.Errorf("shellQuote: %s", got)
	}
}

// A reply typed under an older question card does not answer a newer approval of the same worker.
func TestReplyAnswersOnlyItsOwnCard(t *testing.T) {
	notes, err := querynotes.NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{QueryNotes: notes}
	now := time.Now()
	notes.Add(querynotes.QueryNote{ID: "old", DesktopID: "box", Kind: querynotes.KindQuery, Content: "which file?", CreatedAt: now.Add(-time.Minute)})
	notes.Add(querynotes.QueryNote{ID: "new", DesktopID: "box", Kind: querynotes.KindApproval, Content: "pay 4,50,000?", CreatedAt: now})
	notes.Add(querynotes.QueryNote{ID: "other", DesktopID: "box-2", Kind: querynotes.KindApproval, CreatedAt: now.Add(time.Minute)})

	if err := s.replyStillAnswers("box", "old"); err == nil {
		t.Error("a reply under the older card was let through to the newer approval")
	}
	if err := s.replyStillAnswers("box", "new"); err != nil {
		t.Errorf("a reply under the newest card was refused: %v", err)
	}
	if err := s.replyStillAnswers("box", "gone"); err == nil {
		t.Error("a reply under a closed card was let through")
	}
}
