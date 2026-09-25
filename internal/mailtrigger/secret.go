package mailtrigger

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

// A mailbox's app password is kept encrypted (AES-256-GCM) in the database,
// with the key in a file of its own beside it (KeyFile). That keeps it out of
// a copy of decurix.db made to debug something, a backup of the database
// alone, or anything else that reads the database without the data
// directory. It does not protect it from someone who can read the whole data
// directory as this user - nothing short of an OS keychain would, and a
// control plane in a container has none.

// KeyFile is the name of the key's file in the data directory.
const KeyFile = "mail.key"

const sealedPrefix = "enc1:"

// ErrPasswordUnreadable is a saved password that cannot be decrypted - the
// key file was deleted or replaced. Like a refused login, only typing the
// password again fixes it.
var ErrPasswordUnreadable = errors.New("the saved password cannot be read (the key file it was encrypted with is gone); enter it again")

// LoadKey reads the encryption key from dir, creating it (0600) the first
// time.
func LoadKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, KeyFile)

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
		return LoadKey(dir)
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

// seal encrypts a password for the database. With no key it is stored as
// is (tests, and a data directory that could not be written).
func seal(key []byte, password string) (string, error) {
	if key == nil {
		return password, nil
	}

	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	return sealedPrefix + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(password), nil)), nil
}

// unseal decrypts what seal stored. A value without the prefix was stored
// before there was a key and is returned as it is.
func unseal(key []byte, stored string) (string, error) {
	if !strings.HasPrefix(stored, sealedPrefix) {
		return stored, nil
	}

	if key == nil {
		return "", ErrPasswordUnreadable
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, sealedPrefix))
	if err != nil {
		return "", ErrPasswordUnreadable
	}

	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}

	if len(raw) < aead.NonceSize() {
		return "", ErrPasswordUnreadable
	}

	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", ErrPasswordUnreadable
	}

	return string(plain), nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	return cipher.NewGCM(block)
}
