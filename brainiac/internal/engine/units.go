package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/catalog"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/worker"
)

// unit is one installed model and the program that runs it.
type unit struct {
	k       string
	model   string
	title   string
	runtime string
	pl      string
	mem     float64
	roles   []string
	vision  bool
	thinks  bool // a reasoning model: give it room to think
	ctx     int  // the window it runs at (0: the standard one)
	src     catalog.Source
	files   map[string]string

	llama  *LlamaProc
	ollama *backend.Ollama
	tag    string
	// shared: someone else uses the model in Ollama (it was held when
	// Brainiac first used it, or someone used it after Brainiac last did),
	// so Brainiac never unloads it.
	shared bool
	// lastCall is when Brainiac last sent the model a request.
	lastCall time.Time
	callMu   sync.Mutex
	wk       *worker.Worker
	sw       *sharedProc // the worker process this unit shares with the other worker units
	// rec is what starts its process (fingerprint); gen is the generation
	// of the engine that built it. The residency knows a unit by key and
	// recipe, and hands a resident to the newest engine's unit (G-11).
	rec string
	gen uint64
}

func (u *unit) recipe() string     { return u.rec }
func (u *unit) generation() uint64 { return u.gen }

// inherit takes over what an older engine's unit for the same running
// model knew: whether someone else uses it in Ollama, and when Brainiac
// last called it.
func (u *unit) inherit(old loadable) {
	o, ok := old.(*unit)
	if !ok || o == u || u.ollama == nil {
		return
	}
	o.callMu.Lock()
	shared, last := o.shared, o.lastCall
	o.callMu.Unlock()
	u.callMu.Lock()
	u.shared = u.shared || shared
	if last.After(u.lastCall) {
		u.lastCall = last
	}
	u.callMu.Unlock()
}

// fingerprint is a short digest of the values that start a process.
func fingerprint(parts ...any) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// fileSig is a file's size and modification time ("" when it has none): a
// file replaced in place is a new recipe, so its old process is not reused.
func fileSig(path string) string {
	if path == "" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano())
}

// stoppable is a process several units share (the Python worker).
type stoppable interface {
	Start(ctx context.Context) error
	Stop() error
}

// sharedProc is one process that several units use (OCR and speech run in
// the same worker). Each unit's load is a hold on it, and the process stops
// only when the last unit holding it is unloaded: evicting an idle OCR unit
// never ends a transcription the speech unit is running.
type sharedProc struct {
	p     stoppable
	mu    sync.Mutex
	holds int
}

func (s *sharedProc) acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.p.Start(ctx); err != nil {
		return err
	}
	s.holds++
	return nil
}

func (s *sharedProc) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds > 0 {
		s.holds--
	}
	if s.holds == 0 {
		// A process exit returns every byte; Python rarely gives a heap back.
		_ = s.p.Stop()
	}
}

func (u *unit) key() string    { return u.k }
func (u *unit) pool() string   { return u.pl }
func (u *unit) memGB() float64 { return u.mem }

func (u *unit) load(ctx context.Context) error {
	switch u.runtime {
	case catalog.RuntimeLlamaCpp:
		return u.llama.Start(ctx)
	case catalog.RuntimeOllama:
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if loaded, err := u.ollama.Loaded(lctx); err == nil {
			for name := range loaded {
				if sameTag(name, u.tag) {
					u.callMu.Lock()
					u.shared = true
					u.callMu.Unlock()
				}
			}
		}
		return nil // Ollama loads on the first call
	case catalog.RuntimeWorker:
		return u.sw.acquire(ctx)
	}
	return nil
}

// alive reports whether a loaded unit is still in memory. A llama-server can
// die after loading; Ollama keeps its own models; the worker starts again on
// its next call.
func (u *unit) alive() bool {
	if u.runtime == catalog.RuntimeLlamaCpp {
		return u.llama.Running()
	}
	return true
}

