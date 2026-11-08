// Package catalog is the curated list of models Brainiac may choose from: for
// each kind of work (a role), which open models can do it, how well, how big
// they are, and where to get them.
//
// The quality numbers are priors taken from published results, written down
// in each entry's Basis. They are a starting point for the selector, never a
// claim about this machine: a model found on the Hub joins as untested, and
// Brainiac's own checked runs replace a prior once there are enough of them.
//
// Depends on nothing internal.
package catalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// The roles a model can fill.
const (
	RoleBrain  = "brain"  // reasoning, chat, writing the answer
	RoleVision = "vision" // understanding pictures: describe, read charts, answer about an image
	RoleGround = "ground" // finding a described thing on a screenshot (a point)
	RoleOCR    = "ocr"    // reading the text on a picture, line by line with positions
	RoleASR    = "asr"    // speech to text
	RoleVAD    = "vad"    // finding where speech is in audio
	RoleEmbed  = "embed"  // text to vectors for search
)

// The runtimes a model runs on.
const (
	RuntimeOllama   = "ollama"   // an Ollama server (shared with others)
	RuntimeLlamaCpp = "llamacpp" // Brainiac's own llama-server process
	RuntimeWorker   = "worker"   // Brainiac's Python worker (ONNX Runtime, no torch)
)

// The kinds of Source.
const (
	SourceOllama = "ollama" // an Ollama library tag
	SourceHF     = "hf"     // files of a Hugging Face repo
	SourceGitHub = "github" // a GitHub release asset, usually an archive to unpack
	SourcePip    = "pip"    // shipped inside a pip package the worker installs
	SourceSystem = "system" // a program already on the machine (tesseract)
)

// Licence classes.
const (
	LicensePermissive    = "permissive"    // Apache-2.0, MIT, BSD: usable commercially
	LicenseRestricted    = "restricted"    // usable with conditions (Llama, Gemma terms)
	LicenseNonCommercial = "noncommercial" // research or non-commercial only
)

// Catalog is the whole list.
type Catalog struct {
	Version string  `json:"version"`
	Updated string  `json:"updated"`
	Roles   []Role  `json:"roles"`
	Runtime Runtime `json:"runtime"`
	Models  []Model `json:"models"`
	// GeneralExamples are requests that belong to no field, so the router
	// learns what "none of the above" looks like.
	GeneralExamples []string `json:"general_examples,omitempty"`
}

// Role is one kind of work and what it takes to be usable at it.
type Role struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Weight is how much this role counts when one choice trades against
	// another (the brain counts most).
	Weight float64 `json:"weight"`
	// MinQuality is the floor below which a model is not offered for the role
	// at all (a 0.6B model cannot follow an agent's reply contract).
	MinQuality float64 `json:"min_quality"`
	// SpeedFloor is the slowest a model may be and still be chosen, in
	// SpeedUnit: "tok/s" (higher is better), "s/page" or "rtf" (lower is better).
	SpeedFloor float64 `json:"speed_floor"`
	SpeedUnit  string  `json:"speed_unit"`
	// Kind is "domain" for a field of knowledge (math, law, code...): a
	// specialist for it is chosen only when it beats the general brain there,
	// and requests reach it through the router. Empty for the core roles.
	Kind string `json:"kind,omitempty"`
	// Description says what belongs to the field, for people and the router.
	Description string `json:"description,omitempty"`
	// Examples are typical requests in the field; the router learns from them
	// (and from checked cases added later).
	Examples []string `json:"examples,omitempty"`
	// Note says, for a field the catalog has no specialist for, why: what
	// was looked at and found no better than a general model.
	Note string `json:"note,omitempty"`
}

// KindDomain marks a field-of-knowledge role.
const KindDomain = "domain"

// Domains lists the field-of-knowledge roles.
func (c *Catalog) Domains() []Role {
	var out []Role
	for _, r := range c.Roles {
		if r.Kind == KindDomain {
			out = append(out, r)
		}
	}
	return out
}

// DomainQuality is how good a model is in a field: its own score for the
// field when the catalog has one, else its general brain score (a general
// model is as good in a field as it is in general, unless measured).
func (m Model) DomainQuality(domain string) float64 {
	if q, ok := m.Roles[domain]; ok {
		return q
	}
	return m.Roles[RoleBrain]
}

