// Speculative decoding, gated by evidence. A local model drafts tokens from
// text already in its context (llama-server's n-gram drafting) and checks
// them in a batch: faster when it copies, as when filling a form from a
// document. Batched checking is not bit-exact, so the answers can differ;
// a model serves with it only after checked cases show it as good with it
// as without, in every field both were measured in. To measure it, the
// twin "<model>+ngram" is served on request (brainiac eval --models m,m+ngram).
package engine

import (
	"fmt"
	"sort"
	"strings"

	"brainiac/internal/config"
	"brainiac/internal/evals"
)

// specSuffix names a model's speculative twin.
const specSuffix = "+" + SpecNgram

// SpecName is the name a model's n-gram speculative twin is served and
// measured under.
func SpecName(model string) string { return model + specSuffix }

// specEarned reports whether checked cases prove a model as good with
// n-gram speculation as without, and says why or why not.
func specEarned(res *evals.Results, model string) (bool, string) {
	if res == nil {
		return false, "no checked cases measured it (brainiac eval --models " + model + "," + SpecName(model) + ")"
	}
	var fields []string
	for f := range res.Scores[SpecName(model)] {
		if _, ok := res.Scores[model][f]; ok {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return false, "no field has checked cases for both " + model + " and " + SpecName(model) + " (brainiac eval --models " + model + "," + SpecName(model) + ")"
	}
	sort.Strings(fields)
	for _, f := range fields {
		if !res.Proven(SpecName(model), model, f) {
			return false, fmt.Sprintf("in %s, checked cases do not prove it as good with speculation as without", f)
		}
	}
	return true, "proven as good with speculation as without in " + strings.Join(fields, ", ")
}

// applySpeculative turns n-gram speculation on for the llama-server models
// that earned it, when the operator allowed it (speculative: ngram).
func (e *Engine) applySpeculative() {
	if e.cfg.Speculative != config.SpeculativeNgram {
		return
	}
	res := e.evals.get()
	for _, u := range e.units {
		if u.llama == nil || u.llama.Embedding {
			continue
		}
		ok, why := specEarned(res, u.model)
		if ok {
			u.llama.Spec = SpecNgram
			e.log.Printf("%s answers with n-gram speculation: %s", u.model, why)
		} else {
			e.log.Printf("%s answers without speculation: %s", u.model, why)
		}
	}
}

// specVariant is the speculative twin of an installed llama-server model,
// made on first request, or nil when name is not one.
func (e *Engine) specVariant(name string) *unit {
	base, ok := strings.CutSuffix(name, specSuffix)
	if !ok {
		return nil
	}
	var u *unit
	for _, x := range e.units {
		if strings.EqualFold(x.model, base) && x.llama != nil && !x.llama.Embedding {
			u = x
		}
	}
	if u == nil {
		return nil
	}
	e.varMu.Lock()
	defer e.varMu.Unlock()
	if v, ok := e.variants[u.k]; ok {
		return v
	}
	p := u.llama.clone()
	p.Spec = SpecNgram
	v := &unit{k: u.k + specSuffix, model: u.model, title: u.title, runtime: u.runtime, pl: u.pl, mem: u.mem,
		vision: u.vision, thinks: u.thinks, ctx: u.ctx, src: u.src, files: u.files, llama: p,
		rec: fingerprint(u.rec, SpecNgram), gen: e.gen}
	if e.variants == nil {
		e.variants = map[string]*unit{}
	}
	e.variants[u.k] = v
	return v
}

// clone is a new server for the same model with the same settings.
func (p *LlamaProc) clone() *LlamaProc {
	return &LlamaProc{Bin: p.Bin, Model: p.Model, MMProj: p.MMProj, Ctx: p.Ctx, Threads: p.Threads, Embedding: p.Embedding,
		LogPath: strings.TrimSuffix(p.LogPath, ".log") + specSuffix + ".log", TemplateSHA: p.TemplateSHA, NoSandbox: p.NoSandbox,
		Required: p.Required, Logf: p.Logf, CacheRAMMiB: p.CacheRAMMiB, Checkpoints: p.Checkpoints, OnGPU: p.OnGPU,
		SlotDir: p.SlotDir, Spec: p.Spec}
}
