package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"thebot/internal/llm"
)

// maxFileContextChars caps how much of one already-written file is quoted
// into another file's generation prompt. Enough for real signatures and
// structure; small enough that a six-file project doesn't blow the window.
const maxFileContextChars = 2500

// Code generation needs a far bigger window than a chat turn, and it is
// worth being explicit about why. The prompt alone carries the whole
// architecture, the real content of every file already written, and (on a
// retry) the previous attempt's real compiler output - several thousand
// tokens before the model writes a single line. The answer then has to be
// a COMPLETE source file.
//
// Found live on the harness's first real build: with the old fixed 8192
// window shared between prompt and response, the model was left a few
// hundred tokens to write a whole file in and returned one cut off
// mid-function. Nothing reports that - Ollama returns a clean 200 with a
// truncated body - so it looked exactly like a model that kept writing
// broken Python, and nine attempts in a row were spent "fixing" a file
// that had simply never been given room to finish.
const (
	codeGenNumCtx    = 32768
	codeGenMaxTokens = 8192
)

// codeGenOptions is the single place every generation call gets its real
// room from, so no one call can quietly fall back to the default window.
func codeGenOptions() llm.Options {
	return llm.Options{
		TimeoutMs:   240000,
		Temperature: 0.1,
		NumCtx:      codeGenNumCtx,
		MaxTokens:   codeGenMaxTokens,
	}
}

// ------------------------------------------------------------- scaffolding

// scaffold creates the real project skeleton and stands its environment
// up: the manifest files that make this a project its tooling recognises,
// then the real setup commands (venv, module init, dependency install).
//
// Setup failures go through the same diagnosis path as everything else,
// because this is exactly where the interesting environment failures live -
// no uv, no python3-venv, an externally-managed interpreter, no network.
func (h *Harness) scaffold(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, report *BuildReport) {
	h.logf("coding: setting up a real %s project (%s)...", d.Language, d.Kind)

	if tc.ScaffoldFiles != nil {
		for _, sf := range tc.ScaffoldFiles(d) {
			abs := filepath.Join(r.Dir, filepath.FromSlash(sf.Path))
			if sf.OnlyIfMissing {
				if _, err := os.Stat(abs); err == nil {
					continue
				}
			}
			if err := writeFile(abs, sf.Content); err != nil {
				h.logf("coding: could not write %s: %s", sf.Path, err.Error())
				continue
			}
			h.logf("coding: wrote %s", sf.Path)
		}
	}

	if tc.SetupCommands == nil {
		return
	}
	for _, cmd := range tc.SetupCommands(d, env) {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		res := r.Run(ctx, cmd, installTimeout)
		if res.OK {
			continue
		}

		h.logf("coding: setup step failed - diagnosing before giving up: %s", truncate(cmd, 90))
		if !h.recoverEnvironment(ctx, r, d, tc, env, res.Tail(3000), report) {
			report.Failures = append(report.Failures, fmt.Sprintf("setup step failed: %s", truncate(cmd, 120)))
			continue
		}
		// The environment changed for real - re-probe (a venv that now
		// exists changes how every later command addresses Python) and
		// retry this exact step once.
		h.refreshEnv(r, env)
		if retry := r.Run(ctx, cmd, installTimeout); !retry.OK {
			report.Failures = append(report.Failures, fmt.Sprintf("setup step still failed after a fix: %s", truncate(cmd, 120)))
		}
	}

	h.refreshEnv(r, env)
}

// refreshEnv re-checks the facts that a fix can genuinely change mid-build:
// whether the project venv now exists (so Python commands should use it),
// and whether uv got installed along the way.
func (h *Harness) refreshEnv(r *Runner, env *EnvFacts) {
	if _, err := os.Stat(filepath.Join(r.Dir, ".venv", "bin", "python")); err == nil {
		if env.VenvPython == "" {
			h.logf("coding: project virtualenv is ready at .venv - using it for every Python command from here")
		}
		env.VenvPython = ".venv/bin/python"
	}
}

