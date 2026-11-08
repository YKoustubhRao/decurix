package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/trace"
	"thebot/internal/withheld"
)

// /api/llm/call is one model call soul makes for the Python agent (plan rule 3, phase 1: one path for every
// worker-side call). The agent's own questions - the final check, a taught skill's values, the grounder - used to go
// straight to the control plane's proxy, so the run record had two paths and only soul's left a trace line per call.
// Through here a call goes out through soul's own provider chain (the concurrency limit, usage metering, the trace,
// the proxy's tag headers and the prompt-cache key) exactly as soul's own calls do, with the caller's purpose (py.*)
// recorded verbatim.
//
// Not a job: nothing here is added to any job's usage (llm.WithUsage is never on this context), so the agent counts
// the call once, from the usage this route answers with (usage.py), and soul never counts it a second time.
//
// The approval gate never calls here (agent/decurix_agent/soul_call.py refuses to under IN_GATE): the gate is another
// user's process and must not depend on, or believe an answer from, a process of the worker's user.
//
// llmCallPath is the route.
const llmCallPath = "/api/llm/call"

// maxLLMCallBody bounds one call's request: the control plane's proxy takes no larger one (its MaxRequestBytes), and
// a scanned document's pages are the largest thing the agent sends.
const maxLLMCallBody = 24 << 20

// defaultLLMCallTimeout is the agent's llm_client timeout (DECURIX_LLM_TIMEOUT_SECONDS' default); maxLLMCallTimeout
// is the most a caller may ask for (a local model's cold read of a long prompt).
const (
	defaultLLMCallTimeout = 240 * time.Second
	maxLLMCallTimeout     = 15 * time.Minute
)

// maxLLMCallMessages and maxLLMCallTokens bound what one call may ask for.
const (
	maxLLMCallMessages = 256
	maxLLMCallTokens   = 64000
	maxLLMCallStops    = 8
)

// callModel is the form of a model name a caller may name ("" or "default" is soul's own default model).
var callModel = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// llmCallRequest is one call as the agent sends it: OpenAI-shaped messages, the call's limits and what it is for.
type llmCallRequest struct {
	// Brain is the brain the caller expects soul to have: "hosted" (llm_client) or "local" (ollama_client). A soul
	// with the other refuses, and the caller goes its own way.
	Brain    string        `json:"brain"`
	Purpose  string        `json:"purpose"`
	Task     string        `json:"task"`
	Turn     string        `json:"turn"`
	Model    string        `json:"model"`
	Messages []callMessage `json:"messages"`
	// MaxTokens caps the answer; Temperature is sent when set and not zero (a provider's default otherwise).
	MaxTokens   int      `json:"max_tokens"`
	Temperature *float64 `json:"temperature"`
	// ResponseFormat is an OpenAI response_format object, or null.
	ResponseFormat json.RawMessage `json:"response_format"`
	Stop           []string        `json:"stop"`
	TimeoutMs      int             `json:"timeout_ms"`
	// Secrets are the task's secret values: withheld from everything soul writes or says of the call, as a job's are.
	Secrets []string `json:"secrets"`
}

// callMessage is one OpenAI-shaped message: content is a string or a list of text and image_url parts.
type callMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// callPart is one part of a message's content list.
type callPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// callRefusal is a request soul will not make (400 "where":"soul"): the caller takes its own path instead.
type callRefusal struct{ why string }

func (e callRefusal) Error() string { return e.why }

