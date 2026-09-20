package winhost

import (
	"os"
	"path/filepath"
	"runtime"
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
	// Windows has no mode bits to check: there a file is private through the ACL its folder gives it (the data
	// folder is under the user's own profile), not through 0600.
	if fi, err := os.Stat(filepath.Join(dir, secretFile)); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	other, _ := LoadSecret(t.TempDir())
	if string(other) == string(first) {
		t.Fatal("two installs share a key")
	}
}

func TestExecTokenBindsDesktopProgramAndEveryArgument(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok := ExecToken(secret, "vm1", "echo", []string{"a b"})
	if !ValidExecToken(secret, "vm1", "echo", []string{"a b"}, tok) {
		t.Fatal("the token for a command does not validate that command")
	}
	for name, ok := range map[string]bool{
		"another desktop":       ValidExecToken(secret, "vm2", "echo", []string{"a b"}, tok),
		"another program":       ValidExecToken(secret, "vm1", "cat", []string{"a b"}, tok),
		"split arguments":       ValidExecToken(secret, "vm1", "echo", []string{"a", "b"}, tok),
		"an appended argument":  ValidExecToken(secret, "vm1", "echo", []string{"a b", "; id"}, tok),
		"no arguments":          ValidExecToken(secret, "vm1", "echo", nil, tok),
		"an empty token":        ValidExecToken(secret, "vm1", "echo", []string{"a b"}, ""),
		"another secret":        ValidExecToken([]byte("ffffffffffffffffffffffffffffffff"), "vm1", "echo", []string{"a b"}, tok),
		"a host-gui launch tok": ValidToken(secret, "vm1", "echo", `["a b"]`, tok),
	} {
		if ok {
			t.Errorf("a token was accepted for %s", name)
		}
	}
	if !ValidExecToken(secret, "vm1", "echo", nil, ExecToken(secret, "vm1", "echo", nil)) ||
		!ValidExecToken(secret, "vm1", "echo", []string{}, ExecToken(secret, "vm1", "echo", nil)) {
		t.Error("no arguments should sign the same whether nil or empty")
	}
}
