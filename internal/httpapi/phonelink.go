package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/webui"
)

// Setting up a phone link from the phone itself. An Android phone that pairs
// (the gate's "Pair this device") is sent to /phone/link, which asks whether
// to use its apps in VMs; on yes it gets one command to paste into Termux.
// That command fetches /phone/setup.sh, which installs ADB, downloads the
// phone build of this binary from this control plane (/phone/decurix) and
// runs `decurix --phone-setup`, which claims its own sign-in
// (/api/phones/claim) and links. Termux has no browser cookie, so those
// three paths are let past the gate (PhoneSetupOpenPaths) and each checks a
// one-time setup token instead: issued to a paired device, good for
// phoneSetupTTL, spent by the claim.

// PhoneSetupOpenPaths are the paths the gate lets through for the phone
// setup; every one of them checks a setup token itself.
var PhoneSetupOpenPaths = []string{"/phone/setup.sh", "/phone/decurix", "/api/phones/claim"}

// phoneSetupTTL is how long a setup token works.
const phoneSetupTTL = 15 * time.Minute

// PhoneBinaryName is the phone build of decurix that /phone/decurix serves,
// looked for beside the running binary (the Docker image puts it there).
const PhoneBinaryName = "decurix-android-arm64"

type phoneSetup struct {
	deviceID string
	name     string
	expires  time.Time
}

type phoneSetups struct {
	mu     sync.Mutex
	tokens map[string]phoneSetup
}

// issue makes a setup token for device.
func (p *phoneSetups) issue(deviceID, name string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.tokens == nil {
		p.tokens = make(map[string]phoneSetup)
	}
	now := time.Now()
	for t, s := range p.tokens {
		if now.After(s.expires) {
			delete(p.tokens, t)
		}
	}
	p.tokens[token] = phoneSetup{deviceID: deviceID, name: name, expires: now.Add(phoneSetupTTL)}

	return token, nil
}

// check returns the setup token's device while it is valid; spend also uses it up.
func (p *phoneSetups) check(token string, spend bool) (phoneSetup, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for t, s := range p.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			if time.Now().After(s.expires) {
				delete(p.tokens, t)
				return phoneSetup{}, false
			}
			if spend {
				delete(p.tokens, t)
			}
			return s, true
		}
	}

	return phoneSetup{}, false
}

// PhoneLinkAfterPair is where a newly paired device goes (lanaccess.SetAfterPair):
// an Android phone is asked whether to link its apps.
func (s *Server) PhoneLinkAfterPair(userAgent string) string {
	if s.Phones != nil && strings.Contains(userAgent, "Android") {
		return "/phone/link"
	}

	return ""
}

// phoneBinaryPath finds the phone build of decurix: DECURIX_PHONE_BINARY, or
// PhoneBinaryName beside the running executable. "" when there is none.
func phoneBinaryPath() string {
	if p := os.Getenv("DECURIX_PHONE_BINARY"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return ""
	}

	p := filepath.Join(filepath.Dir(exe), PhoneBinaryName)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}

	return ""
}

// phoneLinkPageHandler asks a paired phone whether to use its apps in VMs,
// and walks it through the setup.
func (s *Server) phoneLinkPageHandler(w http.ResponseWriter, r *http.Request) {
	data := webui.PhoneLinkPageData{BinaryReady: phoneBinaryPath() != "", Enabled: s.Phones != nil}

	if s.Devices != nil {
		if c, err := r.Cookie(phoneDeviceCookie); err == nil {
			if d, ok := s.Devices.Lookup(c.Value); ok {
				data.DeviceID, data.Name = d.ID, d.Name
			}
		}
	}

	var out bytes.Buffer
	if err := s.Web.RenderPhoneLink(&out, data); err != nil {
		jsonError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
}

// phoneSetupTokenHandler issues the one-time token the setup command carries,
// to a paired device only.
func (s *Server) phoneSetupTokenHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Phones == nil || s.Devices == nil {
		jsonStatusError(w, http.StatusNotFound, "phone links are not available")
		return
	}

	c, err := r.Cookie(phoneDeviceCookie)
	if err != nil {
		jsonStatusError(w, http.StatusUnauthorized, "pair this phone first")
		return
	}
	d, ok := s.Devices.Lookup(c.Value)
	if !ok {
		jsonStatusError(w, http.StatusForbidden, "this phone's pairing is not known here")
		return
	}

	// The page asks the person to accept what linking hands over (full
	// control of the phone's apps, which the agents' approval check cannot
	// see into) before it asks for a token; a request without that is refused.
	if r.FormValue("understood") != "1" {
		jsonStatusError(w, http.StatusBadRequest, "read and accept the warning on the phone link page first")
		return
	}

	token, err := s.phoneSetup.issue(d.ID, d.Name)
	if err != nil {
		jsonError(w, err)
		return
	}

	jsonResponse(w, map[string]any{"token": token, "expires_in": int(phoneSetupTTL.Seconds())})
}

// setupBase is the control plane's address as the phone reached it, given by
// the page (b) and checked to be a plain http(s) origin.
func setupBase(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", false
	}
	if strings.ContainsAny(u.Host, "'\"\\ $`") {
		return "", false
	}

	return u.Scheme + "://" + u.Host, true
}

