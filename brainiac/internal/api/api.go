// Package api is Brainiac's HTTP surface, shaped like OpenAI's so any client
// that speaks it - TheBot, Decurix's grounder, an SDK - can use Brainiac as one
// more model:
//
//	POST /v1/chat/completions      text, image_url (data: URLs), input_audio; stream or not
//	POST /v1/audio/transcriptions  multipart file -> text
//	POST /v1/embeddings            text -> vectors
//	POST /v1/brainiac/ocr          a picture -> text lines with boxes
//	GET  /v1/models  /healthz  /v1/brainiac/status  /v1/brainiac/traces[/{id}]
//
// Every answer carries a "brainiac" object (mode, brain, the atoms that ran,
// what the checks found) and a trace id. Pictures are taken only inline: a
// URL to fetch would let a client make this machine fetch anything.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/engine"
	"brainiac/internal/trace"
	"brainiac/internal/web"
)

// Server serves one engine.
type Server struct {
	// Eng is the engine at start; SetEngine replaces it while serving.
	Eng    *engine.Engine
	APIKey string
	Log    *log.Logger

	cur    atomic.Pointer[engine.Engine]
	active atomic.Int64 // work requests being answered
	last   atomic.Int64 // when the last one ended (unix nanoseconds)

	// beforeEnter, in tests, runs between reading the engine and holding it.
	beforeEnter func(*engine.Engine)
}

// engine is the engine requests go to now.
func (s *Server) engine() *engine.Engine {
	if e := s.cur.Load(); e != nil {
		return e
	}
	return s.Eng
}

// Current is the engine requests go to now.
func (s *Server) Current() *engine.Engine { return s.engine() }

// SetEngine sends new requests to e and returns the engine it replaces,
// which the caller retires (Engine.Retire): requests already holding it
// finish there, and nothing new reaches it.
func (s *Server) SetEngine(e *engine.Engine) *engine.Engine {
	old := s.engine()
	s.cur.Store(e)
	return old
}

// errNoEngine is no engine taking requests (Brainiac is shutting down).
var errNoEngine = errors.New("Brainiac is shutting down; try again in a moment")

// use takes a hold on the current engine for one work request; done lets it
// go. An engine retired between being read and being held refuses the hold,
// and the engine that replaced it is used instead, so no request runs on an
// engine that is closing.
func (s *Server) use() (*engine.Engine, func(), error) {
	var last *engine.Engine
	for {
		e := s.engine()
		if e == last {
			return nil, nil, errNoEngine
		}
		if s.beforeEnter != nil {
			s.beforeEnter(e)
		}
		if e.Enter() {
			return e, e.Leave, nil
		}
		last = e
	}
}

// IdleFor is how long no work request has run (0 while one runs). Reading
// status, models or traces is not work.
func (s *Server) IdleFor() time.Duration {
	if s.active.Load() > 0 {
		return 0
	}
	last := s.last.Load()
	if last == 0 {
		return time.Duration(1 << 62) // nothing asked since start
	}
	return time.Since(time.Unix(0, last))
}

// track counts work requests for IdleFor.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		s.active.Add(1)
		defer func() {
			s.last.Store(time.Now().UnixNano())
			s.active.Add(-1)
		}()
		next.ServeHTTP(w, r)
	})
}

// Handler is the routes behind the key check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.transcribe)
	mux.HandleFunc("POST /v1/embeddings", s.embed)
	mux.HandleFunc("POST /v1/brainiac/ocr", s.ocr)
	mux.HandleFunc("POST /v1/brainiac/search", s.search)
	mux.HandleFunc("POST /v1/brainiac/fetch", s.fetch)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/brainiac/status", s.status)
	mux.HandleFunc("GET /v1/brainiac/paths", s.paths)
	mux.HandleFunc("POST /v1/brainiac/outcomes", s.outcome)
	mux.HandleFunc("GET /v1/brainiac/traces", s.traces)
	mux.HandleFunc("GET /v1/brainiac/scout", s.scoutReport)
	mux.HandleFunc("GET /v1/brainiac/update", s.updateReport)
	mux.HandleFunc("GET /v1/brainiac/traces/{id}", s.traceByID)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	return s.auth(s.track(mux))
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.APIKey != "" && r.URL.Path != "/healthz" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.APIKey)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid_api_key", "a valid API key is needed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logf(f string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(f, a...)
	}
}

