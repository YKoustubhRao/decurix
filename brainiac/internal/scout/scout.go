// Package scout keeps Brainiac's arsenal current: it looks on the Hugging
// Face Hub (and the web) for open models newer than the catalog, checks each
// one the way a careful person would - licence, gating, a GGUF file of a
// usable size, whether the pinned llama.cpp knows its architecture, whether it
// fits this machine - and reports what it found with links and the vendor's
// own claims.
//
// It never changes what Brainiac runs. A candidate becomes part of the
// catalog only when a person adopts it (`brainiac scout adopt`), and it takes
// work from the current pick only when `brainiac eval` shows it is better on
// checked cases. A model is a candidate, not an upgrade, until measured.
package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/hub"
	"brainiac/internal/web"
)

// Options configure one scouting run.
type Options struct {
	Hub     *hub.Client
	Web     *web.Client
	GitHub  *http.Client
	Catalog *catalog.Catalog
	// BudgetGB is the host pool's model budget from the last plan; 0 = unknown.
	BudgetGB float64
	// Since: only models created after it (default 120 days ago).
	Since time.Time
	// MaxVerify caps the candidates checked in detail (default 30).
	MaxVerify int
	// Links looks up benchmark pages for the best candidates on the web.
	Links bool
	Log   io.Writer
	// GitHubAPI and RawBase are where releases and source files are read
	// (https://api.github.com, https://raw.githubusercontent.com).
	GitHubAPI string
	RawBase   string
}

// Report is one scouting run.
type Report struct {
	MadeAt         time.Time   `json:"made_at"`
	CatalogVersion string      `json:"catalog_version"`
	LlamaCpp       LlamaReport `json:"llamacpp"`
	Candidates     []Candidate `json:"candidates"`
	Notes          []string    `json:"notes,omitempty"`
}

// LlamaReport says whether a newer llama.cpp build is worth pinning.
type LlamaReport struct {
	Pinned      string `json:"pinned"`
	NewestSafe  string `json:"newest_safe,omitempty"` // newest release at least two weeks old
	NewestDate  string `json:"newest_date,omitempty"`
	PinnedArchs int    `json:"pinned_archs"`
	// NewArchs are architectures the newer build runs that the pinned one does not.
	NewArchs []string `json:"new_archs,omitempty"`
}

// Candidate is one model worth a person's look.
type Candidate struct {
	Repo string `json:"repo"`
	Base string `json:"base"`
	// BaseRepo is the repo this one quantises (where the maker's card and
	// its benchmark tables usually are).
	BaseRepo     string      `json:"base_repo,omitempty"`
	Role         string      `json:"role"`
	Found        string      `json:"found"`
	Created      time.Time   `json:"created"`
	ParamsB      float64     `json:"params_b,omitempty"`
	Arch         string      `json:"arch,omitempty"`
	ArchOK       bool        `json:"arch_ok"`
	ArchNote     string      `json:"arch_note,omitempty"`
	License      string      `json:"license,omitempty"`
	LicenseClass string      `json:"license_class"`
	Gated        string      `json:"gated,omitempty"`
	File         string      `json:"file,omitempty"`
	SizeGB       float64     `json:"size_gb,omitempty"`
	MMProj       string      `json:"mmproj,omitempty"`
	MMProjGB     float64     `json:"mmproj_gb,omitempty"`
	MemGB        float64     `json:"mem_gb,omitempty"`
	FitsHere     *bool       `json:"fits_here,omitempty"`
	Downloads    int         `json:"downloads"`
	Likes        int         `json:"likes"`
	Claims       []hub.Claim `json:"claims,omitempty"`
	Links        []string    `json:"links,omitempty"`
	Usable       bool        `json:"usable"` // GGUF, known licence class, architecture supported
	Problems     []string    `json:"problems,omitempty"`
}

type query struct {
	label string
	role  string
	q     hub.SearchQuery
}

