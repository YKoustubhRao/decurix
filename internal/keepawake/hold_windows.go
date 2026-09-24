package keepawake

import (
	"fmt"
	"runtime"
	"syscall"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var setThreadExecutionState = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// threadHolder holds the request on one OS thread of its own: Windows ties an
// ES_CONTINUOUS request to the thread that made it, and a goroutine moves
// between threads unless locked to one.
type threadHolder struct {
	requests chan bool
	results  chan error
}

func newHolder() (holder, string) {
	if err := setThreadExecutionState.Find(); err != nil {
		return nil, "SetThreadExecutionState is not available, so Windows may still sleep"
	}

	h := &threadHolder{requests: make(chan bool), results: make(chan error)}

	go func() {
		runtime.LockOSThread()

		for on := range h.requests {
			flags := uintptr(esContinuous)
			if on {
				flags |= esSystemRequired
			}

			if prev, _, err := setThreadExecutionState.Call(flags); prev == 0 {
				h.results <- fmt.Errorf("SetThreadExecutionState: %v", err)
				continue
			}

			h.results <- nil
		}

		// Returning with the thread still locked ends the thread, which ends its request.
	}()

	return h, "SetThreadExecutionState"
}

func (h *threadHolder) set(on bool) error {
	h.requests <- on
	return <-h.results
}

func (h *threadHolder) alive() bool { return true }

func (h *threadHolder) close() { close(h.requests) }
