package selector

import (
	"bytes"
	"strings"
	"testing"

	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/store"
)

func cat(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func emptyStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func noPrograms(string) bool { return false }

var online = census.Network{HuggingFace: true, GitHub: true, PyPI: true}
var python = &census.Python{Path: "python3", Version: "3.12.3", Venv: true}

// thisLaptop is the repo's own 8 GB laptop as WSL sees it: a 4.9 GB VM with
// 3.4 GB free, an Ollama holding the models Decurix pulled, and a ~17 GB/s copy test.
func thisLaptop() census.Census {
	return census.Census{
		Host:  census.Host{OS: "linux", Arch: "amd64", WSL: true},
		CPU:   census.CPU{Model: "Intel i5-10210U", PhysicalCores: 3, LogicalCores: 6, Flags: []string{"avx", "avx2", "fma", "f16c"}},
		Pools: []census.Pool{{Name: "host", Kind: "ram", TotalGB: 4.92, AvailableGB: 3.4}},
		Disk:  census.Disk{FreeGB: 75},
		Bench: &census.Bench{MemCopyGBps: 17},
		Ollama: &census.Ollama{URL: "http://127.0.0.1:11434", Pool: "host", Models: []census.OllamaModel{
			{Name: "qwen3-vl:2b-instruct", SizeGB: 1.93}, {Name: "nomic-embed-text:latest", SizeGB: 0.27},
			{Name: "qwen2.5-coder:3b", SizeGB: 1.93}, {Name: "qwen3:8b", SizeGB: 5.2},
		}},
		Network: online, Python: python,
	}
}

func TestThisLaptopGetsOneSmallVisionModelForEverything(t *testing.T) {
	p := Select(thisLaptop(), cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	brain, ok := p.Pick(catalog.RoleBrain)
	if !ok {
		t.Fatalf("no brain: %+v", p.Missing)
	}
	if brain.Model != "qwen3.5-2b" || brain.Source.Runtime != catalog.RuntimeLlamaCpp {
		t.Fatalf("brain = %s via %s, want qwen3.5-2b on llama.cpp\n%s", brain.Model, brain.Source.Label(), render(p))
	}
	vision, _ := p.Pick(catalog.RoleVision)
	ground, _ := p.Pick(catalog.RoleGround)
	if vision.Model != brain.Model || ground.Model != brain.Model {
		t.Fatalf("vision %s / ground %s should share the brain's model on a small machine", vision.Model, ground.Model)
	}
	if brain.MemGB > 2.9 {
		t.Fatalf("brain needs %.2f GB, over the 2.9 GB budget", brain.MemGB)
	}
	if brain.Speed < 3 {
		t.Fatalf("brain speed %.1f tok/s under the floor", brain.Speed)
	}
	// The installed Ollama copy of qwen3-vl 2B is too big at 8k context here.
	found := false
	for _, r := range brain.Rejected {
		if r.Model == "qwen3-vl-2b-instruct" && strings.Contains(r.Source, "ollama") && strings.Contains(r.Why, "needs") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the Ollama qwen3-vl 2B to be rejected for memory\n%s", render(p))
	}
	// The non-commercial coder is refused by licence, not chosen.
	for _, r := range brain.Rejected {
		if r.Model == "qwen2.5-coder-3b" && !strings.Contains(r.Why, "licence") {
			t.Errorf("qwen2.5-coder-3b rejected for %q, want the licence", r.Why)
		}
	}
	ocr, ok := p.Pick(catalog.RoleOCR)
	if !ok || ocr.Model != "rapidocr-ppocr" {
		t.Fatalf("ocr = %+v", ocr)
	}
	asr, ok := p.Pick(catalog.RoleASR)
	if !ok {
		t.Fatalf("no asr\n%s", render(p))
	}
	if _, ok := p.Pick(catalog.RoleVAD); !ok {
		t.Fatalf("asr chosen without vad")
	}
	t.Logf("asr: %s (swaps=%v)", asr.Model, asr.Swaps)
	if p.LlamaCpp == nil || p.LlamaCpp.Variant != "cpu" || !strings.Contains(p.LlamaCpp.Asset, "ubuntu-x64") {
		t.Fatalf("llama.cpp = %+v", p.LlamaCpp)
	}
	if len(p.Pip) == 0 {
		t.Fatalf("pip groups missing")
	}
	t.Log("\n" + render(p))
}

func TestUnifiedMemoryBoxGetsTheBigModel(t *testing.T) {
	cs := census.Census{
		Host:    census.Host{OS: "darwin", Arch: "arm64"},
		CPU:     census.CPU{PhysicalCores: 16, LogicalCores: 16, Flags: []string{"neon", "dotprod"}},
		Pools:   []census.Pool{{Name: "host", Kind: "unified", TotalGB: 128, AvailableGB: 100}},
		GPUs:    []census.GPU{{Vendor: "apple", Name: "M4 Max", Unified: true, Backends: []string{"metal"}, Pool: "host"}},
		Disk:    census.Disk{FreeGB: 900},
		Network: online, Python: python,
	}
	p := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	brain, _ := p.Pick(catalog.RoleBrain)
	if brain.Model != "qwen3.8-27b" || !brain.OnGPU {
		t.Fatalf("brain = %s (gpu %v)\n%s", brain.Model, brain.OnGPU, render(p))
	}
	if p.LlamaCpp == nil || p.LlamaCpp.Variant != "metal" {
		t.Fatalf("llama.cpp = %+v", p.LlamaCpp)
	}
	// A 27B brain already beats the math, code and SQL specialists; only
	// medicine and translation specialists are still better than it.
	for _, d := range []string{"math", "code", "sql"} {
		if k, ok := p.Pick(d); ok {
			t.Errorf("%s specialist %s chosen beside a 27B brain", d, k.Model)
		}
	}
	if k, ok := p.Pick("medicine"); !ok || k.Model != "hulu-med-27b" {
		t.Errorf("medicine = %+v", k)
	}
	if k, ok := p.Pick("translate"); !ok || k.Model != "hy-mt2-30b-a3b" {
		t.Errorf("translate = %+v", k)
	}
}

func TestGPUBoxPutsTheBrainInVRAM(t *testing.T) {
	cs := census.Census{
		Host:    census.Host{OS: "linux", Arch: "amd64"},
		CPU:     census.CPU{PhysicalCores: 8, LogicalCores: 16, Flags: []string{"avx2", "fma"}},
		Pools:   []census.Pool{{Name: "host", Kind: "ram", TotalGB: 64, AvailableGB: 50}, {Name: "gpu0", Kind: "vram", TotalGB: 24, AvailableGB: 23.5}},
		GPUs:    []census.GPU{{Vendor: "nvidia", Name: "RTX 4090", VRAMTotalGB: 24, VRAMFreeGB: 23.5, Backends: []string{"cuda", "vulkan"}, Pool: "gpu0"}},
		Disk:    census.Disk{FreeGB: 500},
		Network: online, Python: python,
	}
	p := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	brain, _ := p.Pick(catalog.RoleBrain)
	if !brain.OnGPU || brain.Pool != "gpu0" {
		t.Fatalf("brain %s not on the GPU\n%s", brain.Model, render(p))
	}
	if brain.Model != "qwen3.8-27b" {
		t.Fatalf("brain = %s, want qwen3.8-27b on a 24 GB card\n%s", brain.Model, render(p))
	}
	if p.LlamaCpp == nil || p.LlamaCpp.Variant != "vulkan" {
		t.Fatalf("llama.cpp = %+v", p.LlamaCpp)
	}
}

func TestOfflineMachineUsesOnlyWhatIsInstalled(t *testing.T) {
	cs := thisLaptop()
	cs.Network = census.Network{}
	cs.Pools[0].AvailableGB = 4.5 // room for the installed Ollama VLM
	p := Select(cs, cat(t), emptyStore(t), Policy{Offline: true, HaveProgram: func(n string) bool { return n == "tesseract" }})
	if p.DownloadGB != 0 {
		t.Fatalf("offline plan downloads %.2f GB\n%s", p.DownloadGB, render(p))
	}
	brain, ok := p.Pick(catalog.RoleBrain)
	if !ok || !brain.Installed {
		t.Fatalf("brain %+v\n%s", brain, render(p))
	}
	if ocr, ok := p.Pick(catalog.RoleOCR); !ok || ocr.Model != "tesseract" {
		t.Fatalf("ocr = %+v\n%s", ocr, render(p))
	}
}

func TestNoRoomMeansNoBrainAndSaysSo(t *testing.T) {
	cs := thisLaptop()
	cs.Pools[0].AvailableGB = 1.0
	p := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	if _, ok := p.Pick(catalog.RoleBrain); ok {
		t.Fatalf("a brain was chosen with 0.5 GB to give\n%s", render(p))
	}
	if len(p.Missing) == 0 || !strings.Contains(strings.Join(p.Warnings, " "), "remote brain") {
		t.Fatalf("missing/warnings do not explain\n%s", render(p))
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	a := render(Select(thisLaptop(), cat(t), emptyStore(t), Policy{HaveProgram: noPrograms}))
	b := render(Select(thisLaptop(), cat(t), emptyStore(t), Policy{HaveProgram: noPrograms}))
	if a != b {
		t.Fatalf("two runs differ:\n%s\n---\n%s", a, b)
	}
}

func TestPreviousChoiceIsKeptWhenNotClearlyWorse(t *testing.T) {
	cs := thisLaptop()
	first := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms})
	again := Select(cs, cat(t), emptyStore(t), Policy{HaveProgram: noPrograms, Previous: first})
	a, _ := first.Pick(catalog.RoleBrain)
	b, _ := again.Pick(catalog.RoleBrain)
	if a.Model != b.Model || a.Source.Label() != b.Source.Label() {
		t.Fatalf("re-planning moved the brain from %s to %s", a.Model, b.Model)
	}
}

func render(p *Plan) string {
	var buf bytes.Buffer
	p.Render(&buf, true)
	return buf.String()
}
