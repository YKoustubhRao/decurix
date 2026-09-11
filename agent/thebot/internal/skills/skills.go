// Package skills is the "limbs" - every real capability the agent has
// (terminal today, web/download/file/code-project later, without losing
// any of them) implements Skill and self-registers. First match in the
// registry wins, mirroring the Node prototype's skill priority order.
package skills

import (
	"context"
	"fmt"
	"strings"

	"thebot/internal/feedback"
	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/rag"
	"thebot/internal/workspace"
)

type Subtask struct {
	Step string
	Type string
}

type Result struct {
	Subtask    Subtask
	SkillName  string
	Output     string
	Verified   bool
	Meta       map[string]any
	Error      string
	DurationMs int64
}

// RunContext is everything a skill needs to do real work - never more
// than that (no ambient globals), mirroring the Node prototype's
// per-call context object.
type RunContext struct {
	Ctx            context.Context
	OnLog          func(string)
	OriginalPrompt string
	PriorResults   []Result
	Provider       llm.Provider
	Model          string
	Workspace      *workspace.Workspace
	// RAG and Feedback are both optional (nil-safe) - only skills that
	// need them (rag-query, feedback) check for nil before using them.
	RAG      *rag.RAG
	Feedback *feedback.Store
	// SessionHistory is this UI conversation's real prior turns (see
	// internal/session) - nil/empty for a request with no session id,
	// in which case a skill just has no prior context, same as before
	// session support existed. Only general.go threads it into the LLM
	// call today; other skills don't need conversational continuity the
	// same way.
	SessionHistory []llm.Message
	// RecalledFacts is what L1 already knew that's relevant to this
	// request (see internal/memory.L1), recalled once per job by the
	// scheduler. Empty when L1 is unconfigured or has nothing relevant.
	RecalledFacts []string
	// SelfStats is real introspection - the agent's own current RAG/L1/L2/
	// feedback counts, fetched once per job by the scheduler. Exists
	// because a self-awareness question ("how much do you have indexed?")
	// used to get "I don't have the ability to check" even though that
	// exact data is what the UI's own memory drawer displays. Only
	// general.go threads it in today.
	SelfStats string
	// L1 and SkillLib are the memory tiers a skill can both READ and WRITE
	// during its own run, as opposed to RecalledFacts above, which is a
	// read-only snapshot the scheduler took once at the start of the job.
	// The coding harness needs the live handles: diagnosing a build failure
	// queries memory mid-run with the real error text (which nothing could
	// have recalled up front, since the failure hadn't happened yet), and a
	// genuinely new fix it researches gets written straight back. Optional
	// and nil-safe like every other tier here.
	L1       *memory.L1
	SkillLib *memory.SkillLibrary
	// OfflineMode turns off everything that reaches the network. Set by
	// tests so the suite never depends on a search engine being up; left
	// false in real runs, where research is the whole point.
	OfflineMode bool
}

func (rc RunContext) Logf(prefix, format string, args ...any) {
	if rc.OnLog != nil {
		rc.OnLog(fmt.Sprintf("skill:"+prefix+" -> "+format, args...))
	}
}

// priorFindingMaxChars caps how much of one earlier result is carried
// forward. Enough to hold a real research answer, small enough that a
// five-step plan can't blow the context window.
const priorFindingMaxChars = 3000

// PriorFindings formats what earlier steps in this same plan actually
// found, for a skill that needs to build on them.
//
// This is what makes a plan a plan. Without it, "search the web for the
// newest Go version" then "write a note about it to go-version.md" ran as
// two unrelated steps: step 1 correctly found 1.27.1, and step 2, seeing
// only its own step text, wrote "Go 1.21" from stale training data with a
// literal "[insert release date]" placeholder still in it. Only verified
// results are carried - a failed step's output is not a finding.
func (rc RunContext) PriorFindings() string {
	var b strings.Builder
	for i, r := range rc.PriorResults {
		if !r.Verified || strings.TrimSpace(r.Output) == "" {
			continue
		}
		out := r.Output
		if len(out) > priorFindingMaxChars {
			out = out[:priorFindingMaxChars] + "\n[...truncated]"
		}
		fmt.Fprintf(&b, "Step %d (%s):\n%s\n", i+1, r.Subtask.Step, out)
		// The real URLs a step actually fetched, appended independent of
		// its own prose. Live bug: the web skill fetched the real SEC
		// filing at .../aapl-20250927.htm, but its own synthesis
		// misjudged the page and wrote "no information found", citing no
		// URL at all - so a download step reading only that prose had
		// nothing to grab even though the real link was one struct field
		// away the whole time. Ground truth about what was fetched must
		// not be hostage to whether the model's summary of it was good.
		if urls := metaSourceURLs(r.Meta); len(urls) > 0 {
			fmt.Fprintf(&b, "Sources: %s\n", strings.Join(urls, ", "))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// metaSourceURLs pulls the real URLs a skill recorded it actually fetched
// out of a Result's Meta, independent of anything the skill's own output
// text says about them. Recognises the shapes the web skill produces:
// Meta["sources"] ([]string, the multi-result search case) and
// Meta["url"] (string, the single-page-read case).
func metaSourceURLs(meta map[string]any) []string {
	var out []string
	switch v := meta["sources"].(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	if u, ok := meta["url"].(string); ok && u != "" {
		out = append(out, u)
	}
	return out
}

// PriorSourceURLs collects every real URL a verified earlier step in this
// plan actually fetched, most recent step first - so a later step (like a
// download that names no URL of its own) can recover one deterministically
// rather than by regex-scraping formatted prose.
func (rc RunContext) PriorSourceURLs() []string {
	seen := map[string]bool{}
	var out []string
	for i := len(rc.PriorResults) - 1; i >= 0; i-- {
		r := rc.PriorResults[i]
		if !r.Verified {
			continue
		}
		for _, u := range metaSourceURLs(r.Meta) {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// Skill is one real capability - matches a subtask, executes it for
// real, and reports an honestly-verified result. Never claims success
// without checking real state.
type Skill interface {
	Name() string
	Description() string
	Matches(subtask Subtask) bool
	Run(rc RunContext, subtask Subtask) (Result, error)
}

var (
	registry []Skill
	fallback Skill
)

func Register(s Skill) {
	registry = append(registry, s)
}

// SetFallback registers the skill Pick() falls back to when nothing in
// the registry matches - the structural guarantee that the agent never
// just announces "I don't have this skill" and stops. Real gap found
// live: a user's plain "hello" hit exactly this dead end before
// general.go and this fallback wiring existed - the fix isn't only
// "add a general-chat skill", it's "make it structurally impossible for
// an unmatched task type to short-circuit to a canned refusal", so a
// FUTURE classifier added without its own skill can't reintroduce the
// same bug. Call once at startup with the real conversational fallback
// (skills.NewGeneral()).
func SetFallback(s Skill) {
	fallback = s
}

// Pick returns the first matching skill, or the registered fallback if
// nothing in the registry matches. Only returns nil if SetFallback was
// never called - a startup wiring bug, not a normal "can't help with
// this" runtime case.
func Pick(subtask Subtask) Skill {
	for _, s := range registry {
		if s.Matches(subtask) {
			return s
		}
	}
	return fallback
}
