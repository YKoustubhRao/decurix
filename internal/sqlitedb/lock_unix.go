//go:build !windows

package sqlitedb

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes f's exclusive flock without waiting: false when another open file holds it (another process, or
// another open of the file in this one - flock locks belong to the open file, not the process).
func tryLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// unlock lets f's flock go.
func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