// queries are the searches one run makes: what is trending and newest among
// GGUF models, and a few per field. About fifteen API calls.
func queries() []query {
	qs := []query{
		{"trending vision GGUF", catalog.RoleVision, hub.SearchQuery{Filter: []string{"gguf"}, PipelineTag: "image-text-to-text", Sort: "trendingScore", Limit: 30}},
		{"trending text GGUF", catalog.RoleBrain, hub.SearchQuery{Filter: []string{"gguf"}, PipelineTag: "text-generation", Sort: "trendingScore", Limit: 40}},
		{"newest text GGUF", catalog.RoleBrain, hub.SearchQuery{Filter: []string{"gguf"}, PipelineTag: "text-generation", Sort: "createdAt", Limit: 40}},
		{"trending speech", catalog.RoleASR, hub.SearchQuery{PipelineTag: "automatic-speech-recognition", Sort: "trendingScore", Limit: 15}},
		{"trending embeddings GGUF", catalog.RoleEmbed, hub.SearchQuery{Filter: []string{"gguf"}, Search: "embedding", Sort: "trendingScore", Limit: 10}},
		{"trending OCR GGUF", catalog.RoleOCR, hub.SearchQuery{Filter: []string{"gguf"}, Search: "ocr", Sort: "trendingScore", Limit: 10}},
	}
	for _, f := range []struct{ role, kw string }{{"math", "math"}, {"code", "coder"}, {"sql", "sql"}, {"medicine", "med"},
		{"law", "legal"}, {"finance", "fin"}, {"translate", "translat"}, {"science", "chem"}} {
		qs = append(qs, query{"field " + f.role, f.role, hub.SearchQuery{Filter: []string{"gguf"}, Search: f.kw, Sort: "trendingScore", Limit: 10}})
	}
	return qs
}

// labs are publishers whose new models are worth a look on sight. A model
// from anyone else must show adoption and published results (see worthALook).
// It is a curation list, kept here so it is easy to see and change.
var labs = map[string]bool{}

func init() {
	for _, l := range strings.Fields(`qwen google mistralai openai deepseek-ai zai-org thudm nvidia ibm-granite microsoft meta-llama meta-models
		moonshotai tencent openbmb huggingfacetb allenai ai4bharat sarvamai baidu paddlepaddle internlm opengvlab xiaomimimo inclusionai
		stepfun-ai minimaxai cohereforai coherelabs snowflake salesforce servicenow-ai jetbrains bigcode bytedance-seed freedomintelligence
		baichuan-inc hcompany lightonai numind k2-fsa nousresearch apple amazon tiiuae swiss-ai utter-project ai-mo ornith-ai
		weiboai nanbeige openai-community rednote-hilab datalab-to opendatalab facebook`) {
		labs[l] = true
	}
}

// quantisers are uploaders of other labs' models that Brainiac takes files from.
var quantisers = map[string]bool{"ggml-org": true, "unsloth": true, "bartowski": true, "mradermacher": true, "lmstudio-community": true}

// TrustedUploader reports whether a repo's owner is a lab or quantiser on the
// lists here. The Hub's tags and a repo's name are the uploader's own words,
// so nothing else about a repo can say who made its files. The updater adds
// only these on its own; anything else takes a person (`scout adopt`).
func TrustedUploader(repo string) bool {
	org := strings.ToLower(strings.SplitN(repo, "/", 2)[0])
	return labs[org] || quantisers[org]
}

// junkRe marks derivatives that are not better models: safety training taken
// out, merges, role-play tunes.
var junkRe = regexp.MustCompile(`(?i)(uncensor|abliterat|obliterat|heretic|nsfw|roleplay|\brp\b|erotic|lewd|merge|frankenmerge|humanizer|dpo-?test|toxic)`)

var (
	quantSuffix = regexp.MustCompile(`(?i)[-_.](gguf|i1|imatrix|mtp|q\d.*|iq\d.*|f16|bf16|fp16|mlx.*|awq|gptq|int4|int8|fp8)$`)
	splitRe     = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
	nonAlnum    = regexp.MustCompile(`[^a-z0-9.]+`)
	archRe      = regexp.MustCompile(`\{\s*LLM_ARCH_[A-Z0-9_]+\s*,\s*"([^"]+)"\s*\}`)
)

