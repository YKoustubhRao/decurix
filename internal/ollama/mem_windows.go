package ollama

import (
	"context"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemory   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// readMemory returns total and available physical memory in bytes.
func readMemory() (total, avail uint64, err error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))

	if r, _, callErr := procGlobalMemory.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx: %v", callErr)
	}

	return ms.TotalPhys, ms.AvailPhys, nil
}

func filetime(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// systemTimes returns idle and total (kernel, which includes idle, plus user)
// CPU time in 100ns units.
func systemTimes() (idle, total uint64, err error) {
	var i, k, u syscall.Filetime

	r, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&i)),
		uintptr(unsafe.Pointer(&k)),
		uintptr(unsafe.Pointer(&u)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("GetSystemTimes: %v", callErr)
	}

	return filetime(i), filetime(k) + filetime(u), nil
}

// cpuBusy samples the system's CPU times twice, window apart.
func cpuBusy(ctx context.Context, window time.Duration) (float64, error) {
	idle1, total1, err := systemTimes()
	if err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(window):
	}

	idle2, total2, err := systemTimes()
	if err != nil {
		return 0, err
	}

	return busyFraction(idle1, total1, idle2, total2), nil
}
