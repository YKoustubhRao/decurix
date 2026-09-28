package mailtrigger

import (
	"errors"

	"github.com/YKoustubhRao/decurix/internal/sealed"
)

// A mailbox's app password is kept encrypted in the database with the key in
// a file of its own beside it (KeyFile) - see internal/sealed for what that
// does and does not protect against.

// KeyFile is the name of the key's file in the data directory.
const KeyFile = "mail.key"

// sealedPrefix is what a sealed password starts with in the database.
const sealedPrefix = "enc1:"

// ErrPasswordUnreadable is a saved password that cannot be decrypted - the
// key file was deleted or replaced. Like a refused login, only typing the
// password again fixes it.
var ErrPasswordUnreadable = errors.New("the saved password cannot be read (the key file it was encrypted with is gone); enter it again")

// LoadKey reads the encryption key from dir, creating it (0600) the first
// time.
func LoadKey(dir string) ([]byte, error) {
	return sealed.LoadKey(dir, KeyFile)
}

// seal encrypts a password for the database. With no key it is stored as
// is (tests, and a data directory that could not be written).
func seal(key []byte, password string) (string, error) {
	return sealed.Seal(key, password)
}

// unseal decrypts what seal stored. A value without the prefix was stored
// before there was a key and is returned as it is.
func unseal(key []byte, stored string) (string, error) {
	plain, err := sealed.Open(key, stored)
	if errors.Is(err, sealed.ErrUnreadable) {
		return "", ErrPasswordUnreadable
	}

	return plain, err
}