func (u *unit) unload() {
	switch u.runtime {
	case catalog.RuntimeLlamaCpp:
		u.llama.Stop()
	case catalog.RuntimeOllama:
		u.callMu.Lock()
		shared := u.shared
		u.callMu.Unlock()
		if shared {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if u.usedByOthers(ctx) {
			u.callMu.Lock()
			u.shared = true // a desktop started using it: never unload it from under them
			u.callMu.Unlock()
			return
		}
		_ = u.ollama.Unload(ctx, u.tag)
	case catalog.RuntimeWorker:
		u.sw.release()
	}
}

// chat sends one conversation turn to the unit's model.
// usedByOthers reports whether Ollama's copy of the model was used after
// Brainiac's last call: its unload time is later than Brainiac's keep_alive
// from that call. When Ollama cannot be asked, it is treated as used.
func (u *unit) usedByOthers(ctx context.Context) bool {
	exp, err := u.ollama.Expires(ctx)
	if err != nil {
		return true
	}
	u.callMu.Lock()
	last := u.lastCall
	u.callMu.Unlock()
	for name, at := range exp {
		if sameTag(name, u.tag) {
			return !last.IsZero() && at.After(last.Add(u.ollama.KeepAliveFor()+time.Minute))
		}
	}
	return false // not loaded at all: nothing to unload, nothing to take
}

// called notes a request Brainiac sent to an Ollama model.
func (u *unit) called() {
	u.callMu.Lock()
	u.lastCall = time.Now()
	u.callMu.Unlock()
}

func (u *unit) chat(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	switch u.runtime {
	case catalog.RuntimeLlamaCpp:
		c := u.llama.Client()
		if c == nil {
			if err := u.llama.Start(ctx); err != nil {
				return backend.ChatResult{}, err
			}
			c = u.llama.Client()
		}
		req.Model = u.model
		return c.Chat(ctx, req)
	default:
		req.Model = u.tag
		res, err := u.ollama.Chat(ctx, req)
		u.called()
		return res, err
	}
}

func (u *unit) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if u.runtime == catalog.RuntimeLlamaCpp {
		c := u.llama.Client()
		if c == nil {
			if err := u.llama.Start(ctx); err != nil {
				return nil, err
			}
			c = u.llama.Client()
		}
		return c.Embed(ctx, u.model, inputs)
	}
	v, err := u.ollama.Embed(ctx, u.tag, inputs)
	u.called()
	return v, err
}

// asrModel describes the unit's speech model for the worker.
func (u *unit) asrModel(threads int) worker.ASRModel {
	return worker.ASRModel{Kind: u.src.Worker, Files: u.files, Language: "auto", Threads: threads}
}

func sameTag(a, b string) bool {
	norm := func(t string) string {
		t = strings.ToLower(t)
		if !strings.Contains(t, ":") {
			t += ":latest"
		}
		return t
	}
	return norm(a) == norm(b)
}

// buildUnits turns the lock into units, one per installed model source.
// wk and sw are the worker and the holds on it its units share.
// slotDir is where llama-server units save their prompt state ("" for none).
func buildUnits(lock *install.Lock, cat *catalog.Catalog, ollamaURL, logDir, slotDir string, threads int, wk *worker.Worker, sw *sharedProc) (map[string]*unit, map[string]*unit) {
	units := map[string]*unit{}
	byRole := map[string]*unit{}
	var ol *backend.Ollama
	if wk != nil && sw == nil {
		sw = &sharedProc{p: wk}
	}
	for _, p := range lock.Plan.Picks {
		k := install.UnitKey(p.Model, p.Source)
		u, ok := units[k]
		if !ok {
			inst := lock.Units[k]
			u = &unit{k: k, model: p.Model, title: p.Title, runtime: p.Source.Runtime, pl: p.Pool, mem: p.MemGB,
				src: p.Source, files: inst.Files}
			switch p.Source.Runtime {
			case catalog.RuntimeLlamaCpp:
				u.vision = u.files["mmproj"] != ""
				ctx := selector.Context
				if cat != nil {
					if m, ok := cat.Model(p.Model); ok {
						ctx = selector.ContextFor(m, p.Source)
					}
				}
				u.ctx = ctx
				// The plan counted this cache, these checkpoints and this
				// placement; the server runs exactly that.
				u.llama = &LlamaProc{Bin: lock.LlamaServer, Model: u.files["model"], MMProj: u.files["mmproj"],
					Ctx: ctx, Threads: threads, LogPath: filepath.Join(logDir, "llama-"+p.Model+".log"), TemplateSHA: inst.TemplateSHA,
					CacheRAMMiB: p.PromptCacheMB, Checkpoints: selector.CtxCheckpoints, OnGPU: p.OnGPU, SlotDir: slotDir}
			case catalog.RuntimeOllama:
				if ol == nil {
					ol = backend.NewOllama(ollamaURL)
				}
				u.ollama, u.tag = ol, p.Source.Tag
			case catalog.RuntimeWorker:
				u.wk, u.sw = wk, sw
			}
			units[k] = u
		}
		u.roles = append(u.roles, p.Role)
		if p.Role == catalog.RoleVision && p.Source.Runtime == catalog.RuntimeOllama {
			u.vision = true
		}
		if p.Role == catalog.RoleEmbed && u.llama != nil {
			u.llama.Embedding = true
			u.llama.SlotDir = "" // no conversation to keep
		}
		byRole[p.Role] = u
	}
	return units, byRole
}
