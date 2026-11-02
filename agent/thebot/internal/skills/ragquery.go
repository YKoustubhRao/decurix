package skills

import (
	"fmt"
	"strings"

	"thebot/internal/llm"
)

// RAGQuerySkill answers a question grounded in the real, already-ingested
// Data RAG content - never invents a file or excerpt, and always names
// the real source path(s) an answer comes from.
type RAGQuerySkill struct{}

func NewRAGQuery() *RAGQuerySkill { return &RAGQuerySkill{} }

func (q *RAGQuerySkill) Name() string { return "rag-query" }
func (q *RAGQuerySkill) Description() string {
	return "Answers using the real Data RAG index of already-created/downloaded file content - grounded retrieval, never a guess."
}

func (q *RAGQuerySkill) Matches(subtask Subtask) bool {
	return subtask.Type == "rag-query"
}

func (q *RAGQuerySkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	if rc.RAG == nil {
		return Result{
			Subtask: subtask, SkillName: q.Name(),
			Output: "No Data RAG is configured for this session.", Verified: false, Meta: map[string]any{},
		}, nil
	}

	rc.Logf("rag-query", "querying the Data RAG for: %s", subtask.Step)
	results, err := rc.RAG.Query(rc.Ctx, subtask.Step, 5)
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: q.Name(),
			Output: fmt.Sprintf("RAG query failed: %s", err.Error()), Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}
	if len(results) == 0 {
		return Result{
			Subtask: subtask, SkillName: q.Name(),
			Output:   "Nothing has been indexed into the Data RAG yet, or nothing indexed is relevant to this query.",
			Verified: false, Meta: map[string]any{"matches": 0},
		}, nil
	}

	// Chunks are already bounded at ingestion time (internal/rag caps each
	// one at 1500 runes) - there is no need to truncate a second time
	// here, and doing so silently discards real data. Found live: asked
	// which region/product grew fastest in a real 10-K, the model
	// confidently answered "Americas" and "Wearables" - both wrong. The
	// real answers (Japan at 15%, Services at 14%) were sitting in the
	// SAME retrieved chunks, just past this truncate(..., 800) cutoff,
	// which sliced two financial tables off mid-row with no signal to the
	// model that anything was missing. It reasoned correctly from what it
	// was shown; what it was shown had been mutilated first. 5 chunks at
	// up to 1500 runes each is a few thousand characters total - trivial
	// for any real LLM context window, so there's no good reason to cut
	// further.
	var excerpts strings.Builder
	sources := make([]string, 0, len(results))
	for _, r := range results {
		fmt.Fprintf(&excerpts, "--- %s (score %.3f) ---\n%s\n\n", r.RelPath, r.Score, r.Text)
		sources = append(sources, r.RelPath)
	}

	prompt := fmt.Sprintf("Answer using ONLY the real indexed excerpts below - always name the actual file path(s) the answer comes from. Do not invent content that isn't shown.\n\nRequest: %s\n\nReal indexed excerpts:\n%s", subtask.Step, excerpts.String())
	resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{rc.SystemMessage(), {Role: "user", Content: prompt}}, llm.Options{TimeoutMs: 120000})
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: q.Name(),
			Output:   fmt.Sprintf("Retrieved %d relevant chunk(s) but the model call to compose an answer failed: %s", len(results), err.Error()),
			Verified: false, Meta: map[string]any{"matches": len(results), "sources": sources, "error": err.Error()},
		}, nil
	}

	return Result{
		Subtask: subtask, SkillName: q.Name(),
		Output:   resp.Text,
		Verified: true,
		Meta:     map[string]any{"matches": len(results), "sources": sources, "userFacing": true, MetaModelReply: true},
	}, nil
}
