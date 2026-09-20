package ollama

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The numbers below are estimates, not measurements of a particular machine.
// They are here so the choice is explainable and testable; the comments say
// what each one stands for.
const (
	// BudgetFraction is how much of what is free a model may take: the user's
	// own rule, (total - used by everything else) * 0.8. The rest stays with
	// the desktops, the browser and the OS.
	BudgetFraction = 0.8

	// embedReserveGB is held back for the embedding model the thebot harness
	// loads next to the chat model.
	embedReserveGB = 0.4

	// defaultPerCoreGBps is the memory traffic one core can stream through a
	// quantised model. CPU decoding is memory-bound: each token reads the
	// weights that are active for it once, so tokens/s ~ cores * this / GB
	// touched per token. ~1.5 matches this repo's own measurements (a 1.9 GB
	// 3B model on an ordinary 8-thread laptop: a few tokens per second).
	defaultPerCoreGBps = 1.5

	// defaultMinTokensPerSecond is the slowest decode an agent loop stays
	// usable at. A task is a dozen model turns; below this a desktop feels
	// dead.
	defaultMinTokensPerSecond = 3.0

	// minParamsB is the floor below which a model cannot follow the agent's
	// COMMAND/DONE/HELP contract (see Model's comment: 0.6B did not).
	minParamsB = 1.0

	// keepInstalledRatio: an installed model within this fraction of the best
	// possible one's quality is preferred over pulling a bigger download.
	keepInstalledRatio = 0.75

	// switchRatio is how much better a fresh pick must be than the one
	// already in use before it replaces it, so a small change in load never
	// swaps a model out from under desktops that already run it.
	switchRatio = 1.25
)

// perCoreGBps and minTokensPerSecond are the two speed estimates above, which
// a machine that decodes faster or slower than assumed can correct without a
// rebuild: FLOWERA_MODEL_CORE_GBPS and FLOWERA_MODEL_MIN_TPS.
func perCoreGBps() float64 { return envFloat("FLOWERA_MODEL_CORE_GBPS", defaultPerCoreGBps) }

func minTokensPerSecond() float64 {
	return envFloat("FLOWERA_MODEL_MIN_TPS", defaultMinTokensPerSecond)
}

func envFloat(key string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil && v > 0 {
		return v
	}

	return fallback
}

// Candidate is a chat model the selector can weigh.
type Candidate struct {
	Name      string  `json:"name"`
	SizeGB    float64 `json:"size_gb"`
	ParamsB   float64 `json:"params_b"`
	ActiveB   float64 `json:"active_b,omitempty"` // mixture-of-experts: parameters used per token; 0 = dense
	Installed bool    `json:"installed"`
}

// catalog is the set of models the selector may recommend, or pull into the
// bundled container, beyond whatever is already installed. Sizes are the
// default (4-bit) downloads on the Ollama library.
var catalog = []Candidate{
	{Name: "qwen2.5-coder:1.5b", SizeGB: 1.0, ParamsB: 1.5},
	{Name: "llama3.2:3b", SizeGB: 2.0, ParamsB: 3.2},
	{Name: "qwen2.5-coder:3b", SizeGB: 1.9, ParamsB: 3.1},
	{Name: "qwen3:4b", SizeGB: 2.5, ParamsB: 4.0},
	{Name: "qwen2.5-coder:7b", SizeGB: 4.7, ParamsB: 7.6},
	{Name: "qwen3:8b", SizeGB: 5.2, ParamsB: 8.2},
	{Name: "qwen2.5-coder:14b", SizeGB: 9.0, ParamsB: 14.8},
	{Name: "qwen3:14b", SizeGB: 9.3, ParamsB: 14.8},
	{Name: "gpt-oss:20b", SizeGB: 13, ParamsB: 21, ActiveB: 3.6},
	{Name: "devstral-small-2:24b", SizeGB: 15, ParamsB: 24},
	{Name: "qwen3:30b", SizeGB: 19, ParamsB: 30.5, ActiveB: 3.3},
	{Name: "qwen2.5-coder:32b", SizeGB: 20, ParamsB: 32.8},
	{Name: "qwen3:32b", SizeGB: 20, ParamsB: 32.8},
}