// Runtime pins the inference programs Brainiac downloads.
type Runtime struct {
	// LlamaCppRepo and LlamaCppTag name the llama.cpp release whose prebuilt
	// llama-server is used; the tag is pinned, never "latest".
	LlamaCppRepo string `json:"llamacpp_repo"`
	LlamaCppTag  string `json:"llamacpp_tag"`
	// LlamaCppSHA256 are the release assets' sha256 by asset name. An asset
	// without one here is not downloaded; GitHub's own digest must agree.
	LlamaCppSHA256 map[string]string `json:"llamacpp_sha256,omitempty"`
}

// Model is one model, with every way to get it.
type Model struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Family string `json:"family"`
	// Roles maps a role to the model's quality at it, 0-100.
	Roles map[string]float64 `json:"roles"`
	// Basis says where the quality numbers come from.
	Basis   string  `json:"basis"`
	ParamsB float64 `json:"params_b"`
	// ActiveB is the parameters used per token for a mixture-of-experts
	// model; 0 means dense (all of ParamsB).
	ActiveB float64 `json:"active_b,omitempty"`
	// KVPerTokenMB is the f16 KV cache one token of context takes, read off
	// the architecture (2 x layers x kv_heads x head_dim x 2 bytes).
	KVPerTokenMB float64 `json:"kv_per_token_mb,omitempty"`
	// Languages the model is good at, for speech; empty means not language-bound.
	Languages []string `json:"languages,omitempty"`
	// Thinks marks a reasoning model that writes a long chain of thought
	// before answering: it is given room to think, and on a CPU it is slow.
	Thinks bool `json:"thinks,omitempty"`
	// ThinkTokens is a typical chain of thought for a hard request, for
	// estimating how long an answer takes on this machine.
	ThinkTokens int `json:"think_tokens,omitempty"`
	// Ctx is the context window Brainiac's own llama-server runs the model
	// at; 0 means the standard 8192. A reasoning model needs room for its
	// thought plus the answer.
	Ctx          int    `json:"ctx,omitempty"`
	License      string `json:"license"`
	LicenseClass string `json:"license_class"`
	// Needs are CPU flags (avx2) or "gpu" a model cannot run without.
	Needs   []string `json:"needs,omitempty"`
	Sources []Source `json:"sources"`
	Notes   string   `json:"notes,omitempty"`
	// Auto marks a model the updater added from public evaluations. The
	// updater refreshes or replaces such entries, never the operator's, and
	// never uses them as the yardstick for rating other models.
	Auto bool `json:"auto,omitempty"`
}

// Source is one way to get and run a model.
type Source struct {
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	// Tag is an Ollama tag; for SourceGitHub it is the release tag.
	Tag string `json:"tag,omitempty"`
	// Repo is a Hugging Face or GitHub repository.
	Repo string `json:"repo,omitempty"`
	// Revision pins a Hugging Face commit (40 hex). A Hugging Face source
	// without one is not installed (Unpinned): "main" moves when anyone
	// with write access to the repo pushes.
	Revision string `json:"revision,omitempty"`
	// Asset is the GitHub release asset; Unpack says it is an archive.
	Asset string `json:"asset,omitempty"`
	// SHA256 pins a GitHub asset's content. An asset without it is not
	// downloaded (Unpinned); GitHub's own digest must agree when it has one.
	SHA256 string `json:"sha256,omitempty"`
	Unpack bool   `json:"unpack,omitempty"`
	// Files are the files the runtime is given, by role ("model", "mmproj",
	// "encoder", "decoder", "joiner", "tokens", "vad"...). For an unpacked
	// archive the paths are inside it.
	Files []File `json:"files,omitempty"`
	// Pip lists the worker requirement groups this source needs ("ocr", "asr").
	Pip []string `json:"pip,omitempty"`
	// Program is the executable a SourceSystem needs on PATH.
	Program string `json:"program,omitempty"`
	Quant   string `json:"quant,omitempty"`
	// SizeGB is what it takes on disk (and to download, unless installed).
	SizeGB float64 `json:"size_gb"`
	// WeightsGB is the weights read per token by a dense model (the text
	// model alone, without a vision projector); 0 means SizeGB.
	WeightsGB float64 `json:"weights_gb,omitempty"`
	// MemGB overrides the selector's memory estimate (used for workers,
	// whose footprint is the interpreter plus the model).
	MemGB float64 `json:"mem_gb,omitempty"`
	// Speed is the measured or published speed at RefCores CPU cores, in the
	// role's unit (s/page for OCR, rtf for speech); LLM speed is computed.
	Speed    float64 `json:"speed,omitempty"`
	RefCores int     `json:"ref_cores,omitempty"`
	// Worker tells the worker how to run it ("rapidocr", "tesseract",
	// "transducer", "whisper", "moonshine", "sense_voice", "silero").
	Worker string `json:"worker,omitempty"`
}

