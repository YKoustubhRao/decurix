package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The setup token comes with the hash of the script it fetches, and the
// script checks the helper it downloads against the phone build's hash before
// running it (review 2026-11-03, W-13).
func TestThePhoneSetupScriptAndHelperArePinned(t *testing.T) {
	_, srv, token, _ := phoneServer(t)

	bin := filepath.Join(t.TempDir(), PhoneBinaryName)
	if err := os.WriteFile(bin, []byte("phone build"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECURIX_PHONE_BINARY", bin)

	base := "http://192.168.1.20:8090"
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/phones/setup-token",
		strings.NewReader(url.Values{"understood": {"1"}, "b": {base}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: phoneDeviceCookie, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Token        string `json:"token"`
		ScriptSHA256 string `json:"script_sha256"`
	}
	json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()
	if issued.Token == "" || len(issued.ScriptSHA256) != 64 {
		t.Fatalf("issued %+v", issued)
	}

	resp, err = http.Get(srv.URL + "/phone/setup.sh?t=" + issued.Token + "&b=" + url.QueryEscape(base))
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	sum := sha256.Sum256(script)
	if hex.EncodeToString(sum[:]) != issued.ScriptSHA256 {
		t.Fatal("the script served is not the one whose hash the page was given")
	}

	helper := sha256.Sum256([]byte("phone build"))
	text := string(script)
	check := strings.Index(text, "sha256sum -c")
	run := strings.Index(text, `mv "$BIN/decurix.part" "$BIN/decurix"`)
	if !strings.Contains(text, "HELPER_SHA256='"+hex.EncodeToString(helper[:])+"'") || check < 0 || run < check {
		t.Fatalf("the script does not check the helper before keeping it:\n%s", text)
	}
}