// familyFactor scales a model's size-based score by how well its family does
// the work an agent does (following a strict reply format, running commands,
// writing code). A judgement, kept in one place.
func familyFactor(name string) float64 {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "gpt-oss"):
		return 1.3
	case strings.HasPrefix(n, "qwen3"):
		return 1.15
	case strings.HasPrefix(n, "devstral"):
		return 1.1
	case strings.HasPrefix(n, "qwen2.5-coder"), strings.HasPrefix(n, "qwen3-coder"):
		return 1.05
	case strings.HasPrefix(n, "llama"), strings.HasPrefix(n, "mistral"), strings.HasPrefix(n, "phi"):
		return 0.9
	}

	return 1.0
}

// Quality ranks how capable a model is, in "billions of parameters of a plain
// dense model". A mixture-of-experts model counts as the geometric mean of its
// total and active parameters, the usual rule of thumb for its dense
// equivalent.
func (m Candidate) Quality() float64 {
	eff := m.ParamsB
	if m.ActiveB > 0 {
		eff = math.Sqrt(m.ParamsB * m.ActiveB)
	}

	return eff * familyFactor(m.Name)
}

// activeGB is the weight data read per generated token.
func (m Candidate) activeGB() float64 {
	if m.ActiveB > 0 && m.ParamsB > 0 {
		return m.SizeGB * m.ActiveB / m.ParamsB
	}

	return m.SizeGB
}

// NeedGB is the memory the model takes while running at NumCtx: the weights,
// plus the KV cache and runtime buffers, which grow with the model.
func (m Candidate) NeedGB() float64 {
	return m.SizeGB + 0.5 + 0.06*m.ParamsB
}

// Budget is what the model may use: BudgetFraction of the free resources.
type Budget struct {
	RAMGB  float64 `json:"ram_gb"`
	VRAMGB float64 `json:"vram_gb,omitempty"`
	Cores  float64 `json:"cores"`
	Basis  string  `json:"basis"` // "gpu" or "cpu": what the pick runs on
}

// BudgetFor applies BudgetFraction to the free resources.
func BudgetFor(r Resources) Budget {
	freeCores := float64(r.Cores) * (1 - r.CPUBusy)
	b := Budget{
		RAMGB: math.Max(0, r.RAMFreeGB*BudgetFraction-embedReserveGB),
		Cores: math.Max(1, freeCores*BudgetFraction),
		Basis: "cpu",
	}

	if r.HasGPU() {
		b.VRAMGB = math.Max(0, r.VRAMFreeGB*BudgetFraction-embedReserveGB)
	}

	return b
}

// Fit says how a model would run within a budget.
type Fit struct {
	OK     bool    // fits and is fast enough
	OnGPU  bool    // runs entirely in VRAM
	TPS    float64 // estimated tokens per second on CPU (0 on a GPU)
	Reason string  // why not, when !OK
}

// FitFor decides whether m runs well within b. A model that fits the free
// VRAM runs on the GPU and is not speed-limited. Otherwise it must fit the RAM
// budget and decode at least minTokensPerSecond on the free cores. A model
// split between GPU and CPU is not modelled: it counts as CPU-only, which
// errs toward a smaller pick.
func FitFor(m Candidate, b Budget) Fit {
	need := m.NeedGB()

	if b.VRAMGB > 0 && need <= b.VRAMGB {
		return Fit{OK: true, OnGPU: true}
	}

	if need > b.RAMGB {
		return Fit{Reason: fmt.Sprintf("needs %.1f GB, budget is %.1f GB", need, b.RAMGB)}
	}

	tps := b.Cores * perCoreGBps() / m.activeGB()
	if tps < minTokensPerSecond() {
		return Fit{TPS: tps, Reason: fmt.Sprintf("~%.1f tokens/s on the free CPU, too slow", tps)}
	}

	return Fit{OK: true, TPS: tps}
}

// Selection is the outcome of a choice.
type Selection struct {
	Model    Candidate   `json:"model"` // what to run
	Fits     bool        `json:"fits"`  // false: nothing fit, this is the smallest there is
	OnGPU    bool        `json:"on_gpu"`
	TPS      float64     `json:"est_tokens_per_second,omitempty"`
	Best     Candidate   `json:"best"`      // the best fit of everything considered, installed or not
	AlsoFits []Candidate `json:"also_fits"` // other installed models that fit too, best first
	Reason   string      `json:"reason"`
	Budget   Budget      `json:"budget"`
}

