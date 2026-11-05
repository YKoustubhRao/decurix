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
	// A fix a web page or a model proposed runs only with the person's yes to
	// that exact command, once (the scheduler's single-use grant). On resume
	// the build re-runs with the proposals already asked about, so the same
	// failure gets the command they saw; a proposal not approved is declined.
	cp, _ := rc.Resume.(*codingCheckpoint)
	if cp != nil {
		h.Proposed = cp.Proposed
		h.Declined = append([]string(nil), cp.Declined...)
		if cp.Asked != "" {
			if rc.IsApproved(cp.Asked) {
				granted := cp.Asked
				h.Approve = func(action string) bool {
					if granted == "" || strings.TrimSpace(action) != granted {
						return false
					}
					granted = ""
					return true
				}
			} else {
				h.Declined = append(h.Declined, cp.Asked)
			}
		}
	}

	// Everything already established in this job - recalled memory and
	// verified findings from earlier steps - so the architecture and the
	// code are grounded in real current facts rather than training data.
	// This is what makes "search the web for the current API, then build
	// against it" actually produce code that uses the current API.
	report, err := h.Run(rc.Ctx, subtask.Step, c.knownContext(rc))
	if err != nil {
		// A build that cannot even be designed has not failed at anything
		// the recovery machinery can help with - there is no code yet to
		// fix and no environment to repair. Marking it fatal stops the
		// scheduler from spending a full reason-and-decompose cycle (two
		// more model calls, against the same model that just failed) on a
		// problem whose real answer is "this model is not usable".
		//
		// Found live routing builds to gemma4:26b: every call died with
		// "llama-server process has terminated ... CUDA error: shared
		// object initialization failed" because that Ollama build cannot
		// run the Gemma4 architecture at all. The harness correctly
		// refused to claim success, but then re-ran the whole thing
		// through recovery and failed identically, minutes later.
		return Result{
			Subtask: subtask, SkillName: c.Name(),
			Output:   "The build could not be started: " + err.Error(),
			Verified: false,
			Meta: map[string]any{
				"error":            err.Error(),
				"fatal":            true,
				"needsUserInput":   true,
				"failureReasoning": "The coding model could not be used at all (" + err.Error() + "). Nothing about the request is wrong - check that the configured CODING_MODEL is installed and can actually load on the server.",
			},
		}, nil
	}

	if p := report.NeedsApproval; p != nil {
		next := &codingCheckpoint{Asked: p.Action()}
		if cp != nil {
			next.Proposed = append(next.Proposed, cp.Proposed...)
			next.Declined = append(next.Declined, h.Declined...)
		}
		next.Proposed = append(next.Proposed, *p)
		reason := fmt.Sprintf("this fix for a build failure was proposed by %s, not checked by anyone (%s)", proposalSource(p.Source), p.Cause)
		res := NeedsApproval(subtask, c.Name(), p.Action(), reason, next)
		res.Output += "\n\n" + report.Summary()
		return res, nil
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
	// passed, the project really builds, and what is meant to run really
	// starts (BuildReport.Succeeded). A project whose files all got written but whose tests fail is
	// NOT a success - reporting it as one is how "it's done!" ends up
	// meaning "there is a folder with some code in it", which is the exact
	// failure this whole harness exists to eliminate.
	return Result{
		Subtask: subtask, SkillName: c.Name(),
		Output:   report.Summary(),
		Verified: report.Succeeded(),
		Meta:     meta,
	}, nil
}

// codingCheckpoint is a build paused at a proposed fix: the proposals asked
// about so far (re-used for the same failure on the re-run), the command
// this pause asks about, and those the person did not approve. It never
// carries an approval: the yes arrives as the scheduler's single-use grant.
type codingCheckpoint struct {
	Proposed []coding.Diagnosis
	Asked    string
	Declined []string
}

// proposalSource names who put a proposed fix forward.
func proposalSource(source string) string {
	switch source {
	case "web":
		return "a web page (read by the model)"
	case "model":
		return "the model"
	case "memory:approved":
		return "a command you approved for this failure before (asked again: an earlier yes is not this one)"
	default:
		return source
	}
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
