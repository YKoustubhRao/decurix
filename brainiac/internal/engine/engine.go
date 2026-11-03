// Package engine is Brainiac's core: it takes a request in the shape of a chat
// (text, pictures, recordings), breaks it into small jobs (Atomize), runs each
// on the best installed model for it while keeping memory within budget
// (Residency), hands the brain the findings as labelled evidence, streams the
// brain's answer back, and checks the answer against its sources in code.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/cache"
	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/config"
	"brainiac/internal/evals"
	"brainiac/internal/install"
	"brainiac/internal/isolate"
	"brainiac/internal/router"
	"brainiac/internal/store"
	"brainiac/internal/trace"
	"brainiac/internal/verify"
	"brainiac/internal/web"
	"brainiac/internal/worker"
)

// Model names a client can ask for besides a concrete installed model.
const (
	ModelAuto   = "brainiac"
	ModelLocal  = "brainiac-local"
	ModelHybrid = "brainiac-hybrid"
	ModelRemote = "brainiac-remote"
)

// Errors a client is told about in plain words.
var (
	ErrNoLocalBrain = errors.New("no local brain fits this machine: set a remote brain (BRAINIAC_REMOTE) or free memory and run brainiac up again")
	ErrNoRemote     = errors.New("no remote brain is set up (BRAINIAC_REMOTE=decurix:azure, decurix:claude, azure or openai)")
	ErrNoASR        = errors.New("no speech model is installed on this machine")
	ErrNoOCR        = errors.New("no OCR model is installed on this machine")
	ErrNoEmbed      = errors.New("no embedding model is installed on this machine")
)

// RemoteError is the remote brain failing; its own message is not passed to
// clients (it can carry provider details), only the status.
type RemoteError struct {
	Status int
	Err    error
}

// Error names the failure without the provider's text.
func (e *RemoteError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("the remote brain failed (HTTP %d)", e.Status)
	}
	return "the remote brain could not be reached"
}

// Unwrap is the underlying error.
func (e *RemoteError) Unwrap() error { return e.Err }

// Request is one chat request.
type Request struct {
	// ID is the trace id to use; empty makes a new one.
	ID    string
	Model string
	// Domain names the field (math, law...) when the client knows it; empty
	// lets the router decide.
	Domain string
	// Web asks to read the web (true) or forbids it (false); nil follows the
	// configured policy. A lookup also needs WebQuery.
	Web *bool
	// WebQuery is the search query the client wrote. It is the only text a
	// web lookup sends out: the conversation itself never becomes a query.
	WebQuery    string
	Messages    []InMessage
	MaxTokens   int
	Temperature *float64
	JSON        bool
	Stop        []string
	Think       bool
	// OnDelta receives the answer as it is written.
	OnDelta func(string)
	// OnProgress receives a short note as each job starts ("reading image-1"),
	// for keep-alive comments on a stream.
	OnProgress func(string)
}

// Response is the answer and what it took.
type Response struct {
	Text string
	// Complete is the brain saying its answer ended. A response without it
	// is never returned without an error; the API checks it again.
	Complete         bool
	Truncated        bool
	Brain            string
	Mode             string
	PromptTokens     int
	CompletionTokens int
	Trace            *trace.Record
}

// Options configure an engine.
type Options struct {
	Config config.Config
	Store  *store.Store
	Lock   *install.Lock
	Logger *log.Logger
	// Catalog is the catalog with the operator's overlay; nil uses the shipped one.
	Catalog *catalog.Catalog
}

// Engine runs requests on the installed models.
type Engine struct {
	cfg      config.Config
	st       *store.Store
	lock     *install.Lock
	units    map[string]*unit
	byRole   map[string]*unit
	res      *Residency
	wk       *worker.Worker
	cache    *cache.Cache
	traces   *trace.Store
	remote   backend.Chatter
	rname    string
	threads  int
	log      *log.Logger
	mu       sync.Mutex
	cat      *catalog.Catalog
	rt       *router.Router
	evals    *evalCache
	outcomes *evals.Outcomes
	web      *web.Client
	// refs counts the holds callers took with Enter; once retired, no new
	// hold is given and drained closes when the last one is let go.
	refMu   sync.Mutex
	refs    int
	retired bool
	drained chan struct{}
}

// ErrRetired is an engine that was replaced or closed: ask the current one.
var ErrRetired = errors.New("engine: this engine was replaced; use the current one")

// Enter takes a hold on the engine for one request, so it is not closed
// under the request. It is false once the engine is retired or closed; the
// caller then asks for the current engine again. Every true is paired with
// one Leave.
func (e *Engine) Enter() bool {
	e.refMu.Lock()
	defer e.refMu.Unlock()
	if e.retired {
		return false
	}
	e.refs++
	return true
}