// baseName is a model's name without its uploader and quantisation marks:
// "unsloth/Qwen3.9-4B-GGUF" -> "qwen3.9-4b".
func baseName(repo string) string {
	name := repo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for {
		trimmed := quantSuffix.ReplaceAllString(name, "")
		if trimmed == name {
			break
		}
		name = trimmed
	}
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// known is every base name the catalog already covers.
func known(cat *catalog.Catalog) map[string]bool {
	k := map[string]bool{}
	for _, m := range cat.Models {
		k[baseName(m.ID)] = true
		for _, s := range m.Sources {
			if s.Repo != "" {
				k[baseName(s.Repo)] = true
			}
			if s.Tag != "" {
				k[baseName(strings.ReplaceAll(s.Tag, ":", "-"))] = true
			}
		}
	}
	return k
}

// uploaderRank prefers the original publisher and well-known quantisers. A
// stranger's repo comes last whatever it is called, or it would hide a
// trusted copy of the same model.
func uploaderRank(repo string) int {
	org := strings.ToLower(strings.SplitN(repo, "/", 2)[0])
	switch org {
	case "ggml-org":
		return 0
	case "unsloth":
		return 2
	case "bartowski":
		return 3
	case "mradermacher", "lmstudio-community":
		return 4
	}
	if labs[org] {
		return 1 // the publisher's own repo
	}
	return 5
}

// derivedFromJunk reports a repo whose base model or tags say what its name
// does not.
func derivedFromJunk(d hub.Details) bool {
	for _, b := range d.BaseModels {
		if junkRe.MatchString(b) {
			return true
		}
	}
	for _, t := range d.Tags {
		if !strings.HasPrefix(t, "dataset:") && junkRe.MatchString(t) {
			return true
		}
	}
	return false
}

// Run scouts once.
func Run(ctx context.Context, opt Options) (*Report, error) {
	if opt.Hub == nil {
		opt.Hub = hub.New()
	}
	if opt.Web == nil {
		opt.Web = web.New()
	}
	if opt.GitHub == nil {
		opt.GitHub = &http.Client{Timeout: 30 * time.Second}
	}
	if opt.MaxVerify <= 0 {
		opt.MaxVerify = 30
	}
	if opt.Since.IsZero() {
		opt.Since = time.Now().AddDate(0, 0, -120)
	}
	if opt.GitHubAPI == "" {
		opt.GitHubAPI = "https://api.github.com"
	}
	if opt.RawBase == "" {
		opt.RawBase = "https://raw.githubusercontent.com"
	}
	logf := func(f string, a ...any) {
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, f+"\n", a...)
		}
	}
	rep := &Report{MadeAt: time.Now().UTC(), CatalogVersion: opt.Catalog.Version}
	rep.LlamaCpp = scoutLlama(ctx, opt, rep, logf)
	pinnedArchs := archSet(ctx, opt, rep.LlamaCpp.Pinned)
	newerArchs := map[string]bool{}
	if rep.LlamaCpp.NewestSafe != "" && rep.LlamaCpp.NewestSafe != rep.LlamaCpp.Pinned {
		newerArchs = archSet(ctx, opt, rep.LlamaCpp.NewestSafe)
		for a := range newerArchs {
			if !pinnedArchs[a] {
				rep.LlamaCpp.NewArchs = append(rep.LlamaCpp.NewArchs, a)
			}
		}
		sort.Strings(rep.LlamaCpp.NewArchs)
	}
	rep.LlamaCpp.PinnedArchs = len(pinnedArchs)
	if len(pinnedArchs) == 0 {
		rep.Notes = append(rep.Notes, "could not read llama.cpp's architecture list at the pinned build; architecture support is not checked")
	}

	// Discover.
	cover := known(opt.Catalog)
	type seen struct {
		info  hub.ModelInfo
		role  string
		found string
	}
	byBase := map[string]seen{}
	for _, q := range queries() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := opt.Hub.Search(ctx, q.q)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("search %q failed: %v", q.label, err))
			continue
		}
		logf("  %-26s %d models", q.label, len(res))
		for _, m := range res {
			base := baseName(m.ID)
			if base == "" || cover[base] || junkRe.MatchString(m.ID) {
				continue
			}
			role := guessRole(m.ID, m.PipelineTag, q.role)
			old, dup := byBase[base]
			if !dup || uploaderRank(m.ID) < uploaderRank(old.info.ID) || (uploaderRank(m.ID) == uploaderRank(old.info.ID) && m.Downloads > old.info.Downloads) {
				byBase[base] = seen{m, role, q.label}
			}
		}
	}
	var pool []seen
	for _, s := range byBase {
		pool = append(pool, s)
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].info.Likes != pool[j].info.Likes {
			return pool[i].info.Likes > pool[j].info.Likes
		}
		return pool[i].info.ID < pool[j].info.ID
	})

	// Check the most liked ones in detail.
	for _, s := range pool {
		if len(rep.Candidates) >= opt.MaxVerify || ctx.Err() != nil {
			break
		}
		c, ok := verify(ctx, opt, s.info, s.role, s.found, pinnedArchs, newerArchs)
		if !ok {
			continue
		}
		rep.Candidates = append(rep.Candidates, c)
	}
	sort.SliceStable(rep.Candidates, func(i, j int) bool {
		if rep.Candidates[i].Usable != rep.Candidates[j].Usable {
			return rep.Candidates[i].Usable
		}
		return rep.Candidates[i].Likes > rep.Candidates[j].Likes
	})
	if opt.Links {
		n := 0
		for i := range rep.Candidates {
			if !rep.Candidates[i].Usable || n >= 5 {
				continue
			}
			n++
			name := rep.Candidates[i].Repo[strings.LastIndex(rep.Candidates[i].Repo, "/")+1:]
			name = strings.TrimSuffix(strings.TrimSuffix(name, "-GGUF"), "-gguf")
			res, err := opt.Web.Search(ctx, `"`+name+`" model benchmark`, 8)
			if err == nil {
				for _, r := range res {
					if aboutModel(r, rep.Candidates[i].Base) && len(rep.Candidates[i].Links) < 3 {
						rep.Candidates[i].Links = append(rep.Candidates[i].Links, r.URL)
					}
				}
			}
		}
	}
	return rep, nil
}

