package winhost

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenBindsTheSessionTheProgramAndItsArguments(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok := Token(secret, "vm1-chrome", `C:\a\chrome.exe`, "--x")
	if !ValidToken(secret, "vm1-chrome", `C:\a\chrome.exe`, "--x", tok) {
		t.Fatal("the token for a launch does not validate that launch")
	}
	for name, ok := range map[string]bool{
		"another session id":   ValidToken(secret, "vm2-chrome", `C:\a\chrome.exe`, "--x", tok),
		"another program":      ValidToken(secret, "vm1-chrome", `C:\a\cmd.exe`, "--x", tok),
		"other arguments":      ValidToken(secret, "vm1-chrome", `C:\a\chrome.exe`, "--y", tok),
		"another secret":       ValidToken([]byte("ffffffffffffffffffffffffffffffff"), "vm1-chrome", `C:\a\chrome.exe`, "--x", tok),
		"an empty token":       ValidToken(secret, "vm1-chrome", `C:\a\chrome.exe`, "--x", ""),
		"no secret configured": ValidToken(nil, "vm1-chrome", `C:\a\chrome.exe`, "--x", tok),
	} {
		if ok {
			t.Errorf("a token was accepted for %s", name)
		}
	}
	// Field boundaries matter: moving a character across one must not give the same token.
	if Token(secret, "a", "bc", "") == Token(secret, "ab", "c", "") {
		t.Error("tokens do not separate their fields")
	}
}

func TestSecretIsCreatedOnceAndKeptPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadSecret(dir)
	if err != nil || len(first) != 32 {
		t.Fatalf("LoadSecret = %d bytes, %v", len(first), err)
	}
	again, err := LoadSecret(dir)
	if err != nil || string(again) != string(first) {
		t.Fatal("the key changed between loads: every launcher already written would stop working")
	}
	if fi, err := os.Stat(filepath.Join(dir, secretFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	other, _ := LoadSecret(t.TempDir())
	if string(other) == string(first) {
		t.Fatal("two installs share a key")
	}
}
