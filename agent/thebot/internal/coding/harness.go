package coding

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"thebot/internal/llm"
	"thebot/internal/memory"
	"thebot/internal/websearch"
	"thebot/internal/workspace"
)

// Default budgets. Deliberately generous - the point of this harness is
// that it keeps working until the code really works, and a build that gives
// up at three tries is just a slower way of printing broken code.
//
// They are still HARD limits, because "until it works" has to terminate:
// a model that cannot fix a given bug will not fix it on attempt 500
// either, and an unbounded loop against a paid/slow model is the kind of
// thing that runs all night.
const (
	DefaultMaxFileAttempts  = 20
	DefaultMaxTotalAttempts = 50
	DefaultDeadline         = 45 * time.Minute
)

// Harness runs a complete build: architecture, scaffolding, per-file
// implementation with real tests, integration testing, and real failure
// diagnosis in between.
type Harness struct {
	Provider  llm.Provider
	Model     string
	Workspace *workspace.Workspace

	// L1 is long-term memory - read when diagnosing a failure, written
	// when a genuinely new fix is found. Optional and nil-safe.
	L1 *memory.L1
	// SkillLib records a build's own hard-won recipe for reuse. Optional.
	SkillLib *memory.SkillLibrary

	OnLog func(string)

	// AllowWebSearch gates the research layer of diagnosis. Off in unit
	// tests (no network), on in real runs.
	AllowWebSearch bool

	MaxFileAttempts  int
	MaxTotalAttempts int
	Timeout          time.Duration

	// ---- run state ----
	attempts int
	deadline time.Time
	// fixesApplied records every real environment fix that worked, so the
	// final report can say what it actually had to do to get the build
	// running (install uv, create a venv, add a package).
	fixesApplied []string
	// learnedFixes are the genuinely new things this run figured out,
	// surfaced to the user transparently as asked.
	learnedFixes []string
}

// BuildReport is the honest outcome of a build.
type BuildReport struct {
	Design       *Design
	Dir          string
	FilesWritten []BuiltFile
	// TestsPassed is the real result of the real full test suite - false
	// means the suite genuinely did not pass, never "we didn't check".
	TestsPassed  bool
	TestOutput   string
	BuildPassed  bool
	SmokeRan     bool
	SmokeOutput  string
	Attempts     int
	FixesApplied []string
	LearnedFixes []string
	// Incomplete marks a build that ran out of budget rather than finishing.
	Incomplete bool
	Failures   []string
}

// BuiltFile is one real file that really exists on disk.
type BuiltFile struct {
	Path        string
	Lines       int
	Bytes       int64
	TestPath    string
	TestsPassed bool
	Attempts    int
}

func (h *Harness) logf(format string, args ...any) {
	if h.OnLog != nil {
		h.OnLog(fmt.Sprintf(format, args...))
	}
}

// budgetLeft reports whether there is any budget left to spend, and is the
// single place both limits are enforced so no loop can accidentally skip
// one of them.
func (h *Harness) budgetLeft() bool {
	if h.attempts >= h.MaxTotalAttempts {
		return false
	}
	if !h.deadline.IsZero() && time.Now().After(h.deadline) {
		return false
	}
	return true
}

func (h *Harness) spend() int {
	h.attempts++
	return h.attempts
}

