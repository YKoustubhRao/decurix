package cftunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sealed"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// fakeCloudflare is just enough of the v4 API for Configure and Status.
type fakeCloudflare struct {
	mu      sync.Mutex
	token   string
	tunnels map[string]string // id -> name
	ingress map[string]any
	records []dnsRecord
	calls   []string
}

func newFakeCloudflare(token string) *fakeCloudflare {
	return &fakeCloudflare{token: token, tunnels: map[string]string{}}
}

func (f *fakeCloudflare) reply(w http.ResponseWriter, status int, result any, errs ...string) {
	messages := []apiMessage{}
	for _, e := range errs {
		messages = append(messages, apiMessage{Code: 1000, Message: e})
	}

	raw, _ := json.Marshal(result)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": len(errs) == 0, "errors": messages, "result": json.RawMessage(raw),
	})
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.reply(w, http.StatusForbidden, nil, "Authentication error")
		return
	}

	path := r.URL.Path
	body, _ := io.ReadAll(r.Body)

	switch {
	case r.Method == http.MethodGet && path == "/zones":
		if r.URL.Query().Get("name") != "example.com" {
			f.reply(w, 200, []zone{})
			return
		}
		f.reply(w, 200, []map[string]any{{"id": "zone1", "name": "example.com", "account": map[string]string{"id": "acct1"}}})

	case r.Method == http.MethodGet && path == "/accounts/acct1/cfd_tunnel":
		list := []map[string]string{}
		for id, name := range f.tunnels {
			if name == r.URL.Query().Get("name") {
				list = append(list, map[string]string{"id": id, "name": name})
			}
		}
		f.reply(w, 200, list)

	case r.Method == http.MethodPost && path == "/accounts/acct1/cfd_tunnel":
		var req map[string]string
		json.Unmarshal(body, &req)
		id := "tun" + string(rune('0'+len(f.tunnels)+1))
		f.tunnels[id] = req["name"]
		f.reply(w, 200, map[string]string{"id": id, "name": req["name"]})

	case r.Method == http.MethodPut && strings.HasSuffix(path, "/configurations"):
		json.Unmarshal(body, &f.ingress)
		f.reply(w, 200, map[string]any{})

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/token"):
		f.reply(w, 200, "connector-token-"+strings.Split(path, "/")[4])

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/accounts/acct1/cfd_tunnel/"):
		f.reply(w, 200, map[string]any{"id": "x", "status": "healthy", "connections": []map[string]string{{"id": "c1"}, {"id": "c2"}}})

	case r.Method == http.MethodGet && path == "/zones/zone1/dns_records":
		list := []dnsRecord{}
		for _, rec := range f.records {
			if rec.Name == r.URL.Query().Get("name") {
				list = append(list, rec)
			}
		}
		f.reply(w, 200, list)

	case r.Method == http.MethodPost && path == "/zones/zone1/dns_records":
		var rec dnsRecord
		json.Unmarshal(body, &rec)
		rec.ID = "rec" + string(rune('0'+len(f.records)+1))
		f.records = append(f.records, rec)
		f.reply(w, 200, rec)

	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/zones/zone1/dns_records/"):
		id := strings.TrimPrefix(path, "/zones/zone1/dns_records/")
		var patch dnsRecord
		json.Unmarshal(body, &patch)
		for i := range f.records {
			if f.records[i].ID == id {
				if patch.Content != "" {
					f.records[i].Content = patch.Content
				}
				f.records[i].Proxied = true
			}
		}
		f.reply(w, 200, map[string]any{})

	default:
		f.reply(w, 404, nil, "no route "+r.Method+" "+path)
	}
}

func newManager(t *testing.T, api *fakeCloudflare, key []byte) *Manager {
	t.Helper()

	db, err := sqlitedb.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	m, err := NewManager(db, key, 8090)
	if err != nil {
		t.Fatal(err)
	}

	m.APIBase = srv.URL
	m.conn.command = func(ctx context.Context, token string) (*exec.Cmd, error) {
		return nil, errors.New("no cloudflared in tests")
	}
	t.Cleanup(func() { m.conn.run("") })

	return m
}

func TestConfigureCreatesTheTunnelRouteAndRecord(t *testing.T) {
	api := newFakeCloudflare("secret-api-token")
	key := make([]byte, 32)
	m := newManager(t, api, key)

	st, err := m.Configure(context.Background(), "secret-api-token", " https://Example.com/ ", "")
	if err != nil {
		t.Fatal(err)
	}

	if st.URL != "https://decurix.example.com" || m.URL() != st.URL {
		t.Errorf("URL = %q", st.URL)
	}
	if st.Connections != 2 || st.Cloud != "healthy" {
		t.Errorf("status = %+v, want 2 connections, healthy", st)
	}

	ingress := api.ingress["config"].(map[string]any)["ingress"].([]any)
	first := ingress[0].(map[string]any)
	if first["hostname"] != "decurix.example.com" || first["service"] != "http://127.0.0.1:8090" {
		t.Errorf("ingress = %v, want the hostname routed to the gated port only", ingress)
	}
	if last := ingress[len(ingress)-1].(map[string]any); last["service"] != "http_status:404" {
		t.Errorf("ingress does not end in a 404 catch-all: %v", ingress)
	}

	if len(api.records) != 1 || api.records[0].Content != "tun1.cfargotunnel.com" || !api.records[0].Proxied {
		t.Errorf("records = %+v", api.records)
	}

	// Saved sealed, and read back the same.
	var stored string
	m.db.QueryRow(`SELECT api_token FROM cftunnel_config`).Scan(&stored)
	if !sealed.IsSealed(stored) || strings.Contains(stored, "secret-api-token") {
		t.Errorf("the API token is stored as %q", stored)
	}

	again, err := NewManager(m.db, key, 8090)
	if err != nil {
		t.Fatal(err)
	}
	if again.URL() != "https://decurix.example.com" {
		t.Errorf("after a restart URL = %q", again.URL())
	}
}

