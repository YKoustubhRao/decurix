package census

import (
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx         = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW          = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetLogicalProcessorInfo      = kernel32.NewProc("GetLogicalProcessorInformation")
	procIsProcessorFeaturePresent    = kernel32.NewProc("IsProcessorFeaturePresent")
	errInsufficientBuffer            = syscall.Errno(122)
	pfARMv82DotProductInstructionsOK = uintptr(43)
)

// memoryStatusEx is MEMORYSTATUSEX.
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

// hostMemory asks GlobalMemoryStatusEx.
func hostMemory() (total, avail uint64, kind, source string, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0, "", "", callErr
	}
	return m.TotalPhys, m.AvailPhys, "ram", "GlobalMemoryStatusEx", nil
}

// diskUsage asks GetDiskFreeSpaceExW.
func diskUsage(path string) (free, total uint64, err error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeToCaller, totalBytes, totalFree uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)), uintptr(unsafe.Pointer(&totalBytes)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0, 0, callErr
	}
	return freeToCaller, totalBytes, nil
}

// slpi is SYSTEM_LOGICAL_PROCESSOR_INFORMATION (the union is 16 bytes).
type slpi struct {
	ProcessorMask uintptr
	Relationship  uint32
	Union         [2]uint64
}

// physicalCores counts RelationProcessorCore entries.
func physicalCores() int {
	var length uint32
	r, _, err := procGetLogicalProcessorInfo.Call(0, uintptr(unsafe.Pointer(&length)))
	if r == 0 && err != errInsufficientBuffer || length == 0 {
		return 0
	}
	size := uint32(unsafe.Sizeof(slpi{}))
	buf := make([]slpi, length/size+1)
	r, _, _ = procGetLogicalProcessorInfo.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)))
	if r == 0 {
		return 0
	}
	cores := 0
	for _, e := range buf[:length/size] {
		if e.Relationship == 0 { // RelationProcessorCore
			cores++
		}
	}
	return cores
}

// cpuModelOS falls back to PROCESSOR_IDENTIFIER.
func cpuModelOS() string { return os.Getenv("PROCESSOR_IDENTIFIER") }

// osFlags reads arm64 features (x86 uses CPUID).
func osFlags() []string {
	if runtime.GOARCH != "arm64" {
		return nil
	}
	flags := []string{"neon"}
	if r, _, _ := procIsProcessorFeaturePresent.Call(pfARMv82DotProductInstructionsOK); r != 0 {
		flags = append(flags, "dotprod")
	}
	return flags
}

// hostFlags has nothing to mark on Windows.
func hostFlags(h *Host) {}

// extraPools adds the WSL VM when one is already running (never starts one).
func extraPools(ctx context.Context, n *notes) []Pool {
	distro, err := runningWSL(ctx)
	if err != nil || distro == "" {
		return nil
	}
	total, avail, err := wslMemory(ctx, distro)
	if err != nil {
		n.add("WSL is running but its memory could not be read: %v", err)
		return nil
	}
	return []Pool{{Name: "wsl", Kind: "ram", TotalGB: toGB(total), AvailableGB: toGB(avail), Source: "wsl /proc/meminfo (" + distro + ")"}}
}

// runningWSL names a running WSL distribution, or "" when none runs.
func runningWSL(ctx context.Context) (string, error) {
	out, err := run(ctx, 4*time.Second, "wsl.exe", "--list", "--running", "--quiet")
	if err != nil && len(out) == 0 {
		return "", err
	}
	return pickWSLDistro(parseWSLRunning(out)), nil
}

// wslMemory reads /proc/meminfo inside a running distribution.
func wslMemory(ctx context.Context, distro string) (total, avail uint64, err error) {
	out, err := run(ctx, 5*time.Second, "wsl.exe", "-d", distro, "-e", "cat", "/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	total, avail, ok := parseMeminfo(decodeConsole(out))
	if !ok {
		return 0, 0, errUnsupported
	}
	return total, avail, nil
}

// liveWSL re-reads the WSL VM's available memory.
func liveWSL(ctx context.Context) (float64, error) {
	distro, err := runningWSL(ctx)
	if err != nil {
		return 0, err
	}
	if distro == "" {
		return 0, errNoWSL
	}
	_, avail, err := wslMemory(ctx, distro)
	if err != nil {
		return 0, err
	}
	return toGB(avail), nil
}

// windowsOllamaRunning reports whether ollama.exe runs on Windows itself.
func windowsOllamaRunning(ctx context.Context) bool {
	out, err := run(ctx, 4*time.Second, "tasklist", "/FI", "IMAGENAME eq ollama.exe", "/NH")
	return err == nil && strings.Contains(strings.ToLower(decodeConsole(out)), "ollama.exe")
}

// osGPUs notes integrated GPUs, which Brainiac does not use on Windows.
func osGPUs(ctx context.Context, n *notes, hostTotal uint64, have map[string]bool) []GPU {
	if have["nvidia"] {
		return nil
	}
	out, err := run(ctx, 6*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance Win32_VideoController).Name")
	if err != nil {
		return nil
	}
	for _, name := range strings.Split(decodeConsole(out), "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		n.add("%s: not used (no CUDA, or an integrated GPU sharing system memory)", name)
	}
	return nil
}
