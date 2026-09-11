package skills

import (
	"fmt"
	"strings"

	"thebot/internal/coding"
)

// CodingSkill is the real software-engineering path: when the request is
// to WRITE CODE, this hands the whole thing to internal/coding's build
// harness instead of doing what file-write does (ask a model for one file's
// contents and save it, unexecuted and untested).
//
// The difference is the whole point. file-write produces a file that LOOKS
// like code. This produces a project that has been designed, scaffolded
// with a real toolchain, implemented file by file, unit-tested, integration-
// tested, and fixed until the tests genuinely pass - with every one of
// those claims backed by a real process exit code.
type CodingSkill struct{}

func NewCoding() *CodingSkill { return &CodingSkill{} }

func (c *CodingSkill) Name() string { return "coding" }
func (c *CodingSkill) Description() string {
	return "Designs, builds, tests and fixes a real software project end to end - real files, real toolchain, real tests that really run."
}

func (c *CodingSkill) Matches(subtask Subtask) bool { return subtask.Type == "coding" }

func (c *CodingSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	if rc.Workspace == nil {
		return Result{
			Subtask: subtask, SkillName: c.Name(),
			Output:   "No workspace configured, so there is nowhere to build a project.",
			Verified: false, Meta: map[string]any{},
		}, nil
	}

	h := &coding.Harness{
		Provider:  rc.Provider,
		Model:     rc.Model,
		Workspace: rc.Workspace,
		L1:        rc.L1,
		SkillLib:  rc.SkillLib,
		OnLog:     rc.OnLog,
		// Real runs research genuinely unknown failures; unit tests run
		// with this off so nothing in the suite touches the network.
		AllowWebSearch: !rc.OfflineMode,
	}

	// Everything already established in this job - recalled memory and
	// verified findings from earlier steps - so the architecture and the
	// code are grounded in real current facts rather than training data.
	// This is what makes "search the web for the current API, then build
	// against it" actually produce code that uses the current API.
	report, err := h.Run(rc.Ctx, subtask.Step, c.knownContext(rc))
	if err != nil {
		return Result{
			Subtask: subtask, SkillName: c.Name(),
			Output:   "The build could not be started: " + err.Error(),
			Verified: false, Meta: map[string]any{"error": err.Error()},
		}, nil
	}

	meta := map[string]any{
		"dir":         report.Dir,
		"language":    report.Design.Language,
		"kind":        report.Design.Kind,
		"files":       len(report.FilesWritten),
		"testsPassed": report.TestsPassed,
		"attempts":    report.Attempts,
	}
	if len(report.LearnedFixes) > 0 {
		meta["learnedFixes"] = report.LearnedFixes
	}
	if len(report.Failures) > 0 {
		meta["failures"] = report.Failures
	}

	// Verified means exactly one thing here: the real test suite really
	// passed. A project whose files all got written but whose tests fail is
	// NOT a success - reporting it as one is how "it's done!" ends up
	// meaning "there is a folder with some code in it", which is the exact
	// failure this whole harness exists to eliminate.
	return Result{
		Subtask: subtask, SkillName: c.Name(),
		Output:   report.Summary(),
		Verified: report.TestsPassed && !report.Incomplete,
		Meta:     meta,
	}, nil
}

// knownContext assembles what is already genuinely known: facts recalled
// from long-term memory, plus what earlier verified steps in this same
// plan actually found (a web search for a current API, a RAG lookup).
func (c *CodingSkill) knownContext(rc RunContext) string {
	var b strings.Builder

	if len(rc.RecalledFacts) > 0 {
		b.WriteString("Things you already know (from long-term memory):\n")
		for _, f := range rc.RecalledFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}

	if findings := rc.PriorFindings(); findings != "" {
		b.WriteString("Findings from earlier steps of this same task (verified just now):\n")
		b.WriteString(findings)
		b.WriteString("\n")
	}

	return strings.TrimSpace(b.String())
}