// Leave lets go of a hold taken with Enter.
func (e *Engine) Leave() {
	e.refMu.Lock()
	defer e.refMu.Unlock()
	e.refs--
	if e.refs == 0 && e.retired && e.drained != nil {
		close(e.drained)
		e.drained = nil
	}
}

// Retire stops new holds, waits for every request holding the engine to
// finish, however long a CPU answer takes, and then closes it. A request is
// never cut short to make way for a new engine. logf, when set, is told
// every few minutes what it still waits for.
func (e *Engine) Retire(logf func(string, ...any)) {
	e.refMu.Lock()
	e.retired = true
	var wait chan struct{}
	if e.refs > 0 {
		if e.drained == nil {
			e.drained = make(chan struct{})
		}
		wait = e.drained
	}
	e.refMu.Unlock()
	if wait != nil {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
	loop:
		for {
			select {
			case <-wait:
				break loop
			case <-t.C:
				if logf != nil {
					e.refMu.Lock()
					n := e.refs
					e.refMu.Unlock()
					logf("the replaced engine waits for %d request(s) to finish before it closes", n)
				}
			}
		}
	}
	e.Close()
}

// Web is the engine's browser (nil when the web is off).
func (e *Engine) Web() *web.Client { return e.web }

// Store is where the engine keeps its files.
func (e *Engine) Store() *store.Store { return e.st }

// New builds an engine from the lock file's plan.
func New(opt Options) (*Engine, error) {
	if opt.Lock == nil || opt.Lock.Plan == nil {
		return nil, errors.New("engine: no lock")
	}
	lg := opt.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	threads := opt.Config.Threads
	if threads <= 0 {
		threads = max(1, runtime.NumCPU()/2)
	}
	e := &Engine{cfg: opt.Config, st: opt.Store, lock: opt.Lock, threads: threads, log: lg,
		cache: cache.New(opt.Store.Path("cache")), traces: trace.NewStore(opt.Store.Path("traces"))}
	if opt.Lock.Python != "" && opt.Lock.WorkerScript != "" {
		// The worker's own venv is writable because its OCR package may fetch
		// its default models there on first use; its temp files stay in the store.
		tmp := opt.Store.Path("tmp")
		_ = os.MkdirAll(tmp, 0o700)
		e.wk = worker.New(opt.Lock.Python, opt.Lock.WorkerScript, []string{"OMP_NUM_THREADS=" + fmt.Sprint(threads), "TMPDIR=" + tmp})
		e.wk.Logf = func(f string, a ...any) { lg.Printf("worker: "+f, a...) }
		if opt.Config.Sandbox != config.SandboxOff {
			pol := isolate.ForProgram(opt.Lock.Python, []string{filepath.Dir(opt.Lock.WorkerScript), opt.Store.Path("models")}, []string{opt.Store.Venv(), tmp})
			e.wk.Sandbox = &pol
		}
	}
	e.cat = opt.Catalog
	if e.cat == nil {
		e.cat, _ = catalog.Default()
	}
	e.units, e.byRole = buildUnits(opt.Lock, e.cat, opt.Config.OllamaURL, opt.Store.Path("logs"), threads, e.wk)
	if e.cat != nil {
		for _, u := range e.units {
			if m, ok := e.cat.Model(u.model); ok {
				u.thinks = m.Thinks
			}
		}
	}
	for _, u := range e.units {
		if u.llama != nil {
			u.llama.Logf = lg.Printf
			u.llama.NoSandbox = opt.Config.Sandbox == config.SandboxOff
		}
	}
	e.rt = buildRouter(e.cat, opt.Store.Path(RouterExamplesFile))
	e.evals = &evalCache{path: opt.Store.Path("evals.json")}
	e.outcomes = evals.OpenOutcomes(opt.Store.Path(OutcomesFile))
	if opt.Config.Web != config.WebOff {
		e.web = web.New()
		e.web.SearxURL, e.web.BraveKey = opt.Config.SearxURL, opt.Config.BraveKey()
	}
	budgets := map[string]float64{}
	for _, b := range opt.Lock.Plan.Budgets {
		if !b.Unmeasured {
			budgets[b.Pool] = b.BudgetGB
		}
	}
	e.res = NewResidency(budgets, census.LiveAvailableGB)
	if r := opt.Config.Remote; r != nil {
		e.remote, e.rname = remoteChatter(r, opt.Config.RemoteKey())
	}
	return e, nil
}

