package practice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// Chatter is the hosted-model hub (*llm.Hub): the broker forwards a practice
// desktop's model calls to it, and the drafter writes generators with it.
type Chatter interface {
	Chat(ctx context.Context, provider string, body []byte) (int, []byte, http.Header, error)
	Configured(provider string) bool
}

// Observation is how the worker ended a case, as its own callback said it.
type Observation struct {
	Kind    string
	Text    string
	Checked bool
}

// maxBrokerBody bounds every request a practice desktop sends the broker.
const maxBrokerBody = 24 << 20

// Broker stands in for the control plane for the one practice desktop of the
// active run. It serves five paths and answers 404 to everything else: model
// calls (counted against the run's token cap), the agent's end-of-turn report
// (recorded as the case's observation, never answered), its step events, its
// drafts (kept as practice drafts, never sendable) and its memory notes (dropped).
type Broker struct {
	LLM Chatter
	// Steps, when set, records a step event on the case's task.
	Steps func(workerID, taskID, text string)

	mu  sync.Mutex
	run *brokerRun
}

type brokerRun struct {
	desktopID string
	maxTokens int64
	tokens    int64
	exhausted chan struct{}
	closed    bool
	taskID    string
	obs       chan Observation
	drafts    []brokerDraft
	nextDraft int
}

type brokerDraft struct {
	ID          string
	Draft       qualification.PracticeDraft
	Attachments map[string][]byte
}

// Begin opens the broker for one run's practice desktop with a token cap.
func (b *Broker) Begin(desktopID string, maxTokens int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.run = &brokerRun{desktopID: desktopID, maxTokens: maxTokens, exhausted: make(chan struct{})}
}

// End closes the broker; later calls get 404.
func (b *Broker) End() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.run = nil
}

// StartCase starts recording a case: its task, its first observation, its drafts.
func (b *Broker) StartCase(taskID string) <-chan Observation {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == nil {
		return nil
	}
	b.run.taskID, b.run.obs, b.run.drafts = taskID, make(chan Observation, 1), nil
	return b.run.obs
}

// EndCase stops recording the case and returns its drafts with their attachments.
func (b *Broker) EndCase() ([]qualification.PracticeDraft, map[string][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == nil {
		return nil, nil
	}
	drafts := []qualification.PracticeDraft{}
	files := map[string][]byte{}
	for i, d := range b.run.drafts {
		drafts = append(drafts, d.Draft)
		for name, body := range d.Attachments {
			files[fmt.Sprintf("draft-%d-%s", i+1, name)] = body
		}
	}
	b.run.taskID, b.run.obs, b.run.drafts = "", nil, nil
	return drafts, files
}

// Tokens is what the run's model calls have used so far.
func (b *Broker) Tokens() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == nil {
		return 0
	}
	return b.run.tokens
}

// Exhausted is closed once the run reaches its token cap.
func (b *Broker) Exhausted() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == nil {
		return nil
	}
	return b.run.exhausted
}

// Handler is the broker's mux, for its own listener on the practice network.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/llm/", b.llm)
	mux.HandleFunc("/api/querytext", b.queryText)
	mux.HandleFunc("/api/agent/events", b.events)
	mux.HandleFunc("/api/outbox", b.outbox)
	mux.HandleFunc("/api/desktops/memory", b.memory)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern == "" {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBrokerBody)
		mux.ServeHTTP(w, r)
	})
}

func (b *Broker) active() *brokerRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.run
}