// File is one file a source provides.
type File struct {
	Path string `json:"path"`
	Role string `json:"role"`
	// SHA256 pins a Hugging Face file's content (its LFS object id); unused
	// for an archive's members, which the archive's own SHA256 covers.
	SHA256 string `json:"sha256,omitempty"`
}

//go:embed catalog.json
var embedded []byte

// Default returns the catalog shipped with this build.
func Default() (*Catalog, error) {
	return Parse(embedded)
}

// Overlay is the operator's additions, kept in <home>/catalog.local.json:
// models adopted from the scout's findings and a newer llama.cpp pin. It
// is merged over the shipped catalog; a model with the same id replaces the
// shipped one.
type Overlay struct {
	Updated string   `json:"updated"`
	Runtime *Runtime `json:"runtime,omitempty"`
	Models  []Model  `json:"models,omitempty"`
}

// ReadOverlay reads an overlay file; a missing file is an empty overlay.
func ReadOverlay(path string) (Overlay, error) {
	var o Overlay
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	return parseOverlay(data, path)
}

func parseOverlay(data []byte, path string) (Overlay, error) {
	var o Overlay
	if err := json.Unmarshal(data, &o); err != nil {
		return o, fmt.Errorf("%s: %w", path, err)
	}
	return o, nil
}

