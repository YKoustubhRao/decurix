package lanaccess

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is an in-memory DeviceRegistry.
type fakeRegistry struct {
	mu     sync.Mutex
	tokens map[string]string // token -> name
	n      int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{tokens: map[string]string{}}
}

func (f *fakeRegistry) Pair(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.n++
	token := "tok-" + strings.Repeat("x", f.n)
	f.tokens[token] = name

	return token, nil
}

func (f *fakeRegistry) Valid(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.tokens[token]
	return ok
}

func (f *fakeRegistry) Forget(token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.tokens, token)
	return nil
}

func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return c
		}
	}

	return nil
}

// getWith GETs path on srv carrying only cookie, and reports whether the
// protected handler (not the gate) answered.
func getWith(t *testing.T, srv *httptest.Server, cookie *http.Cookie) bool {
	t.Helper()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)

	return strings.HasPrefix(string(body[:n]), "protected:")
}

func TestARememberedDeviceSurvivesRotation(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp, err := noRedirectClient().PostForm(srv.URL+loginPath, map[string][]string{
		"code":        {store.Password()},
		"next":        {"/"},
		"remember":    {"1"},
		"device_name": {"Pixel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	device := cookieNamed(resp, deviceCookie)
	session := cookieNamed(resp, sessionCookie)
	if device == nil || session == nil {
		t.Fatalf("want both a session and a device cookie, got %v", resp.Cookies())
	}
	if registry.tokens[device.Value] != "Pixel" {
		t.Errorf("paired under %q, want Pixel", registry.tokens[device.Value])
	}

	store.Rotate()

	if getWith(t, srv, session) {
		t.Error("an ordinary session survived rotation")
	}
	if !getWith(t, srv, device) {
		t.Error("a remembered device was signed out by rotation")
	}

	// Revoked: the gate again.
	registry.Forget(device.Value)
	if getWith(t, srv, device) {
		t.Error("a revoked device still gets in")
	}
}

func TestWithoutRememberNoDeviceIsPaired(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp := login(t, srv, store.Password(), "/")
	resp.Body.Close()

	if cookieNamed(resp, deviceCookie) != nil || len(registry.tokens) != 0 {
		t.Fatal("a device was paired without asking")
	}
}

func TestAWrongCodeNeverPairs(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	resp, err := noRedirectClient().PostForm(srv.URL+loginPath, map[string][]string{
		"code": {"wrong"}, "next": {"/"}, "remember": {"1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(registry.tokens) != 0 {
		t.Fatal("a wrong code paired a device")
	}
}

func TestAPairingLinkOnlyPairsWhenThePageIsSubmitted(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	// What a chat app's link preview, or a browser prefetch, does: GET it.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+authPath+"?code="+store.Password()+"&next=/&remember=1", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14; Pixel 8) Mobile Safari/537.36")

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 8192)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()

	if len(registry.tokens) != 0 || cookieNamed(resp, deviceCookie) != nil || cookieNamed(resp, sessionCookie) != nil {
		t.Fatal("opening a pairing link paired or signed in without the person pressing Pair")
	}
	page := string(body[:n])
	if !strings.Contains(page, "Pair this device") || !strings.Contains(page, `value="Android phone"`) {
		t.Fatalf("the pairing page is not shown with the device's name: %s", page)
	}

	// The person presses Pair: the page posts the code with remember.
	resp, err = noRedirectClient().PostForm(srv.URL+loginPath, map[string][]string{
		"code": {store.Password()}, "next": {"/"}, "remember": {"1"}, "device_name": {"Android phone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	device := cookieNamed(resp, deviceCookie)
	if device == nil || registry.tokens[device.Value] != "Android phone" {
		t.Fatalf("pressing Pair did not pair: %v", registry.tokens)
	}
}

func TestAPairedDeviceIsNotPairedTwice(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	token, _ := registry.Pair("Android phone")

	req, _ := http.NewRequest(http.MethodPost, srv.URL+loginPath, strings.NewReader(
		"code="+store.Password()+"&next=/&remember=1&device_name=Android+phone"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: deviceCookie, Value: token})

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(registry.tokens) != 1 {
		t.Fatalf("the same phone is listed %d times", len(registry.tokens))
	}
}

func TestForgetUnpairsTheDeviceAndSignsItOut(t *testing.T) {
	store := NewStore()
	registry := newFakeRegistry()
	store.SetDevices(registry)

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	token, _ := registry.Pair("phone")
	device := &http.Cookie{Name: deviceCookie, Value: token}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+forgetPath, nil)
	req.AddCookie(device)

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if registry.Valid(token) {
		t.Fatal("forget left the device paired")
	}
	if getWith(t, srv, device) {
		t.Fatal("a forgotten device still gets in")
	}
}

func TestTheGateOffersPairingOnlyWithARegistry(t *testing.T) {
	for _, withRegistry := range []bool{false, true} {
		store := NewStore()
		if withRegistry {
			store.SetDevices(newFakeRegistry())
		}

		srv := httptest.NewServer(store.Middleware(protectedHandler()))

		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}

		body := make([]byte, 8192)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		srv.Close()

		if got := strings.Contains(string(body[:n]), "Remember this device"); got != withRegistry {
			t.Errorf("registry=%v: offers pairing = %v", withRegistry, got)
		}
	}
}

func TestDeviceName(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) Mobile Safari/537.36": "Android phone",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0)":                  "Mac",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)":                     "Windows PC",
		"curl/8.0": "Device",
	}

	for ua, want := range cases {
		if got := DeviceName(ua); got != want {
			t.Errorf("DeviceName(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestOpenPathsSkipTheGateAndNothingElseDoes(t *testing.T) {
	store := NewStore()
	store.SetOpenPaths("/phone/setup.sh")

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	for path, open := range map[string]bool{"/phone/setup.sh": true, "/phone/setup.sh/x": false, "/": false} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()

		if got := strings.HasPrefix(string(body[:n]), "protected:"); got != open {
			t.Errorf("%s: reached the handler = %v, want %v", path, got, open)
		}
	}
}

func TestAPairedAndroidPhoneIsSentWhereAfterPairSays(t *testing.T) {
	store := NewStore()
	store.SetDevices(newFakeRegistry())
	store.SetAfterPair(func(ua string) string {
		if strings.Contains(ua, "Android") {
			return "/phone/link"
		}
		return ""
	})

	srv := httptest.NewServer(store.Middleware(protectedHandler()))
	defer srv.Close()

	for ua, want := range map[string]string{"Mozilla/5.0 (Linux; Android 15)": "/phone/link", "Mozilla/5.0 (iPhone)": "/"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+loginPath, strings.NewReader(
			"code="+store.Password()+"&next=/&remember=1&device_name=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", ua)

		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if loc := resp.Header.Get("Location"); loc != want {
			t.Errorf("%s: sent to %q, want %q", ua, loc, want)
		}
	}
}
