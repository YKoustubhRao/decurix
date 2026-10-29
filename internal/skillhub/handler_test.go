package skillhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeAgent stands in for the desktops' agents: per worker, the bundles it
// hands out and what it answers to an install.
type fakeAgent struct {
	bundles   map[string]map[string][]byte // worker -> name -> bundle
	refuse    map[string]string            // worker/name -> 409 reason
	unreached map[string]bool              // worker -> transport failure
	crash     bool                         // answer 500 with internals
	installs  []map[string]any
}

func (f *fakeAgent) AgentRequest(_ context.Context, worker, verb, path string, body io.Reader) (*http.Response, error) {
	if f.unreached[worker] {
		return nil, errors.New("dial tcp 10.0.0.9:8081: secret internals")
	}
	if f.crash {
		return answer(500, `{"status":"error","error":"KeyError: internals"}`), nil
	}

	switch {
	case verb == http.MethodGet && strings.HasPrefix(path, "/skills/") && strings.HasSuffix(path, "/bundle"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/skills/"), "/bundle")
		b, ok := f.bundles[worker][name]
		if !ok {
			return answer(404, `{"status":"error","error":"no skill `+name+`"}`), nil
		}
		return answer(200, string(b)), nil
	case verb == http.MethodPost && path == "/skills/install":
		var got map[string]any
		if err := json.NewDecoder(body).Decode(&got); err != nil {
			return answer(400, `{"error":"bad body"}`), nil
		}
		var b struct {
			Skill struct {
				Name string `json:"name"`
			} `json:"skill"`
		}
		raw, _ := json.Marshal(got["bundle"])
		json.Unmarshal(raw, &b)
		if reason, ok := f.refuse[worker+"/"+b.Skill.Name]; ok && got["replace"] != true {
			return answer(409, `{"status":"error","error":"`+reason+`"}`), nil
		}
		got["worker"] = worker
		f.installs = append(f.installs, got)
		return answer(200, `{"name":"`+b.Skill.Name+`","status":"ready"}`), nil
	}

	return answer(404, `{"error":"not found"}`), nil
}

func answer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func newHandler(t *testing.T, agent *fakeAgent) (*Handler, *http.ServeMux) {
	t.Helper()

	h := &Handler{Store: newStore(t), Agent: agent}
	mux := http.NewServeMux()
	h.Register(mux)

	return h, mux
}

func do(t *testing.T, mux *http.ServeMux, verb, path string, body any) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(verb, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: not JSON: %s", verb, path, rec.Body.String())
	}

	return rec.Code, out
}

