package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/lanaccess"
)

func TestAPhoneSetsItsLinkUpFromTheSetupToken(t *testing.T) {
	s, srv, token, dev := phoneServer(t)

	bin := filepath.Join(t.TempDir(), PhoneBinaryName)
	os.WriteFile(bin, []byte("phone build"), 0o755)
	t.Setenv("DECURIX_PHONE_BINARY", bin)

	// Only a paired device gets a setup token.
	resp, _ := http.Post(srv.URL+"/api/phones/setup-token", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}

	// Without accepting the warning: refused.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/phones/setup-token", nil)
	req.AddCookie(&http.Cookie{Name: phoneDeviceCookie, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a setup token without accepting the warning: %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/phones/setup-token", strings.NewReader("understood=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: phoneDeviceCookie, Value: token})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var issued struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()
	if issued.Token == "" {
		t.Fatal("no setup token")
	}

	// The script names this control plane and the token, and nothing a URL could inject.
	base := "https://abc.trycloudflare.com"
	resp, _ = http.Get(srv.URL + "/phone/setup.sh?t=" + issued.Token + "&b=" + url.QueryEscape(base))
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(script), "BASE='"+base+"'") || !strings.Contains(string(script), "--phone-setup") {
		t.Fatalf("script:\n%s", script)
	}
	resp, _ = http.Get(srv.URL + "/phone/setup.sh?t=" + issued.Token + "&b=" + url.QueryEscape("https://x.com/'$(id)'"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an injected base was accepted: %d", resp.StatusCode)
	}

	// The binary, to the token only.
	resp, _ = http.Get(srv.URL + "/phone/decurix?t=" + issued.Token)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "phone build" {
		t.Fatalf("binary = %q", body)
	}
	resp, _ = http.Get(srv.URL + "/phone/decurix?t=wrong")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a wrong token got the binary: %d", resp.StatusCode)
	}

	// The claim: a sign-in for the same device, once.
	resp, _ = http.PostForm(srv.URL+"/api/phones/claim", url.Values{"t": {issued.Token}})
	var claimed struct{ Token, Name string }
	json.NewDecoder(resp.Body).Decode(&claimed)
	resp.Body.Close()
	if d, ok := s.Devices.Lookup(claimed.Token); !ok || d.ID != dev.ID || claimed.Name != "Pixel 8" {
		t.Fatalf("the claimed sign-in is %+v, %v - want the same device", d, ok)
	}
	if list, _ := s.Devices.List(); len(list) != 1 {
		t.Errorf("the phone is listed %d times", len(list))
	}

	resp, _ = http.PostForm(srv.URL+"/api/phones/claim", url.Values{"t": {issued.Token}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a setup token worked twice: %d", resp.StatusCode)
	}
	resp, _ = http.Get(srv.URL + "/phone/setup.sh?t=" + issued.Token + "&b=" + url.QueryEscape(base))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a spent token still serves the script: %d", resp.StatusCode)
	}
}

func TestOnlyAnAndroidPhoneIsOfferedItsLink(t *testing.T) {
	s := &Server{}
	if s.PhoneLinkAfterPair("Mozilla/5.0 (Linux; Android 15)") != "" {
		t.Error("offered with phone links off")
	}

	s, _, _, _ = phoneServer(t)
	if got := s.PhoneLinkAfterPair("Mozilla/5.0 (Linux; Android 15)"); got != "/phone/link" {
		t.Errorf("Android: %q", got)
	}
	if got := s.PhoneLinkAfterPair("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0)"); got != "" {
		t.Errorf("iPhone: %q", got)
	}
}

