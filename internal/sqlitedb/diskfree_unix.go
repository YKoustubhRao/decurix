//go:build !windows

package sqlitedb

import "golang.org/x/sys/unix"

// freeDisk is the bytes free to an unprivileged writer on the filesystem holding dir.
func freeDisk(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