// handleLLMCall makes one model call for the agent and answers {text, truncated, usage, model, provider}.
func (s *server) handleLLMCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "where": "soul"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLLMCallBody)
	var req llmCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": fmt.Sprintf("a call is at most %d bytes", maxLLMCallBody), "where": "soul"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the call is not JSON of the expected shape", "where": "soul"})
		return
	}
	model, messages, opts, timeout, err := s.callOf(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "where": "soul"})
		return
	}
	secrets := withheld.Clean(req.Secrets)

	task := req.Task
	if !llm.TaskIDShape.MatchString(task) {
		task = ""
	}
	// The turn is the operator's "<job>:<n>" the asking command ran in (DECURIX_TURN_ID), kept only when it is of the
	// proxy's turn form (llm.WithTags drops it otherwise); its job is the part before the colon.
	ctx := llm.WithTags(r.Context(), llm.Tags{Task: task, Turn: req.Turn})
	if turn := llm.TagsFrom(ctx).Turn; turn != "" {
		job, _, _ := strings.Cut(turn, ":")
		ctx = llm.WithTags(r.Context(), llm.Tags{Task: task, Job: job, Turn: turn})
	}
	ctx = withheld.With(trace.WithWriter(ctx, s.tracer), secrets)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := s.provider.Chat(ctx, model, messages, opts)
	if err != nil {
		status, why := callFailure(err)
		why = memory.RedactSecrets(secrets.Text(why)) // the task's values, then anything secret-shaped, as the trace does
		log.Printf("[soul] a call for the agent (%s) failed: HTTP %d", req.Purpose, status)
		writeJSON(w, status, map[string]any{"error": why, "where": "provider"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"text": res.Text, "truncated": res.Truncated, "model": model, "provider": s.provider.Name(),
		"usage": map[string]int{"prompt_tokens": res.PromptTokens, "completion_tokens": res.ResponseTokens,
			"cached_tokens": res.CachedTokens},
	})
}

// callOf is the call a request asks for - the model, soul's messages, the options and the deadline - or a
// callRefusal saying why soul will not make it.
func (s *server) callOf(req llmCallRequest) (string, []llm.Message, llm.Options, time.Duration, error) {
	var none llm.Options
	if !llm.ValidAgentPurpose(req.Purpose) {
		return "", nil, none, 0, callRefusal{"purpose must be one of the agent's own (py.* or legacy.*)"}
	}
	if want, have := req.Brain, brainKind(s.provider); want != have {
		return "", nil, none, 0, callRefusal{fmt.Sprintf("this soul's brain is %s, not %q", have, want)}
	}
	if len(req.Messages) == 0 || len(req.Messages) > maxLLMCallMessages {
		return "", nil, none, 0, callRefusal{fmt.Sprintf("a call has 1 to %d messages", maxLLMCallMessages)}
	}
	if req.MaxTokens < 0 || req.MaxTokens > maxLLMCallTokens {
		return "", nil, none, 0, callRefusal{fmt.Sprintf("max_tokens is at most %d", maxLLMCallTokens)}
	}
	if len(req.Stop) > maxLLMCallStops {
		return "", nil, none, 0, callRefusal{fmt.Sprintf("at most %d stop sequences", maxLLMCallStops)}
	}
	if len(req.Secrets) > withheld.MaxValues || slices.ContainsFunc(req.Secrets, func(v string) bool {
		return len(v) > withheld.MaxValueLen
	}) {
		return "", nil, none, 0, callRefusal{fmt.Sprintf("too many or too long secret values (at most %d, each at most %d bytes)",
			withheld.MaxValues, withheld.MaxValueLen)}
	}
	format, err := responseFormatOf(req.ResponseFormat)
	if err != nil {
		return "", nil, none, 0, err
	}
	model := s.defaultModel()
	if req.Model != "" && req.Model != "default" {
		if !callModel.MatchString(req.Model) {
			return "", nil, none, 0, callRefusal{"model is not a model name"}
		}
		model = req.Model
	}
	messages := make([]llm.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		converted, err := messagesOf(m)
		if err != nil {
			return "", nil, none, 0, callRefusal{fmt.Sprintf("message %d: %v", i+1, err)}
		}
		messages = append(messages, converted...)
	}
	timeout := defaultLLMCallTimeout
	if req.TimeoutMs > 0 {
		timeout = min(time.Duration(req.TimeoutMs)*time.Millisecond, maxLLMCallTimeout)
	}
	opts := llm.Options{TimeoutMs: int(timeout / time.Millisecond), MaxTokens: req.MaxTokens, Stop: req.Stop,
		Purpose: llm.AgentPurpose(req.Purpose), ResponseFormat: format}
	if req.Temperature != nil && *req.Temperature > 0 && *req.Temperature <= 2 {
		opts.Temperature = *req.Temperature
	}
	return model, messages, opts, timeout, nil
}

// defaultModel is the model soul's own calls use by default.
func (s *server) defaultModel() string {
	if s.sched != nil && s.sched.Models.Default != "" {
		return s.sched.Models.Default
	}
	return "default"
}

// brainKind is "local" for a soul whose brain is Ollama, "hosted" for any other.
func brainKind(p llm.Provider) string {
	if p.Name() == "ollama" {
		return "local"
	}
	return "hosted"
}

