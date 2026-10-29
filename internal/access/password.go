package access

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// PasswordIterations is the PBKDF2-SHA256 work factor for new hashes; a
// stored hash keeps the count it was made with. Tests lower it.
var PasswordIterations = 600_000

const (
	hashScheme = "pbkdf2-sha256"
	saltBytes  = 16
	keyBytes   = 32
)

// HashPassword returns "pbkdf2-sha256$<iterations>$<salt hex>$<key hex>" for password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("access: salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, PasswordIterations, keyBytes)
	if err != nil {
		return "", fmt.Errorf("access: hash: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, PasswordIterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches a hash made by HashPassword, in constant time.
func CheckPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

var (
	dummyOnce sync.Once
	dummy     string
)

// dummyHash is a hash of nothing anyone knows, checked against for an unknown name.
func dummyHash() string {
	dummyOnce.Do(func() {
		dummy, _ = HashPassword(randomToken())
	})
	return dummy
}

// randomToken returns 32 random bytes in hex.
func randomToken() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // crypto/rand never returns an error; it crashes instead
	return hex.EncodeToString(raw)
}

// tokenHash is what a session token is stored as.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