// recoverEnvironment diagnoses a real failure and, if it's an environment
// problem with a real fix, runs that fix and reports whether it worked.
// Returns false for anything that isn't fixable by running a command.
func (h *Harness) recoverEnvironment(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, failure string, report *BuildReport) bool {
	if !h.budgetLeft() {
		return false
	}
	h.spend()

	diag := h.Diagnose(ctx, failure, d, env, tc)
	if diag.Kind != DiagEnvironment || len(diag.FixCommands) == 0 {
		return false
	}

	h.logf("coding-diagnosis: %s", diag.Cause)
	for _, fix := range diag.FixCommands {
		h.logf("coding: applying a real fix: %s", truncate(fix, 120))
		if res := r.Run(ctx, fix, installTimeout); !res.OK {
			h.logf("coding: that fix did not work (exit %d)", res.ExitCode)
			return false
		}
	}

	h.fixesApplied = append(h.fixesApplied, strings.Join(diag.FixCommands, " && "))
	if diag.Learned {
		h.learnedFixes = append(h.learnedFixes, diag.Cause+" → "+fixSummary(diag))
		h.Learn(ctx, failure, diag)
	}
	return true
}

// ------------------------------------------------------------ file builder

const codeGenPrompt = `You are implementing ONE file of a real software project. It will be written to disk, compiled/imported for real, and tested for real - so it must be complete and correct, not illustrative.

PROJECT: %s - %s
LANGUAGE: %s | KIND: %s

THE FULL ARCHITECTURE (every file in this project):
%s

THE FILE YOU ARE WRITING NOW: %s
ITS PURPOSE: %s
IT MUST DEFINE EXACTLY THESE:
%s
%s%s
RULES:
- Write PRODUCTION-QUALITY code: real error handling at the boundaries, clear names, a short docstring/comment for anything non-obvious.
- NEVER leave a TODO, a placeholder, an ellipsis, a "implement this later", or a stub that raises NotImplementedError. Every function you write must really work.
- The file must be COMPLETE and valid on its own - all imports present, nothing referenced that doesn't exist.
- Match the declared signatures EXACTLY. Other files are generated against them and will really call them.
- Use ONLY the standard library and these declared dependencies: %s
- Do not write tests in this file. Tests are written separately.

Respond with ONLY the raw file content. No markdown fence, no commentary before or after.`

const testGenPrompt = `Write a real test file for one real source file. These tests will really be executed and their result really decides whether this code is accepted.

PROJECT: %s (%s)
SOURCE FILE UNDER TEST: %s
PROJECT LAYOUT (import paths must match this exactly, rooted at the project directory):
%s

THE REAL CODE UNDER TEST (this is exactly what is on disk right now):
%s

TEST FRAMEWORK: %s

REQUIREMENTS:
- Test EVERY public function/class in the file above - at least one real test each.
- Test the real edge cases too: empty input, boundary values, invalid input, error paths. Cover every branch you can see in the code.
- Assertions must be REAL: each one must genuinely fail if the code is wrong. Never write assert True, never assert something trivially true, never write a test with no assertion.
- Do NOT test the standard library or the framework. Test THIS code.
- The imports must match the real project layout above - this file lives at %s and the project root is on the import path.
- No network calls, no sleeping, no reliance on files that don't exist - a test must pass deterministically on a clean machine.

Respond with ONLY the raw file content. No markdown fence, no commentary.`

