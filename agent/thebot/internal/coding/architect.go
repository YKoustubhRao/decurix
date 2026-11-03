package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"thebot/internal/llm"
)

// Design is the real architecture decided BEFORE a single line of code is
// written - what is being built, in what language, in which directory,
// which files exist, and what each one is responsible for.
//
// It exists as a real struct (rather than prose the code generator is told
// to "follow") because every later phase reads it as data: the scaffolder
// needs Language and Dependencies, the per-file loop iterates Files, each
// file's generation prompt is grounded in its own Symbols plus the other
// files' signatures, and the user-facing announcement is rendered from it.
// A plan nothing can read is not a plan.
type Design struct {
	ProjectName  string     `json:"project_name"`
	Language     string     `json:"language"`
	Kind         string     `json:"kind"`
	Dir          string     `json:"dir"`
	Summary      string     `json:"summary"`
	Dependencies []string   `json:"dependencies"`
	Files        []FileSpec `json:"files"`
	// RunCommand is how a human would actually start the thing once built
	// (a server, a CLI, a dashboard). Reported, and used as a real smoke
	// check for runnable kinds - never assumed to work.
	RunCommand string `json:"run_command"`
	// IntegrationTest describes, in plain terms, what the end-to-end test
	// has to prove about the whole system working together - written by the
	// architect, implemented later as a real test file.
	IntegrationTest string `json:"integration_test"`
}

// FileSpec is one real source file and the contract it has to satisfy.
type FileSpec struct {
	Path    string   `json:"path"`
	Purpose string   `json:"purpose"`
	Symbols []Symbol `json:"symbols"`
	// Imports names the project's OWN other files this one depends on, so
	// the build order can be topological instead of arbitrary - a file is
	// far likelier to compile first time when what it imports already
	// exists on disk with its real signatures.
	Imports []string `json:"imports"`
}

// Symbol is one function/class/type the file must define. Signature is
// what other files will be told to call, so it is a real interface
// contract between files, not decoration.
type Symbol struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
	Purpose   string `json:"purpose"`
}

// TestPath is where this file's unit tests live, per the language's own
// convention. Empty means the toolchain keeps tests inside the source file
// (Rust), or that this file is not worth testing on its own.
func (f FileSpec) TestPath(tc *Toolchain) string {
	if tc.TestPathFor == nil {
		return ""
	}
	return tc.TestPathFor(f.Path)
}

// Testable reports whether writing a separate unit test for this file is
// real work or busywork. An empty __init__.py or a pure re-export module
// has nothing to assert about; a test for it passes trivially and teaches
// the loop nothing.
func (f FileSpec) Testable(tc *Toolchain) bool {
	// Only real source files of this project's own language. A Dockerfile
	// or a schema.sql is a genuine part of the project and genuinely has
	// no unit test - writing a pytest file "for" it would assert nothing.
	if tc != nil && !tc.IsSource(f.Path) {
		return false
	}
	return len(f.Symbols) > 0
}

