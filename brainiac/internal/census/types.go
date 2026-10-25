// Package census measures what one machine can give models right now: CPU,
// memory pools, GPUs, disk, a short memory-bandwidth test, the local Ollama
// and its models, network reach and a Python for workers.
//
// It is the first thing build.sh runs, and everything Brainiac chooses
// follows from it, so it measures instead of trusting spec sheets: this
// repo's 8 GB laptop is rated at 43 GB/s of memory bandwidth and delivers
// about 9 GB/s to a model.
//
// Depends on nothing internal.
package census

import (
	"strings"
	"time"
)

// Schema is the version of the Census JSON shape.
const Schema = 1

// Census is one measurement of a machine.
type Census struct {
	Schema  int       `json:"schema"`
	TakenAt time.Time `json:"taken_at"`
	Host    Host      `json:"host"`
	CPU     CPU       `json:"cpu"`
	// Pools are the places a model's weights can live: "host" (the RAM of the
	// OS Brainiac runs on), "wsl" (the WSL VM, seen from Windows), "gpu0"...
	// (one per discrete GPU). On Apple Silicon "host" is unified memory.
	Pools   []Pool  `json:"pools"`
	GPUs    []GPU   `json:"gpus,omitempty"`
	Disk    Disk    `json:"disk"`
	Bench   *Bench  `json:"bench,omitempty"`
	Ollama  *Ollama `json:"ollama,omitempty"`
	Network Network `json:"network"`
	Python  *Python `json:"python,omitempty"`
	// Notes say plainly what could not be measured and what was assumed.
	Notes []string `json:"notes,omitempty"`
}

// Host is the operating system Brainiac runs on.
type Host struct {
	OS        string `json:"os"`   // runtime.GOOS
	Arch      string `json:"arch"` // runtime.GOARCH
	Hostname  string `json:"hostname"`
	Container bool   `json:"container,omitempty"` // inside Docker/Podman/k8s
	WSL       bool   `json:"wsl,omitempty"`       // a Linux inside WSL
}

// CPU is the processor.
type CPU struct {
	Model         string `json:"model"`
	PhysicalCores int    `json:"physical_cores"`
	LogicalCores  int    `json:"logical_cores"`
	// Flags are lower-case instruction-set names that matter to inference
	// runtimes: avx, avx2, fma, f16c, avx512f, avx512_vnni, avx512_bf16,
	// avx_vnni, amx_tile, amx_bf16, amx_int8, neon, sve, dotprod.
	Flags []string `json:"flags"`
}

// Pool is memory a model can be given.
type Pool struct {
	Name string `json:"name"`
	// Kind is "ram", "vram" or "unified".
	Kind        string  `json:"kind"`
	TotalGB     float64 `json:"total_gb"`
	AvailableGB float64 `json:"available_gb"`
	// Source says where the numbers came from ("cgroup v2", "/proc/meminfo",
	// "GlobalMemoryStatusEx", "nvidia-smi", "wsl /proc/meminfo", ...).
	Source string `json:"source"`
}

// GPU is one accelerator.
type GPU struct {
	Vendor      string   `json:"vendor"` // nvidia | amd | apple | intel
	Name        string   `json:"name"`
	VRAMTotalGB float64  `json:"vram_total_gb,omitempty"`
	VRAMFreeGB  float64  `json:"vram_free_gb,omitempty"`
	Unified     bool     `json:"unified,omitempty"`
	Backends    []string `json:"backends,omitempty"` // cuda | rocm | metal | vulkan
	ComputeCap  string   `json:"compute_cap,omitempty"`
	Driver      string   `json:"driver,omitempty"`
	// Pool names the Pool this GPU's memory is ("gpu0", or "host" when unified).
	Pool string `json:"pool"`
}

// Disk is where models are stored.
type Disk struct {
	Path    string  `json:"path"`
	FreeGB  float64 `json:"free_gb"`
	TotalGB float64 `json:"total_gb"`
}

// Bench is the short memory test. CPU decoding reads every active weight once
// per token, so memory bandwidth, not FLOPs, sets tokens per second.
type Bench struct {
	// MemCopyGBps counts bytes read plus bytes written per second by a
	// multi-threaded copy of buffers far larger than the CPU caches.
	MemCopyGBps float64 `json:"mem_copy_gbps"`
	Threads     int     `json:"threads"`
	Seconds     float64 `json:"seconds"`
}

// Ollama is a local Ollama server and what it holds.
type Ollama struct {
	URL     string `json:"url"`
	Version string `json:"version,omitempty"`
	// Pool is where its models live: "host", "wsl", or "remote" when the URL
	// is another machine (its memory is then unknown).
	Pool   string        `json:"pool"`
	Models []OllamaModel `json:"models"`
	// LoadedGB is what it holds in memory now (/api/ps), by anyone.
	LoadedGB float64 `json:"loaded_gb"`
}

// OllamaModel is one installed Ollama model, read without loading it.
type OllamaModel struct {
	Name          string  `json:"name"`
	SizeGB        float64 `json:"size_gb"`
	Family        string  `json:"family,omitempty"`
	ParamsB       float64 `json:"params_b,omitempty"`
	Quant         string  `json:"quant,omitempty"`
	Vision        bool    `json:"vision,omitempty"`
	ContextLength int     `json:"context_length,omitempty"`
}

// Network says which download sources answered.
type Network struct {
	HuggingFace bool `json:"huggingface"`
	GitHub      bool `json:"github"`
	PyPI        bool `json:"pypi"`
}

// Python is an interpreter workers can run on.
type Python struct {
	Path    string `json:"path"`
	Version string `json:"version"` // "3.11.3"
	Venv    bool   `json:"venv"`    // the venv module works
}

// Pool returns the pool with that name.
func (c Census) Pool(name string) (Pool, bool) {
	for _, p := range c.Pools {
		if p.Name == name {
			return p, true
		}
	}
	return Pool{}, false
}

// HasFlag reports whether the CPU has an instruction-set flag.
func (c Census) HasFlag(flag string) bool {
	for _, f := range c.CPU.Flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

// GPUFor returns the GPU whose memory is the named pool.
func (c Census) GPUFor(pool string) (GPU, bool) {
	for _, g := range c.GPUs {
		if g.Pool == pool {
			return g, true
		}
	}
	return GPU{}, false
}
