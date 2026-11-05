package phoneagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths on the gated listener the pairing link and its form use
// (internal/lanaccess).
const (
	authPath  = "/__decurix_access/authenticate"
	loginPath = "/__decurix_access/login"
)

// Saved is what a phone keeps between runs: where the control plane is and
// the token it was paired with.
type Saved struct {
	Base  string `json:"base"`
	Token string `json:"token"`
	Name  string `json:"name"`
}

// SavedFile is the file in the agent's directory that holds Saved.
const SavedFile = "link.json"

// ParseLink reads a pairing link (…/__decurix_access/authenticate?code=…),
// returning the control plane's address and the access code in it.
func ParseLink(link string) (base, code string, err error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", errors.New("that is not a Decurix pairing link (copy it from the Mobile menu)")
	}

	code = u.Query().Get("code")
	if u.Path != authPath || code == "" {
		return "", "", errors.New("that link has no access code in it; copy the pairing link from the Mobile menu")
	}

	return u.Scheme + "://" + u.Host, code, nil
}

// Pair signs in at base with the access code, pairing this phone as name,
// and returns its device token - the same form a person submits on the
// "Pair this device" page.
func Pair(ctx context.Context, base, code, name string) (string, error) {
	form := url.Values{"code": {code}, "next": {"/"}, "remember": {"1"}, "device_name": {name}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+loginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reaching %s: %w", base, err)
	}
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name == DeviceCookie && c.Value != "" {
			return c.Value, nil
		}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", errors.New("too many wrong codes were tried from here; wait a few minutes")
	}

	return "", errors.New("the access code in that link was not accepted (it changes every 48 hours, and with New code); copy a fresh pairing link")
}

// Load reads the saved link from dir.
func Load(dir string) (Saved, error) {
	var s Saved

	raw, err := os.ReadFile(filepath.Join(dir, SavedFile))
	if err != nil {
		return s, err
	}

	return s, json.Unmarshal(raw, &s)
}

// Save writes s to dir, readable by this user only (the token is a sign-in).
func Save(dir string, s Saved) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, SavedFile), raw, 0o600)
}

// SameControlPlane reports whether a pairing link names the control plane
// the saved link was paired with (scheme and host alike). Only then may the
// saved token be sent to it: a link that names another address is paired
// with the code it carries, so a sign-in never goes to a host it was not made
// for, nor from https down to http (review 2026-11-03, B-18).
func SameControlPlane(saved Saved, base string) bool {
	return saved.Token != "" && saved.Base != "" && strings.EqualFold(strings.TrimRight(saved.Base, "/"), strings.TrimRight(base, "/"))
}

// Accepted reports whether the control plane at base still accepts token -
// how a phone given a new link (the tunnel's address changed) keeps its
// pairing instead of being listed a second time. The gate answers an
// unknown token with its code page, not the JSON list asked for.
func Accepted(ctx context.Context, base, token string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/phones", nil)
	if err != nil {
		return false
	}
	req.AddCookie(&http.Cookie{Name: DeviceCookie, Value: token})

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json")
}
