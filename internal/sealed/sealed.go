// Package sealed encrypts the few secrets the control plane keeps in its
// database (a mailbox's app password, the Cloudflare API token) with
// AES-256-GCM, under a key kept in a file of its own in the data directory.
//
// That keeps a secret out of a copy of decurix.db made to debug something, a
// backup of the database alone, or anything else that reads the database
// without the data directory. It does not protect it from someone who can
// read the whole data directory as this user - nothing short of an OS
// keychain would, and a control plane in a container has none.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// prefix marks a stored value as sealed; one without it predates the key.
const prefix = "enc1:"

// ErrUnreadable is a sealed value that cannot be decrypted: the key file was
// deleted or replaced. Only entering the secret again fixes it.
var ErrUnreadable = errors.New("sealed: the value cannot be decrypted with this key")

// LoadKey reads the 32-byte key in dir/name, creating it (0600) the first
// time.
func LoadKey(dir, name string) ([]byte, error) {
	path := filepath.Join(dir, name)

	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s is not a key this program wrote (%d bytes)", path, len(key))
		}

		return key, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}

	// O_EXCL: two processes starting at once must not each write a key and
	// leave one of them encrypting with a key that is then overwritten.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadKey(dir, name)
	}

	if err != nil {
		return nil, err
	}

	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}

	return key, f.Close()
}

// Seal encrypts plain for the database. With no key it is stored as is
// (tests, and a data directory that could not be written).
func Seal(key []byte, plain string) (string, error) {
	if key == nil {
		return plain, nil
	}

	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	return prefix + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Open decrypts what Seal stored. A value without the prefix was stored
// before there was a key and is returned as it is.
func Open(key []byte, stored string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}

	if key == nil {
		return "", ErrUnreadable
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, prefix))
	if err != nil {
		return "", ErrUnreadable
	}

	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}

	if len(raw) < aead.NonceSize() {
		return "", ErrUnreadable
	}

	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", ErrUnreadable
	}

	return string(plain), nil
}

// IsSealed reports whether stored was written by Seal with a key.
func IsSealed(stored string) bool {
	return strings.HasPrefix(stored, prefix)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	return cipher.NewGCM(block)
}
