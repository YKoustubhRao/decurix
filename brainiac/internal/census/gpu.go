package census

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// probeGPUs finds accelerators with their memory, and names their pools:
// discrete GPUs are gpu0, gpu1, ...; one that shares system memory is "host".
func probeGPUs(ctx context.Context, n *notes, hostTotal uint64) []GPU {
	var gpus []GPU
	have := map[string]bool{}

	if out, err := run(ctx, 6*time.Second, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.free,driver_version,compute_cap",
		"--format=csv,noheader,nounits"); err == nil {
		found := parseNvidiaSMI(string(out), hostTotal)
		for _, g := range found {
			if g.Unified {
				n.add("NVIDIA %s reports no separate memory; treated as sharing system memory", g.Name)
			}
		}
		gpus = append(gpus, found...)
		have["nvidia"] = len(found) > 0
	}

	if runtime.GOOS == "linux" {
		if mem, err := run(ctx, 6*time.Second, "rocm-smi", "--showmeminfo", "vram", "--json"); err == nil {
			names, _ := run(ctx, 6*time.Second, "rocm-smi", "--showproductname", "--json")
			found := parseRocmSMI(mem, names)
			gpus = append(gpus, found...)
			have["amd"] = len(found) > 0
		} else if mem, err := run(ctx, 6*time.Second, "amd-smi", "metric", "--mem-usage", "--json"); err == nil {
			found := parseAmdSMI(mem)
			if len(found) == 0 {
				n.add("amd-smi answered but its output was not understood")
			}
			gpus = append(gpus, found...)
			have["amd"] = len(found) > 0
		}
	}

	gpus = append(gpus, osGPUs(ctx, n, hostTotal, have)...)

	next := 0
	for i := range gpus {
		if gpus[i].Unified {
			gpus[i].Pool = "host"
			continue
		}
		gpus[i].Pool = fmt.Sprintf("gpu%d", next)
		next++
	}
	return gpus
}

// parseNvidiaSMI reads nvidia-smi CSV (name, total MiB, free MiB, driver,
// compute capability). A GPU with no memory figure (GB10, Grace Hopper)
// shares system memory.
func parseNvidiaSMI(text string, hostTotal uint64) []GPU {
	var gpus []GPU
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		g := GPU{Vendor: "nvidia", Name: parts[0], Backends: []string{"cuda", "vulkan"}}
		if len(parts) > 3 {
			g.Driver = parts[3]
		}
		if len(parts) > 4 && !strings.HasPrefix(parts[4], "[") {
			g.ComputeCap = parts[4]
		}
		total, errT := strconv.ParseFloat(parts[1], 64)
		free, errF := strconv.ParseFloat(parts[2], 64)
		if errT != nil {
			g.Unified = true
			g.VRAMTotalGB = toGB(hostTotal)
		} else {
			g.VRAMTotalGB = round2(total / 1024)
			if errF == nil {
				g.VRAMFreeGB = round2(free / 1024)
			}
		}
		gpus = append(gpus, g)
	}
	return gpus
}

// parseRocmSMI reads `rocm-smi --showmeminfo vram --json` and, for names,
// `rocm-smi --showproductname --json`.
func parseRocmSMI(memJSON, nameJSON []byte) []GPU {
	var mem map[string]map[string]any
	if json.Unmarshal(memJSON, &mem) != nil {
		return nil
	}
	var names map[string]map[string]any
	_ = json.Unmarshal(nameJSON, &names)

	cards := make([]string, 0, len(mem))
	for card := range mem {
		if strings.HasPrefix(card, "card") {
			cards = append(cards, card)
		}
	}
	sort.Strings(cards)

	var gpus []GPU
	for _, card := range cards {
		fields := mem[card]
		total := anyFloat(fields["VRAM Total Memory (B)"])
		used := anyFloat(fields["VRAM Total Used Memory (B)"])
		if total <= 0 {
			continue
		}
		name := "AMD GPU"
		for _, key := range []string{"Card series", "Card Series", "Card model", "Card Model"} {
			if v, ok := names[card][key].(string); ok && strings.TrimSpace(v) != "" {
				name = strings.TrimSpace(v)
				break
			}
		}
		gpus = append(gpus, GPU{
			Vendor:      "amd",
			Name:        name,
			VRAMTotalGB: round2(total / gib),
			VRAMFreeGB:  round2((total - used) / gib),
			Backends:    []string{"rocm", "vulkan"},
		})
	}
	return gpus
}

// parseAmdSMI reads `amd-smi metric --mem-usage --json`: a list (or
// {"gpu_data": [...]}) of {"gpu": N, "mem_usage": {"total_vram": {"value", "unit"}, ...}}.
func parseAmdSMI(raw []byte) []GPU {
	var list []map[string]any
	if json.Unmarshal(raw, &list) != nil {
		var wrapped struct {
			GPUData []map[string]any `json:"gpu_data"`
		}
		if json.Unmarshal(raw, &wrapped) != nil {
			return nil
		}
		list = wrapped.GPUData
	}
	var gpus []GPU
	for _, entry := range list {
		usage, _ := entry["mem_usage"].(map[string]any)
		total := sizeGB(usage["total_vram"])
		if total <= 0 {
			continue
		}
		free := sizeGB(usage["free_vram"])
		if free <= 0 {
			free = total - sizeGB(usage["used_vram"])
		}
		gpus = append(gpus, GPU{
			Vendor:      "amd",
			Name:        fmt.Sprintf("AMD GPU %v", entry["gpu"]),
			VRAMTotalGB: round2(total),
			VRAMFreeGB:  round2(free),
			Backends:    []string{"rocm", "vulkan"},
		})
	}
	return gpus
}

// sizeGB reads {"value": 24560, "unit": "MB"} or a bare number of MB.
func sizeGB(v any) float64 {
	switch t := v.(type) {
	case map[string]any:
		value := anyFloat(t["value"])
		switch strings.ToUpper(fmt.Sprint(t["unit"])) {
		case "B":
			return value / gib
		case "KB":
			return value / (1 << 20)
		case "GB":
			return value
		default: // MB
			return value / 1024
		}
	default:
		return anyFloat(v) / 1024
	}
}

// anyFloat reads a JSON number or numeric string.
func anyFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	}
	return 0
}

// amdFromSysfs builds an AMD GPU from amdgpu's sysfs counters. A small VRAM
// carve-out with a large GTT is an APU (Strix Halo and the like): its real
// working memory is the GTT, which is system RAM.
func amdFromSysfs(vram, vramUsed, gtt, gttUsed uint64) GPU {
	g := GPU{Vendor: "amd", Name: "AMD GPU", Backends: []string{"rocm", "vulkan"}}
	if vram <= 4*gib && gtt >= 16*gib {
		g.Unified = true
		g.VRAMTotalGB = toGB(gtt)
		if gttUsed < gtt {
			g.VRAMFreeGB = toGB(gtt - gttUsed)
		}
		return g
	}
	g.VRAMTotalGB = toGB(vram)
	if vramUsed < vram {
		g.VRAMFreeGB = toGB(vram - vramUsed)
	}
	return g
}
