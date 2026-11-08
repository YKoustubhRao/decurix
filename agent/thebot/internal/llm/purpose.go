package llm

import (
	"log"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Purposes say what a model call is for. Every call soul makes names one (Options.Purpose); the OpenAI provider sends
// it as X-Decurix-Purpose, so the control plane's proxy can count and record calls by job (plan R3, PLAN-P2 C1). A
// purpose is a tag on the side of the call: it never enters the request body, so the prompt prefix does not change.
//
// purpose_test.go checks that every llm.Options literal in soul sets Purpose to one of these constants.
const (
	// PurposeOperatorTurn is one turn of the operator's loop: the model reads the transcript and says the next command.
	PurposeOperatorTurn = "operator.turn"
	// PurposeOperatorOutOfTurns is the operator's last word when it ran out of turns with the screen changed.
	PurposeOperatorOutOfTurns = "operator.out_of_turns"
	// PurposeOperatorPlan is the plan of the work, written once before the first command that changes something.
	PurposeOperatorPlan = "operator.plan"
	// PurposeReview is the reviewer of a report or a commit.
	PurposeReview = "review"
	// PurposeVerifyCommit is the verifier working out a screen's figures from the sources before a commit.
	PurposeVerifyCommit = "verify.commit"
	// PurposeCase is the case matrix: the screen about to be committed, read against the request's cases.
	PurposeCase = "case"
	// PurposeCompact is the notes written when the operator's transcript is compacted.
	PurposeCompact = "compact"
	// PurposeReplyRead is the one reading of a person's reply to what they were asked.
	PurposeReplyRead = "reply.read"
	// PurposeSecondRead is a second, independent transcription of a document's pages.
	PurposeSecondRead = "second_read"
	// PurposeFileWrite is the path and content of a file the file-write skill writes.
	PurposeFileWrite = "filewrite"
	// PurposeGeneral is the conversational fallback skill's answer.
	PurposeGeneral = "general"
	// PurposeRAG is an answer from the indexed data's excerpts.
	PurposeRAG = "rag"
	// PurposeWebSearch is an answer from web search results and the pages read from them.
	PurposeWebSearch = "web.search"
	// PurposeWebRead is an answer from one fetched web page.
	PurposeWebRead = "web.read"
	// PurposePlan is the atomizer's plan of a request.
	PurposePlan = "plan"
	// PurposePlanDecompose is the atomizer's plan of a recovery the failure reasoner described.
	PurposePlanDecompose = "plan.decompose"
	// PurposePlanReplan is the rest of a plan after a step failed for good.
	PurposePlanReplan = "plan.replan"
	// PurposeCompose is the scheduler's reply written from what the steps found.
	PurposeCompose = "compose"
	// PurposeRecover is the scheduler's failure reasoner: retry, decompose or ask.
	PurposeRecover = "recover"
	// PurposeMemoryL1 is the judgement of whether an exchange holds a fact worth remembering.
	PurposeMemoryL1 = "memory.l1"
	// PurposeCodingArchitect is the coding harness's design of a project.
	PurposeCodingArchitect = "coding.architect"
	// PurposeCodingGen is the coding harness writing or fixing a file.
	PurposeCodingGen = "coding.gen"
	// PurposeCodingDiagnose is the coding harness working out why a build or test failed.
	PurposeCodingDiagnose = "coding.diagnose"
	// PurposeEmbed is an embeddings call.
	PurposeEmbed = "embed"
	// PurposeUntagged is what a call that named no purpose goes out as.
	PurposeUntagged = "untagged"
)

// purposes is every valid purpose.
var purposes = map[string]bool{
	PurposeOperatorTurn: true, PurposeOperatorOutOfTurns: true, PurposeOperatorPlan: true, PurposeReview: true,
	PurposeVerifyCommit: true, PurposeCase: true, PurposeCompact: true, PurposeReplyRead: true, PurposeSecondRead: true,
	PurposeFileWrite: true, PurposeGeneral: true, PurposeRAG: true, PurposeWebSearch: true, PurposeWebRead: true,
	PurposePlan: true, PurposePlanDecompose: true, PurposePlanReplan: true, PurposeCompose: true, PurposeRecover: true,
	PurposeMemoryL1: true, PurposeCodingArchitect: true, PurposeCodingGen: true, PurposeCodingDiagnose: true,
	PurposeEmbed: true, PurposeUntagged: true,
}

// agentPurposeShape is the form of the Python agent's own purposes (py.* or legacy.*, the set
// agent/tests/test_purpose_tags.py holds), within the proxy's purpose form.
var agentPurposeShape = regexp.MustCompile(`^(py|legacy)\.[a-z0-9_.]{1,44}$`)

// ValidAgentPurpose reports whether p is of the Python agent's own purpose form: never one of soul's own purposes, so
// the record never shows the agent's question as, say, soul's operator turn.
func ValidAgentPurpose(p string) bool { return agentPurposeShape.MatchString(p) }

// AgentPurpose is the purpose of a call soul makes for the Python agent (/api/llm/call): the agent's own py.* or
// legacy.* purpose, verbatim, or PurposeUntagged for anything else. The one way a purpose that is not a constant above
// reaches an Options literal (purpose_test.go allows it and nothing else).
func AgentPurpose(p string) string {
	if ValidAgentPurpose(p) {
		return p
	}
	return PurposeUntagged
}

// purposeShape is the proxy's accepted form of a purpose (PLAN-P2 C1).
var purposeShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,47}$`)

// ValidPurpose reports whether p is one of the purposes above.
func ValidPurpose(p string) bool { return purposes[p] }

// untaggedSeen holds the call sites already logged for calling with no purpose: each is logged once per process.
var untaggedSeen sync.Map

// tagPurpose is opts with its Purpose settled: an empty one becomes PurposeUntagged, and the first such call from each
// call site is logged with that site's file:line (the first frame outside this package). It never fails the call.
func tagPurpose(opts Options) Options {
	if opts.Purpose != "" {
		return opts
	}
	opts.Purpose = PurposeUntagged
	site := callerOutsideLLM()
	if _, logged := untaggedSeen.LoadOrStore(site, true); !logged {
		log.Printf("[soul] a model call names no purpose (sent as %q): %s", PurposeUntagged, site)
	}
	return opts
}

// callerOutsideLLM is file:line of the nearest caller that is not in this package (a test of it counts as outside),
// or "unknown".
func callerOutsideLLM() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		// This package's own frames are skipped, but not its tests: a test stands in for a caller.
		if !strings.HasPrefix(f.Function, "thebot/internal/llm.") || strings.HasSuffix(f.File, "_test.go") {
			return f.File + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return "unknown"
		}
	}
}