// remoteChatter builds the client for a remote brain.
func remoteChatter(r *config.Remote, key string) (backend.Chatter, string) {
	switch r.Kind {
	case config.RemoteDecurix:
		return &backend.OpenAI{Name: "decurix", BaseURL: strings.TrimRight(r.URL, "/") + "/api/llm/" + r.Provider,
			Model: r.Model, NoStream: true}, "decurix:" + r.Provider
	case config.RemoteAzure:
		path := "/models/chat/completions"
		if strings.HasSuffix(strings.TrimRight(r.URL, "/"), "/models") {
			path = "/chat/completions"
		}
		return &backend.OpenAI{Name: "azure", BaseURL: r.URL, ChatPath: path, Query: "api-version=" + r.APIVersion,
			Key: key, KeyHeader: "api-key", Model: r.Model, Reasoning: reasoningModel(r.Model)}, "azure:" + r.Model
	default:
		return &backend.OpenAI{Name: "openai", BaseURL: r.URL, Key: key, Model: r.Model, Reasoning: reasoningModel(r.Model)}, "openai:" + r.Model
	}
}

func reasoningModel(m string) bool {
	m = strings.ToLower(m)
	return strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt-6") || strings.HasPrefix(m, "o1") ||
		strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4")
}

// Unload frees every model this engine loaded, keeping it usable (models
// load again on the next request). The updater calls it on an idle engine
// to make room for checking new models.
func (e *Engine) Unload() { e.res.UnloadAll() }

// Close unloads everything Brainiac loaded. A closed engine gives no new
// holds (Enter is false), so nothing starts a model server on it again.
func (e *Engine) Close() {
	e.refMu.Lock()
	e.retired = true
	e.refMu.Unlock()
	e.res.UnloadAll()
	if e.wk != nil {
		_ = e.wk.Stop()
	}
}

// Traces is the trace store.
func (e *Engine) Traces() *trace.Store { return e.traces }

// brainChoice is who answers a request.
type brainChoice struct {
	mode   string
	local  *unit
	remote backend.Chatter
	name   string
	sees   bool
	// domain is the field the request was routed to ("" for none), with
	// the router's confidence; why says how the choice was made.
	domain  string
	domainP float64
	thinks  bool
	why     string
}

// route decides who answers: the requested model name sets the mode, then a
// field specialist may take the request (routeDomain).
//
//	brainiac | brainiac-local | -hybrid | -remote   the mode, with field routing
//	brainiac-<field>                                that field's specialist
//	remote | <remote name>                          the remote brain alone
//	<installed model id>                            that model alone
func (e *Engine) route(req Request) (brainChoice, error) {
	name := strings.ToLower(strings.TrimSpace(req.Model))
	mode := e.cfg.Mode
	explicit := strings.ToLower(strings.TrimSpace(req.Domain))
	switch {
	case name == "" || name == ModelAuto:
	case name == ModelLocal:
		mode = config.ModeLocal
	case name == ModelHybrid:
		mode = config.ModeHybrid
	case name == ModelRemote:
		mode = config.ModeRemote
	case name == RemoteEvalName || (e.rname != "" && name == strings.ToLower(e.rname)):
		if e.remote == nil {
			return brainChoice{}, ErrNoRemote
		}
		sees := e.cfg.Remote.Vision && e.cfg.ShareMedia
		return brainChoice{mode: config.ModeHybrid, remote: e.remote, name: e.rname, sees: sees, why: "the client asked for the remote brain"}, nil
	case strings.HasPrefix(name, "brainiac-") && e.isDomain(strings.TrimPrefix(name, "brainiac-")):
		explicit = strings.TrimPrefix(name, "brainiac-")
	default:
		// A concrete installed model, used as the brain locally, with no routing.
		var local *unit
		for _, u := range e.units {
			if strings.EqualFold(u.model, name) && (u.runtime == catalog.RuntimeLlamaCpp || u.runtime == catalog.RuntimeOllama) && !hasRole(u, catalog.RoleEmbed) {
				local = u
			}
		}
		if local == nil {
			return brainChoice{}, fmt.Errorf("model %q is not installed here (see /v1/models)", req.Model)
		}
		return brainChoice{mode: config.ModeLocal, local: local, name: local.model, sees: local.vision, thinks: local.thinks}, nil
	}
	if explicit != "" && !e.isDomain(explicit) {
		return brainChoice{}, fmt.Errorf("domain %q is not a field Brainiac knows (see /v1/models)", explicit)
	}
	local := e.byRole[catalog.RoleBrain]
	var base brainChoice
	var baseErr error
	switch mode {
	case config.ModeLocal:
		if local == nil {
			baseErr = ErrNoLocalBrain
		} else {
			base = brainChoice{mode: mode, local: local, name: local.model, sees: local.vision, thinks: local.thinks}
		}
	default:
		if e.remote == nil {
			baseErr = ErrNoRemote
		} else {
			sees := e.cfg.Remote.Vision && (mode == config.ModeRemote || e.cfg.ShareMedia)
			base = brainChoice{mode: mode, local: local, remote: e.remote, name: e.rname, sees: sees}
		}
	}
	return e.routeDomain(req, explicit, base, baseErr)
}

