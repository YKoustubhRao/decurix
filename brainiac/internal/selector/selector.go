// Package selector chooses, for one machine, the best model for each role:
// the census says what the machine has, the catalog says what each model
// needs and how good it is, and the policy says what is allowed. The answer is
// a Plan that explains every choice and every rejection, so the person can
// see why their laptop got a 2B model and their GPU box a 27B one.
//
// It is deterministic: the same census, catalog and policy always give the
// same plan. Nothing here downloads or loads anything.
//
// The numbers it uses are estimates with their reasons in the comments; a
// calibration run replaces the speed estimate with a measurement.
package selector

import (
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strings"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/gguf"
	"brainiac/internal/speed"
	"brainiac/internal/store"
)

// Schema is the version of the Plan JSON shape.
const Schema = 1

// Context is the context window every LLM-class model is planned at. It is
// fixed: Ollama treats one model at two window sizes as two loaded copies
// (minutes to reload on a small host), and desktops sharing that Ollama use
// 8192.
const Context = 8192

// The roles chosen together, because one vision-language model can fill all
// three, and on a small machine one resident model beats two that swap.
var heavyRoles = []string{catalog.RoleBrain, catalog.RoleVision, catalog.RoleGround}

// The roles chosen one by one after the heavy set.
var lightRoles = []string{catalog.RoleOCR, catalog.RoleASR, catalog.RoleVAD, catalog.RoleEmbed}

// Policy is what the person allows.
type Policy struct {
	// Licenses are the licence classes allowed; empty means permissive only.
	Licenses []string
	// MemFraction is the share of a pool's available memory the plan may use
	// (after ReserveGB). Default 1.
	MemFraction float64
	// ReserveGB is kept free in every pool; 0 means max(0.5 GB, 10% of the pool).
	ReserveGB float64
	// PlannedLoadGB is memory other work will need later (desktops not yet
	// started); it is taken off the host pool.
	PlannedLoadGB float64
	// DiskFraction is the share of free disk downloads may take. Default 0.5.
	DiskFraction float64
	// Offline allows only what is already installed.
	Offline bool
	// KeepInstalled: an installed choice within this fraction of the best
	// possible one's value is kept instead of downloading. Default 0.9.
	KeepInstalled float64
	// DecodeGBps is the measured weight bandwidth one token of decoding
	// reaches on the CPU (from calibration); 0 means estimate it.
	DecodeGBps float64
	// Previous is the last plan; its heavy choice is kept unless the new one
	// is SwitchRatio better. Default 1.1.
	Previous    *Plan
	SwitchRatio float64
	// HaveProgram reports whether a program is on PATH (tests replace it).
	HaveProgram func(name string) bool
	// Domains are the fields to look for specialists in; empty means every
	// field the catalog knows. "none" turns specialists off.
	Domains []string
	// DomainMargin is how much better than the brain (quality points) a
	// specialist must be in its field to be worth a download and a swap.
	// Default 5.
	DomainMargin float64
	// PromptPrefixes is how many conversations' prompt starts the brain's
	// server keeps in its host-memory prompt cache, so desktops that take
	// turns each find theirs instead of reading the whole prompt again.
	// 0 means 3; negative turns the cache off.
	PromptPrefixes int
}

// CtxCheckpoints is how many context checkpoints llama-server keeps per slot.
// A checkpoint is a copy of a hybrid model's recurrent state (about 20 MB for
// Qwen3.5-2B): llama.cpp's default of 32 would take over half a gigabyte
// nobody counted. A turn that only appends to the last prompt needs none; a
// few cover a reply the template rewrites.
const CtxCheckpoints = 3

// MaxCacheShare is the most of a pool's budget the prompt caches may take.
const MaxCacheShare = 0.2

func (p Policy) withDefaults() Policy {
	if len(p.Licenses) == 0 {
		p.Licenses = []string{catalog.LicensePermissive}
	}
	if p.MemFraction <= 0 || p.MemFraction > 1 {
		p.MemFraction = 1
	}
	if p.DiskFraction <= 0 || p.DiskFraction > 1 {
		p.DiskFraction = 0.5
	}
	if p.KeepInstalled <= 0 || p.KeepInstalled > 1 {
		p.KeepInstalled = 0.9
	}
	if p.SwitchRatio < 1 {
		p.SwitchRatio = 1.1
	}
	if p.HaveProgram == nil {
		p.HaveProgram = func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	}
	if p.DomainMargin <= 0 {
		p.DomainMargin = 5
	}
	if p.PromptPrefixes == 0 {
		p.PromptPrefixes = 3
	}
	return p
}