// writeSourceFile implements ONE real source file and proves it at least
// compiles, fixing it until it does or its attempt budget is spent.
//
// It deliberately stops there, without testing. Tests come in a second
// pass over the whole project (see Harness.Run), because of a real failure
// found live: the first file of an LRU-cache build imported two sibling
// modules that the design had not written YET - they were files 2 and 3 -
// so its unit test died on ModuleNotFoundError for code the build was
// about to create, and the fix loop then tried to install the project's
// own module from PyPI. Nothing is wrong with that file; it simply cannot
// be tested until the project exists. Writing every file first and testing
// afterwards removes the whole class of problem.
func (h *Harness) writeSourceFile(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, spec FileSpec, built map[string]string, report *BuildReport) BuiltFile {
	bf := BuiltFile{Path: spec.Path}

	// ---- write the source, fixing until it at least compiles ----
	var lastFailure string
	var diag Diagnosis
	for attempt := 1; attempt <= h.MaxFileAttempts && h.budgetLeft(); attempt++ {
		h.spend()
		bf.Attempts = attempt

		content, err := h.generateCode(ctx, d, tc, spec, built, lastFailure, diag, attempt)
		if errors.Is(err, errTruncated) {
			// Real but incomplete. Writing it would produce a guaranteed
			// syntax error and send the next attempt hunting for a bug that
			// does not exist, so tell the next attempt the truth instead.
			lastFailure = "Your previous response was CUT OFF before the file was finished - it was not wrong, it was too long to fit. Write the same file more concisely so it fits: keep every required symbol and real behaviour, but use shorter docstrings and no redundant comments."
			diag = Diagnosis{Kind: DiagCode, Cause: "the previous response was truncated", CodeHint: "Emit the complete file, more concisely.", Source: "provider"}
			continue
		}
		if err != nil {
			h.logf("coding: could not generate %s: %s", spec.Path, err.Error())
			lastFailure = err.Error()
			continue
		}

		abs := filepath.Join(r.Dir, filepath.FromSlash(spec.Path))
		if err := writeFile(abs, content); err != nil {
			h.logf("coding: could not write %s: %s", spec.Path, err.Error())
			break
		}
		bf.Lines = strings.Count(content, "\n")
		if info, err := os.Stat(abs); err == nil {
			bf.Bytes = info.Size()
		}

		// The cheapest real check there is: does it even parse/compile?
		//
		// Which check depends on what the file actually IS. A design
		// legitimately includes a Dockerfile, a docker-compose.yml or a
		// schema.sql alongside the code; running the language's own
		// compiler on those fails every time, forever, for reasons that
		// have nothing to do with their contents.
		check := ""
		if tc.IsSource(spec.Path) {
			if tc.SyntaxCheck != nil {
				check = tc.SyntaxCheck(d, env, spec.Path)
			}
		} else {
			check = SupportFileCheck(spec.Path, env)
		}
		if check == "" {
			break // nothing honest to check - writing it is the whole job
		}
		res := r.Run(ctx, check, quickTimeout)
		if res.OK {
			if attempt > 1 {
				h.logf("coding: %s compiles now (attempt %d)", spec.Path, attempt)
			}
			break
		}

		lastFailure = res.Tail(2500)
		// The real compiler message, not just "it failed". Without it the log
		// says "does not compile yet" twenty times in a row with no way to
		// tell a stubborn model from a broken toolchain - exactly the hole
		// this filled the first time the harness ran against a real VM.
		h.logf("coding: %s does not compile yet (attempt %d/%d): %s", spec.Path, attempt, h.MaxFileAttempts, truncate(firstMeaningfulLine(lastFailure), 200))
		diag = h.Diagnose(ctx, lastFailure, d, env, tc)
		h.logf("coding-diagnosis: %s", diag.Cause)

		if diag.Kind == DiagEnvironment && len(diag.FixCommands) > 0 {
			if h.applyFix(ctx, r, diag, report) {
				h.refreshEnv(r, env)
				// The code may have been fine all along - re-check before
				// spending an attempt rewriting something that now works.
				if again := r.Run(ctx, check, quickTimeout); again.OK {
					break
				}
			}
		}
	}

	built[spec.Path] = readFileOr(filepath.Join(r.Dir, filepath.FromSlash(spec.Path)), "")
	return bf
}

// testSourceFile is the second pass: now that every file in the project
// really exists, write real tests for this one and keep fixing until they
// genuinely pass.
func (h *Harness) testSourceFile(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, spec FileSpec, built map[string]string, report *BuildReport, bf *BuiltFile) {
	if !spec.Testable(tc) {
		return
	}
	testPath := spec.TestPath(tc)
	if testPath == "" {
		// Rust and friends keep tests inside the source file - the full
		// suite covers them; there is no separate file to write.
		return
	}
	bf.TestPath = testPath
	bf.TestsPassed = h.testFileUntilPassing(ctx, r, d, tc, env, spec, testPath, built, report, bf)
}

