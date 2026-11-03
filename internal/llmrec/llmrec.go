// Package llmrec records every hosted model call the control plane forwards (plan R2): who asked (the desktop, from
// the connection, never a header), what for (the purpose, task and turn the caller tagged it with), which deployment
// served it, what it used and how long it took, and the request and answer themselves, withheld and stored message
// by message in a content-addressed blob store.
//
// Storing message by message matters: an agent's turn re-sends its whole conversation, so whole bodies would grow
// with the square of the turns, while each message here is stored once. It also gives every call its prefix hash
// (the hash of every message but the last), which offline replay at the same prefix needs.
//
// Recording is decoration on the call path: a failure to record is logged and counted, and the call is still
// answered. The tags travel in HTTP headers only, so the prompt's bytes (and the provider's cache, and Ollama's
// prefix) do not change.
//
// Depends on blobstore, llm, sqlitedb and withhold.
package llmrec

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/YKoustubhRao/decurix/internal/llm"
)

// The headers a caller tags a call with (PLAN-P2 contract C1). The desktop is never one of them: it is the
// connection's own (httpapi's requestDesktop).
const (
	HeaderPurpose = "X-Decurix-Purpose"
	HeaderTask    = "X-Decurix-Task"
	HeaderTurn    = "X-Decurix-Turn"
	HeaderCaller  = "X-Decurix-Caller"
)

// PurposeUntagged is the purpose stored for a call that named none; PurposeInvalid (also used for any other tag)
// is stored for a value that did not match its pattern. Both are counted on the stats route, never rejected unless
// DECURIX_LLM_REQUIRE_PURPOSE refuses an untagged call.
const (
	PurposeUntagged = "untagged"
	PurposeInvalid  = "invalid"
)

// Callers that tag their calls (C1), plus the practice broker's own.
const (
	CallerSoul     = "soul"
	CallerAgent    = "agent"
	CallerGate     = "gate"
	CallerPractice = "practice"
	CallerEval     = "eval"
)

var (
	purposePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,47}$`)
	turnPattern    = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,64}$`)
	taskPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	callerPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)
)

// ValidPurpose reports whether p is a well-formed purpose tag.
func ValidPurpose(p string) bool { return purposePattern.MatchString(p) }

// ValidTurn reports whether t is a well-formed turn tag.
func ValidTurn(t string) bool { return turnPattern.MatchString(t) }

// ValidTask reports whether t is a well-formed task id tag.
func ValidTask(t string) bool { return taskPattern.MatchString(t) }

// ValidCaller reports whether c is a well-formed caller tag.
func ValidCaller(c string) bool { return callerPattern.MatchString(c) }

// Call is one forwarded model call, as the proxy (or the practice broker) saw it. Request and Response are the bodies
// as they went upstream and came back; the Recorder withholds them before anything is stored. Problems are what the
// caller already found wrong with the tags ("untagged", "invalid turn", ...); the Recorder adds its own.
type Call struct {
	Provider   string
	Deployment string
	Desktop    string
	Task       string
	Turn       string
	Purpose    string
	Caller     string
	Request    []byte
	Response   []byte
	Status     int
	Latency    time.Duration
	Usage      llm.Usage
	At         time.Time // when the call was sent; zero is "now, minus Latency"
	Problems   []string
}

// Recorder stores calls. Record never fails the call it records: problems are logged and counted inside it.
type Recorder interface {
	Record(ctx context.Context, c Call)
}

// Row is one stored call, as the routes and the run record read it: no bodies, only the hashes of its messages and
// answer (each one a blob).
type Row struct {
	ID                string          `json:"id"`
	At                time.Time       `json:"at"`
	MS                int64           `json:"ms"`
	Provider          string          `json:"provider"`
	Deployment        string          `json:"deployment"`
	Status            int             `json:"status"`
	DesktopID         string          `json:"desktop_id"`
	TaskID            string          `json:"task_id"`
	Turn              string          `json:"turn"`
	Purpose           string          `json:"purpose"`
	Caller            string          `json:"caller"`
	Messages          []string        `json:"messages"`
	PrefixSHA         string          `json:"prefix_sha"`
	PromptSHA         string          `json:"prompt_sha"`
	Params            json.RawMessage `json:"params"`
	ResponseSHA       string          `json:"response_sha"`
	InputTokens       *int64          `json:"input_tokens"`
	CachedInputTokens *int64          `json:"cached_input_tokens"`
	OutputTokens      *int64          `json:"output_tokens"`
	Problems          []string        `json:"problems"`
}

// Stats counts the stored calls: by purpose, untagged and invalid ones, and recordings that failed since start.
type Stats struct {
	Calls          int64            `json:"calls"`
	ByPurpose      map[string]int64 `json:"by_purpose"`
	Untagged       int64            `json:"untagged"`
	Invalid        int64            `json:"invalid"`
	RecordFailures int64            `json:"record_failures"`
}

// Nop is a Recorder that keeps nothing.
type Nop struct{}

// Record does nothing.
func (Nop) Record(context.Context, Call) {}