func hasRole(u *unit, r string) bool {
	for _, x := range u.roles {
		if x == r {
			return true
		}
	}
	return false
}

// Chat answers one request.
func (e *Engine) Chat(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	id := req.ID
	if id == "" {
		id = trace.NewID()
	}
	rec := &trace.Record{ID: id, At: start.UTC(), Model: req.Model}
	defer func() {
		rec.Ms = time.Since(start).Milliseconds()
		e.traces.Add(rec)
	}()
	fail := func(err error) (*Response, error) {
		rec.Error = err.Error()
		return &Response{Trace: rec}, err
	}

	bc, err := e.route(req)
	if err != nil {
		return fail(err)
	}
	rec.Mode, rec.Brain, rec.Domain, rec.DomainP = bc.mode, bc.name, bc.domain, bc.domainP
	if bc.domain != "" {
		shape := shapeOf(req)
		rec.Shape = &shape
	}
	if bc.why != "" {
		rec.Notes = append(rec.Notes, bc.why)
	}

	vision := e.byRole[catalog.RoleVision]
	caps := Caps{
		BrainSees: bc.sees,
		OCR:       e.byRole[catalog.RoleOCR] != nil && e.wk != nil,
		ASR:       e.byRole[catalog.RoleASR] != nil && e.wk != nil,
		Vision:    vision != nil && vision.vision,
	}
	atoms := Atomize(req.Messages, caps)
	evidence, err := e.runAtoms(ctx, req, atoms, rec)
	if err != nil {
		return fail(err)
	}
	var webEv []string
	if ok, why := e.wantsWeb(req, bc.mode); ok {
		rec.Notes = append(rec.Notes, "read the web: "+why)
		webEv = e.webEvidence(ctx, req, rec)
	} else if why != "" {
		rec.Notes = append(rec.Notes, why)
	}

	msgs, sources := e.brainMessages(req.Messages, evidence, webEv, bc)
	breq := backend.ChatRequest{Messages: msgs, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		JSON: req.JSON, Stop: req.Stop, OnDelta: req.OnDelta, Think: req.Think || bc.thinks}
	if breq.MaxTokens <= 0 && bc.remote == nil {
		breq.MaxTokens = 2048 // a local model on a CPU should not run on for minutes by default
		if bc.thinks && bc.local != nil {
			// A reasoning model's answer comes after its chain of thought;
			// cut it short and there is no answer at all. Leave room in the
			// window for the prompt.
			ctx := bc.local.ctx
			if ctx <= 0 {
				ctx = 8192
			}
			breq.MaxTokens = min(ctx-2048, 12288)
		}
	}
	if bc.thinks && req.OnProgress != nil {
		var mu sync.Mutex
		chars, next := 0, 1200
		breq.OnThink = func(s string) {
			mu.Lock()
			defer mu.Unlock()
			chars += len(s)
			if chars >= next {
				next += 1200
				req.OnProgress(fmt.Sprintf("thinking (about %d tokens so far)", chars/4))
			}
		}
	}
	if req.OnProgress != nil {
		req.OnProgress("answering with " + bc.name)
	}
	// When the local brain may take over from a failing remote one, the
	// remote's words are held until it finishes: a client must never get the
	// start of one answer followed by another whole answer.
	fallback := bc.remote != nil && e.cfg.LocalFallback && bc.local != nil
	first := breq
	if fallback && req.OnDelta != nil {
		first.OnDelta = func(string) {}
	}
	res, atom, err := e.callBrain(ctx, bc, first)
	if err == nil && fallback && req.OnDelta != nil && res.Text != "" {
		req.OnDelta(res.Text)
	}
	if err != nil && fallback {
		rec.Notes = append(rec.Notes, "the remote brain failed ("+err.Error()+"); the local brain answered instead")
		rec.Atoms = append(rec.Atoms, atom)
		bc = brainChoice{mode: config.ModeLocal, local: bc.local, name: bc.local.model, sees: bc.local.vision}
		rec.Brain = bc.name + " (fallback)"
		rec.Mode = bc.mode
		msgs, sources = e.brainMessages(req.Messages, evidence, webEv, bc)
		breq.Messages = msgs
		res, atom, err = e.callBrain(ctx, bc, breq)
	}
	rec.Atoms = append(rec.Atoms, atom)
	if err != nil {
		return fail(err)
	}
	if atom.Tokens != nil {
		if atom.Tokens.Remote {
			rec.RemoteT += atom.Tokens.Prompt + atom.Tokens.Completion
			if r := e.cfg.Remote; r != nil {
				rec.USD += (float64(atom.Tokens.Prompt)*r.USDPerMTokIn + float64(atom.Tokens.Completion)*r.USDPerMTokOut) / 1e6
			}
		} else {
			rec.LocalT += atom.Tokens.Prompt + atom.Tokens.Completion
		}
	}
	if res.Truncated {
		if strings.TrimSpace(res.Text) == "" && bc.thinks {
			rec.Notes = append(rec.Notes, "the reasoning model used all its room thinking and wrote no answer: ask again with a larger max_tokens, or name the general brain's model")
		} else {
			rec.Notes = append(rec.Notes, "the answer was cut off at the length limit")
		}
	}
	if figs := verify.UnsupportedFigures(res.Text, sources...); len(figs) > 0 {
		rec.Checks = append(rec.Checks, trace.Check{Kind: "unsupported_figures",
			Detail: "figures in the answer that appear in no message or evidence (computed ones are expected here; read ones are not)", Items: figs})
	}
	return &Response{Text: res.Text, Complete: true, Truncated: res.Truncated, Brain: rec.Brain, Mode: bc.mode,
		PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens, Trace: rec}, nil
}