// verify checks one model on the Hub and decides whether it is worth showing.
func verify(ctx context.Context, opt Options, m hub.ModelInfo, role, found string, pinned, newer map[string]bool) (Candidate, bool) {
	d, err := opt.Hub.Details(ctx, m.ID)
	if err != nil {
		return Candidate{}, false
	}
	if !d.CreatedAt.IsZero() && d.CreatedAt.Before(opt.Since) {
		return Candidate{}, false
	}
	if d.Architecture == "clip" {
		return Candidate{}, false // a vision projector alone, not a model
	}
	if derivedFromJunk(d) {
		return Candidate{}, false
	}
	c := Candidate{Repo: m.ID, Base: baseName(m.ID), Role: role, Found: found, Created: d.CreatedAt, ParamsB: round2(d.ParamsB),
		Arch: d.Architecture, License: d.License, Gated: d.Gated, Downloads: d.Downloads, Likes: d.Likes, Claims: d.Claims}
	if len(d.BaseModels) > 0 {
		c.BaseRepo = d.BaseModels[0]
	}
	// A quantiser's repo often carries no licence of its own: read the base model's.
	if (c.License == "" || c.License == "other") && len(d.BaseModels) > 0 {
		if bd, err := opt.Hub.Details(ctx, d.BaseModels[0]); err == nil {
			if bd.License != "" {
				c.License = bd.License
			}
			if len(c.Claims) == 0 {
				c.Claims = bd.Claims
			}
			if c.ParamsB == 0 {
				c.ParamsB = round2(bd.ParamsB)
			}
		}
	}
	c.LicenseClass = LicenseClass(c.License)
	if !worthALook(m.ID, d, role, known(opt.Catalog)) {
		return Candidate{}, false
	}
	if c.Gated != "" {
		c.Problems = append(c.Problems, "gated: accept its terms on the Hub and set HF_TOKEN")
	}
	if c.LicenseClass == catalog.LicenseNonCommercial {
		c.Problems = append(c.Problems, "licence "+c.License+" is non-commercial")
	}
	if role != catalog.RoleASR {
		files, err := opt.Hub.Files(ctx, m.ID, "main")
		if err == nil {
			c.File, c.SizeGB, c.MMProj, c.MMProjGB = pickGGUF(files)
		}
		if c.File == "" {
			c.Problems = append(c.Problems, "no GGUF file of a usable quantisation")
		}
		switch {
		case c.Arch == "":
			c.ArchNote = "architecture not stated by the Hub"
		case len(pinned) == 0:
			c.ArchNote = "not checked"
		case pinned[c.Arch]:
			c.ArchOK = true
		case newer[c.Arch]:
			c.ArchNote = "needs a newer llama.cpp than the pinned build"
			c.Problems = append(c.Problems, c.ArchNote)
		default:
			c.ArchNote = "llama.cpp does not run this architecture"
			c.Problems = append(c.Problems, c.ArchNote)
		}
		if c.SizeGB > 0 {
			c.MemGB = round2(c.SizeGB + c.MMProjGB + 0.25 + 0.02*c.ParamsB + 0.2)
			if opt.BudgetGB > 0 {
				fits := c.MemGB <= opt.BudgetGB
				c.FitsHere = &fits
			}
		}
		c.Usable = c.File != "" && (c.ArchOK || len(pinned) == 0) && c.LicenseClass != catalog.LicenseNonCommercial && c.Gated == ""
	} else {
		c.ArchNote = "speech models run on the worker (sherpa-onnx); needs an ONNX export in the sherpa-onnx release before Brainiac can run it"
		c.Problems = append(c.Problems, "not runnable until sherpa-onnx publishes it")
	}
	return c, true
}