// testFileUntilPassing writes real tests and then runs the real fix loop:
// on each failure it decides whether the CODE or the TEST is wrong and
// rewrites whichever genuinely is, until they pass or budget runs out.
func (h *Harness) testFileUntilPassing(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, spec FileSpec, testPath string, built map[string]string, report *BuildReport, bf *BuiltFile) bool {
	sourceContent := built[spec.Path]

	testContent, err := h.generateTest(ctx, d, tc, spec, testPath, sourceContent)
	if err != nil {
		h.logf("coding: could not generate tests for %s: %s", spec.Path, err.Error())
		return false
	}
	testAbs := filepath.Join(r.Dir, filepath.FromSlash(testPath))
	if err := writeFile(testAbs, testContent); err != nil {
		h.logf("coding: could not write %s: %s", testPath, err.Error())
		return false
	}
	h.logf("coding: wrote real tests for %s -> %s", spec.Path, testPath)

	for attempt := 1; attempt <= h.MaxFileAttempts && h.budgetLeft(); attempt++ {
		h.spend()

		res := r.Run(ctx, tc.UnitTestCmd(d, env, testPath), testTimeout)
		if res.OK {
			h.logf("coding: ✓ %s - tests pass", testPath)
			return true
		}

		failure := res.Tail(3000)
		h.logf("coding: tests for %s failing (attempt %d/%d): %s", spec.Path, attempt, h.MaxFileAttempts, truncate(firstMeaningfulLine(failure), 200))
		diag := h.Diagnose(ctx, failure, d, env, tc)
		h.logf("coding-diagnosis: [%s] %s", diag.Kind, diag.Cause)

		if diag.Kind == DiagEnvironment && len(diag.FixCommands) > 0 {
			if h.applyFix(ctx, r, diag, report) {
				h.refreshEnv(r, env)
				continue // retry the same tests against the fixed environment
			}
		}

		// A real judgment call, made explicitly rather than by always
		// rewriting the same side: a failing assertion is as often a wrong
		// test as wrong code, and blindly "fixing the code" to satisfy a
		// bad test is how a suite ends up green and meaningless.
		target, newContent, ferr := h.repairCodeOrTest(ctx, d, tc, spec, testPath,
			built[spec.Path], readFileOr(testAbs, ""), failure, diag)
		if ferr != nil {
			h.logf("coding: could not work out a fix: %s", ferr.Error())
			continue
		}

		switch target {
		case "test":
			if err := writeFile(testAbs, newContent); err == nil {
				h.logf("coding: the TEST was wrong - rewrote %s", testPath)
			}
		default:
			srcAbs := filepath.Join(r.Dir, filepath.FromSlash(spec.Path))
			if err := writeFile(srcAbs, newContent); err == nil {
				built[spec.Path] = newContent
				bf.Lines = strings.Count(newContent, "\n")
				h.logf("coding: the CODE was wrong - rewrote %s", spec.Path)
			}
		}
	}

	report.Failures = append(report.Failures, fmt.Sprintf("%s: tests never passed", spec.Path))
	return false
}

func (h *Harness) applyFix(ctx context.Context, r *Runner, diag Diagnosis, report *BuildReport) bool {
	for _, fix := range diag.FixCommands {
		h.logf("coding: applying a real fix: %s", truncate(fix, 120))
		if res := r.Run(ctx, fix, installTimeout); !res.OK {
			h.logf("coding: that fix did not work (exit %d): %s", res.ExitCode, truncate(res.Tail(400), 200))
			return false
		}
	}
	h.fixesApplied = append(h.fixesApplied, strings.Join(diag.FixCommands, " && "))
	if diag.Learned {
		h.learnedFixes = append(h.learnedFixes, diag.Cause+" → "+fixSummary(diag))
		h.Learn(ctx, diag.Cause, diag)
	}
	return true
}

// ------------------------------------------------------- code generation

func (h *Harness) generateCode(ctx context.Context, d *Design, tc *Toolchain, spec FileSpec, built map[string]string, lastFailure string, diag Diagnosis, attempt int) (string, error) {
	var symbols strings.Builder
	for _, s := range spec.Symbols {
		sig := s.Signature
		if strings.TrimSpace(sig) == "" {
			sig = s.Name
		}
		fmt.Fprintf(&symbols, "  - %s\n      %s\n", sig, s.Purpose)
	}
	if symbols.Len() == 0 {
		symbols.WriteString("  (whatever this file's purpose genuinely requires)\n")
	}

	contextBlock := ""
	if written := formatWrittenFiles(built, spec.Path); written != "" {
		contextBlock = "\nFILES ALREADY WRITTEN IN THIS PROJECT (real content on disk - call into them exactly as they really are):\n" + written + "\n"
	}

	retryBlock := ""
	if attempt > 1 && lastFailure != "" {
		retryBlock = fmt.Sprintf(`
YOUR PREVIOUS ATTEMPT AT THIS FILE FAILED FOR REAL. The real output:
%s

DIAGNOSIS: %s
WHAT TO CHANGE: %s

Fix it properly. Re-emit the COMPLETE file, not a patch or a fragment.
`, lastFailure, orDefault(diag.Cause, "see the output above"), orDefault(diag.CodeHint, "correct whatever the output says is wrong"))
	}

	deps := "none - standard library only"
	if len(d.Dependencies) > 0 {
		deps = strings.Join(d.Dependencies, ", ")
	}

	prompt := fmt.Sprintf(codeGenPrompt,
		d.ProjectName, d.Summary, d.Language, d.Kind,
		architectureOutline(d), spec.Path, spec.Purpose, symbols.String(),
		contextBlock, retryBlock, deps)

	resp, err := h.Provider.Chat(ctx, h.Model, []llm.Message{{Role: "user", Content: prompt}},
		codeGenOptions())
	if err != nil {
		return "", err
	}
	// A truncated response is a real, distinct failure that looks exactly
	// like bad code: the file is cut off mid-function and fails to compile
	// with a syntax error. Saying so plainly stops the fix loop from
	// "correcting" code that was never finished being written.
	if resp.Truncated {
		h.logf("coding: the model ran out of room writing %s - it was cut off mid-file, not wrong. Asking again for a shorter, complete file.", spec.Path)
	}
	content := cleanCode(resp.Text)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("the model returned an empty file")
	}
	if resp.Truncated {
		return content, errTruncated
	}
	return content, nil
}