// callBrain runs the brain and returns its trace atom.
func (e *Engine) callBrain(ctx context.Context, bc brainChoice, req backend.ChatRequest) (backend.ChatResult, trace.Atom, error) {
	atom := trace.Atom{ID: "answer", Op: "brain", Model: bc.name}
	t0 := time.Now()
	var res backend.ChatResult
	var err error
	if bc.remote != nil {
		atom.Runtime = "remote"
		res, err = bc.remote.Chat(ctx, req)
		if err != nil {
			var h *backend.HTTPError
			status := 0
			if errors.As(err, &h) {
				status = h.Status
			}
			e.log.Printf("remote brain: %v", err)
			err = &RemoteError{Status: status, Err: err}
		}
	} else {
		atom.Runtime = bc.local.runtime
		release, loadMs, aerr := e.res.Acquire(ctx, bc.local)
		if aerr != nil {
			atom.Error = aerr.Error()
			return res, atom, aerr
		}
		atom.LoadMs = loadMs
		res, err = bc.local.chat(ctx, req)
		release()
	}
	atom.Ms = time.Since(t0).Milliseconds()
	if err == nil && !res.Complete {
		// No backend may pass a cut answer off as a whole one.
		err = fmt.Errorf("%s: %w", bc.name, backend.ErrCutShort)
	}
	if err != nil {
		atom.Error = err.Error()
		return res, atom, err
	}
	atom.OutSize = len(res.Text)
	atom.Tokens = &trace.Tokens{Prompt: res.PromptTokens, Completion: res.CompletionTokens, Cached: res.CachedTokens, Remote: bc.remote != nil}
	return res, atom, nil
}

// opOrder runs all worker jobs before vision jobs, so models swap as few
// times as possible on a machine that can hold one big model at a time.
var opOrder = map[string]int{"ocr": 0, "asr": 1, "describe": 2}

// runAtoms runs the perception jobs and returns the evidence per media id.
func (e *Engine) runAtoms(ctx context.Context, req Request, atoms []Atom, rec *trace.Record) (map[string][]string, error) {
	sort.SliceStable(atoms, func(i, j int) bool { return opOrder[atoms[i].Op] < opOrder[atoms[j].Op] })
	evidence := map[string][]string{}
	for _, a := range atoms {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part := req.Messages[a.Msg].Parts[a.Part]
		if req.OnProgress != nil {
			req.OnProgress(a.Op + " " + a.ID)
		}
		var ev string
		var ta trace.Atom
		var err error
		switch a.Op {
		case "ocr":
			ev, ta, err = e.ocrAtom(ctx, a.ID, part.Data)
		case "asr":
			ev, ta, err = e.asrAtom(ctx, a.ID, part)
		case "describe":
			ev, ta, err = e.describeAtom(ctx, a.ID, part.Data)
		}
		rec.Atoms = append(rec.Atoms, ta)
		if err != nil {
			var nr *ErrNoRoom
			if errors.As(err, &nr) {
				return nil, err
			}
			// One unreadable picture does not sink the request; the brain is told.
			ev = missingEvidence(a.ID, a.Op, err.Error())
		}
		if ta.Tokens != nil {
			rec.LocalT += ta.Tokens.Prompt + ta.Tokens.Completion
		}
		evidence[a.ID] = append(evidence[a.ID], ev)
	}
	return evidence, nil
}

