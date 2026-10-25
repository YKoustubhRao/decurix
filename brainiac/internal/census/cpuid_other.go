//go:build !amd64

package census

// x86Flags is empty off amd64.
func x86Flags() []string { return nil }

// x86Brand is empty off amd64.
func x86Brand() string { return "" }