// --- chat ----------------------------------------------------------------

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Name    string          `json:"name,omitempty"`
	} `json:"messages"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           int              `json:"max_tokens"`
	MaxCompletionTokens int              `json:"max_completion_tokens"`
	Temperature         *float64         `json:"temperature"`
	Stop                json.RawMessage  `json:"stop"`
	ResponseFormat      *json.RawMessage `json:"response_format"`
	Tools               json.RawMessage  `json:"tools"`
	ReasoningEffort     string           `json:"reasoning_effort"`
	// Think is Brainiac's own switch for a local thinking model.
	Think *bool `json:"think"`
	// Domain is Brainiac's own field: the request's field when the client
	// knows it (math, law...), so the router does not have to guess.
	Domain string `json:"domain"`
	// Web is Brainiac's own field: true reads the web for this answer, false
	// never does; absent follows the configured policy.
	Web *bool `json:"web"`
	// WebQuery is Brainiac's own field: the search query, in the client's
	// words. It is the only text a lookup sends out; with none, nothing is
	// looked up.
	WebQuery string `json:"web_query"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	File *struct {
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
	} `json:"file"`
}

// badRequest is a client mistake, said back in our own words.
type badRequest struct{ msg string }

func (b badRequest) Error() string { return b.msg }

func parseChat(r *http.Request) (*chatRequest, []engine.InMessage, error) {
	var cr chatRequest
	if err := json.NewDecoder(r.Body).Decode(&cr); err != nil {
		return nil, nil, badRequest{"the body is not a chat request: " + err.Error()}
	}
	if len(cr.Tools) > 0 && string(cr.Tools) != "null" && string(cr.Tools) != "[]" {
		return nil, nil, badRequest{"tools are not supported yet; describe the tool in the prompt and read the answer as text"}
	}
	if len(cr.Messages) == 0 {
		return nil, nil, badRequest{"messages is empty"}
	}
	var msgs []engine.InMessage
	for i, m := range cr.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant":
		default:
			return nil, nil, badRequest{fmt.Sprintf("message %d has role %q; system, user and assistant are supported", i, m.Role)}
		}
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		parts, err := parseContent(m.Content)
		if err != nil {
			return nil, nil, badRequest{fmt.Sprintf("message %d: %v", i, err)}
		}
		msgs = append(msgs, engine.InMessage{Role: role, Parts: parts})
	}
	return &cr, msgs, nil
}

func parseContent(raw json.RawMessage) ([]engine.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []engine.Part{{Kind: "text", Text: s}}, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("content is neither text nor a list of parts")
	}
	var out []engine.Part
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			out = append(out, engine.Part{Kind: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				return nil, errors.New("an image_url part has no url")
			}
			data, mt, err := dataURL(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			out = append(out, engine.Part{Kind: "image", Data: data, Mime: mt})
		case "input_audio":
			if p.InputAudio == nil {
				return nil, errors.New("an input_audio part has no data")
			}
			data, err := base64.StdEncoding.DecodeString(p.InputAudio.Data)
			if err != nil {
				return nil, errors.New("input_audio data is not base64")
			}
			format := strings.ToLower(p.InputAudio.Format)
			if format == "" {
				format = sniffAudio(data)
			}
			out = append(out, engine.Part{Kind: "audio", Data: data, Format: format})
		case "file":
			if p.File == nil {
				return nil, errors.New("a file part has no file")
			}
			data, mt, err := dataURL(p.File.FileData)
			if err != nil {
				return nil, err
			}
			switch {
			case strings.HasPrefix(mt, "image/"):
				out = append(out, engine.Part{Kind: "image", Data: data, Mime: mt, Name: p.File.Filename})
			case strings.HasPrefix(mt, "audio/"):
				out = append(out, engine.Part{Kind: "audio", Data: data, Format: formatOf(p.File.Filename, data), Name: p.File.Filename})
			default:
				return nil, fmt.Errorf("files of type %s are not supported yet; send pages as images", mt)
			}
		default:
			return nil, fmt.Errorf("content part type %q is not supported", p.Type)
		}
	}
	return out, nil
}

