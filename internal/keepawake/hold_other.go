//go:build !linux && !darwin && !windows

package keepawake

func newHolder() (holder, string) {
	return nil, "not supported on this system, so it may still sleep"
}