// errTruncated marks a file that is real but INCOMPLETE. It is returned
// alongside the content (not instead of it) so the caller can decide:
// there is no point writing half a file to disk and compiling it.
var errTruncated = errors.New("the model's response was cut off before the file was finished")

func (h *Harness) generateTest(ctx context.Context, d *Design, tc *Toolchain, spec FileSpec, testPath, sourceContent string) (string, error) {
	prompt := fmt.Sprintf(testGenPrompt,
		d.ProjectName, d.Language, spec.Path, architectureOutline(d),
		sourceContent, tc.TestFrameworkNote, testPath)

	resp, err := h.Provider.Chat(ctx, h.Model, []llm.Message{{Role: "user", Content: prompt}},
		codeGenOptions())
	if err != nil {
		return "", err
	}
	content := cleanCode(resp.Text)
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("the model returned an empty test file")
	}
	return content, nil
}

const repairPrompt = `A real test run failed. Decide what is actually wrong and fix it properly.

REAL FAILURE OUTPUT:
%s

DIAGNOSIS: %s
%s
THE SOURCE FILE (%s):
%s

THE TEST FILE (%s):
%s

Decide honestly which one is wrong:
- If the CODE does not do what it is supposed to, fix the code.
- If the TEST asserts something incorrect (wrong expected value, wrong import path, testing something the design never promised), fix the test.
- NEVER weaken, delete, or trivialise an assertion just to make it pass. A green suite that proves nothing is worse than a red one.

Respond in EXACTLY this format, nothing else:
TARGET: code or test
CONTENT:
<the COMPLETE new content of whichever file you chose, raw, no markdown fence>`

var targetRe = regexp.MustCompile(`(?im)^TARGET:\s*(code|test)\b`)

// repairCodeOrTest makes the real call about which side of a failing test
// is actually wrong, and returns the complete replacement for it.
func (h *Harness) repairCodeOrTest(ctx context.Context, d *Design, tc *Toolchain, spec FileSpec, testPath, sourceContent, testContent, failure string, diag Diagnosis) (target, content string, err error) {
	hintBlock := ""
	if diag.CodeHint != "" {
		hintBlock = "WHAT TO CHANGE: " + diag.CodeHint + "\n"
	}

	prompt := fmt.Sprintf(repairPrompt, failure, orDefault(diag.Cause, "not identified"), hintBlock,
		spec.Path, sourceContent, testPath, testContent)

	resp, err := h.Provider.Chat(ctx, h.Model, []llm.Message{{Role: "user", Content: prompt}},
		codeGenOptions())
	if err != nil {
		return "", "", err
	}

	target = "code"
	if m := targetRe.FindStringSubmatch(resp.Text); m != nil {
		target = strings.ToLower(m[1])
	}

	idx := strings.Index(resp.Text, "CONTENT:")
	if idx == -1 {
		return "", "", fmt.Errorf("repair response had no CONTENT block")
	}
	content = cleanCode(strings.TrimLeft(resp.Text[idx+len("CONTENT:"):], "\r\n"))
	if strings.TrimSpace(content) == "" {
		return "", "", fmt.Errorf("repair response had an empty CONTENT block")
	}
	return target, content, nil
}

// ------------------------------------------------------- integration phase

