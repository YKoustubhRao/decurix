//go:build !linux && !darwin && !freebsd && !windows

package census

// diskUsage is not measured on this OS.
func diskUsage(path string) (free, total uint64, err error) { return 0, 0, errUnsupported }
