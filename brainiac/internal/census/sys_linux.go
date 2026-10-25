package census

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// hostMemory reads /proc/meminfo, capped by the cgroup this process is in.
func hostMemory() (total, avail uint64, kind, source string, err error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, "", "", err
	}
	total, avail, ok := parseMeminfo(string(raw))
	if !ok {
		return 0, 0, "", "", errUnsupported
	}
	source = "/proc/meminfo"
	if limit, usage, inactive, which, found := cgroupMemory(); found {
		capped, free := applyCgroup(total, avail, limit, usage, inactive)
		if capped < total {
			total, avail = capped, free
			source = which
		}
	}
	return total, avail, "ram", source, nil
}

// cgroupMemory finds the tightest memory limit on this process's cgroup path
// (v2), or the v1 memory controller's limit.
func cgroupMemory() (limit, usage, inactive uint64, which string, found bool) {
	raw, _ := os.ReadFile("/proc/self/cgroup")
	v2Path, v1Path := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2Path = parts[2]
		}
		for _, ctl := range strings.Split(parts[1], ",") {
			if ctl == "memory" {
				v1Path = parts[2]
			}
		}
	}

	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		dir := filepath.Join("/sys/fs/cgroup", v2Path)
		for {
			if l, ok := readCgroupLimit(filepath.Join(dir, "memory.max")); ok && (!found || l < limit) {
				limit, found = l, true
				usage = readUint(filepath.Join(dir, "memory.current"))
				stat, _ := os.ReadFile(filepath.Join(dir, "memory.stat"))
				inactive = memStatValue(string(stat), "inactive_file")
				which = "cgroup v2"
			}
			if dir == "/sys/fs/cgroup" || dir == "/" || dir == "." {
				break
			}
			dir = filepath.Dir(dir)
		}
		return limit, usage, inactive, which, found
	}

	for _, dir := range []string{filepath.Join("/sys/fs/cgroup/memory", v1Path), "/sys/fs/cgroup/memory"} {
		if l, ok := readCgroupLimit(filepath.Join(dir, "memory.limit_in_bytes")); ok {
			usage = readUint(filepath.Join(dir, "memory.usage_in_bytes"))
			stat, _ := os.ReadFile(filepath.Join(dir, "memory.stat"))
			inactive = memStatValue(string(stat), "total_inactive_file")
			return l, usage, inactive, "cgroup v1", true
		}
	}
	return 0, 0, 0, "", false
}

// readCgroupLimit reads a limit file.
func readCgroupLimit(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseCgroupLimit(string(raw))
}

// readUint reads a file holding one number.
func readUint(path string) uint64 {
	raw, _ := os.ReadFile(path)
	v, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return v
}

// physicalCores counts unique cores in /proc/cpuinfo.
func physicalCores() int {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return parseCPUInfo(string(raw)).PhysicalCores
}

// cpuModelOS reads the model from /proc/cpuinfo.
func cpuModelOS() string {
	raw, _ := os.ReadFile("/proc/cpuinfo")
	return parseCPUInfo(string(raw)).Model
}

// osFlags reads arm64 features from /proc/cpuinfo (x86 uses CPUID).
func osFlags() []string {
	if runtime.GOARCH != "arm64" {
		return nil
	}
	raw, _ := os.ReadFile("/proc/cpuinfo")
	return armFlags(parseCPUInfo(string(raw)).Features)
}

// hostFlags marks WSL and containers.
func hostFlags(h *Host) {
	if v, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(v)), "microsoft") {
		h.WSL = true
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		h.WSL = true
	}
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			h.Container = true
		}
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		h.Container = true
	}
	if raw, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(raw)
		for _, hint := range []string{"docker", "kubepods", "containerd", "libpod"} {
			if strings.Contains(s, hint) {
				h.Container = true
			}
		}
	}
}

// extraPools has nothing to add on Linux.
func extraPools(ctx context.Context, n *notes) []Pool { return nil }

// liveWSL is only meaningful on Windows.
func liveWSL(ctx context.Context) (float64, error) { return 0, errUnsupported }

// windowsOllamaRunning is only meaningful on Windows.
func windowsOllamaRunning(ctx context.Context) bool { return false }

// osGPUs reads AMD and Intel GPUs from sysfs when no vendor tool answered.
func osGPUs(ctx context.Context, n *notes, hostTotal uint64, have map[string]bool) []GPU {
	var gpus []GPU
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/vendor")
	for _, vendorFile := range cards {
		dev := filepath.Dir(vendorFile)
		vendor := strings.TrimSpace(readString(vendorFile))
		switch vendor {
		case "0x1002":
			if have["amd"] {
				continue
			}
			vram := readUint(filepath.Join(dev, "mem_info_vram_total"))
			vramUsed := readUint(filepath.Join(dev, "mem_info_vram_used"))
			gtt := readUint(filepath.Join(dev, "mem_info_gtt_total"))
			gttUsed := readUint(filepath.Join(dev, "mem_info_gtt_used"))
			if vram == 0 && gtt == 0 {
				continue
			}
			g := amdFromSysfs(vram, vramUsed, gtt, gttUsed)
			if g.Unified {
				n.add("AMD GPU treated as an APU sharing system memory (VRAM carve-out %.1f GB, GTT %.1f GB)", toGB(vram), toGB(gtt))
			}
			gpus = append(gpus, g)
			have["amd"] = true
		case "0x8086":
			n.add("Intel GPU present; not used (shares system memory, no usable VRAM figure)")
		}
	}
	return gpus
}

// readString reads a small file.
func readString(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