const integrationTestPrompt = `Write ONE real end-to-end integration test for a project that has just been built.

PROJECT: %s - %s
LANGUAGE: %s | KIND: %s

WHAT THE INTEGRATION TEST MUST PROVE:
%s

THE REAL PROJECT LAYOUT:
%s

THE REAL CODE (exactly what is on disk):
%s

TEST FRAMEWORK: %s

REQUIREMENTS:
- This is an INTEGRATION test: exercise the real pieces TOGETHER through their real public interfaces, in a realistic workflow, end to end. Do not re-test one function in isolation - the unit tests already do that.
- Use the real modules. Do not mock the project's own code.
- It must pass deterministically on a clean machine: no network, no sleeping, no external services, no ports, no files that don't exist. Use temporary directories/in-memory stores where persistence is involved.
- Real assertions that genuinely fail if the system is broken.
- This file lives at %s and the project root is on the import path.

Respond with ONLY the raw file content. No markdown fence, no commentary.`

// runIntegrationPhase is the real end-to-end gate: an integration test
// exercising the pieces together, then the FULL suite, then the build,
// then - for something meant to actually run - a real smoke start.
// Everything here keeps fixing until it genuinely passes or budget is out.
func (h *Harness) runIntegrationPhase(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, built map[string]string, report *BuildReport) {
	if !h.budgetLeft() {
		report.Incomplete = true
		return
	}

	h.logf("coding: ===== every file passes its own tests - now testing the whole thing together =====")

	if testPath := h.writeIntegrationTest(ctx, r, d, tc, built); testPath != "" {
		h.logf("coding: wrote a real end-to-end integration test -> %s", testPath)
	}

	report.TestsPassed = h.fullSuiteUntilPassing(ctx, r, d, tc, env, built, report)

	if tc.BuildCmd != nil {
		if cmd := tc.BuildCmd(d, env); cmd != "" {
			res := r.Run(ctx, cmd, testTimeout)
			report.BuildPassed = res.OK
			if res.OK {
				h.logf("coding: ✓ the project builds")
			} else {
				h.logf("coding: ✗ the project does not build: %s", truncate(res.Tail(600), 300))
				report.Failures = append(report.Failures, "the project does not build")
			}
		} else {
			report.BuildPassed = true
		}
	} else {
		report.BuildPassed = true
	}

	h.smokeRun(ctx, r, d, env, report)
}

func (h *Harness) writeIntegrationTest(ctx context.Context, r *Runner, d *Design, tc *Toolchain, built map[string]string) string {
	want := d.IntegrationTest
	if strings.TrimSpace(want) == "" {
		want = "that the project's real components work together end to end for its main use case: " + d.Summary
	}

	testPath := integrationTestPath(tc)
	if testPath == "" {
		return ""
	}

	prompt := fmt.Sprintf(integrationTestPrompt,
		d.ProjectName, d.Summary, d.Language, d.Kind, want,
		architectureOutline(d), formatWrittenFiles(built, ""), tc.TestFrameworkNote, testPath)

	resp, err := h.Provider.Chat(ctx, h.Model, []llm.Message{{Role: "user", Content: prompt}},
		codeGenOptions())
	if err != nil {
		h.logf("coding: could not generate the integration test: %s", err.Error())
		return ""
	}
	content := cleanCode(resp.Text)
	if strings.TrimSpace(content) == "" {
		return ""
	}
	if err := writeFile(filepath.Join(r.Dir, filepath.FromSlash(testPath)), content); err != nil {
		h.logf("coding: could not write the integration test: %s", err.Error())
		return ""
	}
	return testPath
}

func integrationTestPath(tc *Toolchain) string {
	switch tc.Language {
	case "python":
		return "tests/test_integration.py"
	case "go":
		return "integration_test.go"
	case "node":
		return "integration.test.js"
	case "cpp":
		return "tests/test_integration.cpp"
	default:
		return "" // Rust keeps tests in-source; the full suite covers it
	}
}