// Choose picks the smartest model that fits res.
//
// installed are the chat models the Ollama server already has. canPull says a
// model that is not installed may be fetched (the bundled container): then a
// catalog model can win, unless an installed one is nearly as good, in which
// case skipping a multi-gigabyte download is worth the small loss. When it
// cannot pull, only installed models are used, and the better catalog model
// is reported as Best so a person can choose to pull it.
//
// current names the model already in use, if any. It is kept while it still
// fits unless the new pick is switchRatio times better, so a passing change in
// load does not swap a model out from under desktops that already run it.
func Choose(res Resources, installed []Candidate, canPull bool, current string) Selection {
	b := BudgetFor(res)
	if b.VRAMGB > 0 {
		b.Basis = "gpu"
	}

	have := map[string]bool{}
	for i := range installed {
		installed[i].Installed = true
		have[normName(installed[i].Name)] = true
	}

	all := append([]Candidate(nil), installed...)
	for _, c := range catalog {
		if !have[normName(c.Name)] {
			all = append(all, c)
		}
	}

	bestAll, okAll := bestFitting(all, b)
	bestInst, okInst := bestFitting(installed, b)

	sel := Selection{Budget: b}

	switch {
	case okInst && (!canPull || !okAll || bestInst.Quality() >= keepInstalledRatio*bestAll.Quality()):
		sel.Model, sel.Fits = bestInst, true
	case okAll && canPull:
		sel.Model, sel.Fits = bestAll, true
	default:
		// Nothing fits. Run the smallest usable model rather than none, and
		// say so; a slow answer beats no answer.
		pool := installed
		if canPull || len(pool) == 0 {
			pool = all
		}
		sel.Model = smallest(pool)
	}

	if current != "" && sel.Model.Name != "" && normName(current) != normName(sel.Model.Name) {
		for _, c := range all {
			if normName(c.Name) == normName(current) && FitFor(c, b).OK &&
				sel.Model.Quality() < switchRatio*c.Quality() &&
				(c.Installed || canPull) {
				sel.Model, sel.Fits = c, true
				break
			}
		}
	}

	if okAll {
		sel.Best = bestAll
	} else {
		sel.Best = sel.Model
	}

	fit := FitFor(sel.Model, b)
	sel.OnGPU, sel.TPS = fit.OnGPU, fit.TPS

	for _, m := range rankFitting(installed, b) {
		if m.Name != sel.Model.Name {
			sel.AlsoFits = append(sel.AlsoFits, m)
		}
	}

	sel.Reason = explain(sel, res)

	return sel
}

// bestFitting is the highest-quality model that fits; ties go to the smaller.
func bestFitting(models []Candidate, b Budget) (Candidate, bool) {
	r := rankFitting(models, b)
	if len(r) == 0 {
		return Candidate{}, false
	}

	return r[0], true
}

// rankFitting returns the usable models that fit, best first.
func rankFitting(models []Candidate, b Budget) []Candidate {
	var out []Candidate
	for _, m := range models {
		if m.ParamsB < minParamsB || !FitFor(m, b).OK {
			continue
		}
		out = append(out, m)
	}

	sort.SliceStable(out, func(i, j int) bool {
		qi, qj := out[i].Quality(), out[j].Quality()
		if qi != qj {
			return qi > qj
		}
		return out[i].SizeGB < out[j].SizeGB
	})

	return out
}

// smallest is the smallest usable model, or the smallest of any kind if none
// clears the floor.
func smallest(models []Candidate) Candidate {
	var best Candidate
	found := false
	for _, m := range models {
		if m.ParamsB < minParamsB {
			continue
		}
		if !found || m.SizeGB < best.SizeGB {
			best, found = m, true
		}
	}

	if !found && len(models) > 0 {
		best = models[0]
		for _, m := range models {
			if m.SizeGB < best.SizeGB {
				best = m
			}
		}
	}

	return best
}

func normName(n string) string { return strings.TrimSuffix(strings.ToLower(n), ":latest") }

// explain writes the one sentence that says why this model.
func explain(s Selection, r Resources) string {
	where := fmt.Sprintf("%.1f GB of RAM and %.1f free cores", s.Budget.RAMGB, s.Budget.Cores)
	if s.OnGPU {
		where = fmt.Sprintf("%.1f GB of VRAM on %s", s.Budget.VRAMGB, r.GPUName)
	}

	switch {
	case !s.Fits && s.Model.Name == "":
		return "No chat model is installed and none could be chosen."
	case !s.Fits:
		return fmt.Sprintf("Nothing fits comfortably in %s; %s is the smallest usable model.", where, s.Model.Name)
	case s.OnGPU:
		return fmt.Sprintf("Smartest model that fits %s (80%% of what is free).", where)
	}

	return fmt.Sprintf("Smartest model that fits %s (80%% of what is free), ~%.0f tokens/s.", where, s.TPS)
}