// Run executes the whole build lifecycle for one coding request.
//
// knownContext is real prior knowledge (recalled memory, earlier research
// in the same job) that the architecture and the code should be grounded
// in rather than the model's training data alone.
func (h *Harness) Run(ctx context.Context, request, knownContext string) (*BuildReport, error) {
	h.applyDefaults()

	runCtx, cancel := context.WithDeadline(ctx, h.deadline)
	defer cancel()

	// ----------------------------------------------------------- research
	// Before designing anything, check whether this request depends on
	// something the model genuinely cannot know reliably - a current
	// version, a library's present-day API, anything phrased as "latest".
	// Training data is stale by construction, and code written against a
	// remembered API is code that fails at import time.
	knownContext = h.researchIfNeeded(runCtx, request, knownContext)

	// ---------------------------------------------------------- architect
	h.logf("coding: this is a coding task - designing the architecture before writing any code...")
	design, err := Architect(runCtx, h.Provider, h.Model, request, knownContext, h.OnLog)
	if err != nil {
		return nil, fmt.Errorf("could not design the project: %w", err)
	}
	design.Announce(h.OnLog)

	tc, exact := For(design.Language)
	if !exact {
		h.logf("coding: no dedicated toolchain for %q - building it as a Python project instead", design.Language)
		design.Language = tc.Language
	}

	// ----------------------------------------------------------- scaffold
	projectDir, err := h.Workspace.ResolveSafe(design.Dir)
	if err != nil {
		return nil, fmt.Errorf("refused to build outside the workspace: %w", err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return nil, fmt.Errorf("could not create the project directory: %w", err)
	}

	runner := &Runner{Dir: projectDir, OnLog: h.OnLog}
	env := h.probeEnvironment(runCtx, runner)
	report := &BuildReport{Design: design, Dir: projectDir}

	h.logf("coding: building in %s", projectDir)
	h.scaffold(runCtx, runner, design, tc, env, report)

	// ------------------------------------------------------ file by file
	built := map[string]string{} // real path -> real content, for cross-file context
	for i, spec := range design.Files {
		if !h.budgetLeft() {
			report.Incomplete = true
			report.Failures = append(report.Failures, fmt.Sprintf("ran out of budget before building %s", spec.Path))
			h.logf("coding: budget exhausted (%d attempts) - stopping before %s", h.attempts, spec.Path)
			break
		}

		h.logf("coding-progress: file %d/%d - %s (%s)", i+1, len(design.Files), spec.Path, spec.Purpose)
		bf := h.buildFile(runCtx, runner, design, tc, env, spec, built, report)
		report.FilesWritten = append(report.FilesWritten, bf)

		if content, err := os.ReadFile(filepath.Join(projectDir, spec.Path)); err == nil {
			built[spec.Path] = string(content)
		}

		status := "tests passing"
		if bf.TestPath == "" {
			status = "no separate unit test for this file"
		} else if !bf.TestsPassed {
			status = "TESTS STILL FAILING"
		}
		h.logf("coding: ✓ completed %s (%d lines, %s)", spec.Path, bf.Lines, status)
	}

	// --------------------------------------------------------- integration
	h.runIntegrationPhase(runCtx, runner, design, tc, env, built, report)

	report.Attempts = h.attempts
	report.FixesApplied = h.fixesApplied
	report.LearnedFixes = h.learnedFixes

	h.announceResult(report)
	h.rememberBuild(runCtx, request, design, report)

	return report, nil
}

// needsResearchRe spots a request whose correctness depends on facts that
// are outside a model's training data by definition. Deliberately narrow:
// a search costs real time, and most build requests ("a CLI that reverses
// a string") need no research whatsoever.
var needsResearchRe = regexp.MustCompile(`(?i)\b(latest|newest|current|up[- ]to[- ]date|most recent|` +
	`recently released|new(?:est)? version|20\d\d version|state of the art|cutting[- ]edge)\b` +
	`|\b(search|look up|research|find out|check)\b[\s\S]{0,40}\b(web|internet|online|docs|documentation)\b`)

// researchIfNeeded does one real web search when the request genuinely
// depends on current information, and folds what it finds into the context
// the architecture and every generated file are grounded in.
//
// This is the "research the internet if needed" half of the loop, applied
// BEFORE writing code rather than only after it breaks - much cheaper than
// discovering at test time that the API being coded against was renamed
// two releases ago.
func (h *Harness) researchIfNeeded(ctx context.Context, request, knownContext string) string {
	if !h.AllowWebSearch || !needsResearchRe.MatchString(request) {
		return knownContext
	}

	query := strings.TrimSpace(request)
	if len(query) > 160 {
		query = query[:160]
	}
	h.logf("coding: this request depends on current information - researching it first: %s", truncate(query, 100))

	results, err := websearch.Search(ctx, query, h.OnLog)
	if err != nil || len(results) == 0 {
		h.logf("coding: research turned up nothing usable - building from what's already known")
		return knownContext
	}

	var b strings.Builder
	if knownContext != "" {
		b.WriteString(knownContext)
		b.WriteString("\n\n")
	}
	b.WriteString("Real web research done just now for this build:\n")
	for i, r := range results {
		if i >= 4 {
			break
		}
		fmt.Fprintf(&b, "- %s (%s)\n  %s\n", r.Title, r.URL, r.Snippet)
	}
	if text, ferr := websearch.FetchPageText(ctx, results[0].URL, 5000); ferr == nil && strings.TrimSpace(text) != "" {
		fmt.Fprintf(&b, "\nFull text of the most relevant source (%s):\n%s\n", results[0].URL, text)
	}
	h.logf("coding: research done - %d source(s) will ground the architecture", len(results))
	return b.String()
}

func (h *Harness) applyDefaults() {
	if h.MaxFileAttempts <= 0 {
		h.MaxFileAttempts = DefaultMaxFileAttempts
	}
	if h.MaxTotalAttempts <= 0 {
		h.MaxTotalAttempts = DefaultMaxTotalAttempts
	}
	if h.Timeout <= 0 {
		h.Timeout = DefaultDeadline
	}
	h.deadline = time.Now().Add(h.Timeout)
}

// probeEnvironment asks the real machine what it actually has, once, up
// front. Everything downstream branches on these facts instead of assuming
// a toolchain is present - the difference between "uv pip install" working
// and a confusing "uv: command not found" three steps into a build.
func (h *Harness) probeEnvironment(ctx context.Context, r *Runner) *EnvFacts {
	env := &EnvFacts{
		HasUV:     r.Have(ctx, "uv"),
		HasGo:     r.Have(ctx, "go"),
		HasNode:   r.Have(ctx, "node"),
		HasNPM:    r.Have(ctx, "npm"),
		HasCMake:  r.Have(ctx, "cmake"),
		HasGXX:    r.Have(ctx, "g++"),
		HasCargo:  r.Have(ctx, "cargo"),
		HasDocker: r.Have(ctx, "docker"),
	}
	switch {
	case r.Have(ctx, "python3"):
		env.HasPython, env.PythonBin = true, "python3"
	case r.Have(ctx, "python"):
		env.HasPython, env.PythonBin = true, "python"
	default:
		env.PythonBin = "python3"
	}

	var have []string
	for name, ok := range map[string]bool{
		"uv": env.HasUV, "python": env.HasPython, "go": env.HasGo,
		"node": env.HasNode, "cmake": env.HasCMake, "g++": env.HasGXX,
		"cargo": env.HasCargo, "docker": env.HasDocker,
	} {
		if ok {
			have = append(have, name)
		}
	}
	h.logf("coding: toolchains actually available here: %s", strings.Join(have, ", "))
	return env
}

// announceResult is the honest final word: what really got built, what
// really passed, and - said plainly - what did not.
func (h *Harness) announceResult(report *BuildReport) {
	h.logf("coding: ===== BUILD RESULT =====")
	h.logf("coding: %d file(s) written in %s", len(report.FilesWritten), report.Dir)
	for _, f := range report.FilesWritten {
		mark := "✓"
		if f.TestPath != "" && !f.TestsPassed {
			mark = "✗"
		}
		h.logf("coding:   %s %s (%d lines, %d attempt(s))", mark, f.Path, f.Lines, f.Attempts)
	}
	if report.TestsPassed {
		h.logf("coding: ✓ the full test suite really passes")
	} else {
		h.logf("coding: ✗ the full test suite does NOT pass")
	}
	for _, fix := range report.FixesApplied {
		h.logf("coding: environment fix applied during the build: %s", fix)
	}
	for _, learned := range report.LearnedFixes {
		h.logf("coding-learned: I have acquired a new skill: %s", learned)
	}
	if report.Incomplete {
		h.logf("coding: ran out of its build budget (%d attempts) before finishing everything", report.Attempts)
	}
}

// rememberBuild proposes the real recipe this build followed as an
// acquired skill - but only for a build that genuinely passed its tests.
// Proposed, never auto-trusted: it lands in .skills/pending/ for a human
// to confirm, exactly like every other acquired skill in this codebase.
func (h *Harness) rememberBuild(ctx context.Context, request string, d *Design, report *BuildReport) {
	if h.SkillLib == nil || !report.TestsPassed || report.Incomplete {
		return
	}
	// Only worth remembering when the build had to genuinely work
	// something out. A build that went perfectly first time teaches
	// nothing that re-deriving it wouldn't produce just as cheaply.
	if len(report.FixesApplied) == 0 && len(report.LearnedFixes) == 0 {
		return
	}

	steps := []string{
		fmt.Sprintf("build a %s %s in %s/ with: %s", d.Language, d.Kind, d.Dir, strings.Join(fileNames(d.Files), ", ")),
	}
	steps = append(steps, report.FixesApplied...)
	steps = append(steps, fmt.Sprintf("run the full test suite and fix until it passes"))

	proposed, err := h.SkillLib.Propose(request, steps, "coding-harness")
	if err != nil {
		h.logf("coding: could not propose a skill from this build - %s", err.Error())
		return
	}
	h.logf("coding: proposed a new skill %q from this build - awaiting your confirmation", proposed.Slug)
}

func fileNames(files []FileSpec) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// Summary renders the report as the answer the user actually sees.
func (r *BuildReport) Summary() string {
	var b strings.Builder

	status := "✓ Built and tested"
	if !r.TestsPassed {
		status = "⚠ Built, but the tests do not pass"
	}
	if r.Incomplete {
		status = "⚠ Incomplete (ran out of build budget)"
	}

	fmt.Fprintf(&b, "%s: %s (%s %s)\n\n", status, r.Design.ProjectName, r.Design.Language, r.Design.Kind)
	fmt.Fprintf(&b, "%s\n\n", r.Design.Summary)
	fmt.Fprintf(&b, "Directory: %s\n\n", r.Dir)

	b.WriteString("Files:\n")
	for _, f := range r.FilesWritten {
		mark := "✓"
		if f.TestPath != "" && !f.TestsPassed {
			mark = "✗"
		}
		fmt.Fprintf(&b, "  %s %s (%d lines)", mark, f.Path, f.Lines)
		if f.TestPath != "" {
			fmt.Fprintf(&b, " + %s", f.TestPath)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nTest suite: ")
	if r.TestsPassed {
		b.WriteString("passing\n")
	} else {
		b.WriteString("FAILING\n")
	}
	if out := strings.TrimSpace(r.TestOutput); out != "" {
		fmt.Fprintf(&b, "\n%s\n", lastLines(out, 25))
	}

	if r.SmokeRan {
		fmt.Fprintf(&b, "\nSmoke check (%s): %s\n", r.Design.RunCommand, lastLines(strings.TrimSpace(r.SmokeOutput), 6))
	}

	if len(r.FixesApplied) > 0 {
		b.WriteString("\nEnvironment fixes applied during the build:\n")
		for _, f := range r.FixesApplied {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if len(r.LearnedFixes) > 0 {
		b.WriteString("\nNew skills acquired (researched during this build):\n")
		for _, f := range r.LearnedFixes {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if len(r.Failures) > 0 {
		b.WriteString("\nWhat did not work:\n")
		for _, f := range r.Failures {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	if r.Design.RunCommand != "" {
		fmt.Fprintf(&b, "\nRun it with:\n  cd %s && %s\n", r.Dir, r.Design.RunCommand)
	}

	fmt.Fprintf(&b, "\n(%d build attempt(s) used)\n", r.Attempts)
	return b.String()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "[...]\n" + strings.Join(lines[len(lines)-n:], "\n")
}
