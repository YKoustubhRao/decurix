//go:build !linux && !darwin && !windows

package ollama

import (
	"context"
	"errors"
	"time"
)

func readMemory() (total, avail uint64, err error) {
	return 0, 0, errors.New("memory probe not supported on this OS")
}

func cpuBusy(context.Context, time.Duration) (float64, error) {
	return 0, errors.New("cpu probe not supported on this OS")
}
