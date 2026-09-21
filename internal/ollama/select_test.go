package ollama

import (
	"math"
	"testing"
)

func names(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func inst(name string, sizeGB, paramsB float64) Candidate {
	return Candidate{Name: name, SizeGB: sizeGB, ParamsB: paramsB, Installed: true}
}

// The budget is exactly the user's rule: 80% of what is free.
func TestBudgetIsEightyPercentOfWhatIsFree(t *testing.T) {
	b := BudgetFor(Resources{RAMFreeGB: 10, Cores: 10, CPUBusy: 0.5, VRAMFreeGB: 20})

	if want := 10*BudgetFraction - embedReserveGB; math.Abs(b.RAMGB-want) > 1e-9 {
		t.Errorf("RAM budget = %.3f, want %.3f", b.RAMGB, want)
	}
	if want := 20*BudgetFraction - embedReserveGB; math.Abs(b.VRAMGB-want) > 1e-9 {
		t.Errorf("VRAM budget = %.3f, want %.3f", b.VRAMGB, want)
	}
	// 10 cores, half of them busy elsewhere, 80% of the rest.
	if want := 4.0; math.Abs(b.Cores-want) > 1e-9 {
		t.Errorf("cores budget = %.3f, want %.3f", b.Cores, want)
	}
}

// A machine with a big free GPU takes the biggest dense model that fits it.
func TestBigGPUPicksTheSmartestThatFitsVRAM(t *testing.T) {
	sel := Choose(Resources{RAMTotalGB: 32, RAMFreeGB: 24, Cores: 8, VRAMTotalGB: 24, VRAMFreeGB: 24, GPUName: "RTX"}, nil, true, "")

	if sel.Model.Name != "devstral-small-2:24b" || !sel.OnGPU || !sel.Fits {
		t.Fatalf("picked %q (gpu=%v fits=%v), want devstral-small-2:24b on the GPU", sel.Model.Name, sel.OnGPU, sel.Fits)
	}
}

// On a CPU, decoding speed limits size: a 32B dense model would fit in 64 GB
// but crawl, so the mixture-of-experts model that reads ~2 GB per token wins.
func TestCPUOnlyPrefersFastMixtureOfExperts(t *testing.T) {
	sel := Choose(Resources{RAMTotalGB: 64, RAMFreeGB: 60, Cores: 16}, nil, true, "")

	if sel.Model.Name != "qwen3:30b" {
		t.Fatalf("picked %q, want the MoE qwen3:30b (dense 14B/32B are too slow on CPU)", sel.Model.Name)
	}
}

// Other work on the machine shrinks the pick: same hardware, less free.
func TestPickFollowsCurrentUsage(t *testing.T) {
	idle := Choose(Resources{RAMTotalGB: 16, RAMFreeGB: 14, Cores: 8}, nil, true, "")
	busy := Choose(Resources{RAMTotalGB: 16, RAMFreeGB: 4, Cores: 8, CPUBusy: 0.75}, nil, true, "")

	if busy.Model.Quality() >= idle.Model.Quality() {
		t.Errorf("busy machine picked %s, no smaller than idle machine's %s", busy.Model.Name, idle.Model.Name)
	}
}

// An installed model nearly as good as the best wins over a download; a much
// worse one does not.
func TestInstalledPreferredUnlessMuchWorse(t *testing.T) {
	// 16 cores: an 8B model decodes at ~3.7 tokens/s, above the floor.
	res := Resources{RAMTotalGB: 16, RAMFreeGB: 12, Cores: 16}

	near := Choose(res, []Candidate{inst("qwen3:8b", 5.2, 8.2)}, true, "")
	if near.Model.Name != "qwen3:8b" || !near.Model.Installed {
		t.Errorf("picked %q, want the installed qwen3:8b", near.Model.Name)
	}

	far := Choose(res, []Candidate{inst("qwen2.5-coder:1.5b", 1.0, 1.5)}, true, "")
	if far.Model.Installed {
		t.Errorf("picked installed %q over a much better download", far.Model.Name)
	}
}

// Without permission to pull, only installed models are used, and the better
// one is reported.
func TestCannotPullUsesInstalledAndReportsBest(t *testing.T) {
	res := Resources{RAMTotalGB: 16, RAMFreeGB: 12, Cores: 8}
	sel := Choose(res, []Candidate{inst("qwen2.5-coder:1.5b", 1.0, 1.5)}, false, "")

	if sel.Model.Name != "qwen2.5-coder:1.5b" {
		t.Errorf("picked %q, want the only installed model", sel.Model.Name)
	}
	if sel.Best.Name == sel.Model.Name || sel.Best.Installed {
		t.Errorf("best = %+v, want a better model that is not installed", sel.Best)
	}
}

// Nothing fits: the smallest usable model runs anyway and the selection says so.
func TestNothingFitsFallsBackToSmallest(t *testing.T) {
	sel := Choose(Resources{RAMTotalGB: 2, RAMFreeGB: 1, Cores: 2}, nil, true, "")

	if sel.Fits {
		t.Error("Fits = true for a machine with 1 GB free")
	}
	if sel.Model.Name != "qwen2.5-coder:1.5b" {
		t.Errorf("picked %q, want the smallest usable model", sel.Model.Name)
	}
}

// A small change in load must not swap the model in use.
func TestCurrentModelIsKeptUnlessClearlyBetter(t *testing.T) {
	res := Resources{RAMTotalGB: 16, RAMFreeGB: 12, Cores: 16}
	installed := []Candidate{inst("qwen2.5-coder:7b", 4.7, 7.6), inst("qwen3:8b", 5.2, 8.2)}

	if got := Choose(res, installed, false, "qwen2.5-coder:7b").Model.Name; got != "qwen2.5-coder:7b" {
		t.Errorf("switched to %q for a <25%% gain", got)
	}

	installed = append(installed, inst("qwen3:14b", 9.3, 14.8))
	res.VRAMTotalGB, res.VRAMFreeGB = 16, 16 // a GPU lets the 14B model run
	if got := Choose(res, installed, false, "qwen2.5-coder:7b").Model.Name; got != "qwen3:14b" {
		t.Errorf("stayed on qwen2.5-coder:7b, want a switch to qwen3:14b (much better)")
	}

	// A current model that no longer fits is never kept.
	tight := Resources{RAMTotalGB: 8, RAMFreeGB: 3, Cores: 8}
	if got := Choose(tight, installed, false, "qwen3:14b").Model.Name; got == "qwen3:14b" {
		t.Errorf("kept %q which does not fit", got)
	}
}

func TestQualityMixtureOfExpertsIsBetweenActiveAndTotal(t *testing.T) {
	moe := Candidate{Name: "x", ParamsB: 30, ActiveB: 3}
	q := moe.Quality()
	if q <= 3 || q >= 30 {
		t.Errorf("MoE quality = %.1f, want between active (3) and total (30)", q)
	}
}

func TestFitReportsWhy(t *testing.T) {
	b := BudgetFor(Resources{RAMFreeGB: 4, Cores: 4})
	if f := FitFor(Candidate{Name: "big", SizeGB: 20, ParamsB: 32}, b); f.OK || f.Reason == "" {
		t.Errorf("fit = %+v, want a refusal with a reason", f)
	}
}

func TestParseParamSize(t *testing.T) {
	for in, want := range map[string]float64{"7.6B": 7.6, "494.03M": 0.49403, "": 0, "??": 0} {
		if got := parseParamSize(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("parseParamSize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEmbeddingModelsAreNotChatCandidates(t *testing.T) {
	if !isEmbedding("nomic-embed-text:latest", "nomic-bert", nil) || !isEmbedding("x", "", []string{"bert"}) {
		t.Error("embedding model not recognised")
	}
	if isEmbedding("qwen3:8b", "qwen3", nil) {
		t.Error("chat model taken for an embedding model")
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	name, total, free, _, ok := parseNvidiaSMI("NVIDIA GeForce RTX 4090, 24564, 20000\nNVIDIA GeForce RTX 4090, 24564, 10240\n")
	if !ok || name != "NVIDIA GeForce RTX 4090 x2" {
		t.Fatalf("name = %q ok = %v", name, ok)
	}
	if math.Abs(total-48) > 0.1 || math.Abs(free-(20000+10240)/1024.0) > 1e-9 {
		t.Errorf("total = %.2f free = %.2f", total, free)
	}
	if _, _, _, _, ok := parseNvidiaSMI("NVIDIA-SMI has failed"); ok {
		t.Error("garbage parsed as a GPU")
	}
}

func TestBusyFraction(t *testing.T) {
	if got := busyFraction(100, 200, 150, 300); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("busy = %v, want 0.5", got)
	}
	if got := busyFraction(0, 0, 0, 0); got != 0 {
		t.Errorf("no elapsed time: busy = %v, want 0", got)
	}
}

func TestIsLocalURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://host.docker.internal:11434": true,
		"http://localhost:11434":            true,
		"http://127.0.0.1:11434":            true,
		"http://decurix-ollama:11434":       true,
		"http://gpu-box.example.com:11434":  false,
		"http://203.0.113.9:11434":          false,
	} {
		if got := isLocalURL(raw); got != want {
			t.Errorf("isLocalURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// OLLAMA_MODEL wins; unset or "auto" defers to the selection once enabled.
func TestDesktopModelPinnedAutoAndDefault(t *testing.T) {
	auto.mu.Lock()
	savedOn, savedSel := auto.enabled, auto.sel
	auto.mu.Unlock()
	defer func() {
		auto.mu.Lock()
		auto.enabled, auto.sel = savedOn, savedSel
		auto.mu.Unlock()
	}()

	auto.mu.Lock()
	auto.enabled, auto.sel = true, &Selection{Model: Candidate{Name: "qwen3:8b"}}
	auto.mu.Unlock()

	t.Setenv("OLLAMA_MODEL", "")
	if got := DesktopModel(); got != "qwen3:8b" {
		t.Errorf("unset: %q, want the selected qwen3:8b", got)
	}
	t.Setenv("OLLAMA_MODEL", "AUTO")
	if got := DesktopModel(); got != "qwen3:8b" {
		t.Errorf("auto: %q, want the selected qwen3:8b", got)
	}
	t.Setenv("OLLAMA_MODEL", "llama3.2:1b")
	if got := DesktopModel(); got != "llama3.2:1b" {
		t.Errorf("pinned: %q, want llama3.2:1b", got)
	}
}

// A machine that decodes faster than assumed can say so without a rebuild.
func TestSpeedEstimatesCanBeCorrectedFromTheEnvironment(t *testing.T) {
	res := Resources{RAMTotalGB: 16, RAMFreeGB: 12, Cores: 8}
	m := Candidate{Name: "qwen3:8b", SizeGB: 5.2, ParamsB: 8.2}

	if FitFor(m, BudgetFor(res)).OK {
		t.Fatal("an 8B model on 8 cores should be too slow at the default estimate")
	}

	t.Setenv("DECURIX_MODEL_CORE_GBPS", "4")
	if !FitFor(m, BudgetFor(res)).OK {
		t.Error("DECURIX_MODEL_CORE_GBPS=4 did not let the 8B model through")
	}

	t.Setenv("DECURIX_MODEL_MIN_TPS", "100")
	if FitFor(m, BudgetFor(res)).OK {
		t.Error("DECURIX_MODEL_MIN_TPS=100 did not tighten the floor")
	}
}