// dataURL decodes a data: URL. Remote URLs are refused on purpose.
func dataURL(u string) ([]byte, string, error) {
	if !strings.HasPrefix(u, "data:") {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return nil, "", errors.New("pictures must be sent inline as data: URLs; Brainiac does not fetch URLs")
		}
		// A bare base64 string is accepted too.
		data, err := base64.StdEncoding.DecodeString(u)
		if err != nil {
			return nil, "", errors.New("the picture is not a data: URL or base64")
		}
		return data, backend.ImageMime(data), nil
	}
	head, b64, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
	if !ok || !strings.HasSuffix(head, ";base64") {
		return nil, "", errors.New("a data: URL must be base64")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, "", errors.New("the data: URL is not valid base64")
	}
	return data, strings.TrimSuffix(head, ";base64"), nil
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	cr, msgs, err := parseChat(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req := engine.Request{Model: cr.Model, Domain: cr.Domain, Web: cr.Web, WebQuery: cr.WebQuery, Messages: msgs, Temperature: cr.Temperature, MaxTokens: cr.MaxTokens}
	if cr.MaxCompletionTokens > 0 {
		req.MaxTokens = cr.MaxCompletionTokens
	}
	if cr.ResponseFormat != nil && strings.Contains(string(*cr.ResponseFormat), "json") {
		req.JSON = true
	}
	req.Stop = parseStop(cr.Stop)
	if cr.Think != nil {
		req.Think = *cr.Think
	} else {
		req.Think = cr.ReasoningEffort == "medium" || cr.ReasoningEffort == "high"
	}
	model := cr.Model
	if model == "" {
		model = engine.ModelAuto
	}
	eng, done, err := s.use()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer done()
	if !cr.Stream {
		res, err := answered(eng.Chat(r.Context(), req))
		if err != nil {
			s.chatError(w, err, res)
			return
		}
		writeJSON(w, 200, completion(model, res))
		return
	}

	sse := &sseWriter{w: w}
	stopKeepAlive := sse.keepAlive(15 * time.Second)
	defer stopKeepAlive()
	id := trace.NewID()
	req.ID = id
	first := true
	req.OnProgress = func(note string) { sse.comment(note) }
	req.OnDelta = func(d string) {
		if first {
			first = false
			sse.data(chunk(id, model, map[string]any{"role": "assistant", "content": ""}, nil))
		}
		sse.data(chunk(id, model, map[string]any{"content": d}, nil))
	}
	res, err := answered(eng.Chat(r.Context(), req))
	if err != nil {
		if !sse.started() {
			stopKeepAlive()
			s.chatError(w, err, res)
			return
		}
		sse.data(map[string]any{"error": map[string]any{"message": s.clientMessage(err, res), "type": "server_error"}})
		sse.done()
		return
	}
	if first {
		sse.data(chunk(res.Trace.ID, model, map[string]any{"role": "assistant", "content": ""}, nil))
	}
	finish := "stop"
	if res.Truncated {
		finish = "length"
	}
	last := chunk(res.Trace.ID, model, map[string]any{}, &finish)
	last["brainiac"] = extension(res)
	sse.data(last)
	if cr.StreamOptions != nil && cr.StreamOptions.IncludeUsage {
		u := chunk(res.Trace.ID, model, nil, nil)
		u["choices"] = []any{}
		u["usage"] = usage(res)
		sse.data(u)
	}
	sse.done()
}