func (e *Engine) ocrAtom(ctx context.Context, id string, img []byte) (string, trace.Atom, error) {
	u := e.byRole[catalog.RoleOCR]
	ta := trace.Atom{ID: id, Op: "ocr", Model: u.model, Runtime: u.runtime}
	r, cached, loadMs, ms, err := e.ocr(ctx, u, img)
	ta.Cached, ta.LoadMs, ta.Ms = cached, loadMs, ms
	if err != nil {
		ta.Error = err.Error()
		return "", ta, err
	}
	ev, unsure := ocrEvidence(id, u.model, r)
	ta.OutSize, ta.Unsure = len(ev), unsure
	return ev, ta, nil
}

// ocr reads a picture's text through the cache.
func (e *Engine) ocr(ctx context.Context, u *unit, img []byte) (worker.OCRResult, bool, int64, int64, error) {
	t0 := time.Now()
	key := cache.Key("ocr", u.k, u.src.Worker, img)
	var r worker.OCRResult
	if e.cache.Get(key, &r) {
		return r, true, 0, time.Since(t0).Milliseconds(), nil
	}
	release, loadMs, err := e.res.Acquire(ctx, u)
	if err != nil {
		return r, false, 0, 0, err
	}
	defer release()
	r, err = e.wk.OCR(ctx, img, u.src.Worker)
	if err != nil {
		return r, false, loadMs, time.Since(t0).Milliseconds(), err
	}
	e.cache.Put(key, r)
	return r, false, loadMs, time.Since(t0).Milliseconds(), nil
}

func (e *Engine) asrAtom(ctx context.Context, id string, p Part) (string, trace.Atom, error) {
	u := e.byRole[catalog.RoleASR]
	ta := trace.Atom{ID: id, Op: "asr", Model: u.model, Runtime: u.runtime}
	t, cached, loadMs, ms, err := e.transcribe(ctx, p.Data, p.Format, "")
	ta.Cached, ta.LoadMs, ta.Ms = cached, loadMs, ms
	if err != nil {
		ta.Error = err.Error()
		return "", ta, err
	}
	ev := transcriptEvidence(id, u.model, t)
	ta.OutSize = len(ev)
	return ev, ta, nil
}

// transcribe turns audio into a transcript through the cache.
func (e *Engine) transcribe(ctx context.Context, audio []byte, format, lang string) (worker.Transcript, bool, int64, int64, error) {
	u := e.byRole[catalog.RoleASR]
	var t worker.Transcript
	if u == nil || e.wk == nil {
		return t, false, 0, 0, ErrNoASR
	}
	t0 := time.Now()
	if lang == "" {
		lang = "auto"
	}
	key := cache.Key("asr", u.k, format+"|"+lang, audio)
	if e.cache.Get(key, &t) {
		return t, true, 0, time.Since(t0).Milliseconds(), nil
	}
	release, loadMs, err := e.res.Acquire(ctx, u)
	if err != nil {
		return t, false, 0, 0, err
	}
	defer release()
	vad := ""
	if v := e.byRole[catalog.RoleVAD]; v != nil {
		vad = v.files["vad"]
	}
	m := u.asrModel(e.threads)
	m.Language = lang
	t, err = e.wk.Transcribe(ctx, audio, format, m, vad)
	if err != nil {
		return t, false, loadMs, time.Since(t0).Milliseconds(), err
	}
	e.cache.Put(key, t)
	return t, false, loadMs, time.Since(t0).Milliseconds(), nil
}

const describeSystem = "You describe pictures for someone who cannot see them. Be factual and complete; never invent what you cannot see."
const describePrompt = "Describe this picture: what kind it is (screenshot, document, photo, chart...), its layout, every piece of text you can read, and anything notable such as errors, highlighted items or numbers. Say when something is unclear."

