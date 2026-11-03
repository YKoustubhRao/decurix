package engine

import (
	"context"
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
	// shared: Ollama already held the model for someone else when Brainiac
	// first used it, so Brainiac never unloads it.
	shared bool
	wk     *worker.Worker
	sw     *sharedProc // the worker process this unit shares with the other worker units
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
					u.shared = true
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
		if !u.shared {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = u.ollama.Unload(ctx, u.tag)
		}
	case catalog.RuntimeWorker:
		u.sw.release()
	}
}

// chat sends one conversation turn to the unit's model.
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
		return u.ollama.Chat(ctx, req)
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
	return u.ollama.Embed(ctx, u.tag, inputs)
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
func buildUnits(lock *install.Lock, cat *catalog.Catalog, ollamaURL, logDir string, threads int, wk *worker.Worker) (map[string]*unit, map[string]*unit) {
	units := map[string]*unit{}
	byRole := map[string]*unit{}
	var ol *backend.Ollama
	var sw *sharedProc
	if wk != nil {
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
				u.llama = &LlamaProc{Bin: lock.LlamaServer, Model: u.files["model"], MMProj: u.files["mmproj"],
					Ctx: ctx, Threads: threads, LogPath: filepath.Join(logDir, "llama-"+p.Model+".log"), TemplateSHA: inst.TemplateSHA}
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
		}
		byRole[p.Role] = u
	}
	return units, byRole
}
