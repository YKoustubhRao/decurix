//go:build !linux

package desktop

// OptimizeHostKSM is a no-op on non-Linux operating systems.
func OptimizeHostKSM() {}

// DisableHostKSM is a no-op on non-Linux operating systems.
func DisableHostKSM() {}