// aboutModel keeps a search result only when it names the model's own
// words (not just a town that shares its first one).
func aboutModel(r web.Result, base string) bool {
	text := strings.ToLower(r.URL + " " + r.Title + " " + r.Snippet)
	var parts []string
	for _, p := range strings.FieldsFunc(base, func(c rune) bool { return c == '-' || c == '.' }) {
		if len(p) > 2 && !sizeRe.MatchString(p) {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return strings.Contains(text, base)
	}
	for _, p := range parts[:min(2, len(parts))] {
		if !strings.Contains(text, p) {
			return false
		}
	}
	return true
}

var sizeRe = regexp.MustCompile(`^(a?\d+(\.\d+)?[bmk]|e\d+b|\d+gb|v\d+|it|instruct|thinking|base|chat)$`)

// worthALook keeps a model only when it is something new: from a recognised
// lab (or a quantiser's copy of one), or else widely used with published
// results; and not a general-purpose remix of a model the catalog has
// already (a field fine-tune of one is kept: that is how specialists are made).
func worthALook(repo string, d hub.Details, role string, have map[string]bool) bool {
	org := strings.ToLower(strings.SplitN(repo, "/", 2)[0])
	lab := org
	if len(d.BaseModels) > 0 {
		lab = strings.ToLower(strings.SplitN(d.BaseModels[0], "/", 2)[0])
	}
	general := role == catalog.RoleBrain || role == catalog.RoleVision
	for _, b := range d.BaseModels {
		if have[baseName(b)] && general && baseName(b) != baseName(repo) {
			return false // a remix of something the catalog has
		}
	}
	if labs[lab] || labs[org] {
		return true
	}
	return d.Downloads >= 500 && len(d.Claims) > 0
}

// pickGGUF chooses the file a laptop or a GPU box would run: Q4_K_M, then
// other 4-bit, then Q8_0 for small models; and the vision projector.
func pickGGUF(files []hub.File) (file string, gb float64, mmproj string, mmgb float64) {
	prefs := []string{"q4_k_m", "q4_k_s", "iq4_xs", "q4_0", "iq4_nl", "q5_k_m", "q8_0"}
	best := len(prefs)
	mmBest := 9
	for _, f := range files {
		name := strings.ToLower(f.Path)
		if !strings.HasSuffix(name, ".gguf") {
			continue
		}
		base := name[strings.LastIndex(name, "/")+1:]
		if strings.HasPrefix(base, "mtp-") || strings.HasPrefix(base, "dflash") || strings.Contains(base, "draft") ||
			strings.HasPrefix(base, "eagle") || strings.Contains(base, "imatrix") {
			continue // a speculative-decoding helper, not the model
		}
		if strings.Contains(name, "mmproj") {
			r := 9
			switch {
			case strings.Contains(name, "q8_0"):
				r = 0
			case strings.Contains(name, "f16"):
				r = 1
			}
			if r < mmBest {
				mmBest, mmproj, mmgb = r, f.Path, round2(float64(f.Size)/1e9)
			}
			continue
		}
		if m := splitRe.FindStringSubmatch(name); m != nil && m[1] != "00001" {
			continue // later parts of a split file: counted with the first
		}
		for i, p := range prefs {
			if strings.Contains(name, p) && i < best {
				best, file, gb = i, f.Path, round2(float64(splitTotal(files, f))/1e9)
			}
		}
	}
	return
}

// splitTotal is the size of a GGUF split into parts (all parts), or of the file.
func splitTotal(files []hub.File, first hub.File) int64 {
	loc := splitRe.FindStringIndex(first.Path)
	if loc == nil {
		return first.Size
	}
	prefix := first.Path[:loc[0]]
	var total int64
	for _, f := range files {
		if strings.HasPrefix(f.Path, prefix) && splitRe.MatchString(f.Path) {
			total += f.Size
		}
	}
	return total
}

// guessRole names the role a model probably fills, from its name and tag.
func guessRole(repo, tag, fallback string) string {
	n := strings.ToLower(repo)
	switch {
	case tag == "automatic-speech-recognition" || strings.Contains(n, "whisper") || strings.Contains(n, "parakeet") || strings.Contains(n, "-asr"):
		return catalog.RoleASR
	case strings.Contains(n, "embed"):
		return catalog.RoleEmbed
	case strings.Contains(n, "ocr"):
		return catalog.RoleOCR
	case strings.Contains(n, "coder") || strings.Contains(n, "code"):
		return "code"
	case strings.Contains(n, "sql"):
		return "sql"
	case strings.Contains(n, "math") || strings.Contains(n, "prover"):
		return "math"
	case strings.Contains(n, "med") && !strings.Contains(n, "medium"):
		return "medicine"
	case strings.Contains(n, "legal") || strings.Contains(n, "law"):
		return "law"
	case strings.Contains(n, "fin") && !strings.Contains(n, "fine"):
		return "finance"
	case strings.Contains(n, "translat") || hasWord(n, "mt") || mtRe.MatchString(n):
		return "translate"
	case strings.Contains(n, "chem") || strings.Contains(n, "bio"):
		return "science"
	case tag == "image-text-to-text" || strings.Contains(n, "-vl"):
		return catalog.RoleVision
	}
	return fallback
}

var mtRe = regexp.MustCompile(`(^|[^a-z])mt\d`)

// hasWord reports whether a name has w as a whole part (split at - _ . / and spaces).
func hasWord(name, w string) bool {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' || r == '/' || r == ' ' }) {
		if part == w {
			return true
		}
	}
	return false
}