func TestPublishInstallAndSync(t *testing.T) {
	agent := &fakeAgent{
		bundles: map[string]map[string][]byte{
			"w1": {"pay-bill": bundle(t, "pay-bill", nil), "draft-one": bundle(t, "draft-one", map[string]any{"status": "draft"})},
		},
		refuse: map[string]string{"w3/pay-bill": "this desktop changed pay-bill since it was installed"},
	}
	h, mux := newHandler(t, agent)

	code, out := do(t, mux, http.MethodPost, "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "pay-bill", "note": "first"})
	if code != 200 || out["created"] != true {
		t.Fatalf("publish = %d %v", code, out)
	}
	code, out = do(t, mux, http.MethodPost, "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "pay-bill"})
	if code != 200 || out["created"] != false {
		t.Fatalf("republish = %d %v", code, out)
	}

	// The publisher holds what it published, so it is not outdated.
	if got, _ := h.Store.Outdated("w1"); len(got) != 0 {
		t.Fatalf("publisher outdated: %+v", got)
	}

	code, out = do(t, mux, http.MethodPost, "/api/skillhub/install", map[string]any{"worker": "w2", "name": "pay-bill"})
	if code != 200 || out["version"] != float64(1) {
		t.Fatalf("install = %d %v", code, out)
	}
	if len(agent.installs) != 1 || agent.installs[0]["sha"] == "" || agent.installs[0]["version"] != float64(1) {
		t.Fatalf("agent saw %+v", agent.installs)
	}

	// Roles drive sync: w3 carries "clerk", which carries pay-bill.
	if code, out = do(t, mux, http.MethodPost, "/api/skillhub/assign", map[string]any{"name": "pay-bill", "role": "clerk"}); code != 200 {
		t.Fatalf("assign = %d %v", code, out)
	}
	if code, out = do(t, mux, http.MethodPost, "/api/skillhub/roles", map[string]any{"worker": "w4", "roles": []string{"clerk"}}); code != 200 {
		t.Fatalf("roles = %d %v", code, out)
	}
	code, out = do(t, mux, http.MethodGet, "/api/skillhub/outdated?worker=w4", nil)
	if code != 200 || len(out["outdated"].([]any)) != 1 {
		t.Fatalf("outdated = %d %v", code, out)
	}
	code, out = do(t, mux, http.MethodPost, "/api/skillhub/sync", map[string]any{"worker": "w4"})
	if code != 200 || len(out["installed"].([]any)) != 1 || len(out["failed"].([]any)) != 0 {
		t.Fatalf("sync = %d %v", code, out)
	}
	if got, _ := h.Store.Outdated("w4"); len(got) != 0 {
		t.Fatalf("still outdated after sync: %+v", got)
	}

	// A desktop that changed the skill locally refuses; sync reports it.
	do(t, mux, http.MethodPost, "/api/skillhub/roles", map[string]any{"worker": "w3", "roles": []string{"clerk"}})
	code, out = do(t, mux, http.MethodPost, "/api/skillhub/sync", map[string]any{"worker": "w3"})
	failed := out["failed"].([]any)
	if code != 200 || len(failed) != 1 || !strings.Contains(failed[0].(map[string]any)["error"].(string), "changed") {
		t.Fatalf("sync with refusal = %d %v", code, out)
	}
	// ...and replace overrides it.
	code, out = do(t, mux, http.MethodPost, "/api/skillhub/install", map[string]any{"worker": "w3", "name": "pay-bill", "replace": true})
	if code != 200 {
		t.Fatalf("replace install = %d %v", code, out)
	}

	code, out = do(t, mux, http.MethodGet, "/api/skillhub", nil)
	skills := out["skills"].([]any)
	if code != 200 || len(skills) != 1 || len(skills[0].(map[string]any)["installs"].([]any)) != 4 {
		t.Fatalf("list = %d %v", code, out)
	}
	if out["workers"].(map[string]any)["w4"] == nil {
		t.Fatalf("list has no worker roles: %v", out)
	}
}

func TestHandlerRefusals(t *testing.T) {
	agent := &fakeAgent{
		bundles: map[string]map[string][]byte{
			"w1": {"draft-one": bundle(t, "draft-one", map[string]any{"status": "draft"}), "liar": bundle(t, "other", nil)},
		},
		unreached: map[string]bool{"gone": true},
	}
	_, mux := newHandler(t, agent)

	cases := []struct {
		name, verb, path string
		body             any
		code             int
		says             string
	}{
		{"publish draft", "POST", "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "draft-one"}, 409, "confirmed"},
		{"publish unknown", "POST", "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "nope"}, 404, "no skill nope"},
		{"publish mismatched", "POST", "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "liar"}, 400, "different skill"},
		{"publish bad name", "POST", "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "../etc"}, 400, "skill name"},
		{"publish no worker", "POST", "/api/skillhub/publish", map[string]any{"name": "x"}, 400, "worker"},
		{"unreachable", "POST", "/api/skillhub/publish", map[string]any{"worker": "gone", "name": "x"}, 502, "could not"},
		{"install unknown", "POST", "/api/skillhub/install", map[string]any{"worker": "w1", "name": "nope"}, 404, "registry"},
		{"assign unknown", "POST", "/api/skillhub/assign", map[string]any{"name": "nope", "role": "clerk"}, 404, "registry"},
		{"assign bad role", "POST", "/api/skillhub/assign", map[string]any{"name": "nope", "role": ""}, 400, "role"},
		{"roles bad worker", "POST", "/api/skillhub/roles", map[string]any{"worker": "", "roles": []string{}}, 400, "worker"},
		{"outdated no worker", "GET", "/api/skillhub/outdated", nil, 400, "worker"},
		{"list wrong method", "POST", "/api/skillhub", map[string]any{}, 405, ""},
		{"publish wrong method", "GET", "/api/skillhub/publish", nil, 405, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := do(t, mux, c.verb, c.path, c.body)
			if code != c.code {
				t.Fatalf("code = %d, want %d (%v)", code, c.code, out)
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, c.says) || strings.Contains(msg, "internals") {
				t.Fatalf("error = %q, want it to say %q and nothing internal", msg, c.says)
			}
		})
	}
}

// A 5xx from a desktop carries an exception's text; it never reaches the client.
func TestAgentCrashIsNotEchoed(t *testing.T) {
	_, mux := newHandler(t, &fakeAgent{crash: true})

	code, out := do(t, mux, http.MethodPost, "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "x"})
	if code != http.StatusBadGateway || strings.Contains(out["error"].(string), "KeyError") {
		t.Fatalf("crash = %d %v", code, out)
	}
}

// An oversized answer from a desktop is refused, not stored.
func TestOversizedBundleRefused(t *testing.T) {
	huge := []byte(`{"format":"decurix-skill/1","skill":{"name":"big","status":"ready","pad":"` + strings.Repeat("a", MaxBundleBytes) + `"}}`)
	h, mux := newHandler(t, &fakeAgent{bundles: map[string]map[string][]byte{"w1": {"big": huge}}})

	code, _ := do(t, mux, http.MethodPost, "/api/skillhub/publish", map[string]any{"worker": "w1", "name": "big"})
	if code != http.StatusBadGateway {
		t.Fatalf("code = %d", code)
	}
	if list, _ := h.Store.List(); len(list) != 0 {
		t.Fatalf("stored an oversized bundle: %+v", list)
	}
}

// The routes also take a plain form, as the dashboard's other forms send.
func TestFormBodies(t *testing.T) {
	h, mux := newHandler(t, &fakeAgent{})

	form := url.Values{"worker": {"w1"}, "roles": {"clerk, auditor"}}
	req := httptest.NewRequest(http.MethodPost, "/api/skillhub/roles", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("form roles = %d %s", rec.Code, rec.Body.String())
	}
	if roles, _ := h.Store.WorkerRoles("w1"); strings.Join(roles, ",") != "auditor,clerk" {
		t.Fatalf("roles = %v", roles)
	}
}