func TestConfigureTwiceReusesTheTunnel(t *testing.T) {
	api := newFakeCloudflare("tok")
	m := newManager(t, api, nil)

	for i := 0; i < 2; i++ {
		// The second time without the token: the saved one is used.
		token := "tok"
		if i == 1 {
			token = ""
		}
		if _, err := m.Configure(context.Background(), token, "example.com", "work"); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	if len(api.tunnels) != 1 || len(api.records) != 1 {
		t.Errorf("tunnels = %v, records = %v; want one of each", api.tunnels, api.records)
	}
}

func TestSomeoneElsesRecordIsNeverOverwritten(t *testing.T) {
	api := newFakeCloudflare("tok")
	api.records = []dnsRecord{{ID: "r0", Type: "A", Name: "decurix.example.com", Content: "203.0.113.9"}}
	m := newManager(t, api, nil)

	_, err := m.Configure(context.Background(), "tok", "example.com", "decurix")
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	if api.records[0].Content != "203.0.113.9" {
		t.Error("the existing record was changed")
	}
	if m.URL() != "" {
		t.Error("a failed setup was saved")
	}
}

func TestOurOwnOldRecordIsRepointed(t *testing.T) {
	api := newFakeCloudflare("tok")
	api.records = []dnsRecord{{ID: "r0", Type: "CNAME", Name: "decurix.example.com", Content: "old.cfargotunnel.com", Comment: recordComment}}
	m := newManager(t, api, nil)

	if _, err := m.Configure(context.Background(), "tok", "example.com", "decurix"); err != nil {
		t.Fatal(err)
	}
	if api.records[0].Content != "tun1.cfargotunnel.com" {
		t.Errorf("record = %+v, want it repointed", api.records[0])
	}
}

func TestBadSettingsAndBadTokensAreRefused(t *testing.T) {
	api := newFakeCloudflare("tok")
	m := newManager(t, api, nil)

	for _, c := range []struct{ token, domain, sub string }{
		{"", "example.com", ""},
		{"tok", "not a domain", ""},
		{"tok", "example.com", "bad_sub"},
	} {
		if _, err := m.Configure(context.Background(), c.token, c.domain, c.sub); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v, want ErrInvalid", c, err)
		}
	}

	var apiErr *APIError
	if _, err := m.Configure(context.Background(), "wrong", "example.com", ""); !errors.As(err, &apiErr) || apiErr.Message != "Authentication error" {
		t.Errorf("wrong token: err = %v, want Cloudflare's own message", err)
	}

	if _, err := m.Configure(context.Background(), "tok", "other.org", ""); !errors.Is(err, ErrZoneNotFound) {
		t.Errorf("unknown domain: err = %v, want ErrZoneNotFound", err)
	}
}

func TestDisableForgetsTheSetup(t *testing.T) {
	api := newFakeCloudflare("tok")
	m := newManager(t, api, nil)

	if _, err := m.Configure(context.Background(), "tok", "example.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}

	if st := m.Status(context.Background()); st.Configured || m.URL() != "" {
		t.Errorf("after Disable: %+v", st)
	}
}

// fakeCloudflared is a shell stand-in for cloudflared: it reports its token
// and a registered connection, then waits to be killed.
func fakeCloudflared(t *testing.T, seen chan<- string) func(ctx context.Context, token string) (*exec.Cmd, error) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}

	return func(ctx context.Context, token string) (*exec.Cmd, error) {
		seen <- token
		cmd := exec.CommandContext(ctx, "sh", "-c", `echo "2026-01-01T00:00:00Z INF Registered tunnel connection connIndex=0" >&2; exec sleep 60`)
		return cmd, nil
	}
}

func waitState(t *testing.T, c *connector, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := c.state(); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	got, reason := c.state()
	t.Fatalf("connector state = %s (%s), want %s", got, reason, want)
}

func TestTheConnectorRunsSwitchesAndStops(t *testing.T) {
	seen := make(chan string, 4)
	c := newConnector()
	c.command = fakeCloudflared(t, seen)

	c.run("one")
	if got := <-seen; got != "one" {
		t.Fatalf("ran with %q", got)
	}
	waitState(t, c, stateRunning)

	c.run("one") // unchanged: no restart
	select {
	case got := <-seen:
		t.Fatalf("restarted with %q for the same token", got)
	case <-time.After(200 * time.Millisecond):
	}

	c.run("two")
	if got := <-seen; got != "two" {
		t.Fatalf("switched to %q", got)
	}
	waitState(t, c, stateRunning)

	c.run("")
	waitState(t, c, stateStopped)
}

func TestAMissingCloudflaredIsReported(t *testing.T) {
	c := newConnector()
	c.command = func(context.Context, string) (*exec.Cmd, error) { return nil, exec.ErrNotFound }

	c.run("tok")
	defer c.run("")

	waitState(t, c, stateMissing)
}
