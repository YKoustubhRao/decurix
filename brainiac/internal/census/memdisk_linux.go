//go:build linux

package census

import "syscall"

// memoryBacked reports whether path is on a RAM filesystem (tmpfs, ramfs):
// models stored there take memory away from the models themselves, and the
// space is gone on the next boot. WSL mounts /tmp this way.
func memoryBacked(path string) bool {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return false
	}
	const tmpfs, ramfs = 0x01021994, 0x858458f6
	return st.Type == tmpfs || st.Type == ramfs
}