// SaveOverlay writes an overlay file. A signature beside it is removed: what
// is written here is the operator's, not what the signer signed.
func SaveOverlay(path string, o Overlay) error {
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	if err := os.Remove(SigPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// WithOverlay is the shipped catalog with the overlay at path merged in, and
// checked as a whole. The overlay is used as ReadTrustedOverlay decides; the
// note says why it was not, or is "".
func WithOverlay(path string, signedOnly bool) (*Catalog, string, error) {
	o, note, err := ReadTrustedOverlay(path, signedOnly)
	if err != nil {
		return nil, "", err
	}
	c, err := Merge(o)
	if err != nil {
		return nil, note, fmt.Errorf("with %s: %w", path, err)
	}
	return c, note, nil
}

// Merge is the shipped catalog with an overlay merged in, checked as a whole.
func Merge(o Overlay) (*Catalog, error) {
	c, err := Default()
	if err != nil {
		return nil, err
	}
	if o.Runtime != nil && o.Runtime.LlamaCppTag != "" {
		c.Runtime = *o.Runtime
	}
	for _, m := range o.Models {
		replaced := false
		for i := range c.Models {
			if c.Models[i].ID == m.ID {
				c.Models[i], replaced = m, true
			}
		}
		if !replaced {
			c.Models = append(c.Models, m)
		}
	}
	if len(o.Models) > 0 || o.Runtime != nil {
		c.Version += "+local"
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Parse reads and validates a catalog.
func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Role returns the role with that id.
func (c *Catalog) Role(id string) (Role, bool) {
	for _, r := range c.Roles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// Model returns the model with that id.
func (c *Catalog) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// ForRole lists the models that can fill a role, best quality first.
func (c *Catalog) ForRole(role string) []Model {
	var out []Model
	for _, m := range c.Models {
		if _, ok := m.Roles[role]; ok {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Roles[role] != out[j].Roles[role] {
			return out[i].Roles[role] > out[j].Roles[role]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Validate checks the catalog is internally consistent: known roles,
// runtimes and source kinds, unique ids, sizes present, licences classed.
func (c *Catalog) Validate() error {
	for name, sha := range c.Runtime.LlamaCppSHA256 {
		if !IsSHA256(sha) {
			return fmt.Errorf("catalog: llama.cpp asset %s: sha256 %q is not 64 lower-case hex", name, sha)
		}
	}
	roles := map[string]bool{}
	for _, r := range c.Roles {
		if r.ID == "" || roles[r.ID] {
			return fmt.Errorf("catalog: role %q is empty or repeated", r.ID)
		}
		switch r.SpeedUnit {
		case "tok/s", "s/page", "rtf", "":
		default:
			return fmt.Errorf("catalog: role %s has unknown speed unit %q", r.ID, r.SpeedUnit)
		}
		roles[r.ID] = true
	}
	ids := map[string]bool{}
	for _, m := range c.Models {
		if m.ID == "" || ids[m.ID] {
			return fmt.Errorf("catalog: model %q is empty or repeated", m.ID)
		}
		ids[m.ID] = true
		if len(m.Roles) == 0 {
			return fmt.Errorf("catalog: model %s fills no role", m.ID)
		}
		for r, q := range m.Roles {
			if !roles[r] {
				return fmt.Errorf("catalog: model %s names unknown role %q", m.ID, r)
			}
			if q < 0 || q > 100 {
				return fmt.Errorf("catalog: model %s has quality %v for %s (0-100)", m.ID, q, r)
			}
		}
		switch m.LicenseClass {
		case LicensePermissive, LicenseRestricted, LicenseNonCommercial:
		default:
			return fmt.Errorf("catalog: model %s has licence class %q", m.ID, m.LicenseClass)
		}
		if len(m.Sources) == 0 {
			return fmt.Errorf("catalog: model %s has no source", m.ID)
		}
		domainModel := false
		for r := range m.Roles {
			if role, _ := c.Role(r); role.Kind == KindDomain {
				domainModel = true
			}
		}
		for i, s := range m.Sources {
			if err := s.validate(); err != nil {
				return fmt.Errorf("catalog: model %s source %d: %w", m.ID, i, err)
			}
			if domainModel && s.Runtime == RuntimeWorker {
				return fmt.Errorf("catalog: model %s fills a field role but runs on the worker; a field specialist must answer chats (llama.cpp or Ollama)", m.ID)
			}
		}
	}
	return nil
}

func (s Source) validate() error {
	switch s.Runtime {
	case RuntimeOllama, RuntimeLlamaCpp, RuntimeWorker:
	default:
		return fmt.Errorf("unknown runtime %q", s.Runtime)
	}
	switch s.Kind {
	case SourceOllama:
		if s.Tag == "" || s.Runtime != RuntimeOllama {
			return fmt.Errorf("an ollama source needs a tag and the ollama runtime")
		}
	case SourceHF:
		if s.Repo == "" || len(s.Files) == 0 {
			return fmt.Errorf("an hf source needs a repo and files")
		}
	case SourceGitHub:
		if s.Repo == "" || s.Tag == "" || s.Asset == "" {
			return fmt.Errorf("a github source needs repo, tag and asset")
		}
	case SourcePip:
		if len(s.Pip) == 0 {
			return fmt.Errorf("a pip source names its requirement group")
		}
	case SourceSystem:
		if s.Program == "" {
			return fmt.Errorf("a system source names its program")
		}
	default:
		return fmt.Errorf("unknown source kind %q", s.Kind)
	}
	if s.Kind != SourceSystem && s.Kind != SourcePip && s.SizeGB <= 0 {
		return fmt.Errorf("size_gb missing")
	}
	if s.Runtime == RuntimeWorker && s.Worker == "" {
		return fmt.Errorf("a worker source says which worker engine runs it")
	}
	// A pin that is there must be well formed; a missing one is Unpinned's.
	if s.Revision != "" && !IsCommit(s.Revision) {
		return fmt.Errorf("revision %q is not a 40-hex commit", s.Revision)
	}
	if s.SHA256 != "" && !IsSHA256(s.SHA256) {
		return fmt.Errorf("sha256 %q is not 64 lower-case hex", s.SHA256)
	}
	for _, f := range s.Files {
		if f.SHA256 != "" && !IsSHA256(f.SHA256) {
			return fmt.Errorf("%s: sha256 %q is not 64 lower-case hex", f.Path, f.SHA256)
		}
	}
	return nil
}

// FileFor returns the path of the file with that role, or "".
func (s Source) FileFor(role string) string {
	for _, f := range s.Files {
		if f.Role == role {
			return f.Path
		}
	}
	return ""
}

// Label is a short human name for a source ("ollama qwen3-vl:2b-instruct",
// "hf Qwen/Qwen3-VL-2B-Instruct-GGUF Q4_K_M").
func (s Source) Label() string {
	switch s.Kind {
	case SourceOllama:
		return "ollama " + s.Tag
	case SourceHF:
		return strings.TrimSpace("hf " + s.Repo + " " + s.Quant)
	case SourceGitHub:
		return "github " + s.Repo + " " + s.Asset
	case SourcePip:
		return "pip " + strings.Join(s.Pip, ",")
	case SourceSystem:
		return "system " + s.Program
	}
	return s.Kind
}
