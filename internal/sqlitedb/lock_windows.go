//go:build windows

package sqlitedb

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffset is where the locked byte is: far past the holder's line, which a byte-range lock on Windows would
// otherwise keep another process from reading.
const lockOffset = 1 << 30

// tryLock takes an exclusive lock on one byte of f without waiting (LockFileEx): false when another handle holds it.
func tryLock(f *os.File) (bool, error) {
	ol := &windows.Overlapped{Offset: lockOffset}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return err == nil, err
}

// unlock lets f's lock go.
func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{Offset: lockOffset})
}