// fullSuiteUntilPassing runs the real, complete test suite and keeps
// repairing whichever file is actually at fault until it passes.
func (h *Harness) fullSuiteUntilPassing(ctx context.Context, r *Runner, d *Design, tc *Toolchain, env *EnvFacts, built map[string]string, report *BuildReport) bool {
	for attempt := 1; h.budgetLeft(); attempt++ {
		h.spend()

		res := r.Run(ctx, tc.FullTestCmd(d, env), testTimeout)
		report.TestOutput = res.Tail(4000)
		if res.OK {
			h.logf("coding: ✓ the FULL test suite passes (attempt %d)", attempt)
			return true
		}

		failure := res.Tail(4000)
		h.logf("coding: full suite failing (attempt %d): %s", attempt, truncate(firstMeaningfulLine(failure), 200))
		diag := h.Diagnose(ctx, failure, d, env, tc)
		h.logf("coding-diagnosis: [%s] %s", diag.Kind, diag.Cause)

		if diag.Kind == DiagEnvironment && len(diag.FixCommands) > 0 && h.applyFix(ctx, r, diag, report) {
			h.refreshEnv(r, env)
			continue
		}

		fixedPath, newContent, err := h.repairProject(ctx, r, d, built, failure, diag)
		if err != nil {
			h.logf("coding: could not work out which file to fix: %s", err.Error())
			continue
		}
		abs := filepath.Join(r.Dir, filepath.FromSlash(fixedPath))
		if err := writeFile(abs, newContent); err != nil {
			h.logf("coding: could not write the fix to %s: %s", fixedPath, err.Error())
			continue
		}
		built[fixedPath] = newContent
		h.logf("coding: rewrote %s to fix the suite", fixedPath)
	}

	report.Failures = append(report.Failures, "the full test suite never passed within the build budget")
	report.Incomplete = true
	return false
}

const projectRepairPrompt = `The FULL test suite of a real project is failing. Work out which SINGLE file is genuinely at fault and fix it.

REAL TEST OUTPUT:
%s

DIAGNOSIS: %s
%s
EVERY FILE IN THE PROJECT (real content on disk):
%s

Pick the ONE file that is actually wrong - source or test - and rewrite it completely. Never weaken an assertion to make a test pass.

Respond in EXACTLY this format, nothing else:
FILE: <exact path of the file to replace, from the list above>
CONTENT:
<the COMPLETE new content of that file, raw, no markdown fence>`

var fileTargetRe = regexp.MustCompile(`(?im)^FILE:\s*(.+)$`)

// repairProject decides which single real file is at fault for a full-suite
// failure and returns its complete replacement. Scoped to one file per
// attempt on purpose: a change that rewrites three files at once makes it
// impossible to tell which edit helped when the next run still fails.
func (h *Harness) repairProject(ctx context.Context, r *Runner, d *Design, built map[string]string, failure string, diag Diagnosis) (string, string, error) {
	all := h.collectProjectFiles(r, d)

	hintBlock := ""
	if diag.CodeHint != "" {
		hintBlock = "WHAT TO CHANGE: " + diag.CodeHint + "\n"
	}

	prompt := fmt.Sprintf(projectRepairPrompt, failure, orDefault(diag.Cause, "not identified"), hintBlock, formatWrittenFiles(all, ""))

	resp, err := h.Provider.Chat(ctx, h.Model, []llm.Message{{Role: "user", Content: prompt}},
		codeGenOptions())
	if err != nil {
		return "", "", err
	}

	m := fileTargetRe.FindStringSubmatch(resp.Text)
	if m == nil {
		return "", "", fmt.Errorf("repair response named no file")
	}
	target := strings.TrimSpace(strings.Trim(m[1], "`\"' "))

	idx := strings.Index(resp.Text, "CONTENT:")
	if idx == -1 {
		return "", "", fmt.Errorf("repair response had no CONTENT block")
	}
	content := cleanCode(strings.TrimLeft(resp.Text[idx+len("CONTENT:"):], "\r\n"))
	if strings.TrimSpace(content) == "" {
		return "", "", fmt.Errorf("repair response had an empty CONTENT block")
	}

	// Never let a repair response write outside the project, whatever path
	// it names - a fix is always a fix to a file inside this build.
	if path.IsAbs(target) || strings.Contains(target, "..") {
		return "", "", fmt.Errorf("repair named an unsafe path: %q", target)
	}
	if _, known := all[target]; !known {
		return "", "", fmt.Errorf("repair named a file that isn't part of this project: %q", target)
	}
	return target, content, nil
}

// collectProjectFiles reads what is REALLY on disk right now - source and
// tests both - rather than trusting the in-memory map, which only knows
// about files this run happened to write.
func (h *Harness) collectProjectFiles(r *Runner, d *Design) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(r.Dir, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".venv", "node_modules", "build", "target", ".git", "__pycache__", ".pytest_cache":
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(r.Dir, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !isSourceLike(rel) {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil && info.Size() > 200_000 {
			return nil // never pull a huge generated file into a prompt
		}
		out[rel] = readFileOr(p, "")
		return nil
	})
	return out
}