const architectPrompt = `You are the architect for a real software project. Design it completely BEFORE any code is written. Your design will be executed literally by a build system: every file you list will really be created, every symbol you declare will really be implemented, and real tests will really be run against all of it.

REQUEST:
%s
%s
Decide:
1. LANGUAGE - exactly one of: python, go, node, cpp, rust. Pick what the request asks for; if it doesn't say, pick what a professional would actually use for this kind of program.
2. KIND - one of: library, cli, api, web-app, dashboard, script, service.
3. DIR - the directory to build in, relative and lowercase-with-hyphens (e.g. "task-api"). One new directory for this project - never "." and never an absolute path.
4. DEPENDENCIES - real installable package names ONLY (pypi/go module paths/npm names). Leave empty unless genuinely needed. NEVER list the language's own standard library. NEVER list a test framework - the harness installs and runs the right one itself.
5. FILES - every source file, each with the functions/classes it defines and their real signatures.

Rules that matter:
- Design the FUNCTIONS FIRST, then put each one in the file where it belongs. Group by responsibility, not alphabetically.
- Every file needs a single clear purpose. If you cannot say a file's purpose in one sentence, split it.
- Signatures are a real contract: other files will be generated against exactly what you write here, so make them precise and consistent (parameter names, types, return types).
- List a file's project-internal dependencies in "imports" using the other files' paths. Files are built in dependency order.
- Keep it to the smallest structure that genuinely does the job well - typically 2-6 files. Do not invent layers a real engineer wouldn't write for this request.
- Do NOT list test files. The harness writes and runs tests for every file itself.
- For a web-app/dashboard/api/service, "run_command" must be the real command that starts it.

Things that decide whether this is testable at all - they matter as much as the structure:
- SEPARATE LOGIC FROM I/O. Every piece of real behaviour must live in a plain function or class that can be called directly, with no server running, no browser, and no UI. A Streamlit/Dash/Flask/FastAPI file should be a thin layer that calls those functions. A dashboard whose logic only exists inside a page callback cannot be tested by anything, and this build tests everything it writes.
- DATABASES: default to SQLite through the standard library for anything local - it needs no server, so the tests really run. Only choose Postgres/MySQL/Redis if the request explicitly asks for them, and even then put every query behind a repository/adapter file whose tests can point at a temporary local database. Never design tests that need a service someone has to start by hand.
- EXTERNAL APIs: isolate every network call in its own client file, so the rest of the code can be tested without the network. The tests this harness writes are not allowed to make real network calls.
- DOCKER: if the request asks for containers, list the real Dockerfile and/or docker-compose.yml as files, and still design the actual application code alongside them - a Dockerfile on its own builds nothing worth testing.
- Prefer the standard library. Every dependency is one more thing that has to install successfully on a machine you cannot see before any of this runs.

Respond with ONLY a JSON object, no prose, no markdown fence:
{
  "project_name": "short-name",
  "language": "python",
  "kind": "api",
  "dir": "task-api",
  "summary": "one sentence describing what this program does",
  "dependencies": ["fastapi", "uvicorn"],
  "run_command": "uvicorn app.main:app --port 8000",
  "integration_test": "what the end-to-end test must prove about the whole thing working together",
  "files": [
    {
      "path": "app/models.py",
      "purpose": "Task data model and validation",
      "imports": [],
      "symbols": [
        {"name": "Task", "signature": "class Task(BaseModel): id: int; title: str; done: bool = False", "purpose": "One task record"}
      ]
    }
  ]
}`

// jsonBlockRe pulls the real JSON object out of a response that wrapped it
// in a markdown fence or a sentence of preamble despite being told not to.
// Models do this often enough that failing the whole build over it would
// be throwing away a perfectly good design for a formatting habit.
var jsonBlockRe = regexp.MustCompile(`(?s)\{.*\}`)

// Architect makes the real design call and returns a validated Design.
//
// knownContext is whatever the agent already knows that's relevant
// (recalled memory, earlier findings in this same job) - threaded in so a
// design can build on real established facts instead of only the raw
// request.
func Architect(ctx context.Context, provider llm.Provider, model, request, knownContext string, onLog func(string)) (*Design, error) {
	contextBlock := ""
	if strings.TrimSpace(knownContext) != "" {
		contextBlock = "\nWHAT IS ALREADY KNOWN (from real research/memory just now - treat as fact, it overrides your training data):\n" + knownContext + "\n"
	}

	prompt := fmt.Sprintf(architectPrompt, request, contextBlock)

	resp, err := provider.Chat(ctx, model, []llm.Message{{Role: "user", Content: prompt}},
		// Low but non-zero: architecture benefits from a little freedom,
		// but this output is parsed as JSON and fed to a build system, so
		// wandering is expensive.
		llm.Options{TimeoutMs: 180000, Temperature: 0.2, NumCtx: codeGenNumCtx, MaxTokens: codeGenMaxTokens, KeepAlive: codeGenKeepAlive, Purpose: llm.PurposeCodingArchitect})
	if err != nil {
		return nil, fmt.Errorf("architecture call failed: %w", err)
	}

	d, err := parseDesign(resp.Text)
	if err != nil {
		return nil, err
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	d.normalize()
	return d, nil
}

func parseDesign(text string) (*Design, error) {
	raw := strings.TrimSpace(text)
	if m := jsonBlockRe.FindString(raw); m != "" {
		raw = m
	}

	var d Design
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("architecture response was not valid JSON: %w", err)
	}
	return &d, nil
}