// answered passes an engine's answer on only when the brain said it ended:
// a cut answer is an error, never a finish_reason of "stop".
func answered(res *engine.Response, err error) (*engine.Response, error) {
	if err == nil && (res == nil || !res.Complete) {
		err = backend.ErrCutShort
	}
	return res, err
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func completion(model string, res *engine.Response) map[string]any {
	finish := "stop"
	if res.Truncated {
		finish = "length"
	}
	return map[string]any{
		"id": "chatcmpl-" + res.Trace.ID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices":  []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": res.Text}, "finish_reason": finish}},
		"usage":    usage(res),
		"brainiac": extension(res),
	}
}

func usage(res *engine.Response) map[string]any {
	return map[string]any{"prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens,
		"total_tokens": res.PromptTokens + res.CompletionTokens,
		"local_tokens": res.Trace.LocalT, "remote_tokens": res.Trace.RemoteT}
}

func extension(res *engine.Response) map[string]any {
	ext := map[string]any{"trace_id": res.Trace.ID, "mode": res.Mode, "brain": res.Brain, "atoms": res.Trace.Atoms, "ms": time.Since(res.Trace.At).Milliseconds()}
	if res.Trace.Domain != "" {
		ext["domain"] = res.Trace.Domain
		ext["domain_confidence"] = res.Trace.DomainP
	}
	if len(res.Trace.Checks) > 0 {
		ext["checks"] = res.Trace.Checks
	}
	if len(res.Trace.Notes) > 0 {
		ext["notes"] = res.Trace.Notes
	}
	return ext
}

func chunk(id, model string, delta map[string]any, finish *string) map[string]any {
	c := map[string]any{"id": "chatcmpl-" + id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model}
	if delta != nil {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		if finish != nil {
			choice["finish_reason"] = *finish
		}
		c["choices"] = []any{choice}
	}
	return c
}

// clientMessage is what a client is told about a failure: Brainiac's own
// wording for errors it knows, a pointer to the trace for anything else.
func (s *Server) clientMessage(err error, res *engine.Response) string {
	var nr *engine.ErrNoRoom
	var re *engine.RemoteError
	switch {
	case errors.As(err, &nr), errors.As(err, &re), errors.Is(err, engine.ErrNoLocalBrain), errors.Is(err, engine.ErrNoRemote),
		errors.Is(err, engine.ErrNoASR), errors.Is(err, engine.ErrNoOCR), errors.Is(err, engine.ErrNoEmbed):
		return err.Error()
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case strings.Contains(err.Error(), "is not installed here"):
		return err.Error()
	}
	s.logf("request failed: %v", err)
	if res != nil && res.Trace != nil {
		return "the request failed; see trace " + res.Trace.ID + " (GET /v1/brainiac/traces/" + res.Trace.ID + ")"
	}
	return "the request failed"
}

func (s *Server) chatError(w http.ResponseWriter, err error, res *engine.Response) {
	status := http.StatusInternalServerError
	var nr *engine.ErrNoRoom
	var re *engine.RemoteError
	switch {
	case errors.As(err, &nr), errors.Is(err, engine.ErrNoLocalBrain), errors.Is(err, engine.ErrNoRemote),
		errors.Is(err, engine.ErrNoASR), errors.Is(err, engine.ErrNoOCR), errors.Is(err, engine.ErrNoEmbed):
		status = http.StatusServiceUnavailable
	case errors.As(err, &re):
		status = http.StatusBadGateway
	case strings.Contains(err.Error(), "is not installed here"):
		status = http.StatusNotFound
	case errors.Is(err, context.Canceled):
		return
	}
	writeError(w, status, "server_error", s.clientMessage(err, res))
}

// --- speech, OCR, embeddings ------------------------------------------------