func TestThePhoneLinkPageSaysWhatItCanDo(t *testing.T) {
	_, srv, token, _ := phoneServer(t)

	get := func(withCookie bool) string {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/phone/link", nil)
		if withCookie {
			req.AddCookie(&http.Cookie{Name: phoneDeviceCookie, Value: token})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if page := get(false); !strings.Contains(page, "Open the pairing link") {
		t.Errorf("without a pairing:\n%s", page)
	}

	t.Setenv("DECURIX_PHONE_BINARY", filepath.Join(t.TempDir(), "missing"))
	if page := get(true); !strings.Contains(page, "built without the phone helper") {
		t.Errorf("without the phone build:\n%s", page)
	}

	bin := filepath.Join(t.TempDir(), PhoneBinaryName)
	os.WriteFile(bin, []byte("x"), 0o755)
	t.Setenv("DECURIX_PHONE_BINARY", bin)
	if page := get(true); !strings.Contains(page, "Yes, set it up") || !strings.Contains(page, "Pixel 8") ||
		!strings.Contains(page, "open and use any app on this phone") || !strings.Contains(page, "Send, Pay and Buy are blocked until you approve them") ||
		!strings.Contains(page, "Picture only") ||
		!strings.Contains(page, "S.browser_fallback_url=") || !strings.Contains(page, `id="yes" type="button" disabled`) {
		t.Errorf("ready:\n%s", page)
	}
}

// TestThePhoneSetupWorksThroughTheGate wires the gated listener the way
// cmd/decurix/main.go does and walks the whole setup as a phone would: pair
// in the browser, get sent to /phone/link, get a token, then fetch the script,
// the binary and the claim with no cookie at all (Termux has none).
func TestThePhoneSetupWorksThroughTheGate(t *testing.T) {
	s, _, _, _ := phoneServer(t)

	bin := filepath.Join(t.TempDir(), PhoneBinaryName)
	os.WriteFile(bin, []byte("phone build"), 0o755)
	t.Setenv("DECURIX_PHONE_BINARY", bin)

	gate := lanaccess.NewStore()
	gate.SetDevices(s.Devices)
	gate.SetAfterPair(s.PhoneLinkAfterPair)
	gate.SetOpenPaths(PhoneSetupOpenPaths...)

	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(gate.Middleware(GzipMiddleware(mux)))
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	phone := &http.Client{Jar: jar}

	// The pairing link opens the "Pair this device" page; pressing Pair posts the form.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/__decurix_access/login", strings.NewReader(url.Values{
		"code": {gate.Password()}, "next": {"/"}, "remember": {"1"}, "device_name": {"Pixel 9"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 15; Pixel 9) Mobile Safari/537.36")
	resp, err := phone.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Request.URL.Path != "/phone/link" || !strings.Contains(string(page), "Yes, set it up") {
		t.Fatalf("after pairing the phone is at %s:\n%s", resp.Request.URL.Path, page)
	}

	resp, err = phone.Post(srv.URL+"/api/phones/setup-token", "application/x-www-form-urlencoded", strings.NewReader("understood=1"))
	if err != nil {
		t.Fatal(err)
	}
	var issued struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()
	if issued.Token == "" {
		t.Fatalf("no setup token (%d)", resp.StatusCode)
	}

	// Termux: no cookie.
	termux := &http.Client{}
	for _, path := range []string{
		"/phone/setup.sh?t=" + issued.Token + "&b=" + url.QueryEscape(srv.URL),
		"/phone/decurix?t=" + issued.Token,
	} {
		resp, err := termux.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "Enter the access code") {
			t.Fatalf("%s through the gate: %d\n%s", path, resp.StatusCode, body)
		}
	}

	resp, err = termux.PostForm(srv.URL+"/api/phones/claim", url.Values{"t": {issued.Token}})
	if err != nil {
		t.Fatal(err)
	}
	var claimed struct{ Token string }
	json.NewDecoder(resp.Body).Decode(&claimed)
	resp.Body.Close()
	if claimed.Token == "" {
		t.Fatalf("claim through the gate: %d", resp.StatusCode)
	}

	// With what it claimed, the phone link reaches its own route past the gate.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/phones", nil)
	req.AddCookie(&http.Cookie{Name: phoneDeviceCookie, Value: claimed.Token})
	resp, err = termux.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("the claimed sign-in did not get past the gate: %s", resp.Header.Get("Content-Type"))
	}

	// And nothing else is open to a stranger.
	resp, _ = termux.Get(srv.URL + "/api/desktops")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Enter the access code") {
		t.Fatal("the gate let a stranger through to /api/desktops")
	}
}

func TestTermuxIsOneTapFromTheLatestOfficialRelease(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"assets":[
			{"name":"termux-app_v9+github-debug_universal.apk","browser_download_url":"https://github.com/termux/termux-app/releases/download/v9/u.apk"},
			{"name":"termux-app_v9+github-debug_arm64-v8a.apk","browser_download_url":"https://github.com/termux/termux-app/releases/download/v9/termux-app_v9%2Bgithub-debug_arm64-v8a.apk"}]}`))
	}))
	defer api.Close()

	old := termuxReleasesAPI
	termuxReleasesAPI = api.URL
	defer func() { termuxReleasesAPI = old }()

	s := &Server{}
	rec := httptest.NewRecorder()
	s.phoneTermuxHandler(rec, httptest.NewRequest(http.MethodGet, "/phone/termux.apk", nil))
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "arm64-v8a.apk") || rec.Code != http.StatusFound {
		t.Fatalf("sent to %q (%d), want the arm64 installer", loc, rec.Code)
	}

	// GitHub unreachable: the pinned official installer, never nothing.
	termuxReleasesAPI = "http://127.0.0.1:1/none"
	s = &Server{}
	rec = httptest.NewRecorder()
	s.phoneTermuxHandler(rec, httptest.NewRequest(http.MethodGet, "/phone/termux.apk", nil))
	if loc := rec.Header().Get("Location"); loc != termuxFallbackAPK {
		t.Fatalf("with GitHub down, sent to %q", loc)
	}
}