// phoneSetupScriptHandler serves the shell script the pasted command runs in
// Termux. It installs ADB, downloads the phone build, and hands over to it.
func (s *Server) phoneSetupScriptHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	token := q.Get("t")

	if _, ok := s.phoneSetup.check(token, false); !ok {
		http.Error(w, "echo 'This setup link has expired. Open the phone link page again for a new one.'; exit 1\n", http.StatusForbidden)
		return
	}
	if !hexToken(token) {
		http.Error(w, "bad token", http.StatusBadRequest)
		return
	}

	base, ok := setupBase(q.Get("b"))
	if !ok {
		http.Error(w, "echo 'The setup link is damaged. Open the phone link page again.'; exit 1\n", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `#!/data/data/com.termux/files/usr/bin/sh
# Decurix phone link setup (served by your Decurix; see /phone/link there).
set -e
BASE='%s'
T='%s'
BIN="$HOME/.local/bin"

echo "Decurix: setting up this phone. This takes a minute or two."
if ! command -v adb >/dev/null 2>&1; then
    echo "Installing adb..."
    # yes: a fresh Termux can stop to ask about a package's settings file.
    yes | pkg install android-tools >/dev/null 2>&1 || { yes | pkg update && yes | pkg install android-tools; }
fi

echo "Downloading the Decurix phone helper from your Decurix..."
mkdir -p "$BIN"
curl -fL --progress-bar "$BASE/phone/decurix?t=$T" -o "$BIN/decurix.part"
mv "$BIN/decurix.part" "$BIN/decurix"
chmod +x "$BIN/decurix"

# The rest asks for the pairing code, so it reads the keyboard, not this script.
exec "$BIN/decurix" --phone-setup "$BASE" "$T" < /dev/tty
`, base, token)
}

// hexToken reports whether t is shaped like a setup token (it goes into a script).
func hexToken(t string) bool {
	if len(t) != 48 {
		return false
	}
	_, err := hex.DecodeString(t)
	return err == nil
}

// phoneBinaryHandler serves the phone build of decurix to a valid setup token.
func (s *Server) phoneBinaryHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.phoneSetup.check(r.URL.Query().Get("t"), false); !ok {
		http.Error(w, "this setup link has expired", http.StatusForbidden)
		return
	}

	path := phoneBinaryPath()
	if path == "" {
		http.Error(w, "this Decurix was built without the phone helper ("+PhoneBinaryName+")", http.StatusNotFound)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		jsonError(w, err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		jsonError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, PhoneBinaryName, fi.ModTime(), f)
}

// phoneClaimHandler spends a setup token for a sign-in of the phone link's
// own, on the same paired device as the browser that asked for the token.
func (s *Server) phoneClaimHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Devices == nil {
		jsonStatusError(w, http.StatusNotFound, "phone links are not available")
		return
	}

	setup, ok := s.phoneSetup.check(r.FormValue("t"), true)
	if !ok {
		jsonStatusError(w, http.StatusForbidden, "the setup link has expired or was used already")
		return
	}

	token, err := s.Devices.AddToken(setup.deviceID)
	if err != nil {
		jsonStatusError(w, http.StatusForbidden, "this phone was unpaired since the setup link was made")
		return
	}

	jsonResponse(w, map[string]any{"token": token, "name": setup.name})
}

// termuxFallbackAPK is the Termux installer to send a phone to when GitHub's
// release list cannot be read: the official build for 64-bit ARM phones
// (nearly every Android phone in use), from Termux's own GitHub releases.
const termuxFallbackAPK = "https://github.com/termux/termux-app/releases/download/v0.118.3/termux-app_v0.118.3%2Bgithub-debug_arm64-v8a.apk"

// termuxReleasesAPI is where the latest Termux release is read.
var termuxReleasesAPI = "https://api.github.com/repos/termux/termux-app/releases/latest"

type termuxLatest struct {
	mu      sync.Mutex
	url     string
	fetched time.Time
}

// apk returns the latest official 64-bit ARM Termux installer's address,
// looked up at most every 6 hours, or termuxFallbackAPK.
func (t *termuxLatest) apk(ctx context.Context) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.url != "" && time.Since(t.fetched) < 6*time.Hour {
		return t.url
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, termuxReleasesAPI, nil)
	if err != nil {
		return termuxFallbackAPK
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return termuxFallbackAPK
	}
	defer resp.Body.Close()

	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release) != nil {
		return termuxFallbackAPK
	}

	for _, a := range release.Assets {
		if strings.HasSuffix(a.Name, "arm64-v8a.apk") && strings.HasPrefix(a.URL, "https://github.com/termux/termux-app/releases/download/") {
			t.url, t.fetched = a.URL, time.Now()
			return a.URL
		}
	}

	return termuxFallbackAPK
}

// phoneTermuxHandler sends a phone straight to the Termux installer: one tap,
// where F-Droid first needs its catalogue downloaded before search finds
// anything, and the Play Store copy cannot run the phone link.
func (s *Server) phoneTermuxHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.termux.apk(r.Context()), http.StatusFound)
}