func (s *Server) transcribe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 200<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "send the audio as multipart form field \"file\"")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the form has no \"file\"")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read the file")
		return
	}
	format := formatOf(hdr.Filename, data)
	eng, done, err := s.use()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer done()
	t, rec, err := eng.Transcribe(r.Context(), data, format, r.FormValue("language"))
	if err != nil {
		s.chatError(w, err, &engine.Response{Trace: rec})
		return
	}
	switch r.FormValue("response_format") {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, t.Text)
	case "verbose_json":
		writeJSON(w, 200, map[string]any{"task": "transcribe", "language": t.Language, "duration": t.Duration, "text": t.Text,
			"segments": t.Segments, "brainiac": map[string]any{"trace_id": rec.ID, "atoms": rec.Atoms}})
	default:
		writeJSON(w, 200, map[string]any{"text": t.Text, "brainiac": map[string]any{"trace_id": rec.ID}})
	}
}

func (s *Server) ocr(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var in struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Image == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `send {"image": "data:image/png;base64,..."}`)
		return
	}
	data, _, err := dataURL(in.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	eng, done, err := s.use()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer done()
	res, rec, err := eng.OCR(r.Context(), data)
	if err != nil {
		s.chatError(w, err, &engine.Response{Trace: rec})
		return
	}
	writeJSON(w, 200, map[string]any{"engine": res.Engine, "width": res.Width, "height": res.Height, "lines": res.Lines,
		"brainiac": map[string]any{"trace_id": rec.ID, "atoms": rec.Atoms}})
}

func (s *Server) embed(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	var in struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the body is not an embeddings request")
		return
	}
	inputs := parseStop(in.Input)
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is empty")
		return
	}
	eng, done, err := s.use()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer done()
	vecs, model, err := eng.Embed(r.Context(), inputs)
	if err != nil {
		s.chatError(w, err, nil)
		return
	}
	data := make([]any, len(vecs))
	for i, v := range vecs {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data, "model": model, "usage": map[string]any{"prompt_tokens": 0, "total_tokens": 0}})
}

// --- the browser -------------------------------------------------------------

func (s *Server) browser(w http.ResponseWriter) *web.Client {
	b := s.engine().Web()
	if b == nil {
		writeError(w, http.StatusServiceUnavailable, "web_off", "the web is off in this Brainiac's settings (web: off)")
	}
	return b
}

// search is the browser's search for agents: {"query": "...", "n": 8}. The
// query is scrubbed of identity numbers and secrets before it leaves.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		Query string `json:"query"`
		N     int    `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Query) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `send {"query": "..."}`)
		return
	}
	b := s.browser(w)
	if b == nil {
		return
	}
	q, scrubbed := web.QueryFrom(in.Query)
	res, err := b.Search(r.Context(), q, in.N)
	if err != nil {
		writeError(w, http.StatusBadGateway, "search_failed", "every search engine failed; try again later")
		s.logf("search: %v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"query": q, "scrubbed": scrubbed, "results": res})
}

// fetch is the browser's page reader for agents: {"url": "...", "render":
// false, "query": "optional: return only the passages about this"}.
func (s *Server) fetch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in struct {
		URL    string `json:"url"`
		Render bool   `json:"render"`
		Query  string `json:"query"`
		Max    int    `json:"max_chars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `send {"url": "https://..."}`)
		return
	}
	b := s.browser(w)
	if b == nil {
		return
	}
	var p web.Page
	var err error
	if in.Render {
		p, err = b.Render(r.Context(), in.URL)
	} else {
		p, err = b.Fetch(r.Context(), in.URL)
	}
	if err != nil {
		status := http.StatusBadGateway
		msg := "the page could not be read"
		if errors.Is(err, web.ErrBlockedAddress) {
			status, msg = http.StatusForbidden, web.ErrBlockedAddress.Error()
		}
		writeError(w, status, "fetch_failed", msg)
		s.logf("fetch %s: %v", in.URL, err)
		return
	}
	if in.Max <= 0 {
		in.Max = 20000
	}
	if in.Query != "" {
		p.Text = web.Passages(p.Text, in.Query, in.Max)
	} else if len(p.Text) > in.Max {
		p.Text = p.Text[:in.Max]
	}
	writeJSON(w, 200, p)
}

