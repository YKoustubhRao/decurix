//go:build !linux

// nodump_other.go: off Linux there is no worker user to keep soul's memory from; noDump does nothing.
package main

// noDump does nothing off Linux.
func noDump() error { return nil }
