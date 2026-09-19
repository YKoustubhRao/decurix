package winhost

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const secretFile = "hostgui.key"

// LoadSecret returns the key launch tokens are signed with, creating it (32 random bytes, readable only by this user) in
// dir the first time. It lives in the data directory so tokens already written into a desktop's launchers stay valid
// across control-plane restarts.
func LoadSecret(dir string) ([]byte, error) {
	path := filepath.Join(dir, secretFile)
	if b, err := os.ReadFile(path); err == nil {
		if key, err := hex.DecodeString(string(b)); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate host-gui key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, fmt.Errorf("save host-gui key: %w", err)
	}
	return key, nil
}

// Token authorises one launch: it is an HMAC over the session id, the program and its arguments, so it lets whoever
// holds it start exactly that app for exactly that session and nothing else - a guessed session id, or a launcher
// edited to run a different program, has no valid token.
func Token(secret []byte, id, target, args string) string {
	mac := hmac.New(sha256.New, secret)
	for _, part := range []string{id, target, args} {
		fmt.Fprintf(mac, "%d:%s|", len(part), part) // length-prefixed, so ("a","bc") and ("ab","c") differ
	}
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ValidToken reports whether token is the launch token for exactly this id, program and arguments.
func ValidToken(secret []byte, id, target, args, token string) bool {
	if len(secret) == 0 || token == "" {
		return false
	}
	return hmac.Equal([]byte(Token(secret, id, target, args)), []byte(token))
}

// execParts turns a host command into the fields Token signs. Arguments are JSON-encoded so ["a b"] and ["a","b"]
// differ, and the "exec:" prefix keeps an exec token from ever being valid as a launch token for the same strings.
func execParts(desktopID string, args []string) (id, joined string) {
	if args == nil {
		args = []string{}
	}
	b, _ := json.Marshal(args)
	return "exec:" + desktopID, string(b)
}

// ExecToken authorises one host command from one desktop: exactly this program with exactly these arguments. A launcher
// carries it for the command it was written to run, so the control plane runs that and nothing else - a desktop's own
// agent cannot ask the host to run anything a launcher was not written for.
func ExecToken(secret []byte, desktopID, cmd string, args []string) string {
	id, joined := execParts(desktopID, args)
	return Token(secret, id, cmd, joined)
}

// ValidExecToken reports whether token is the exec token for exactly this desktop, program and arguments.
func ValidExecToken(secret []byte, desktopID, cmd string, args []string, token string) bool {
	id, joined := execParts(desktopID, args)
	return ValidToken(secret, id, cmd, joined, token)
}