func isSourceLike(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".py", ".go", ".js", ".mjs", ".ts", ".cpp", ".cc", ".h", ".hpp", ".rs":
		return true
	}
	return false
}

// smokeRun actually starts something meant to run, briefly, and reports
// honestly what happened. A server that stays up until it's killed is a
// PASS (exit 124 from `timeout` means it was still running); a process
// that exits on its own within seconds has almost certainly crashed.
func (h *Harness) smokeRun(ctx context.Context, r *Runner, d *Design, env *EnvFacts, report *BuildReport) {
	cmd := strings.TrimSpace(d.RunCommand)
	if cmd == "" || !h.budgetLeft() {
		return
	}
	switch d.Kind {
	case "api", "web-app", "dashboard", "service", "cli":
	default:
		return
	}

	// Address the venv's interpreter by real path, same as everywhere else,
	// so a smoke run doesn't silently use a system Python without the
	// project's dependencies installed.
	if env.VenvPython != "" {
		cmd = rewriteForVenv(cmd, env.VenvPython)
	}

	h.spend()
	h.logf("coding: smoke-checking that it really starts: %s", truncate(cmd, 120))

	res := r.Run(ctx, fmt.Sprintf("timeout 20 %s 2>&1", cmd), 40*time.Second)
	report.SmokeRan = true
	report.SmokeOutput = res.Tail(1500)

	switch {
	case res.ExitCode == 124:
		h.logf("coding: ✓ it starts and keeps running (still alive after 20s)")
	case res.OK:
		h.logf("coding: ✓ it ran and exited cleanly")
	default:
		h.logf("coding: ✗ it failed to start: %s", truncate(res.Tail(500), 250))
		report.Failures = append(report.Failures, "it does not start: "+truncate(res.Tail(300), 200))
	}
}

// rewriteForVenv points a run command at the project's own interpreter.
func rewriteForVenv(cmd, venvPython string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return cmd
	}
	switch fields[0] {
	case "python", "python3":
		return venvPython + " " + strings.Join(fields[1:], " ")
	case "uvicorn", "gunicorn", "flask", "streamlit", "fastapi":
		// These are console scripts installed into the venv's bin/ - run
		// the venv's copy, not whatever happens to be on PATH.
		return strings.TrimSuffix(venvPython, "python") + cmd
	}
	return cmd
}

// ---------------------------------------------------------------- helpers

func architectureOutline(d *Design) string {
	var b strings.Builder
	for _, f := range d.Files {
		fmt.Fprintf(&b, "  %s - %s\n", f.Path, f.Purpose)
		for _, s := range f.Symbols {
			sig := s.Signature
			if strings.TrimSpace(sig) == "" {
				sig = s.Name
			}
			fmt.Fprintf(&b, "      %s\n", oneLine(sig))
		}
	}
	return b.String()
}

func formatWrittenFiles(built map[string]string, skip string) string {
	var b strings.Builder
	for p, content := range built {
		if p == skip || strings.TrimSpace(content) == "" {
			continue
		}
		body := content
		if len(body) > maxFileContextChars {
			body = body[:maxFileContextChars] + "\n[...truncated...]"
		}
		fmt.Fprintf(&b, "==== %s ====\n%s\n\n", p, body)
	}
	return b.String()
}

// cleanCode strips what a model wraps around raw file content despite
// being told not to: a markdown fence, or a sentence of preamble before
// the fence. Writing either into a source file breaks it on line 1.
func cleanCode(text string) string {
	s := strings.TrimSpace(text)

	if m := fencedFileRe.FindStringSubmatch(s); m != nil {
		return strings.TrimRight(m[1], " \n\r") + "\n"
	}
	// An unterminated fence (the model started one and hit its token
	// limit) - keep everything after the opening marker.
	if strings.HasPrefix(s, "```") {
		if nl := strings.Index(s, "\n"); nl != -1 {
			s = s[nl+1:]
		}
		s = strings.TrimSuffix(strings.TrimRight(s, " \n\r"), "```")
	}
	return strings.TrimRight(s, " \n\r") + "\n"
}

var fencedFileRe = regexp.MustCompile("(?s)```[a-zA-Z0-9+#.-]*\n(.*?)```")

func writeFile(abs, content string) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func readFileOr(abs, fallback string) string {
	b, err := os.ReadFile(abs)
	if err != nil {
		return fallback
	}
	return string(b)
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