func (e *Engine) describeAtom(ctx context.Context, id string, img []byte) (string, trace.Atom, error) {
	u := e.byRole[catalog.RoleVision]
	ta := trace.Atom{ID: id, Op: "describe", Model: u.model, Runtime: u.runtime}
	t0 := time.Now()
	key := cache.Key("describe", u.k, describePrompt, img)
	var text string
	if e.cache.Get(key, &text) {
		ta.Cached, ta.Ms = true, time.Since(t0).Milliseconds()
		ev := describeEvidence(id, u.model, text)
		ta.OutSize = len(ev)
		return ev, ta, nil
	}
	release, loadMs, err := e.res.Acquire(ctx, u)
	if err != nil {
		ta.Error = err.Error()
		return "", ta, err
	}
	defer release()
	ta.LoadMs = loadMs
	zero := 0.0
	res, err := u.chat(ctx, backend.ChatRequest{MaxTokens: 512, Temperature: &zero, Messages: []backend.Message{
		{Role: "system", Text: describeSystem},
		{Role: "user", Text: describePrompt, Images: []backend.Image{{Data: shrink(img, e.imageMaxSide())}}},
	}})
	ta.Ms = time.Since(t0).Milliseconds()
	if err != nil {
		ta.Error = err.Error()
		return "", ta, err
	}
	ta.Tokens = &trace.Tokens{Prompt: res.PromptTokens, Completion: res.CompletionTokens}
	e.cache.Put(key, res.Text)
	ev := describeEvidence(id, u.model, res.Text)
	ta.OutSize = len(ev)
	return ev, ta, nil
}

func (e *Engine) imageMaxSide() int {
	switch {
	case e.cfg.ImageMaxSide < 0:
		return 0
	case e.cfg.ImageMaxSide == 0:
		return 1280
	}
	return e.cfg.ImageMaxSide
}

// brainMessages builds what the brain is sent: each picture or recording is
// replaced, in place, by its evidence; pictures go along too when the brain
// sees them. It also returns the texts an answer's figures may come from.
func (e *Engine) brainMessages(in []InMessage, evidence map[string][]string, webEv []string, bc brainChoice) ([]backend.Message, []string) {
	ids := mediaIDs(in)
	var out []backend.Message
	var sources []string
	anyEvidence := false
	for mi, m := range in {
		var text strings.Builder
		var imgs []backend.Image
		for pi, p := range m.Parts {
			switch p.Kind {
			case "text":
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(p.Text)
				if m.Role != "assistant" {
					sources = append(sources, p.Text)
				}
			case "image", "audio":
				id := ids[[2]int{mi, pi}]
				evs := evidence[id]
				if p.Kind == "image" && bc.sees {
					data := p.Data
					if bc.remote == nil {
						data = shrink(data, e.imageMaxSide())
					}
					label := id
					if p.Name != "" {
						label += " (" + p.Name + ")"
					}
					imgs = append(imgs, backend.Image{Data: data, Mime: backend.ImageMime(data), Label: label})
				}
				if len(evs) == 0 && !(p.Kind == "image" && bc.sees) {
					why := "no model here can read it"
					evs = []string{missingEvidence(id, p.Kind, why)}
				}
				for _, ev := range evs {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(ev)
					sources = append(sources, ev)
					anyEvidence = true
				}
			}
		}
		out = append(out, backend.Message{Role: m.Role, Text: text.String(), Images: imgs})
	}
	// Web evidence goes with the newest user message, after its own words.
	if len(webEv) > 0 {
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Role == "user" {
				out[i].Text = strings.TrimRight(out[i].Text, "\n") + "\n\n" + strings.Join(webEv, "\n")
				break
			}
		}
		sources = append(sources, webEv...)
		anyEvidence = true
	}
	if anyEvidence {
		note := EvidenceNote
		if len(webEv) > 0 {
			note += "\n" + WebNote
		}
		if len(out) > 0 && out[0].Role == "system" {
			out[0].Text = strings.TrimRight(out[0].Text, "\n") + "\n\n" + note
		} else {
			out = append([]backend.Message{{Role: "system", Text: note}}, out...)
		}
	}
	return out, sources
}

// Transcribe is speech to text alone (the /v1/audio/transcriptions route).
func (e *Engine) Transcribe(ctx context.Context, audio []byte, format, lang string) (worker.Transcript, *trace.Record, error) {
	start := time.Now()
	rec := &trace.Record{ID: trace.NewID(), At: start.UTC(), Model: "asr", Mode: config.ModeLocal}
	defer func() { rec.Ms = time.Since(start).Milliseconds(); e.traces.Add(rec) }()
	t, cached, loadMs, ms, err := e.transcribe(ctx, audio, format, lang)
	ta := trace.Atom{ID: "audio-1", Op: "asr", Cached: cached, LoadMs: loadMs, Ms: ms}
	if u := e.byRole[catalog.RoleASR]; u != nil {
		ta.Model, ta.Runtime = u.model, u.runtime
	}
	if err != nil {
		ta.Error, rec.Error = err.Error(), err.Error()
	}
	rec.Atoms = append(rec.Atoms, ta)
	return t, rec, err
}