// Plan is the selector's answer.
type Plan struct {
	Schema         int       `json:"schema"`
	CatalogVersion string    `json:"catalog_version"`
	MadeAt         time.Time `json:"made_at"`
	Machine        string    `json:"machine"`
	Budgets        []Budget  `json:"budgets"`
	DiskBudgetGB   float64   `json:"disk_budget_gb"`
	DecodeGBps     float64   `json:"decode_gbps"`
	DecodeBasis    string    `json:"decode_basis"`
	Picks          []Pick    `json:"picks"`
	Missing        []Missing `json:"missing,omitempty"`
	// ByBrain lists the fields no specialist was chosen for: the brain (or
	// the remote brain) answers them, and Why says what was tried.
	ByBrain    []Missing `json:"by_brain,omitempty"`
	DownloadGB float64   `json:"download_gb"`
	// LlamaCpp is the llama-server build to fetch, when any pick runs on it.
	LlamaCpp *LlamaCpp `json:"llamacpp,omitempty"`
	// Pip lists the worker requirement groups to install.
	Pip      []string `json:"pip,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Budget is the memory the plan may use in one pool.
type Budget struct {
	Pool        string  `json:"pool"`
	AvailableGB float64 `json:"available_gb"`
	ReserveGB   float64 `json:"reserve_gb"`
	BudgetGB    float64 `json:"budget_gb"`
	// Unmeasured is set for a remote Ollama, whose memory is not ours to see.
	Unmeasured bool `json:"unmeasured,omitempty"`
}

// Pick is one role's choice.
type Pick struct {
	Role   string         `json:"role"`
	Model  string         `json:"model"`
	Title  string         `json:"title"`
	Source catalog.Source `json:"source"`
	Pool   string         `json:"pool"`
	OnGPU  bool           `json:"on_gpu,omitempty"`
	MemGB  float64        `json:"mem_gb"`
	// PromptCacheMB is the host-memory prompt cache its llama-server may
	// fill (--cache-ram), counted in MemGB. 0 turns that cache off.
	PromptCacheMB int     `json:"prompt_cache_mb,omitempty"`
	DiskGB        float64 `json:"disk_gb"`
	Download      float64 `json:"download_gb"`
	Installed     bool    `json:"installed"`
	Speed         float64 `json:"speed"`
	SpeedUnit     string  `json:"speed_unit"`
	Quality       float64 `json:"quality"`
	// Shared names the other roles the same model fills.
	Shared []string `json:"shared,omitempty"`
	// Swaps is set when this model cannot stay loaded beside the heavy set,
	// so using it unloads the brain for a while.
	Swaps bool `json:"swaps,omitempty"`
	// PrefillTPS is prompt tokens read per second here; WarmTurnSec and
	// ColdTurnSec are a desktop turn's seconds with its prompt start cached
	// and read from the start (WarmTurnTokens, ColdTurnTokens). SpeedBasis
	// says whether they were "measured" (brainiac bench) or "estimated".
	PrefillTPS  float64 `json:"prefill_tps,omitempty"`
	WarmTurnSec float64 `json:"warm_turn_sec,omitempty"`
	ColdTurnSec float64 `json:"cold_turn_sec,omitempty"`
	SpeedBasis  string  `json:"speed_basis,omitempty"`
	// AnswerSec estimates a reasoning model's time for a hard request here.
	AnswerSec float64  `json:"answer_sec,omitempty"`
	Why       string   `json:"why"`
	Rejected  []Reject `json:"rejected,omitempty"`
}

// Reject is a candidate that was not chosen, and why.
type Reject struct {
	Model  string `json:"model"`
	Source string `json:"source"`
	Why    string `json:"why"`
}

// Missing is a role no model could fill on this machine.
type Missing struct {
	Role     string   `json:"role"`
	Why      string   `json:"why"`
	Rejected []Reject `json:"rejected,omitempty"`
}

// LlamaCpp is the llama-server release to use.
type LlamaCpp struct {
	Repo    string `json:"repo"`
	Tag     string `json:"tag"`
	Variant string `json:"variant"` // cpu | vulkan | cuda | metal
	Asset   string `json:"asset"`
	// Extra are additional assets the variant needs (CUDA runtime DLLs).
	Extra []string `json:"extra,omitempty"`
	// SHA256 are the catalog's pins for the assets, by name (may be empty).
	SHA256 map[string]string `json:"sha256,omitempty"`
}

// Pick returns the pick for a role.
func (p *Plan) Pick(role string) (Pick, bool) {
	for _, k := range p.Picks {
		if k.Role == role {
			return k, true
		}
	}
	return Pick{}, false
}

// candidate is one (model, source) weighed for some roles.
type candidate struct {
	model     catalog.Model
	src       catalog.Source
	pool      string
	onGPU     bool
	memGB     float64
	download  float64
	installed bool
	// speed per role unit: tok/s for LLM roles, s/page or rtf for workers.
	speed float64
}

func (c candidate) key() string { return c.model.ID + "|" + c.src.Label() }

// Select makes the plan.
func Select(cs census.Census, cat *catalog.Catalog, st *store.Store, pol Policy) *Plan {
	pol = pol.withDefaults()
	s := &selection{cs: cs, cat: cat, st: st, pol: pol, budgets: map[string]*Budget{}}
	s.plan = &Plan{
		Schema:         Schema,
		CatalogVersion: cat.Version,
		MadeAt:         time.Now().UTC(),
		Machine:        machineLine(cs),
	}
	s.setBudgets()
	if st != nil {
		s.speeds, _ = speed.Load(st.Path(speed.File)) // none measured yet: estimates
	}
	s.setDecode()
	s.calibrateDecode()
	s.chooseHeavy()
	s.chooseLight()
	s.chooseDomains()
	s.sizePromptCaches()
	s.finish()
	return s.plan
}

type selection struct {
	cs      census.Census
	cat     *catalog.Catalog
	st      *store.Store
	pol     Policy
	plan    *Plan
	budgets map[string]*Budget
	// used is memory the chosen resident set takes per pool.
	used map[string]float64
	// diskLeft is what downloads may still take.
	diskLeft float64
	// speeds are what brainiac bench measured here (nil: nothing yet).
	speeds *speed.Table
	// headers are the installed model files' headers read so far, by path.
	headers map[string]*gguf.ModelInfo
	// chosen maps a model id to the candidate picked for it, so a model
	// filling two roles is downloaded and counted once.
	chosen map[string]candidate
}

func machineLine(cs census.Census) string {
	parts := []string{fmt.Sprintf("%s/%s", cs.Host.OS, cs.Host.Arch)}
	if cs.CPU.Model != "" {
		parts = append(parts, fmt.Sprintf("%s (%d cores, %d threads)", strings.TrimSpace(cs.CPU.Model), cs.CPU.PhysicalCores, cs.CPU.LogicalCores))
	}
	for _, g := range cs.GPUs {
		parts = append(parts, fmt.Sprintf("%s %s %.0f GB", g.Vendor, g.Name, g.VRAMTotalGB))
	}
	if cs.Host.WSL {
		parts = append(parts, "WSL")
	}
	return strings.Join(parts, ", ")
}

// setBudgets turns each pool's available memory into what the plan may use.
func (s *selection) setBudgets() {
	for _, p := range s.cs.Pools {
		reserve := s.pol.ReserveGB
		if reserve <= 0 {
			reserve = math.Max(0.5, 0.1*p.TotalGB)
		}
		avail := p.AvailableGB
		// What the local Ollama already holds is memory a model could have.
		if s.cs.Ollama != nil && s.cs.Ollama.Pool == p.Name {
			avail = math.Min(p.TotalGB, avail+s.cs.Ollama.LoadedGB)
		}
		if p.Name == "host" {
			avail -= s.pol.PlannedLoadGB
		}
		b := &Budget{Pool: p.Name, AvailableGB: round2(avail), ReserveGB: round2(reserve)}
		b.BudgetGB = round2(math.Max(0, (avail-reserve)*s.pol.MemFraction))
		s.budgets[p.Name] = b
		s.plan.Budgets = append(s.plan.Budgets, *b)
	}
	if s.cs.Ollama != nil && s.cs.Ollama.Pool == "remote" {
		b := &Budget{Pool: "remote", Unmeasured: true, BudgetGB: math.Inf(1)}
		s.budgets["remote"] = b
		s.plan.Budgets = append(s.plan.Budgets, Budget{Pool: "remote", Unmeasured: true})
		s.plan.Warnings = append(s.plan.Warnings, "the Ollama is on another machine: its memory is not checked")
	}
	s.diskLeft = math.Max(0, s.cs.Disk.FreeGB*s.pol.DiskFraction)
	s.plan.DiskBudgetGB = round2(s.diskLeft)
	s.used = map[string]float64{}
	s.chosen = map[string]candidate{}
}

// setDecode decides the CPU weight bandwidth one decoded token reaches.
// Decoding reads every active weight once per token, so tokens/s is this
// divided by the active weights' size. A copy benchmark overstates what a
// model reaches about two-fold (measured here: a 1.9 GB model at 4.5 tok/s on
// a machine whose copy test gives ~17 GB/s); with no benchmark, 1.5 GB/s per
// physical core is Decurix's measured rule of thumb.
func (s *selection) setDecode() {
	switch {
	case s.pol.DecodeGBps > 0:
		s.plan.DecodeGBps, s.plan.DecodeBasis = s.pol.DecodeGBps, "measured by calibration"
	case s.cs.Bench != nil && s.cs.Bench.MemCopyGBps > 0:
		s.plan.DecodeGBps = s.cs.Bench.MemCopyGBps * 0.5
		s.plan.DecodeBasis = fmt.Sprintf("half the %.1f GB/s copy benchmark", s.cs.Bench.MemCopyGBps)
	default:
		cores := s.cs.CPU.PhysicalCores
		if cores <= 0 {
			cores = max(1, s.cs.CPU.LogicalCores/2)
		}
		s.plan.DecodeGBps = 1.5 * float64(cores)
		s.plan.DecodeBasis = fmt.Sprintf("1.5 GB/s per core x %d cores (no benchmark)", cores)
	}
	s.plan.DecodeGBps = round2(s.plan.DecodeGBps)
}

func (s *selection) licenceOK(m catalog.Model) bool {
	for _, l := range s.pol.Licenses {
		if l == m.LicenseClass {
			return true
		}
	}
	return false
}

// evaluate decides whether one source of a model can run here, and how.
// It returns the candidate, or why not.
func (s *selection) evaluate(m catalog.Model, src catalog.Source) (candidate, string) {
	if !s.licenceOK(m) {
		return candidate{}, fmt.Sprintf("licence %s is %s (allow it with --licenses %s)", m.License, m.LicenseClass, m.LicenseClass)
	}
	for _, need := range m.Needs {
		if need == "gpu" && len(s.cs.GPUs) == 0 {
			return candidate{}, "needs a GPU"
		}
		if need != "gpu" && !s.cs.HasFlag(need) {
			return candidate{}, "needs the CPU feature " + need
		}
	}
	c := candidate{model: m, src: src}
	switch src.Runtime {
	case catalog.RuntimeOllama:
		if s.cs.Ollama == nil {
			return candidate{}, "no Ollama on this machine"
		}
		c.pool = s.cs.Ollama.Pool
		c.installed = s.ollamaHas(src.Tag)
		c.memGB = llmMem(m, src, 1.0)
	case catalog.RuntimeLlamaCpp:
		if llamaVariant(s.cs) == "" {
			return candidate{}, "no prebuilt llama-server for " + s.cs.Host.OS + "/" + s.cs.Host.Arch
		}
		c.installed = s.st != nil && s.st.HasFiles(src)
		// Brainiac's own server keeps the KV cache at q8_0 and one recurrent
		// state per slot with the model; its CtxCheckpoints copies of that
		// state live in host memory, so they count with a model on the CPU.
		f := s.llamaFacts(m, src, c.installed)
		c.memGB = llamaBaseGB(m, src) + (f.kvMB+f.stateMB)/1024
		c.pool, c.onGPU = s.llamaPool(c.memGB)
		if !c.onGPU {
			c.memGB += f.stateMB * CtxCheckpoints / 1024
		}
	case catalog.RuntimeWorker:
		if s.cs.Python == nil || !s.cs.Python.Venv {
			return candidate{}, "no Python 3.9+ with venv for the worker"
		}
		c.pool = "host"
		c.memGB = src.MemGB
		switch src.Kind {
		case catalog.SourceSystem:
			if !s.pol.HaveProgram(src.Program) {
				return candidate{}, src.Program + " is not installed"
			}
			c.installed = true
		case catalog.SourcePip:
			c.installed = s.st != nil && s.st.VenvReady(src.Pip)
		default:
			c.installed = s.st != nil && s.st.HasFiles(src)
		}
	}
	if !c.installed {
		if s.pol.Offline {
			return candidate{}, "not installed, and the plan is offline"
		}
		if reason := s.networkFor(src); reason != "" {
			return candidate{}, reason
		}
		c.download = src.SizeGB
	}
	b, ok := s.budgets[c.pool]
	if !ok {
		return candidate{}, "no memory figure for pool " + c.pool
	}
	if c.memGB > b.BudgetGB {
		return candidate{}, fmt.Sprintf("needs %.1f GB, the %s pool has %.1f GB to give", c.memGB, c.pool, b.BudgetGB)
	}
	return c, ""
}

func (s *selection) networkFor(src catalog.Source) string {
	switch src.Kind {
	case catalog.SourceHF:
		if !s.cs.Network.HuggingFace {
			return "Hugging Face is not reachable"
		}
	case catalog.SourceGitHub:
		if !s.cs.Network.GitHub {
			return "GitHub is not reachable"
		}
	case catalog.SourcePip:
		if !s.cs.Network.PyPI {
			return "PyPI is not reachable"
		}
	}
	return ""
}

func (s *selection) ollamaHas(tag string) bool {
	want := normTag(tag)
	for _, m := range s.cs.Ollama.Models {
		if normTag(m.Name) == want {
			return true
		}
	}
	return false
}

func normTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if !strings.Contains(t, ":") {
		t += ":latest"
	}
	return t
}

// llmMem is the memory an LLM-class model takes at its window: the file, the
// runtime's buffers (grow with size), and the KV cache (kvFactor 0.5 = q8_0).
func llmMem(m catalog.Model, src catalog.Source, kvFactor float64) float64 {
	if src.MemGB > 0 {
		return src.MemGB
	}
	overhead := 0.25 + 0.02*m.ParamsB
	kv := m.KVPerTokenMB * float64(ContextFor(m, src)) / 1024 * kvFactor
	return round2(src.SizeGB + overhead + kv)
}

// q8KV is the size of a q8_0 KV cache relative to f16: 34 bytes per 32
// values against 64.
const q8KV = 34.0 / 64

// memFacts are what a llama-server model keeps per sequence besides its
// weights: the KV cache at its window (q8_0) and one recurrent state, in MiB.
type memFacts struct {
	kvMB, stateMB float64
	// basis says where they come from: the catalog or the file's header.
	basis string
}

// prefixMB is what one cached prompt start can take in llama-server's prompt
// cache: a full window of KV plus the recurrent state and its checkpoints.
func (f memFacts) prefixMB() float64 { return f.kvMB + f.stateMB*(1+CtxCheckpoints) }

// catalogFacts are a model's memory facts as the catalog gives them.
func catalogFacts(m catalog.Model, src catalog.Source) memFacts {
	return memFacts{kvMB: m.KVPerTokenMB * float64(ContextFor(m, src)) * q8KV, stateMB: m.StateMB, basis: "catalog"}
}

// llamaBaseGB is a llama-server model's weights and runtime buffers.
func llamaBaseGB(m catalog.Model, src catalog.Source) float64 {
	if src.MemGB > 0 {
		return src.MemGB
	}
	return src.SizeGB + 0.25 + 0.02*m.ParamsB
}

// llamaFacts are an installed model's facts read from its own file header
// when the layout is one Brainiac knows how to count; otherwise, or for an
// unknown layout (an upper bound), never less than the catalog says.
func (s *selection) llamaFacts(m catalog.Model, src catalog.Source, installed bool) memFacts {
	f := catalogFacts(m, src)
	if !installed || s.st == nil || src.MemGB > 0 {
		return f
	}
	path := s.st.SourceFiles(src)["model"]
	if path == "" {
		return f
	}
	if s.headers == nil {
		s.headers = map[string]*gguf.ModelInfo{}
	}
	info, ok := s.headers[path]
	if !ok {
		info, _ = gguf.Info(path) // nil on a bad header: the catalog's facts stand
		s.headers[path] = info
	}
	if info == nil {
		return f
	}
	const mib = 1 << 20
	h := memFacts{kvMB: float64(info.KVBytes(ContextFor(m, src), 34.0/32)) / mib, stateMB: float64(info.StateBytes()) / mib, basis: "file header"}
	if !info.LayoutVerified {
		h.kvMB, h.stateMB = math.Max(h.kvMB, f.kvMB), math.Max(h.stateMB, f.stateMB)
	}
	return h
}

// prefixesFor is how many prompt starts a model's server should keep: the
// heavy models serve the desktops' long conversations, an embedding model
// has none, and a field specialist answers one question at a time.
func (s *selection) prefixesFor(m catalog.Model) int {
	if s.pol.PromptPrefixes < 0 {
		return 0
	}
	if len(heavyOf(m)) > 0 {
		return s.pol.PromptPrefixes
	}
	if _, embed := m.Roles[catalog.RoleEmbed]; embed && len(m.Roles) == 1 {
		return 0
	}
	return 1
}

// sizePromptCaches gives each chosen llama-server model a host-memory prompt
// cache and counts it, once every role is chosen. llama.cpp's own default is
// 8 GiB per server, more than a small machine has. Here a cache holds the
// prompt starts its model needs and only memory nothing chosen uses: the
// models that stay loaded together come first, so a cache never pushes the
// OCR worker out from beside the brain. It takes at most MaxCacheShare of
// the pool. On a GPU the cache would sit in host memory while the model is
// counted on the GPU, so it is off there for now.
func (s *selection) sizePromptCaches() {
	idx := map[string][]int{} // model -> its picks
	var order []string
	for i, p := range s.plan.Picks {
		if p.Source.Runtime != catalog.RuntimeLlamaCpp || p.OnGPU {
			continue
		}
		if _, ok := idx[p.Model]; !ok {
			order = append(order, p.Model)
		}
		idx[p.Model] = append(idx[p.Model], i)
	}
	// Models that stay loaded first, in plan order (the heavy set leads).
	sort.SliceStable(order, func(i, j int) bool {
		return !s.plan.Picks[idx[order[i]][0]].Swaps && s.plan.Picks[idx[order[j]][0]].Swaps
	})
	for _, id := range order {
		first := s.plan.Picks[idx[id][0]]
		m, ok := s.cat.Model(id)
		b := s.budgets[first.Pool]
		if !ok || b == nil || m.KVPerTokenMB <= 0 {
			continue
		}
		n, per := s.prefixesFor(m), s.llamaFacts(m, first.Source, first.Installed).prefixMB()
		if n <= 0 || per <= 0 {
			continue
		}
		// A model that swaps others out has the pool to itself while it runs.
		left := b.BudgetGB - s.used[first.Pool]
		if first.Swaps {
			left = b.BudgetGB - first.MemGB
		}
		n = min(n, int(math.Min(MaxCacheShare*b.BudgetGB, left)*1024/per))
		if n <= 0 {
			s.plan.Warnings = append(s.plan.Warnings, fmt.Sprintf("%s has no room for a prompt cache: each conversation it takes up again is read from its start", id))
			continue
		}
		mb := int(math.Ceil(float64(n) * per))
		for _, i := range idx[id] {
			s.plan.Picks[i].PromptCacheMB = mb
			s.plan.Picks[i].MemGB = round2(s.plan.Picks[i].MemGB + float64(mb)/1024)
		}
		if !first.Swaps {
			s.used[first.Pool] += float64(mb) / 1024
		}
	}
}

// ContextFor is the window a model runs at: the shared Ollama always gets
// Context (another window means a second loaded copy), Brainiac's own
// llama-server gets the model's own window when the catalog gives one.
func ContextFor(m catalog.Model, src catalog.Source) int {
	if src.Runtime == catalog.RuntimeLlamaCpp && m.Ctx > 0 {
		return m.Ctx
	}
	return Context
}

// activeGB is the weight data read per decoded token.
func activeGB(m catalog.Model, src catalog.Source) float64 {
	w := src.WeightsGB
	if w <= 0 {
		w = src.SizeGB
	}
	if m.ActiveB > 0 && m.ParamsB > 0 {
		w *= m.ActiveB / m.ParamsB
	}
	return w
}

// llamaPool puts a llama-server model on the GPU when its memory fits there.
func (s *selection) llamaPool(memGB float64) (string, bool) {
	variant := llamaVariant(s.cs)
	if variant == "cpu" {
		return "host", false
	}
	for _, g := range s.cs.GPUs {
		if g.Unified {
			return g.Pool, true
		}
		if b, ok := s.budgets[g.Pool]; ok && memGB <= b.BudgetGB {
			return g.Pool, true
		}
	}
	return "host", false
}

// llamaVariant is the llama.cpp build for this machine, or "" if none is published.
func llamaVariant(cs census.Census) string {
	hasVendor := func(v string) bool {
		for _, g := range cs.GPUs {
			if g.Vendor == v {
				return true
			}
		}
		return false
	}
	switch cs.Host.OS + "/" + cs.Host.Arch {
	case "darwin/arm64":
		return "metal"
	case "windows/amd64":
		switch {
		case hasVendor("nvidia"):
			return "cuda"
		case hasVendor("amd"):
			return "vulkan"
		}
		return "cpu"
	case "windows/arm64":
		return "cpu"
	case "linux/amd64":
		if hasVendor("nvidia") || hasVendor("amd") {
			return "vulkan"
		}
		return "cpu"
	case "linux/arm64":
		return "vulkan"
	}
	return ""
}

// llamaAsset names the release asset for a variant.
func llamaAsset(cs census.Census, tag, variant string) (string, []string) {
	switch cs.Host.OS + "/" + cs.Host.Arch + "/" + variant {
	case "darwin/arm64/metal":
		return "llama-" + tag + "-bin-macos-arm64.tar.gz", nil
	case "windows/amd64/cuda":
		return "llama-" + tag + "-bin-win-cuda-12.4-x64.zip", []string{"cudart-llama-bin-win-cuda-12.4-x64.zip"}
	case "windows/amd64/vulkan":
		return "llama-" + tag + "-bin-win-vulkan-x64.zip", nil
	case "windows/amd64/cpu":
		return "llama-" + tag + "-bin-win-cpu-x64.zip", nil
	case "windows/arm64/cpu":
		return "llama-" + tag + "-bin-win-cpu-arm64.zip", nil
	case "linux/amd64/vulkan":
		return "llama-" + tag + "-bin-ubuntu-vulkan-x64.tar.gz", nil
	case "linux/amd64/cpu":
		return "llama-" + tag + "-bin-ubuntu-x64.tar.gz", nil
	case "linux/arm64/vulkan":
		return "llama-" + tag + "-bin-ubuntu-vulkan-arm64.tar.gz", nil
	}
	return "", nil
}

// gpuBandwidth is a conservative GB/s for a GPU's memory when nothing better
// is known: enough to say a model that fits VRAM is fast, never a promise.
func gpuBandwidth(g census.GPU) float64 {
	switch {
	case g.Vendor == "apple":
		return 200
	case g.Unified:
		return 200
	}
	return 300
}

// speedFor is how fast a candidate does a role, in the role's unit.
func (s *selection) speedFor(c candidate, role catalog.Role) float64 {
	if c.src.Runtime == catalog.RuntimeWorker {
		cores := float64(max(1, s.cs.CPU.PhysicalCores))
		ref := float64(max(1, c.src.RefCores))
		return round3(c.src.Speed * ref / cores)
	}
	if role.SpeedUnit != "tok/s" {
		return 0
	}
	act := activeGB(c.model, c.src)
	if act <= 0 {
		return 0
	}
	bw := s.plan.DecodeGBps
	if r, ok := s.measured(c); ok {
		return r.DecodeTPS
	}
	if c.onGPU {
		if g, ok := s.cs.GPUFor(c.pool); ok {
			bw = gpuBandwidth(g) * 0.6
		} else {
			bw = 120
		}
	}
	return round1(bw / act)
}

// fast reports whether a speed meets a role's floor.
func fast(speed float64, role catalog.Role) bool {
	switch role.SpeedUnit {
	case "tok/s":
		return speed >= role.SpeedFloor
	case "s/page", "rtf":
		return role.SpeedFloor <= 0 || speed <= role.SpeedFloor
	}
	return true
}

func unitText(speed float64, unit string) string {
	switch unit {
	case "tok/s":
		return fmt.Sprintf("%.1f tok/s", speed)
	case "s/page":
		return fmt.Sprintf("%.1f s/page", speed)
	case "rtf":
		if speed > 0 {
			return fmt.Sprintf("%.0fx real time", 1/speed)
		}
	}
	return "-"
}

// option is a heavy-set choice: one or two candidates, each filling roles.
type option struct {
	cands []candidate
	value float64
	fill  map[string]int // role -> index into cands
	swap  bool           // the two cannot stay loaded together
}

// chooseHeavy picks the brain, vision and grounding models together.
func (s *selection) chooseHeavy() {
	// Every feasible candidate for any heavy role, and every rejection by role.
	feasible := map[string]candidate{}
	rejected := map[string][]Reject{}
	for _, m := range s.cat.Models {
		roles := heavyOf(m)
		if len(roles) == 0 {
			continue
		}
		for _, src := range m.Sources {
			c, why := s.evaluate(m, src)
			if why == "" {
				// A role it is too weak or too slow for does not count for it.
				for _, r := range roles {
					if w := s.roleProblem(c, r); w != "" {
						rejected[r] = append(rejected[r], Reject{Model: m.ID, Source: src.Label(), Why: w})
					}
				}
				if len(s.usableRoles(c)) > 0 {
					feasible[c.key()] = c
				}
				continue
			}
			for _, r := range roles {
				rejected[r] = append(rejected[r], Reject{Model: m.ID, Source: src.Label(), Why: why})
			}
		}
	}
	cands := make([]candidate, 0, len(feasible))
	for _, c := range feasible {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].key() < cands[j].key() })

	var options []option
	for i := range cands {
		options = append(options, s.makeOption([]candidate{cands[i]}))
		for j := range cands {
			if j <= i || cands[i].model.ID == cands[j].model.ID {
				continue
			}
			if o := s.makeOption([]candidate{cands[i], cands[j]}); o.value > 0 {
				options = append(options, o)
			}
		}
	}
	if len(options) == 0 {
		for _, r := range heavyRoles {
			s.plan.Missing = append(s.plan.Missing, Missing{Role: r, Why: "no model fits this machine", Rejected: rejected[r]})
		}
		return
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].value != options[j].value {
			return options[i].value > options[j].value
		}
		return optionKey(options[i]) < optionKey(options[j])
	})
	best := options[0]
	why := "the best value that fits"
	// Keep what is installed when it is nearly as good as what would be downloaded.
	if optionDownload(best) > 0 {
		for _, o := range options {
			if optionDownload(o) == 0 && o.value >= best.value*s.pol.KeepInstalled {
				best, why = o, fmt.Sprintf("already installed, within %.0f%% of the best (which needs a download)", s.pol.KeepInstalled*100)
				break
			}
		}
	}
	// Keep the previous plan's choice unless the new one is clearly better.
	if prev := s.previousOption(options); prev != nil && prev.value*s.pol.SwitchRatio >= best.value && optionKey(*prev) != optionKey(best) {
		best, why = *prev, "kept from the last plan (the alternative is not clearly better)"
	}
	for i, c := range best.cands {
		s.chosen[c.model.ID] = c
		if i == 0 || !best.swap {
			s.used[c.pool] += c.memGB
		}
		s.diskLeft -= c.download
	}
	if best.swap {
		s.plan.Warnings = append(s.plan.Warnings, "the two heavy models cannot stay loaded together: a request that needs both swaps them")
	}
	for _, r := range heavyRoles {
		idx, ok := best.fill[r]
		if !ok {
			s.plan.Missing = append(s.plan.Missing, Missing{Role: r, Why: "no model that fits can do it well enough", Rejected: rejected[r]})
			continue
		}
		c := best.cands[idx]
		role, _ := s.cat.Role(r)
		p := s.pickFrom(c, role, why)
		p.Swaps = best.swap && idx == 1
		for other, j := range best.fill {
			if other != r && j == idx {
				p.Shared = append(p.Shared, other)
			}
		}
		sort.Strings(p.Shared)
		p.Rejected = s.rejectsFor(r, rejected[r], best, cands)
		s.plan.Picks = append(s.plan.Picks, p)
	}
}

func heavyOf(m catalog.Model) []string {
	var out []string
	for _, r := range heavyRoles {
		if _, ok := m.Roles[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// roleProblem says why a feasible candidate still cannot fill a role.
func (s *selection) roleProblem(c candidate, r string) string {
	role, _ := s.cat.Role(r)
	q := c.model.Roles[r]
	if q < role.MinQuality {
		return fmt.Sprintf("quality %.0f is under the %.0f floor for %s", q, role.MinQuality, r)
	}
	if sp := s.speedFor(c, role); !fast(sp, role) {
		return fmt.Sprintf("too slow here: %s (floor %s)", unitText(sp, role.SpeedUnit), unitText(role.SpeedFloor, role.SpeedUnit))
	}
	return ""
}

func (s *selection) usableRoles(c candidate) []string {
	var out []string
	for _, r := range heavyRoles {
		if _, ok := c.model.Roles[r]; ok && s.roleProblem(c, r) == "" {
			out = append(out, r)
		}
	}
	return out
}

// makeOption scores a heavy set: each role goes to the set member best at it.
func (s *selection) makeOption(cands []candidate) option {
	o := option{cands: cands, fill: map[string]int{}}
	need := map[string]float64{}
	var download float64
	for _, c := range cands {
		need[c.pool] += c.memGB
		download += c.download
	}
	if download > s.diskLeft {
		return option{}
	}
	for pool, gb := range need {
		if b := s.budgets[pool]; b != nil && gb > b.BudgetGB {
			o.swap = true
		}
	}
	for _, r := range heavyRoles {
		role, _ := s.cat.Role(r)
		best, bestQ := -1, 0.0
		for i, c := range cands {
			if _, ok := c.model.Roles[r]; !ok || s.roleProblem(c, r) != "" {
				continue
			}
			if q := c.model.Roles[r]; q > bestQ {
				best, bestQ = i, q
			}
		}
		if best >= 0 {
			o.fill[r] = best
			o.value += role.Weight * bestQ
		}
	}
	if len(cands) == 2 {
		// The second model must earn its place: each must fill a role.
		used := map[int]bool{}
		for _, i := range o.fill {
			used[i] = true
		}
		if len(used) < 2 {
			return option{}
		}
		if o.swap {
			// Every request needing both pays a model swap.
			o.value *= 0.85
		} else {
			o.value *= 0.97 // two models are more to keep loaded and to go wrong
		}
	}
	return o
}

func optionKey(o option) string {
	var keys []string
	for _, c := range o.cands {
		keys = append(keys, c.key())
	}
	return strings.Join(keys, "+")
}

func optionDownload(o option) float64 {
	var d float64
	for _, c := range o.cands {
		d += c.download
	}
	return d
}

func (s *selection) previousOption(options []option) *option {
	if s.pol.Previous == nil {
		return nil
	}
	want := map[string]bool{}
	for _, p := range s.pol.Previous.Picks {
		for _, r := range heavyRoles {
			if p.Role == r {
				want[p.Model+"|"+p.Source.Label()] = true
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	for i := range options {
		if len(options[i].cands) != len(want) {
			continue
		}
		all := true
		for _, c := range options[i].cands {
			if !want[c.key()] {
				all = false
			}
		}
		if all {
			return &options[i]
		}
	}
	return nil
}

// rejectsFor lists, for a heavy role, the candidates not chosen and why: the
// unfeasible ones as they were, the feasible ones as "less value overall".
func (s *selection) rejectsFor(role string, unfeasible []Reject, best option, cands []candidate) []Reject {
	out := append([]Reject(nil), unfeasible...)
	inBest := map[string]bool{}
	for _, c := range best.cands {
		inBest[c.key()] = true
	}
	for _, c := range cands {
		if inBest[c.key()] {
			continue
		}
		if _, ok := c.model.Roles[role]; !ok || s.roleProblem(c, role) != "" {
			continue
		}
		why := fmt.Sprintf("fits, but the chosen set is better overall (quality %.0f here)", c.model.Roles[role])
		out = append(out, Reject{Model: c.model.ID, Source: c.src.Label(), Why: why})
	}
	return dedupe(out)
}

func dedupe(in []Reject) []Reject {
	seen := map[string]bool{}
	var out []Reject
	for _, r := range in {
		k := r.Model + "|" + r.Source + "|" + r.Why
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// pickFrom turns a candidate into a role's pick.
func (s *selection) pickFrom(c candidate, role catalog.Role, why string) Pick {
	sp := s.speedFor(c, role)
	disk := c.src.SizeGB
	p := Pick{
		Role: role.ID, Model: c.model.ID, Title: c.model.Title, Source: c.src,
		Pool: c.pool, OnGPU: c.onGPU, MemGB: round2(c.memGB), DiskGB: round2(disk),
		Download: round2(c.download), Installed: c.installed,
		Speed: sp, SpeedUnit: role.SpeedUnit, Quality: c.model.Roles[role.ID], Why: why,
	}
	s.turnTimes(&p, c)
	return p
}

// chooseLight picks OCR, speech, VAD and embeddings one by one. Each prefers a
// model that can stay loaded beside the heavy set; a better one that cannot is
// taken only when it is clearly better (15 points), and is marked as swapping.
func (s *selection) chooseLight() {
	for _, r := range lightRoles {
		role, _ := s.cat.Role(r)
		if r == catalog.RoleVAD {
			if _, ok := s.plan.Pick(catalog.RoleASR); !ok {
				continue // nothing to cut speech for
			}
		}
		var cands []candidate
		var rejected []Reject
		for _, m := range s.cat.ForRole(r) {
			// A model already chosen for another role is reused as it is.
			if c, ok := s.chosen[m.ID]; ok {
				cands = append(cands, c)
				continue
			}
			for _, src := range m.Sources {
				c, why := s.evaluate(m, src)
				if why == "" && c.download > s.diskLeft {
					why = fmt.Sprintf("download %.1f GB is over the %.1f GB of disk left to use", c.download, s.diskLeft)
				}
				if why == "" {
					q := m.Roles[r]
					if q < role.MinQuality {
						why = fmt.Sprintf("quality %.0f is under the %.0f floor", q, role.MinQuality)
					} else if sp := s.speedFor(c, role); !fast(sp, role) {
						why = fmt.Sprintf("too slow here: %s", unitText(sp, role.SpeedUnit))
					}
				}
				if why != "" {
					rejected = append(rejected, Reject{Model: m.ID, Source: src.Label(), Why: why})
					continue
				}
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			s.plan.Missing = append(s.plan.Missing, Missing{Role: r, Why: "no model fits this machine", Rejected: rejected})
			continue
		}
		fits := func(c candidate) bool {
			if _, reused := s.chosen[c.model.ID]; reused {
				return true
			}
			b := s.budgets[c.pool]
			return b != nil && s.used[c.pool]+c.memGB <= b.BudgetGB
		}
		score := func(c candidate) float64 {
			q := c.model.Roles[r]
			if c.installed {
				q += 2 // a tie goes to what needs no download
			}
			return q
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if score(cands[i]) != score(cands[j]) {
				return score(cands[i]) > score(cands[j])
			}
			return cands[i].key() < cands[j].key()
		})
		var resident *candidate
		for i := range cands {
			if fits(cands[i]) {
				resident = &cands[i]
				break
			}
		}
		best := cands[0]
		why := "the best that fits"
		swaps := false
		switch {
		case resident == nil:
			swaps = true
			why = "the best that fits; it cannot stay loaded beside the brain"
		case resident.key() == best.key():
			why = "the best that fits, and it stays loaded beside the brain"
		case best.model.Roles[r]-resident.model.Roles[r] >= 15:
			swaps = true
			why = fmt.Sprintf("clearly better than %s, which would stay loaded; it unloads the brain while it runs", resident.model.ID)
		default:
			best = *resident
			why = "stays loaded beside the brain; better ones would swap it out for little gain"
		}
		var notChosen []Reject
		for _, c := range cands {
			if c.key() != best.key() {
				notChosen = append(notChosen, Reject{Model: c.model.ID, Source: c.src.Label(), Why: fmt.Sprintf("quality %.0f, the chosen one is the better fit", c.model.Roles[r])})
			}
		}
		p := s.pickFrom(best, role, why)
		p.Swaps = swaps
		p.Rejected = dedupe(append(rejected, notChosen...))
		if _, reused := s.chosen[best.model.ID]; reused {
			p.Why = "already chosen for another role"
			p.Download = 0
		} else {
			s.chosen[best.model.ID] = best
			if !swaps {
				s.used[best.pool] += best.memGB
			}
			s.diskLeft -= best.download
		}
		s.plan.Picks = append(s.plan.Picks, p)
	}
}

// finish adds up downloads, the llama.cpp build and the pip groups.
func (s *selection) finish() {
	seen := map[string]bool{}
	pip := map[string]bool{}
	needLlama := false
	for _, p := range s.plan.Picks {
		if p.Source.Runtime == catalog.RuntimeLlamaCpp {
			needLlama = true
		}
		for _, g := range p.Source.Pip {
			pip[g] = true
		}
		if seen[p.Model] {
			continue
		}
		seen[p.Model] = true
		s.plan.DownloadGB += p.Download
	}
	s.plan.DownloadGB = round2(s.plan.DownloadGB)
	for g := range pip {
		s.plan.Pip = append(s.plan.Pip, g)
	}
	sort.Strings(s.plan.Pip)
	if needLlama {
		v := llamaVariant(s.cs)
		asset, extra := llamaAsset(s.cs, s.cat.Runtime.LlamaCppTag, v)
		s.plan.LlamaCpp = &LlamaCpp{Repo: s.cat.Runtime.LlamaCppRepo, Tag: s.cat.Runtime.LlamaCppTag, Variant: v, Asset: asset, Extra: extra,
			SHA256: s.cat.Runtime.LlamaCppSHA256}
	}
	if _, ok := s.plan.Pick(catalog.RoleBrain); !ok {
		_, ocr := s.plan.Pick(catalog.RoleOCR)
		_, asr := s.plan.Pick(catalog.RoleASR)
		if ocr || asr {
			s.plan.Warnings = append(s.plan.Warnings, "no local brain fits: Brainiac can still read pictures or audio here, but answers need a remote brain (mode hybrid or remote)")
		} else {
			s.plan.Warnings = append(s.plan.Warnings, "nothing but embeddings fits in the memory free now: free memory (or run where the models' memory is, e.g. inside WSL) and plan again, or use a remote brain")
		}
	} else if p, _ := s.plan.Pick(catalog.RoleBrain); p.Quality < 40 {
		s.plan.Warnings = append(s.plan.Warnings, fmt.Sprintf("the local brain (%s) is small: use a remote brain for judgement-heavy work, or expect more questions", p.Model))
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
