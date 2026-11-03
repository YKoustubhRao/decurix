//go:build !linux

package engine

// SweepOrphans does nothing here: model servers are reached on loopback
// ports, and there is no socket directory to know them by.
func SweepOrphans(func(string, ...any)) int { return 0 }