func (d *Design) validate() error {
	if len(d.Files) == 0 {
		return fmt.Errorf("architecture named no files to build")
	}
	for _, f := range d.Files {
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("architecture has a file with no path")
		}
		if path.IsAbs(f.Path) || strings.Contains(f.Path, "..") {
			return fmt.Errorf("architecture has an unsafe file path: %q", f.Path)
		}
	}
	return nil
}

// normalize repairs the things a model gets slightly wrong in ways that
// are mechanical to fix and pointless to fail a whole build over.
func (d *Design) normalize() {
	d.Language = strings.ToLower(strings.TrimSpace(d.Language))
	if d.Language == "" {
		d.Language = inferLanguage(d.Files)
	}
	// Aliases models reach for that mean a language this harness already
	// knows under a different name.
	switch d.Language {
	case "python3", "py":
		d.Language = "python"
	case "c++", "cplusplus", "cxx":
		d.Language = "cpp"
	case "javascript", "js", "typescript", "ts", "nodejs":
		d.Language = "node"
	case "golang":
		d.Language = "go"
	}

	if strings.TrimSpace(d.ProjectName) == "" {
		d.ProjectName = "project"
	}

	d.Dir = strings.Trim(strings.TrimSpace(d.Dir), "/\\")
	if d.Dir == "" || d.Dir == "." {
		d.Dir = pyProjectName(d.ProjectName)
	}

	// A test framework in dependencies is a real problem, not a cosmetic
	// one: the harness installs and runs its own (pytest, node:test, plain
	// cassert), and a second framework the architect added gets installed,
	// never used, and sometimes shadows the real one's collection.
	var deps []string
	for _, dep := range d.Dependencies {
		dep = strings.TrimSpace(dep)
		if dep == "" || isTestFramework(dep) || isStdlibName(d.Language, dep) {
			continue
		}
		deps = append(deps, dep)
	}
	d.Dependencies = deps

	// Drop test files the architect added anyway - the harness owns tests,
	// and a hand-designed test file would be generated as if it were
	// source, then silently overwritten by the real test phase.
	var files []FileSpec
	for _, f := range d.Files {
		f.Path = strings.TrimPrefix(strings.TrimSpace(f.Path), "./")
		if looksLikeTestPath(f.Path) {
			continue
		}
		files = append(files, f)
	}
	if len(files) > 0 {
		d.Files = files
	}

	d.Files = orderByDependencies(d.Files)
}

func inferLanguage(files []FileSpec) string {
	for _, f := range files {
		switch strings.ToLower(path.Ext(f.Path)) {
		case ".py":
			return "python"
		case ".go":
			return "go"
		case ".js", ".mjs", ".ts":
			return "node"
		case ".cpp", ".cc", ".hpp", ".h":
			return "cpp"
		case ".rs":
			return "rust"
		}
	}
	return "python"
}

func looksLikeTestPath(p string) bool {
	lower := strings.ToLower(p)
	base := path.Base(lower)
	return strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, ".test.") ||
		strings.HasPrefix(lower, "tests/") ||
		strings.HasPrefix(lower, "test/")
}

func isTestFramework(dep string) bool {
	name := strings.ToLower(strings.SplitN(dep, "=", 2)[0])
	name = strings.SplitN(name, ">", 2)[0]
	name = strings.TrimSpace(strings.SplitN(name, "[", 2)[0])
	switch name {
	case "pytest", "unittest", "nose", "nose2", "jest", "vitest", "mocha",
		"chai", "ava", "tap", "gtest", "googletest", "catch2", "doctest",
		"testify", "ginkgo", "gomega":
		return true
	}
	return false
}