// llm forwards a model call when the run is under its cap and counts its usage.
func (b *Broker) llm(w http.ResponseWriter, r *http.Request) {
	run := b.active()
	if run == nil || b.LLM == nil {
		http.NotFound(w, r)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/llm/"), "/")
	provider, route, _ := strings.Cut(rest, "/")
	if provider != "azure" && provider != "claude" {
		http.NotFound(w, r)
		return
	}
	switch route {
	case "v1/models":
		if r.Method != http.MethodGet || !b.LLM.Configured(provider) {
			http.Error(w, "not available", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "default", "object": "model", "owned_by": provider}}})
		return
	case "chat", "v1/chat/completions":
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if b.over(run) {
		http.Error(w, "The practice run reached its token cap.", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	status, answer, retry, err := b.LLM.Chat(r.Context(), provider, body)
	if err != nil {
		http.Error(w, "the hosted model could not be reached", http.StatusBadGateway)
		return
	}
	b.count(run, usageTokens(answer))
	for name, values := range retry {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(answer)
}

func (b *Broker) over(run *brokerRun) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return run.maxTokens > 0 && run.tokens >= run.maxTokens
}

func (b *Broker) count(run *brokerRun, n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	run.tokens += n
	if run.maxTokens > 0 && run.tokens >= run.maxTokens && !run.closed {
		run.closed = true
		close(run.exhausted)
	}
}

// usageTokens reads an OpenAI-shaped answer's usage; 0 when it reports none.
func usageTokens(answer []byte) int64 {
	var u struct {
		Usage struct {
			Total      int64 `json:"total_tokens"`
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(answer, &u) != nil {
		return 0
	}
	if u.Usage.Total > 0 {
		return u.Usage.Total
	}
	return u.Usage.Prompt + u.Usage.Completion
}

// identify decodes a callback and checks it comes from the run's practice desktop.
func (b *Broker) identify(w http.ResponseWriter, r *http.Request, v any, desktopID func() string) (*brokerRun, bool) {
	run := b.active()
	if run == nil {
		http.NotFound(w, r)
		return nil, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return nil, false
	}
	if desktopID() != run.desktopID {
		http.Error(w, "unknown practice desktop", http.StatusForbidden)
		return nil, false
	}
	return run, true
}

// ObservedKind maps an agent's reported kind to an observation, "" when it ends nothing.
func ObservedKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "final", "message":
		return qualification.ObservedFinal
	case "query":
		return qualification.ObservedQuery
	case "approval":
		return qualification.ObservedApproval
	case "error":
		return qualification.ObservedError
	}
	return ""
}

// queryText records the case's first end-of-turn report. It never answers a
// question and never grants an approval: the worker stays where it stopped.
func (b *Broker) queryText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DesktopID    string `json:"desktop_id"`
		Kind         string `json:"kind"`
		Reply        string `json:"reply"`
		Content      string `json:"content"`
		StillWorking bool   `json:"still_working"`
		Checked      *bool  `json:"checked"`
	}
	run, ok := b.identify(w, r, &req, func() string { return req.DesktopID })
	if !ok {
		return
	}
	kind := ObservedKind(req.Kind)
	text := strings.TrimSpace(req.Reply)
	if text == "" {
		text = strings.TrimSpace(req.Content)
	}
	if kind != "" && !req.StillWorking {
		b.mu.Lock()
		if run.obs != nil {
			select {
			case run.obs <- Observation{Kind: kind, Text: text, Checked: req.Checked != nil && *req.Checked}:
			default:
			}
		}
		b.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"received"}`))
}

// events forwards a step to the case's task record.
func (b *Broker) events(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DesktopID string `json:"desktop_id"`
		Text      string `json:"text"`
	}
	run, ok := b.identify(w, r, &req, func() string { return req.DesktopID })
	if !ok {
		return
	}
	b.mu.Lock()
	taskID := run.taskID
	b.mu.Unlock()
	if b.Steps != nil && taskID != "" && strings.TrimSpace(req.Text) != "" {
		text := req.Text
		if len(text) > 4000 {
			text = text[:4000]
		}
		b.Steps(run.desktopID, taskID, text)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// outbox keeps a draft as practice evidence; it never reaches the real outbox.
func (b *Broker) outbox(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DesktopID   string   `json:"desktop_id"`
		To          []string `json:"to"`
		Cc          []string `json:"cc"`
		Subject     string   `json:"subject"`
		Body        string   `json:"body"`
		Replaces    string   `json:"replaces"`
		Attachments []struct {
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"attachments"`
	}
	run, ok := b.identify(w, r, &req, func() string { return req.DesktopID })
	if !ok {
		return
	}
	d := brokerDraft{Draft: qualification.PracticeDraft{To: req.To, Cc: req.Cc, Subject: req.Subject, Body: req.Body, Attachments: []string{}}, Attachments: map[string][]byte{}}
	for _, a := range req.Attachments {
		body, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil || !qualification.FileName(a.Name) {
			http.Error(w, "invalid attachment", http.StatusBadRequest)
			return
		}
		d.Draft.Attachments = append(d.Draft.Attachments, a.Name)
		d.Attachments[a.Name] = body
	}
	b.mu.Lock()
	replaced := false
	if req.Replaces != "" {
		kept := run.drafts[:0]
		for _, old := range run.drafts {
			if old.ID == req.Replaces {
				replaced = true
				continue
			}
			kept = append(kept, old)
		}
		run.drafts = kept
	}
	run.nextDraft++
	d.ID = fmt.Sprintf("practice-draft-%d", run.nextDraft)
	if run.obs != nil {
		run.drafts = append(run.drafts, d)
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID, "status": "draft", "replaced": replaced})
}

// memory accepts the agent's memory notes and drops them.
func (b *Broker) memory(w http.ResponseWriter, r *http.Request) {
	if b.active() == nil {
		http.NotFound(w, r)
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// BrokerListener starts the broker's own listener on the practice network's
// gateway address, once, and never on 0.0.0.0. It is never tunnelled.
type BrokerListener struct {
	// Network ensures the practice network and returns its subnet and gateway.
	Network func(ctx context.Context) (netip.Prefix, netip.Addr, error)
	Port    string
	Handler http.Handler

	mu  sync.Mutex
	srv *http.Server
	url string
}

// URL returns the broker's address for a practice desktop, starting it if needed.
// The network is checked on every call: a practice network removed after a run
// and made again may have another address, and the listener then moves to it.
func (l *BrokerListener) URL(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, gateway, err := l.Network(ctx)
	if err != nil {
		return "", err
	}
	if !gateway.IsValid() || gateway.IsUnspecified() {
		return "", fmt.Errorf("the practice network has no gateway address to bind")
	}
	port := l.Port
	if port == "" {
		port = "8091"
	}
	addr := net.JoinHostPort(gateway.String(), port)
	if l.srv != nil {
		if l.url == "http://"+addr {
			return l.url, nil
		}
		_ = l.srv.Close()
		l.srv, l.url = nil, ""
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	l.srv = &http.Server{Handler: l.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(l.srv)
	l.url = "http://" + addr
	return l.url, nil
}

// Close stops the broker's listener.
func (l *BrokerListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.srv == nil {
		return nil
	}
	err := l.srv.Close()
	l.srv, l.url = nil, ""
	return err
}
