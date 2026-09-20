package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const gb = 1 << 30

// Resources is what the machine has to give a model right now: what is
// installed minus what everything else is already using. Ollama's own loaded
// model is not "everything else" - see ProbeResources.
type Resources struct {
	RAMTotalGB float64 `json:"ram_total_gb"`
	// RAMFreeGB is memory a model could be given: the OS's own "available"
	// figure (free plus reclaimable cache) plus what Ollama already holds.
	RAMFreeGB float64 `json:"ram_free_gb"`

	Cores int `json:"cores"`
	// CPUBusy is the fraction (0-1) of the CPU other work used while sampled.
	CPUBusy float64 `json:"cpu_busy"`

	GPUName       string  `json:"gpu_name,omitempty"`
	VRAMTotalGB   float64 `json:"vram_total_gb,omitempty"`
	VRAMFreeGB    float64 `json:"vram_free_gb,omitempty"`
	UnifiedMemory bool    `json:"unified_memory,omitempty"`
}

// HasGPU reports whether a GPU the model could run on was found.
func (r Resources) HasGPU() bool { return r.VRAMFreeGB > 0 }

// ProbeResources measures this machine. baseURL is the Ollama that will run
// the model (used only to learn what it already holds); useGPU is false when
// that Ollama cannot reach the GPU - the bundled container is created without
// one - so a GPU it cannot use never inflates the budget.
//
// What the model already holds in Ollama is added back to the free figures:
// the machine's "available" memory has that model's footprint taken out of
// it, so without the add-back a model that fits would look too big to keep
// the moment it was loaded, and the choice would shrink every time it was
// re-checked.
func ProbeResources(ctx context.Context, baseURL string, useGPU bool) (Resources, error) {
	total, avail, err := readMemory()
	if err != nil {
		return Resources{}, err
	}

	r := Resources{
		RAMTotalGB: float64(total) / gb,
		RAMFreeGB:  float64(avail) / gb,
		Cores:      runtime.NumCPU(),
	}

	if busy, err := cpuBusy(ctx, 400*time.Millisecond); err == nil {
		r.CPUBusy = busy
	}

	ramHeld, vramHeld := ollamaHeld(ctx, baseURL)

	if useGPU {
		if name, vt, vf, unified, ok := probeGPU(ctx, r); ok {
			r.GPUName, r.VRAMTotalGB, r.VRAMFreeGB, r.UnifiedMemory = name, vt, vf, unified
		}
	}

	if r.UnifiedMemory {
		// One pool: the model sits wholly in the memory the GPU shares.
		r.RAMFreeGB += (ramHeld + vramHeld)
		r.VRAMFreeGB = r.RAMFreeGB
	} else {
		r.RAMFreeGB += ramHeld
		if r.HasGPU() {
			r.VRAMFreeGB += vramHeld
		}
	}

	if r.RAMFreeGB > r.RAMTotalGB {
		r.RAMFreeGB = r.RAMTotalGB
	}
	if r.HasGPU() && r.VRAMFreeGB > r.VRAMTotalGB {
		r.VRAMFreeGB = r.VRAMTotalGB
	}

	return r, nil
}

// probeGPU finds a GPU: nvidia-smi anywhere it exists (WSL included), or the
// unified memory of an Apple Silicon Mac. AMD and Intel GPUs are not probed -
// they are treated as absent, which errs toward a smaller model.
func probeGPU(ctx context.Context, cur Resources) (name string, totalGB, freeGB float64, unified, ok bool) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "Apple Silicon (unified memory)", cur.RAMTotalGB, cur.RAMFreeGB, true, true
	}

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,memory.free",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", 0, 0, false, false
	}

	return parseNvidiaSMI(string(out))
}

// parseNvidiaSMI reads `nvidia-smi --query-gpu=name,memory.total,memory.free`
// (MiB, one line per GPU). Ollama spreads a model across GPUs, so several are
// summed rather than the largest taken.
func parseNvidiaSMI(out string) (name string, totalGB, freeGB float64, unified, ok bool) {
	gpus := 0
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}

		total, err1 := strconv.ParseFloat(strings.TrimSpace(parts[len(parts)-2]), 64)
		free, err2 := strconv.ParseFloat(strings.TrimSpace(parts[len(parts)-1]), 64)
		if err1 != nil || err2 != nil {
			continue
		}

		if gpus == 0 {
			name = strings.TrimSpace(strings.Join(parts[:len(parts)-2], ","))
		}
		gpus++
		totalGB += total / 1024
		freeGB += free / 1024
	}

	if gpus == 0 || totalGB <= 0 {
		return "", 0, 0, false, false
	}
	if gpus > 1 {
		name += " x" + strconv.Itoa(gpus)
	}

	return name, totalGB, freeGB, false, true
}

// ollamaHeld reports, in GB, how much RAM and VRAM the models already loaded
// in the Ollama at baseURL occupy (GET /api/ps). Unreachable means nothing.
func ollamaHeld(ctx context.Context, baseURL string) (ramGB, vramGB float64) {
	ps, err := fetchPS(ctx, baseURL)
	if err != nil {
		return 0, 0
	}

	for _, m := range ps {
		vram := float64(m.SizeVRAM)
		ram := float64(m.Size) - vram
		if ram < 0 {
			ram = 0
		}
		ramGB += ram / gb
		vramGB += vram / gb
	}

	return ramGB, vramGB
}

type psModel struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SizeVRAM int64  `json:"size_vram"`
}

// fetchPS lists the models the Ollama at baseURL has loaded.
func fetchPS(ctx context.Context, baseURL string) ([]psModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/ps", nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Models []psModel `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}

	return body.Models, nil
}

// busyFraction turns two (idle, total) counter readings into the fraction of
// the interval that was not idle.
func busyFraction(idle1, total1, idle2, total2 uint64) float64 {
	if total2 <= total1 || idle2 < idle1 {
		return 0
	}

	busy := 1 - float64(idle2-idle1)/float64(total2-total1)
	switch {
	case busy < 0:
		return 0
	case busy > 1:
		return 1
	}

	return busy
}