// OCR reads a picture's text alone (the /v1/brainiac/ocr route).
func (e *Engine) OCR(ctx context.Context, img []byte) (worker.OCRResult, *trace.Record, error) {
	start := time.Now()
	rec := &trace.Record{ID: trace.NewID(), At: start.UTC(), Model: "ocr", Mode: config.ModeLocal}
	defer func() { rec.Ms = time.Since(start).Milliseconds(); e.traces.Add(rec) }()
	u := e.byRole[catalog.RoleOCR]
	if u == nil || e.wk == nil {
		rec.Error = ErrNoOCR.Error()
		return worker.OCRResult{}, rec, ErrNoOCR
	}
	r, cached, loadMs, ms, err := e.ocr(ctx, u, img)
	ta := trace.Atom{ID: "image-1", Op: "ocr", Model: u.model, Runtime: u.runtime, Cached: cached, LoadMs: loadMs, Ms: ms}
	if err != nil {
		ta.Error, rec.Error = err.Error(), err.Error()
	}
	rec.Atoms = append(rec.Atoms, ta)
	return r, rec, err
}

// Embed turns texts into vectors with the embedding model.
func (e *Engine) Embed(ctx context.Context, inputs []string) ([][]float32, string, error) {
	u := e.byRole[catalog.RoleEmbed]
	if u == nil {
		return nil, "", ErrNoEmbed
	}
	release, _, err := e.res.Acquire(ctx, u)
	if err != nil {
		return nil, u.model, err
	}
	defer release()
	v, err := u.embed(ctx, inputs)
	return v, u.model, err
}

// ModelInfo is one model a client may name.
type ModelInfo struct {
	ID      string   `json:"id"`
	Roles   []string `json:"roles,omitempty"`
	Runtime string   `json:"runtime,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Models lists the virtual models and the installed ones.
func (e *Engine) Models() []ModelInfo {
	out := []ModelInfo{
		{ID: ModelAuto, Note: "mode " + e.cfg.Mode},
		{ID: ModelLocal, Note: "everything on this machine"},
	}
	if e.remote != nil {
		out = append(out, ModelInfo{ID: ModelHybrid, Note: "local perception, remote brain " + e.rname},
			ModelInfo{ID: ModelRemote, Note: "remote brain " + e.rname + " sees pictures"},
			ModelInfo{ID: RemoteEvalName, Note: "the remote brain alone, no field routing"})
	}
	if e.cat != nil {
		for _, d := range e.cat.Domains() {
			note := "no specialist installed; the brain answers"
			if u := e.byRole[d.ID]; u != nil {
				note = "the " + d.Title + " specialist: " + u.model
			}
			out = append(out, ModelInfo{ID: "brainiac-" + d.ID, Roles: []string{d.ID}, Note: note})
		}
	}
	seen := map[string]bool{}
	var keys []string
	for k := range e.units {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		u := e.units[k]
		if seen[u.model] {
			continue
		}
		seen[u.model] = true
		out = append(out, ModelInfo{ID: u.model, Roles: u.roles, Runtime: u.runtime})
	}
	return out
}

// Status is what the engine holds now.
type Status struct {
	Mode    string            `json:"mode"`
	Remote  string            `json:"remote,omitempty"`
	Roles   map[string]string `json:"roles"`
	Loaded  []string          `json:"loaded"`
	Catalog string            `json:"catalog"`
	Machine string            `json:"machine"`
	// Unconfined are the running model servers that can reach more than their
	// own files: what each can still reach ("files", "network", "udp").
	Unconfined map[string][]string `json:"unconfined,omitempty"`
}

// Status reports the mode, the model per role and what is in memory.
func (e *Engine) Status() Status {
	s := Status{Mode: e.cfg.Mode, Remote: e.rname, Roles: map[string]string{}, Loaded: e.res.Loaded(),
		Catalog: e.lock.Plan.CatalogVersion, Machine: e.lock.Plan.Machine}
	for r, u := range e.byRole {
		s.Roles[r] = u.model + " (" + u.runtime + ")"
	}
	for _, u := range e.units {
		if u.llama == nil {
			continue
		}
		if open := u.llama.Open(); len(open) > 0 {
			if s.Unconfined == nil {
				s.Unconfined = map[string][]string{}
			}
			s.Unconfined[u.model] = open
		}
	}
	return s
}

// LogDir is where model server logs go.
func (e *Engine) LogDir() string { return filepath.Join(e.st.Root, "logs") }