// isStdlibName filters the standard-library modules models habitually list
// as installable dependencies. `pip install json` fails outright; `pip
// install os` installs a real, unrelated, abandoned package - which is
// worse, because it succeeds.
func isStdlibName(language, dep string) bool {
	name := strings.ToLower(strings.TrimSpace(dep))
	if language != "python" {
		return false
	}
	switch name {
	case "os", "sys", "json", "re", "math", "time", "datetime", "typing",
		"pathlib", "collections", "itertools", "functools", "dataclasses",
		"sqlite3", "http", "urllib", "unittest", "asyncio", "logging",
		"random", "string", "subprocess", "threading", "csv", "io", "enum",
		"abc", "argparse", "hashlib", "base64", "uuid", "shutil", "glob",
		"tempfile", "socket", "struct", "copy", "heapq", "bisect":
		return true
	}
	return false
}

// orderByDependencies puts a file after everything it imports, so each one
// is generated with its real dependencies already written to disk (and
// their real signatures available to quote into the prompt). Falls back to
// the architect's own order for anything circular rather than refusing to
// build - a cycle is a design smell, not a reason to stop.
func orderByDependencies(files []FileSpec) []FileSpec {
	byPath := map[string]FileSpec{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	var ordered []FileSpec
	placed := map[string]bool{}
	visiting := map[string]bool{}

	var visit func(f FileSpec)
	visit = func(f FileSpec) {
		if placed[f.Path] || visiting[f.Path] {
			return // already done, or we're inside a cycle
		}
		visiting[f.Path] = true
		for _, imp := range f.Imports {
			imp = strings.TrimPrefix(strings.TrimSpace(imp), "./")
			if dep, ok := byPath[imp]; ok {
				visit(dep)
			}
		}
		delete(visiting, f.Path)
		placed[f.Path] = true
		ordered = append(ordered, f)
	}

	for _, f := range files {
		visit(f)
	}
	return ordered
}

// Announce renders the full architecture for the user, exactly as asked:
// what is being built, in which directory, which files, and what each file
// is responsible for - BEFORE any of it is written, so the plan can be
// seen and judged rather than inferred afterwards from a pile of files.
func (d *Design) Announce(onLog func(string)) {
	if onLog == nil {
		return
	}
	onLog("coding-architecture: ===== ARCHITECTURE AND FUNCTION DESIGN =====")
	onLog(fmt.Sprintf("coding-architecture: project: %s - %s", d.ProjectName, d.Summary))
	onLog(fmt.Sprintf("coding-architecture: language: %s | kind: %s", d.Language, d.Kind))
	onLog(fmt.Sprintf("coding-architecture: directory: %s/ (everything below is relative to it)", d.Dir))
	if len(d.Dependencies) > 0 {
		onLog(fmt.Sprintf("coding-architecture: dependencies: %s", strings.Join(d.Dependencies, ", ")))
	} else {
		onLog("coding-architecture: dependencies: none (standard library only)")
	}
	onLog(fmt.Sprintf("coding-architecture: %d file(s) to build:", len(d.Files)))
	for i, f := range d.Files {
		onLog(fmt.Sprintf("coding-architecture:   %d. %s - %s", i+1, f.Path, f.Purpose))
		for _, s := range f.Symbols {
			onLog(fmt.Sprintf("coding-architecture:        - %s  // %s", oneLine(orDefault(s.Signature, s.Name)), s.Purpose))
		}
	}
	if d.RunCommand != "" {
		onLog(fmt.Sprintf("coding-architecture: run with: %s", d.RunCommand))
	}
	if d.IntegrationTest != "" {
		onLog(fmt.Sprintf("coding-architecture: integration test must prove: %s", d.IntegrationTest))
	}
	onLog("coding-architecture: ===== building each file now, one at a time =====")
}
