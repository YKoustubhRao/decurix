package sqlitedb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LockFile is the file in the data directory a running control plane holds locked: two control planes on one data
// directory are unsupported (one database, one instance-id, one set of sealed keys), and after a crash two starting at
// once could each remove the other's fresh instance-id. The lock is the operating system's (flock, LockFileEx), so it
// goes with the process however that ends; the file itself stays, naming the last holder.
const LockFile = "decurix.lock"

// ErrDataDirInUse is returned by LockDataDir when another process holds the data directory's lock.
var ErrDataDirInUse = errors.New("the data directory is in use by another control plane")

// DataDirLock is the data directory's lock, held until Release.
type DataDirLock struct {
	f *os.File
}

// LockDataDir takes the data directory's exclusive lock for this process, making dir if it is missing. When another
// process holds it, the error wraps ErrDataDirInUse and says what that process wrote of itself (its pid and start).
func LockDataDir(dir string) (*DataDirLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("data directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, LockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	held, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !held {
		holder := holderOf(f)
		_ = f.Close()
		return nil, fmt.Errorf("%w (%s, in %s): stop it first, or give this one its own DECURIX_DATA_DIR",
			ErrDataDirInUse, holder, dir)
	}
	// Who holds it, for the message another start shows. Written in place: the lock is on a range past the text.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("pid %d, started %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)
	}
	return &DataDirLock{f: f}, nil
}

// holderOf is what the lock file says of the process that holds it.
func holderOf(f *os.File) string {
	buf := make([]byte, 200)
	n, _ := f.ReadAt(buf, 0)
	said := strings.TrimSpace(string(buf[:n]))
	if said == "" {
		return "a process that has not said which"
	}
	return said
}

// Release lets the lock go; the file stays.
func (l *DataDirLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
