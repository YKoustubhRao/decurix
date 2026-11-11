package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
)

// guardedServer is a Server whose only container network is 172.30.0.0/16 (gateway .1), where 172.30.0.5 is the
// desktop "box-a", and a handler that answers 200 with the desktop it was told it acts for.
func guardedServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{}
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1")
	s.callerAt = func(r *http.Request) (string, bool, error) {
		if strings.HasPrefix(r.RemoteAddr, "172.30.0.5:") {
			return "box-a", true, nil
		}
		return "", false, nil
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

// Found live (2026-11-11): Docker did not answer under load for a few seconds, the caller map read as empty, and a
// desktop's model call was answered 403 "this address is not a desktop's" - which its worker rightly took as final,
// and the task failed. A caller Docker could not place is answered 503 with a Retry-After, whoever it is, and is not
// let through; an address Docker answered for and that is no desktop's is still refused 403.
func TestGuardAnswersAnUnreadDesktopListWithTryAgain(t *testing.T) {
	s, h := guardedServer(t)
	unread := true
	s.callerAt = func(r *http.Request) (string, bool, error) {
		if unread {
			return "", false, desktop.ErrCallersUnread
		}
		if strings.HasPrefix(r.RemoteAddr, "172.30.0.5:") {
			return "box-a", true, nil
		}
		return "", false, nil
	}
	for _, remote := range []string{"172.30.0.5:40000", "172.30.0.9:40000"} {
		w := guardDo(h, http.MethodPost, "/api/llm/azure/chat", remote, map[string]string{"Host": "decurix:8080"})
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s with the desktop list unread: got %d (Retry-After %q), want 503 with a Retry-After", remote, w.Code, w.Header().Get("Retry-After"))
		}
		if strings.Contains(w.Body.String(), "box-a") {
			t.Errorf("%s with the desktop list unread was served: %q", remote, w.Body.String())
		}
	}
	// A route a desktop may never call is refused before the list is asked: unread changes nothing there.
	if got := guardDo(h, http.MethodPost, "/api/desktops/create", "172.30.0.5:40000", map[string]string{"Host": "decurix:8080"}).Code; got != http.StatusForbidden {
		t.Errorf("a route closed to desktops, list unread: got %d, want 403", got)
	}
	unread = false
	if got := guardDo(h, http.MethodPost, "/api/llm/azure/chat", "172.30.0.9:40000", map[string]string{"Host": "decurix:8080"}).Code; got != http.StatusForbidden {
		t.Errorf("an address Docker says is no desktop's: got %d, want 403", got)
	}
	if got := guardDo(h, http.MethodPost, "/api/llm/azure/chat", "172.30.0.5:40000", map[string]string{"Host": "decurix:8080"}); got.Code != http.StatusOK || got.Body.String() != "box-a" {
		t.Errorf("the desktop once the list reads again: %d %q", got.Code, got.Body.String())
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

// staticNets is a network view of one Docker network (subnet, gateway) and any further ranges with no gateway the
// host comes in through (other people's Docker networks).
func staticNets(subnet, gateway string, others ...string) func() desktop.NetView {
	v := desktop.NetView{Subnets: []netip.Prefix{netip.MustParsePrefix(subnet)}, Gateways: []netip.Addr{netip.MustParseAddr(gateway)}}
	for _, o := range others {
		v.Subnets = append(v.Subnets, netip.MustParsePrefix(o))
	}
	return func() desktop.NetView { return v }
}

// Found live in the third review round: a container on a Docker network the control plane did not know read the
// access code, downloaded the backup, created a DAG and started a desktop, because an address in no known range
// counted as a person. On the primary listener only loopback and a known gateway are a person now.
func TestOnlyTheHostIsAPersonOnThePrimaryListener(t *testing.T) {
	s, h := guardedServer(t)
	s.netsFor = staticNets("172.30.0.0/16", "172.30.0.1", "172.31.0.0/16")
	for _, route := range []struct{ method, target string }{
		{http.MethodGet, "/api/lan-access/status"},
		{http.MethodGet, "/api/backup"},
		{http.MethodPost, "/api/dags/create"},
		{http.MethodPost, "/api/desktops/create"},
		{http.MethodPost, "/api/agent/events"}, // a callback route: still only a desktop's own address
	} {
		for remote, want := range map[string]int{
			"127.0.0.1:50000":   http.StatusOK,        // the browser on this machine
			"[::1]:50000":       http.StatusOK,        // the same, IPv6
			"172.30.0.1:50000":  http.StatusOK,        // the browser through a port compose publishes (the gateway)
			"172.25.0.2:50000":  http.StatusForbidden, // a container on a network nobody told the control plane of
			"172.31.0.2:50000":  http.StatusForbidden, // a container on another Docker network
			"192.168.1.20:5000": http.StatusForbidden, // the LAN: it uses the gated listener
			"garbage":           http.StatusForbidden,
		} {
			got := guardDo(h, route.method, route.target, remote, map[string]string{"Host": "127.0.0.1:8080"}).Code
			if got != want {
				t.Errorf("%s %s from %s: got %d, want %d", route.method, route.target, remote, got, want)
			}
		}
	}
	// humansOnly says the same as the guard, on its own.
	page := s.humansOnly(func(w http.ResponseWriter, r *http.Request) {})
	for remote, want := range map[string]int{"127.0.0.1:1": 200, "172.30.0.1:1": 200, "172.25.0.2:1": 403, "172.30.0.5:1": 403} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/mail", nil)
		r.RemoteAddr = remote
		page(w, r)
		if w.Code != want {
			t.Errorf("humansOnly from %s: got %d, want %d", remote, w.Code, want)
		}
	}
}

// While Docker has said nothing of its networks the primary listener still serves the browser on loopback and refuses
// everyone else, and the gated listener still lets a phone reach its login (round two refused LAN, phone and compose
// browser requests then); a desktop's address known from this machine's own interfaces stays refused there.
func TestNoNetworksKnownFailsClosedOnlyWhereThereIsNoLogin(t *testing.T) {
	s := &Server{}
	s.netsFor = func() desktop.NetView { return desktop.NetView{} }
	served := func(h http.Handler, remote string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/mail", nil)
		r.RemoteAddr = remote
		r.Host = "127.0.0.1:8080"
		h.ServeHTTP(w, r)
		return w.Code
	}
	page := http.HandlerFunc(s.humansOnly(func(w http.ResponseWriter, r *http.Request) {}))
	primary := s.GuardPrimary(page)
	gated := s.GuardGated(MarkGated(page))
	if got := served(primary, "127.0.0.1:1"); got != 200 {
		t.Errorf("the browser on loopback with no networks known: %d", got)
	}
	if got := served(primary, "172.18.0.5:1"); got != 403 {
		t.Errorf("an unknown address on the primary listener with no networks known: %d", got)
	}
	if got := served(gated, "192.168.1.20:1"); got != 200 {
		t.Errorf("a phone on the gated listener with no networks known: %d", got)
	}
	s.netsFor = staticNets("172.18.0.0/16", "172.18.0.1")
	if got := served(gated, "172.18.0.5:1"); got != 403 {
		t.Errorf("a desktop on the gated listener: %d", got)
	}
	if got := served(gated, "172.18.0.1:1"); got != 200 {
		t.Errorf("the host on the gated listener: %d", got)
	}
}

// TestADesktopReadsItsOwnWaitingTasks: an agent coming back from a restart asks which of its tasks wait on a person
// (GET /api/agent/waiting); found live 2026-11-11 refused 403 to every desktop. It reads only its own, and cannot post.
func TestADesktopReadsItsOwnWaitingTasks(t *testing.T) {
	_, h := guardedServer(t)
	const fromDesktop = "172.30.0.5:40000"
	host := map[string]string{"Host": "127.0.0.1:8080"}
	if got := guardDo(h, http.MethodGet, "/api/agent/waiting?desktop_id=box-a", fromDesktop, host); got.Code != http.StatusOK || got.Body.String() != "box-a" {
		t.Fatalf("own waiting tasks: %d %q", got.Code, got.Body)
	}
	if got := guardDo(h, http.MethodGet, "/api/agent/waiting?desktop_id=box-b", fromDesktop, host); got.Code != http.StatusForbidden {
		t.Fatalf("another desktop's: %d, want 403", got.Code)
	}
	if containerMayCall(http.MethodPost, "/api/agent/waiting") {
		t.Fatal("a desktop may post to the waiting route")
	}
}
