//go:build !linux

package census

// memoryBacked is only detected on Linux.
func memoryBacked(string) bool { return false }