// --- info ------------------------------------------------------------------

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	var data []any
	for _, m := range s.engine().Models() {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "brainiac", "roles": m.Roles, "runtime": m.Runtime, "note": m.Note})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.engine().Status())
}

// paths says, per field, what could answer here and why each does or does not.
func (s *Server) paths(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.engine().Paths())
}

// outcome files a client's final verdict on real answers: {"ok": false} with
// "trace" for one answer, or "since" (RFC 3339) for every answer since then.
func (s *Server) outcome(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Trace string    `json:"trace"`
		Since time.Time `json:"since"`
		OK    *bool     `json:"ok"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.OK == nil || (body.Trace == "" && body.Since.IsZero()) {
		writeError(w, http.StatusBadRequest, "invalid_request", "an outcome needs \"ok\", and \"trace\" or \"since\"")
		return
	}
	n, err := s.engine().RecordOutcome(body.Trace, body.Since, *body.OK)
	if errors.Is(err, engine.ErrNoTrace) {
		writeError(w, http.StatusNotFound, "not_found", "no recent trace with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "the outcome could not be saved")
		return
	}
	writeJSON(w, 200, map[string]any{"recorded": n})
}

// scoutReport is the newest scouting report (new open models, checked).
func (s *Server) scoutReport(w http.ResponseWriter, r *http.Request) {
	var rep json.RawMessage
	if err := s.engine().Store().ReadJSON("scout.json", &rep); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no scouting report yet: run `brainiac scout`, or wait for serve's daily scout")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(rep)
}

// updateReport is the last update cycle's report: what public evaluations
// rated, what changed, and why.
func (s *Server) updateReport(w http.ResponseWriter, r *http.Request) {
	var rep map[string]any
	if err := s.engine().Store().ReadJSON("update.json", &rep); err != nil {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "no update has run yet"}})
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"traces": s.engine().Traces().Recent(50)})
}

func (s *Server) traceByID(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.engine().Traces().Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no recent trace with that id")
		return
	}
	writeJSON(w, 200, rec)
}

// --- helpers -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": kind}})
}

// sseWriter writes a server-sent event stream, sending headers on first use.
type sseWriter struct {
	mu   sync.Mutex
	w    http.ResponseWriter
	sent bool
}

func (s *sseWriter) start() {
	if s.sent {
		return
	}
	s.sent = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) started() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *sseWriter) flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *sseWriter) data(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
	b, _ := json.Marshal(v)
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flush()
}

func (s *sseWriter) comment(note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
	fmt.Fprintf(s.w, ": %s\n\n", strings.ReplaceAll(note, "\n", " "))
	s.flush()
}

func (s *sseWriter) done() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start()
	io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

// keepAlive sends a comment every interval once the stream has started, so a
// proxy's idle timeout never cuts a long CPU answer.
func (s *sseWriter) keepAlive(every time.Duration) func() {
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s.started() {
					s.comment("working")
				}
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

// formatOf is an audio container from a file name, else from its bytes.
func formatOf(name string, data []byte) string {
	if ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), "."); ext != "" {
		if mt := mime.TypeByExtension("." + ext); mt == "" || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") || ext == "wav" {
			return ext
		}
	}
	return sniffAudio(data)
}

func sniffAudio(b []byte) string {
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "wav"
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return "ogg"
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return "flac"
	case len(b) >= 3 && string(b[:3]) == "ID3", len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "mp3"
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return "m4a"
	case len(b) >= 4 && b[0] == 0x1A && b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3:
		return "webm"
	}
	return "wav"
}