// LicenseClass sorts a Hub licence id into the catalog's classes.
func LicenseClass(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	switch {
	case l == "apache-2.0" || l == "mit" || strings.HasPrefix(l, "bsd") || l == "cc-by-4.0" || l == "cc0-1.0" || l == "openmdw" || strings.HasPrefix(l, "openmdw"):
		return catalog.LicensePermissive
	case strings.Contains(l, "-nc") || strings.Contains(l, "noncommercial") || strings.Contains(l, "non-commercial") || strings.Contains(l, "research"):
		return catalog.LicenseNonCommercial
	}
	return catalog.LicenseRestricted // gemma, llama, openrail, other, unknown: read before use
}

// scoutLlama finds the newest llama.cpp release at least two weeks old.
func scoutLlama(ctx context.Context, opt Options, rep *Report, logf func(string, ...any)) LlamaReport {
	lr := LlamaReport{Pinned: opt.Catalog.Runtime.LlamaCppTag}
	cut := time.Now().AddDate(0, 0, -14)
	// llama.cpp publishes several builds a day: a page of 100 can all be
	// younger than two weeks, so read pages until one is old enough.
	for page := 1; page <= 4 && lr.NewestSafe == ""; page++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", opt.GitHubAPI, opt.Catalog.Runtime.LlamaCppRepo, page), nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := opt.GitHub.Do(req)
		if err != nil {
			rep.Notes = append(rep.Notes, "could not list llama.cpp releases: "+err.Error())
			return lr
		}
		var rels []struct {
			Tag       string     `json:"tag_name"`
			Published time.Time  `json:"published_at"`
			Assets    []struct{} `json:"assets"`
		}
		status := resp.StatusCode
		err = json.NewDecoder(resp.Body).Decode(&rels)
		resp.Body.Close()
		if status != http.StatusOK {
			rep.Notes = append(rep.Notes, fmt.Sprintf("listing llama.cpp releases answered HTTP %d (GitHub's anonymous limit is 60 an hour; set GITHUB_TOKEN)", status))
			return lr
		}
		if err != nil || len(rels) == 0 {
			break
		}
		for _, r := range rels {
			if strings.HasPrefix(r.Tag, "b") && r.Published.Before(cut) && len(r.Assets) > 10 {
				lr.NewestSafe, lr.NewestDate = r.Tag, r.Published.Format("2006-01-02")
				break
			}
		}
	}
	logf("  llama.cpp pinned %s, newest two weeks old %s (%s)", lr.Pinned, lr.NewestSafe, lr.NewestDate)
	return lr
}

// archSet reads the architectures a llama.cpp build knows from its source.
func archSet(ctx context.Context, opt Options, tag string) map[string]bool {
	out := map[string]bool{}
	if tag == "" {
		return out
	}
	p, err := opt.Web.Fetch(ctx, opt.RawBase+"/"+opt.Catalog.Runtime.LlamaCppRepo+"/"+tag+"/src/llama-arch.cpp")
	if err != nil {
		return out
	}
	for _, m := range archRe.FindAllStringSubmatch(p.Text, -1) {
		out[m[1]] = true
	}
	return out
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