// responseFormatOf is a request's response_format as llm.Options takes it: "" for none, the object as it is for
// json_object, json_schema or text; anything else is refused.
func responseFormatOf(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var f struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", callRefusal{"response_format is not an object"}
	}
	switch f.Type {
	case "json_object", "json_schema", "text":
		return string(raw), nil
	}
	return "", callRefusal{"response_format's type is not json_object, json_schema or text"}
}

// messagesOf is one OpenAI-shaped message as soul's messages. A string is one message. A list of parts keeps its
// order: the text before the first picture is the message's text, the text between two pictures labels the picture
// after it (each provider shows a label right before its picture), and text after the last picture is a message of
// its own, of the same role, after it.
func messagesOf(m callMessage) ([]llm.Message, error) {
	switch m.Role {
	case "system", "user", "assistant":
	default:
		return nil, fmt.Errorf("role %q is not system, user or assistant", m.Role)
	}
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return []llm.Message{{Role: m.Role, Content: text}}, nil
	}
	var parts []callPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, errors.New("content is neither text nor a list of parts")
	}
	msg := llm.Message{Role: m.Role}
	var pending []string // text since the last picture
	for _, p := range parts {
		switch p.Type {
		case "text":
			pending = append(pending, p.Text)
		case "image_url":
			if p.ImageURL == nil {
				return nil, errors.New("an image_url part has no url")
			}
			img, err := dataURLBytes(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			label := strings.Join(pending, "\n")
			if len(msg.Images) == 0 {
				msg.Content, label = label, "" // the text before the first picture is the message's own
			}
			msg.Images = append(msg.Images, img)
			msg.ImageLabels = append(msg.ImageLabels, label) // one label per picture, by index
			pending = nil
		default:
			return nil, fmt.Errorf("a part of type %q is neither text nor image_url", p.Type)
		}
	}
	if len(msg.Images) == 0 {
		msg.Content = strings.Join(pending, "\n")
		return []llm.Message{msg}, nil
	}
	if !slices.ContainsFunc(msg.ImageLabels, func(l string) bool { return l != "" }) {
		msg.ImageLabels = nil
	}
	out := []llm.Message{msg}
	if rest := strings.Join(pending, "\n"); rest != "" {
		out = append(out, llm.Message{Role: m.Role, Content: rest})
	}
	return out, nil
}

// dataURLBytes is the picture a data: URL carries in base64.
func dataURLBytes(url string) ([]byte, error) {
	head, data, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(head, "data:") || !strings.HasSuffix(head, ";base64") {
		return nil, errors.New("a picture must be a base64 data: URL")
	}
	img, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, errors.New("a picture's base64 does not decode")
	}
	return img, nil
}

// callFailure is the status and message a failed call is answered with: a provider's own HTTP status as it is (so
// the agent maps a 429, a 5xx and a 400 as it always has), 504 for a deadline, 502 for anything else.
func callFailure(err error) (int, string) {
	var httpErr *llm.HTTPError
	switch {
	case errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status <= 599:
		return httpErr.Status, truncateLog(httpErr.Message, 600)
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "the model did not answer in time"
	}
	return http.StatusBadGateway, truncateLog(err.Error(), 600)
}

// callKeyEnv names the file soul writes its call key to (callKeyPath's default otherwise).
const callKeyEnv = "SOUL_CALL_KEY_FILE"

// callKeyPath is where soul keeps its call key: SOUL_CALL_KEY_FILE, or soul-call.key beside its workspace
// (~/.decurix/soul-call.key, which the agent's soul_call.py reads).
func callKeyPath(workspaceRoot string) string {
	if p := os.Getenv(callKeyEnv); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(filepath.Clean(workspaceRoot)), "soul-call.key")
}

// newCallKey is a fresh random call key: the bearer that opens /api/llm/call and nothing else.
//
// Why a second key: the commands soul runs (the worker's `gui` among them, which asks the model to ground and judge)
// get soul's environment without the agent token (hostshell.ChildEnv), so they could not reach this route. They read
// this key from a 0600 file of the worker's user instead - not from the environment, so `env` never prints it into a
// transcript. It opens only model calls, which any process on the desktop can already make at the control plane's
// proxy; it is not a boundary and opens no job, reply or approval.
func newCallKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeCallKey writes key to path whole (0600, a temporary file renamed over it).
func writeCallKey(path, key string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".soul-call-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(key + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}
