//go:build !linux && !windows && !darwin

package census

import "context"

// hostMemory is not measured on this OS.
func hostMemory() (total, avail uint64, kind, source string, err error) {
	return 0, 0, "", "", errUnsupported
}

// physicalCores is not measured on this OS.
func physicalCores() int { return 0 }

// cpuModelOS is not read on this OS.
func cpuModelOS() string { return "" }

// osFlags is empty on this OS.
func osFlags() []string { return nil }

// hostFlags has nothing to mark on this OS.
func hostFlags(h *Host) {}

// extraPools has nothing to add on this OS.
func extraPools(ctx context.Context, n *notes) []Pool { return nil }

// liveWSL is only meaningful on Windows.
func liveWSL(ctx context.Context) (float64, error) { return 0, errUnsupported }

// windowsOllamaRunning is only meaningful on Windows.
func windowsOllamaRunning(ctx context.Context) bool { return false }

// osGPUs finds nothing on this OS.
func osGPUs(ctx context.Context, n *notes, hostTotal uint64, have map[string]bool) []GPU {
	return nil
}
