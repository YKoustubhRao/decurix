package census

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// sysctl reads one sysctl value.
func sysctl(name string) string {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// hostMemory reads hw.memsize and vm_stat. On Apple Silicon it is unified
// memory, shared with the GPU.
func hostMemory() (total, avail uint64, kind, source string, err error) {
	total, err = strconv.ParseUint(sysctl("hw.memsize"), 10, 64)
	if err != nil || total == 0 {
		return 0, 0, "", "", errUnsupported
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, 0, "", "", err
	}
	avail, ok := parseVMStat(string(out))
	if !ok {
		return 0, 0, "", "", errUnsupported
	}
	if avail > total {
		avail = total
	}
	kind = "ram"
	if runtime.GOARCH == "arm64" {
		kind = "unified"
	}
	return total, avail, kind, "sysctl hw.memsize + vm_stat", nil
}

// physicalCores reads hw.physicalcpu.
func physicalCores() int {
	n, _ := strconv.Atoi(sysctl("hw.physicalcpu"))
	return n
}

// cpuModelOS reads the brand string.
func cpuModelOS() string { return sysctl("machdep.cpu.brand_string") }

// osFlags names Apple Silicon's features (x86 Macs use CPUID).
func osFlags() []string {
	if runtime.GOARCH != "arm64" {
		return nil
	}
	flags := []string{"neon", "dotprod"}
	if sysctl("hw.optional.arm.FEAT_I8MM") == "1" {
		flags = append(flags, "i8mm")
	}
	if sysctl("hw.optional.arm.FEAT_BF16") == "1" {
		flags = append(flags, "bf16")
	}
	return flags
}

// hostFlags has nothing to mark on macOS.
func hostFlags(h *Host) {}

// extraPools has nothing to add on macOS.
func extraPools(ctx context.Context, n *notes) []Pool { return nil }

// liveWSL is only meaningful on Windows.
func liveWSL(ctx context.Context) (float64, error) { return 0, errUnsupported }

// windowsOllamaRunning is only meaningful on Windows.
func windowsOllamaRunning(ctx context.Context) bool { return false }

// osGPUs reports Apple Silicon's GPU, which shares unified memory. Metal lets
// a process use about three quarters of it on large machines and about two
// thirds on small ones by default.
func osGPUs(ctx context.Context, n *notes, hostTotal uint64, have map[string]bool) []GPU {
	if runtime.GOARCH != "arm64" {
		return nil
	}
	share := 0.67
	if hostTotal >= 36*gib {
		share = 0.75
	}
	usable := round2(float64(hostTotal) / gib * share)
	n.add("Apple GPU: Metal can use about %.0f%% of unified memory by default (%.1f GB)", share*100, usable)
	return []GPU{{
		Vendor:      "apple",
		Name:        cpuModelOS(),
		VRAMTotalGB: usable,
		Unified:     true,
		Backends:    []string{"metal"},
		Pool:        "host",
	}}
}
