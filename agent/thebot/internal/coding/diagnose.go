package coding

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"thebot/internal/llm"
	"thebot/internal/websearch"
)

// DiagKind is what KIND of problem a failure is, which decides what the
// fix loop does next. Getting this wrong is expensive in both directions:
// regenerating code to fix a missing system package rewrites working code
// forever without ever installing anything, and installing packages to fix
// a genuine logic bug installs nothing useful forever while the bug stays.
type DiagKind int

const (
	// DiagUnknown means no layer could identify it - the code generator
	// gets the raw error and does its best.
	DiagUnknown DiagKind = iota
	// DiagEnvironment is a missing dependency/tool/permission: fixable by
	// running a real command, NOT by rewriting code.
	DiagEnvironment
	// DiagCode is a real defect in the generated code: fixable only by
	// rewriting it.
	DiagCode
)

func (k DiagKind) String() string {
	switch k {
	case DiagEnvironment:
		return "environment"
	case DiagCode:
		return "code"
	default:
		return "unknown"
	}
}

// Diagnosis is the outcome of examining a real failure.
type Diagnosis struct {
	Kind DiagKind
	// Cause is a plain-language statement of what actually went wrong.
	Cause string
	// FixCommands are real commands to run before retrying (environment
	// fixes only). Each one is executed and checked like any other.
	FixCommands []string
	// CodeHint is concrete guidance handed to the code generator on the
	// next attempt - what specifically to change, not "try again".
	CodeHint string
	// Source records which layer produced this: "known" (the built-in
	// table), "memory" (something it learned before), "web" (real research
	// just now), or "model". Surfaced in logs so "did it actually learn
	// anything" is answerable rather than a matter of faith.
	Source string
	// Learned marks a diagnosis worth remembering - one that came from
	// real research rather than a table lookup it can always redo for free.
	// It is remembered only once its fix ran with approval and the check
	// that failed then passed (settleLearning).
	Learned bool
	// Signature is the failure's errorSignature, which pairs a proposal
	// the person approved with the same failure when the build is re-run.
	Signature string
	// proven marks a fix read back from the proven-fix record (memory.L1.ProvenFixAt): the exact command a person
	// approved for this same failure once, after which the check passed. It is still a proposal, asked about again
	// (mayRun); the question shows when they approved it.
	proven bool
}

// IsProposal reports whether the diagnosis's fix commands are a proposal - a
// command a web page or a model's reply put forward, as opposed to one
// written in this source file's table. A proposal runs only with the
// person's approval of that exact command (Harness.Approve).
func (d Diagnosis) IsProposal() bool {
	return len(d.FixCommands) > 0 && !strings.HasPrefix(d.Source, "known:")
}

// Action is the one command line a proposal asks approval for.
func (d Diagnosis) Action() string { return strings.Join(d.FixCommands, " && ") }

// knownFix is one real, recognised toolchain failure and its real fix.
// This table is the agent's built-in operational experience: every entry
// is a failure that genuinely happens, recognised deterministically with
// no model call and no network round trip.
type knownFix struct {
	name  string
	match *regexp.Regexp
	kind  DiagKind
	cause string
	// fix builds the real commands to run. nil means the cause is
	// informative but there is no generic command that fixes it.
	fix func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string
	// hint is guidance for the code generator when the fix is a code change.
	hint string
}

var knownFixes = []knownFix{
	{
		// Debian/Ubuntu (and now most distros) refuse system-wide pip
		// installs outright. Hit live in this project's own VM image. The
		// fix is never a cleverer pip invocation - it is to have a real
		// venv and install into it by path.
		name:  "externally-managed-environment",
		match: regexp.MustCompile(`(?i)externally[- ]managed[- ]environment|This environment is externally managed`),
		kind:  DiagEnvironment,
		cause: "This Python installation refuses system-wide pip installs (PEP 668, 'externally managed'). Packages have to go into a project virtualenv instead.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			if env.HasUV {
				return []string{"uv venv .venv", "uv pip install --python .venv/bin/python pytest " + strings.Join(d.Dependencies, " ")}
			}
			return []string{
				env.PythonBin + " -m venv .venv",
				".venv/bin/python -m pip install --upgrade pip",
				".venv/bin/python -m pip install pytest " + strings.Join(d.Dependencies, " "),
			}
		},
	},
	{
		// Found live in this project's own desktop image: python3 is
		// present but python3-venv isn't, so `python -m venv` fails with
		// this exact message and every install afterwards has nowhere to go.
		name:  "ensurepip-missing",
		match: regexp.MustCompile(`(?i)ensurepip is not available|No module named venv|python3-venv`),
		kind:  DiagEnvironment,
		cause: "The Python venv module is installed but incomplete (python3-venv is missing), so no virtualenv can be created the normal way.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			// uv brings its own standalone venv implementation and needs
			// neither ensurepip nor root - the real way out of this on a
			// box where apt-get would need a password nobody can type.
			return []string{
				"command -v uv >/dev/null 2>&1 || curl -LsSf https://astral.sh/uv/install.sh | sh",
				"export PATH=\"$HOME/.local/bin:$PATH\"; uv venv .venv",
			}
		},
	},
	{
		name:  "uv-not-installed",
		match: regexp.MustCompile(`(?i)(^|\W)uv: (command )?not found|command not found: uv`),
		kind:  DiagEnvironment,
		cause: "uv (the Python package manager this build prefers) is not installed on this machine.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			// Two real routes, in order of what works without root: the
			// official standalone installer, then pip's own copy of uv.
			return []string{
				"curl -LsSf https://astral.sh/uv/install.sh | sh || " + env.PythonBin + " -m pip install --user uv",
				"export PATH=\"$HOME/.local/bin:$PATH\"; uv --version",
			}
		},
	},
	{
		name:  "python-module-missing",
		match: regexp.MustCompile(`(?i)ModuleNotFoundError: No module named ['"]([\w.]+)['"]|ImportError: No module named ['"]?([\w.]+)`),
		kind:  DiagEnvironment,
		cause: "A Python module the code imports is not installed in the project environment.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			mod := firstNonEmpty(m[1:])
			if mod == "" {
				return nil
			}
			// Never reach for a package registry to satisfy one of the
			// project's OWN modules. Found live: a build's first file
			// imported two sibling modules that were files 2 and 3 of the
			// same design, and the missing-module rule happily proposed
			// installing "lru_cache_ttl.threading_utils" from PyPI. At best
			// that fails; at worst it installs a real, unrelated package
			// that happens to squat the name. This is a code/ordering
			// problem, and saying so sends the loop somewhere useful.
			if isProjectInternalModule(mod, d) {
				return nil
			}
			pkg := pypiPackageFor(topLevelModule(mod))
			if tc.InstallCmd == nil {
				return nil
			}
			if cmd := tc.InstallCmd(d, env, []string{pkg}); cmd != "" {
				return []string{cmd}
			}
			return nil
		},
	},
	{
		name:  "node-module-missing",
		match: regexp.MustCompile(`(?i)Cannot find (?:module|package) ['"]([^'"]+)['"]`),
		kind:  DiagEnvironment,
		cause: "A Node package the code imports is not installed.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			pkg := strings.TrimSpace(m[1])
			// A relative import that can't be found is a real code/path
			// bug - installing anything from the registry for it would be
			// nonsense (and might even succeed on an unrelated package).
			if strings.HasPrefix(pkg, ".") || strings.HasPrefix(pkg, "/") {
				return nil
			}
			return []string{"npm install " + pkg}
		},
	},
	{
		name:  "go-missing-module",
		match: regexp.MustCompile(`(?i)no required module provides package ([^\s;]+)|cannot find module providing package ([^\s;]+)|missing go\.sum entry`),
		kind:  DiagEnvironment,
		cause: "A Go dependency is referenced but not in go.mod/go.sum yet.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			if pkg := firstNonEmpty(m[1:]); pkg != "" {
				return []string{"go get " + pkg, "go mod tidy"}
			}
			return []string{"go mod tidy"}
		},
	},
	{
		name:  "rust-missing-crate",
		match: regexp.MustCompile(`(?i)use of (?:undeclared crate|unresolved module) or module ` + "`" + `([\w_]+)` + "`" + `|can't find crate for ` + "`" + `([\w_]+)` + "`"),
		kind:  DiagEnvironment,
		cause: "A Rust crate is used but not declared in Cargo.toml.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			if c := firstNonEmpty(m[1:]); c != "" {
				return []string{"cargo add " + strings.ReplaceAll(c, "_", "-")}
			}
			return nil
		},
	},
	{
		name:  "cpp-missing-header",
		match: regexp.MustCompile(`(?i)fatal error: ([\w/.+-]+\.(?:h|hpp)): No such file or directory`),
		kind:  DiagCode,
		cause: "An #include names a header that does not exist at that path.",
		hint:  "Fix the #include: use the real path of a header that exists in this project (paths are relative to the project root, which is on the include path), or a genuine standard-library header. Do not include a header for a third-party library that was never declared as a dependency.",
	},
	{
		name:  "port-in-use",
		match: regexp.MustCompile(`(?i)address already in use|EADDRINUSE|bind: address already in use`),
		kind:  DiagCode,
		cause: "The port the program binds is already taken by something else on this machine.",
		hint:  "Bind to a different, less common port (e.g. 8765 instead of 8000/5000/3000), and make the port configurable via an environment variable with that default.",
	},
	{
		name:  "permission-denied-exec",
		match: regexp.MustCompile(`(?i)permission denied`),
		kind:  DiagEnvironment,
		cause: "A file could not be executed or written because of filesystem permissions.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			return []string{"chmod -R u+rwX . 2>/dev/null || true"}
		},
	},
	{
		name:  "pytest-not-found",
		match: regexp.MustCompile(`(?i)No module named pytest|pytest: (command )?not found`),
		kind:  DiagEnvironment,
		cause: "pytest is not installed in the project's virtualenv, so no test can run.",
		fix: func(m []string, d *Design, env *EnvFacts, tc *Toolchain) []string {
			if env.HasUV {
				return []string{"uv pip install --python .venv/bin/python pytest"}
			}
			return []string{".venv/bin/python -m pip install pytest"}
		},
	},
	{
		name:  "network-unreachable",
		match: regexp.MustCompile(`(?i)Temporary failure in name resolution|Could not resolve host|Network is unreachable|Connection refused.*(pypi|registry\.npmjs|proxy\.golang)`),
		kind:  DiagEnvironment,
		cause: "The package registry is unreachable from this machine - this is a network problem, not a code problem.",
		hint:  "Rewrite the code to use only the language's standard library, with no third-party dependency at all, since nothing can be installed here.",
	},
	{
		name:  "python-syntax-error",
		match: regexp.MustCompile(`(?i)SyntaxError:|IndentationError:|TabError:`),
		kind:  DiagCode,
		cause: "The generated Python does not parse.",
		hint:  "Fix the syntax error exactly where the traceback points. Re-emit the COMPLETE file - consistent 4-space indentation throughout, no tabs, no stray markdown fences.",
	},
	{
		name:  "go-compile-error",
		match: regexp.MustCompile(`(?i)undefined: |declared and not used|cannot use .* as .* value|missing return|syntax error: unexpected`),
		kind:  DiagCode,
		cause: "The generated Go does not compile.",
		hint:  "Fix the compile errors exactly as reported. Every declared variable must be used, every import must be used, and every signature must match how other files in this project really call it.",
	},
	{
		name:  "assertion-failed",
		match: regexp.MustCompile(`(?i)AssertionError|assert(ion)? failed|FAILED .*::|--- FAIL:|✗|test failed`),
		kind:  DiagCode,
		cause: "The code compiles and runs, but a real test asserted something that isn't true.",
		hint:  "The logic is wrong, not the syntax. Read the failing assertion carefully: work out whether the CODE is wrong or the TEST's expectation is wrong, fix whichever is genuinely incorrect, and do not weaken or delete the assertion just to make it pass.",
	},
}

// Diagnose examines a real failure through every layer, cheapest first:
// the built-in table (instant, deterministic), then what this agent has
// personally learned before (memory), then real web research, then the
// model's own reasoning. The first layer that produces a real answer wins.
//
// This ordering is the whole point. A known failure should never cost a
// web search, and an unknown one should never be guessed at when the real
// answer is a search away.
func (h *Harness) Diagnose(ctx context.Context, failure string, d *Design, env *EnvFacts, tc *Toolchain) Diagnosis {
	diag := h.diagnose(ctx, failure, d, env, tc)
	diag.Signature = errorSignature(failure)
	return diag
}

func (h *Harness) diagnose(ctx context.Context, failure string, d *Design, env *EnvFacts, tc *Toolchain) Diagnosis {
	if strings.TrimSpace(failure) == "" {
		return Diagnosis{Kind: DiagUnknown, Cause: "the command failed with no output at all", Source: "none"}
	}

	// 1. Built-in operational experience.
	if diag, ok := matchKnownFix(failure, d, env, tc); ok {
		h.logf("coding-diagnosis: recognised this failure: %s (%s)", diag.Cause, diag.Source)
		return diag
	}

	// 1b. A proposal the person was asked about for this same failure: the
	// build re-runs after their reply, and the yes is for that command, not
	// for whatever a fresh search or model call would propose now.
	sig := errorSignature(failure)
	for _, p := range h.Proposed {
		if p.Signature != "" && p.Signature == sig {
			h.logf("coding-diagnosis: the same failure as the fix you were asked about: %s", truncate(p.Action(), 120))
			return p
		}
	}

	// 2a. A fix a person approved for exactly this failure, which then
	// worked: the same command again, asked about again with their earlier
	// yes shown (mayRun) - a remembered yes is never a yes.
	if diag, ok := h.provenFix(ctx, sig); ok {
		h.logf("coding-diagnosis: the fix you approved for this failure before: %s", truncate(diag.Action(), 120))
		return diag
	}

	// 2. Its own memory - has it genuinely solved this before?
	if diag, ok := h.diagnoseFromMemory(ctx, failure); ok {
		h.logf("coding-diagnosis: recalled a fix from memory: %s", diag.Cause)
		return diag
	}

	// 3. Real research. Only reached for something genuinely unrecognised,
	// which is exactly when a search is worth its cost.
	if h.AllowWebSearch {
		if diag, ok := h.diagnoseFromWeb(ctx, failure, d, env, tc); ok {
			h.logf("coding-diagnosis: researched this failure on the web: %s", diag.Cause)
			return diag
		}
	}

	// 4. Fall back to plain reasoning over the real error text.
	return h.diagnoseFromModel(ctx, failure)
}

func matchKnownFix(failure string, d *Design, env *EnvFacts, tc *Toolchain) (Diagnosis, bool) {
	for _, kf := range knownFixes {
		m := kf.match.FindStringSubmatch(failure)
		if m == nil {
			continue
		}
		diag := Diagnosis{Kind: kf.kind, Cause: kf.cause, CodeHint: kf.hint, Source: "known:" + kf.name}
		if kf.fix != nil {
			diag.FixCommands = kf.fix(m, d, env, tc)
		}
		// A fix entry that produced no real command is not an environment
		// fix in practice - downgrade it honestly instead of returning an
		// "environment" diagnosis the loop can't act on, which would burn
		// attempts running nothing.
		if diag.Kind == DiagEnvironment && len(diag.FixCommands) == 0 {
			diag.Kind = DiagCode
			if diag.CodeHint == "" {
				diag.CodeHint = "This looks like a missing dependency, but there is no safe automatic install for it. Rewrite the code to avoid needing it - prefer the standard library."
			}
		}
		return diag, true
	}
	return Diagnosis{}, false
}

// fixScope is the project a proven fix stands in: the build's own folder ("" before the build has one, and then no fix
// is kept or read back). Not the workspace: every project of the desktop lives in that one (round-5 recheck: a fix
// approved in one project stood in every other).
func (h *Harness) fixScope() string {
	return h.project
}

// provenFix is the proven fix recorded for the failure signature sig in this project, as a proposal to ask about
// again, with when the person approved it. None when NoProvenFixes.
func (h *Harness) provenFix(ctx context.Context, sig string) (Diagnosis, bool) {
	if h.NoProvenFixes || h.L1 == nil || sig == "" {
		return Diagnosis{}, false
	}
	command, at, err := h.L1.ProvenFixAt(ctx, sig, h.fixScope())
	if err != nil || command == "" {
		return Diagnosis{}, false
	}
	return Diagnosis{
		Kind: DiagEnvironment,
		Cause: "the same failure as before; you approved this command for it on " + at.UTC().Format("2006-01-02") +
			", and the check passed after it",
		FixCommands: []string{command},
		Source:      "memory:approved",
		Signature:   sig,
		proven:      true,
	}, true
}

// diagnoseFromMemory asks L1 whether this agent already knows why this
// kind of failure happens. Facts get in there via Learn() below, after a
// real web-researched fix genuinely worked - so a hit here is something it
// actually figured out once, not something it was born knowing.
func (h *Harness) diagnoseFromMemory(ctx context.Context, failure string) (Diagnosis, bool) {
	if h.L1 == nil {
		return Diagnosis{}, false
	}
	query := "build failure fix: " + errorSignature(failure)
	entries, err := h.L1.Recall(ctx, query, 3)
	if err != nil || len(entries) == 0 {
		return Diagnosis{}, false
	}

	var relevant []string
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Fact), "build failure fix") {
			relevant = append(relevant, e.Fact)
		}
	}
	if len(relevant) == 0 {
		return Diagnosis{}, false
	}

	return Diagnosis{
		Kind:     DiagCode,
		Cause:    "Previously learned: " + relevant[0],
		CodeHint: strings.Join(relevant, "\n"),
		Source:   "memory",
	}, true
}

const webDiagnosisPrompt = `A real build/test command failed. Real web search results about this exact error are below. Use them to decide the real fix.

THE FAILURE:
%s

REAL SEARCH RESULTS (fetched just now):
%s

Decide whether this is an ENVIRONMENT problem (something missing or misconfigured on the machine - fixable by running a command) or a CODE problem (the generated code itself is wrong - fixable only by rewriting it).

Respond in EXACTLY this format, nothing else:
KIND: ENVIRONMENT or CODE
CAUSE: <one or two sentences saying what is actually wrong>
FIX_COMMAND: <a single real shell command that fixes it, or NONE if this is a CODE problem>
CODE_FIX: <what specifically to change in the code, or NONE if this is an ENVIRONMENT problem>`

// diagnoseFromWeb does real research on a real error: search, deep-read
// the best hits, and reason over what they actually say.
func (h *Harness) diagnoseFromWeb(ctx context.Context, failure string, d *Design, env *EnvFacts, tc *Toolchain) (Diagnosis, bool) {
	query := errorSignature(failure)
	if query == "" {
		return Diagnosis{}, false
	}
	h.logf("coding-diagnosis: this failure isn't recognised - searching the web for: %s", query)

	results, err := websearch.Search(ctx, query, h.OnLog)
	if err != nil || len(results) == 0 {
		h.logf("coding-diagnosis: web search found nothing usable (%v)", err)
		return Diagnosis{}, false
	}

	var b strings.Builder
	for i, r := range results {
		if i >= 4 {
			break
		}
		fmt.Fprintf(&b, "- %s (%s)\n  %s\n", r.Title, r.URL, r.Snippet)
	}
	// One real deep read of the top hit: snippets alone routinely stop
	// right before the actual answer.
	if text, ferr := websearch.FetchPageText(ctx, results[0].URL, 4000); ferr == nil && strings.TrimSpace(text) != "" {
		fmt.Fprintf(&b, "\nFull text of the top result (%s):\n%s\n", results[0].URL, text)
	}

	resp, err := h.Provider.Chat(ctx, h.Model,
		[]llm.Message{{Role: "user", Content: fmt.Sprintf(webDiagnosisPrompt, tailOf(failure, 2500), b.String())}},
		llm.Options{TimeoutMs: 120000, Temperature: 0.1})
	if err != nil {
		return Diagnosis{}, false
	}

	diag := parseDiagnosisResponse(resp.Text)
	if diag.Cause == "" {
		return Diagnosis{}, false
	}
	diag.Source = "web"
	diag.Learned = true
	return diag, true
}

const modelDiagnosisPrompt = `A real build/test command failed. Work out what actually went wrong from the real output.

THE FAILURE:
%s

Respond in EXACTLY this format, nothing else:
KIND: ENVIRONMENT or CODE
CAUSE: <one or two sentences saying what is actually wrong>
FIX_COMMAND: <a single real shell command that fixes it, or NONE if this is a CODE problem>
CODE_FIX: <what specifically to change in the code, or NONE if this is an ENVIRONMENT problem>`

func (h *Harness) diagnoseFromModel(ctx context.Context, failure string) Diagnosis {
	resp, err := h.Provider.Chat(ctx, h.Model,
		[]llm.Message{{Role: "user", Content: fmt.Sprintf(modelDiagnosisPrompt, tailOf(failure, 3000))}},
		llm.Options{TimeoutMs: 90000, Temperature: 0.1})
	if err != nil {
		return Diagnosis{Kind: DiagCode, Cause: "could not diagnose the failure (" + err.Error() + ")", Source: "none"}
	}
	diag := parseDiagnosisResponse(resp.Text)
	diag.Source = "model"
	if diag.Cause == "" {
		diag.Cause = "unrecognised failure"
		diag.Kind = DiagCode
	}
	return diag
}

var (
	kindRe    = regexp.MustCompile(`(?im)^KIND:\s*(.+)$`)
	causeRe   = regexp.MustCompile(`(?im)^CAUSE:\s*(.+)$`)
	fixCmdRe  = regexp.MustCompile(`(?im)^FIX_COMMAND:\s*(.+)$`)
	codeFixRe = regexp.MustCompile(`(?im)^CODE_FIX:\s*(.+)$`)
)

func parseDiagnosisResponse(text string) Diagnosis {
	var d Diagnosis
	if m := causeRe.FindStringSubmatch(text); m != nil {
		d.Cause = strings.TrimSpace(m[1])
	}
	if m := kindRe.FindStringSubmatch(text); m != nil {
		if strings.Contains(strings.ToUpper(m[1]), "ENVIRONMENT") {
			d.Kind = DiagEnvironment
		} else {
			d.Kind = DiagCode
		}
	}
	if m := fixCmdRe.FindStringSubmatch(text); m != nil {
		cmd := strings.TrimSpace(strings.Trim(m[1], "`"))
		if !isNone(cmd) && looksRunnable(cmd) {
			d.FixCommands = []string{cmd}
		}
	}
	if m := codeFixRe.FindStringSubmatch(text); m != nil {
		if hint := strings.TrimSpace(m[1]); !isNone(hint) {
			d.CodeHint = hint
		}
	}
	// An "environment" verdict with no command to run can't be acted on -
	// treat it as a code problem so the loop does something real instead of
	// burning an attempt on nothing.
	if d.Kind == DiagEnvironment && len(d.FixCommands) == 0 {
		d.Kind = DiagCode
	}
	return d
}

func isNone(s string) bool {
	t := strings.ToUpper(strings.TrimSpace(strings.Trim(s, "`\"'.")))
	return t == "" || t == "NONE" || t == "N/A" || t == "NULL"
}

// looksRunnable is a safety gate on a command the MODEL invented (table
// fixes are trusted - they're written here in real code). It blocks the
// genuinely destructive shapes outright and requires the rest to at least
// look like a command rather than a sentence.
func looksRunnable(cmd string) bool {
	lower := strings.ToLower(cmd)
	for _, banned := range []string{
		"rm -rf /", "rm -rf ~", "rm -rf *", ":(){", "mkfs", "dd if=",
		"shutdown", "reboot", "> /dev/sda", "chmod -r 777 /", "curl", "wget",
	} {
		if strings.Contains(lower, banned) {
			// curl/wget are blocked here specifically because a model-
			// invented download-and-run is the one shape that turns a
			// build failure into arbitrary remote code execution. The
			// known-fix table above does use curl for uv's official
			// installer - that URL is written in this source file, reviewed,
			// and not something a failure message can influence.
			return false
		}
	}
	fields := strings.Fields(cmd)
	return len(fields) >= 2 && !strings.HasSuffix(cmd, ".") && len(cmd) < 300
}

// mayRun reports whether diag's fix commands may run now. A command from
// this file's table may; a proposal (a web page's or a model's) runs only
// when the person approved that exact command. An unapproved one is kept as
// the build's pending question - the build stops there and asks - unless the
// person already declined it, in which case it is simply not run.
func (h *Harness) mayRun(diag Diagnosis) bool {
	if !diag.IsProposal() {
		return true
	}
	action := diag.Action()
	for _, d := range h.Declined {
		if d == action {
			h.logf("coding: not running `%s` - you did not approve it", truncate(action, 120))
			return false
		}
	}
	if h.Approve != nil && h.Approve(action) {
		h.logf("coding: running the fix you approved: %s", truncate(action, 120))
		h.approvedAction = action
		return true
	}
	if h.pending == nil {
		p := diag
		h.pending = &p
		h.logf("coding: a %s-proposed fix needs your approval before it runs: %s", diag.Source, truncate(action, 120))
	}
	return false
}

// awaitCheck notes a researched fix that ran: it is remembered only once the
// check that failed passes after it (settleLearning).
func (h *Harness) awaitCheck(diag Diagnosis) {
	if diag.Learned || (diag.IsProposal() && diag.Action() == h.approvedAction) {
		d := diag
		h.unconfirmed = &d
	}
}

// settleLearning ends the wait for a researched fix's check: passed, the fix
// is remembered (L1) and reported as learned; failed or never re-run, it is
// forgotten. No-op when no fix waits.
func (h *Harness) settleLearning(ctx context.Context, passed bool) {
	diag := h.unconfirmed
	h.unconfirmed = nil
	if diag == nil {
		return
	}
	approved := diag.Action() == h.approvedAction
	h.approvedAction = ""
	if !passed {
		h.logf("coding: not remembering the fix `%s` - the check it was for did not pass after it", truncate(diag.Action(), 120))
		return
	}
	// The exact command the person approved, which then worked, is kept so a later question about the same failure
	// can show their earlier yes (provenFix) - never so it runs without asking. Not kept when NoProvenFixes.
	if approved && !h.NoProvenFixes && h.L1 != nil && diag.Signature != "" {
		if err := h.L1.RecordProvenFix(ctx, diag.Signature, h.fixScope(), diag.Action()); err != nil {
			h.logf("coding: could not keep the fix you approved for next time: %s", err.Error())
		}
	}
	if !diag.Learned {
		return
	}
	h.learnedFixes = append(h.learnedFixes, diag.Cause+" → "+fixSummary(*diag))
	h.Learn(ctx, diag.Signature, *diag)
}

// Learn records a genuinely new fix in long-term memory, so the same
// unrecognised failure costs a web search exactly once. Only called after
// the fix ran with the person's approval and the check it was for then
// passed (settleLearning) - remembering a guess that didn't work, or a
// command nobody approved, would be worse than remembering nothing.
func (h *Harness) Learn(ctx context.Context, failure string, diag Diagnosis) {
	if h.L1 == nil || !diag.Learned {
		return
	}
	sig := diag.Signature
	if sig == "" {
		sig = errorSignature(failure)
	}
	fact := fmt.Sprintf("build failure fix: when a build fails with %q, the cause is: %s. The fix that actually worked: %s",
		sig, diag.Cause, fixSummary(diag))
	if err := h.L1.Store(ctx, fact, "coding-harness"); err != nil {
		h.logf("coding-diagnosis: could not store what was learned: %s", err.Error())
		return
	}
	h.logf("coding-learned: remembered a new fix for next time - %s", oneLine(diag.Cause))
}

func fixSummary(d Diagnosis) string {
	if len(d.FixCommands) > 0 {
		return strings.Join(d.FixCommands, " && ")
	}
	if d.CodeHint != "" {
		return d.CodeHint
	}
	return "(no concrete fix recorded)"
}

// ---------------------------------------------------------------- helpers

var (
	// Noise that makes an error signature worthless as a search query -
	// absolute paths, line numbers, temp dirs, memory addresses. Two runs
	// of the same failure produce different text without this.
	pathNoiseRe = regexp.MustCompile(`(/[\w./+-]+)+`)
	numNoiseRe  = regexp.MustCompile(`\b\d{2,}\b`)
	hexNoiseRe  = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	// The line that actually names the error, in the shapes real toolchains
	// print it. Searching for THIS beats searching for a 200-line dump.
	errLineRe = regexp.MustCompile(`(?m)^.*?\b(\w*(?:Error|Exception|error|fatal error|panic|FAILED|undefined|not found|cannot|No such)\b.*)$`)
)

// errorSignature reduces a wall of build output to the one line that
// actually names the failure, stripped of run-specific noise - usable both
// as a real search query and as a stable key for remembering a fix.
func errorSignature(failure string) string {
	// Search the tail: the real error is at the end of build output, and
	// the head is progress noise that often contains the word "error" in
	// an unrelated context (a filename, a package called "error-handling").
	tail := tailOf(failure, 4000)

	line := ""
	if matches := errLineRe.FindAllStringSubmatch(tail, -1); len(matches) > 0 {
		line = strings.TrimSpace(matches[len(matches)-1][1])
	}
	if line == "" {
		lines := strings.Split(strings.TrimSpace(tail), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if s := strings.TrimSpace(lines[i]); s != "" {
				line = s
				break
			}
		}
	}

	line = hexNoiseRe.ReplaceAllString(line, "")
	line = pathNoiseRe.ReplaceAllStringFunc(line, func(p string) string {
		// Keep the basename - "No module named x" vs "test_foo.py failed"
		// are genuinely different failures and the filename is the signal.
		parts := strings.Split(strings.Trim(p, "/"), "/")
		return parts[len(parts)-1]
	})
	line = numNoiseRe.ReplaceAllString(line, "")
	line = strings.Join(strings.Fields(line), " ")

	if len(line) > 180 {
		line = line[:180]
	}
	return strings.TrimSpace(line)
}

// firstMeaningfulLine picks the one line of build output worth putting in
// a log line: the line that actually names the error, keeping its real
// path and line number (unlike errorSignature, which strips exactly those
// so two runs of the same failure hash the same).
func firstMeaningfulLine(failure string) string {
	tail := tailOf(failure, 3000)
	if matches := errLineRe.FindAllStringSubmatch(tail, -1); len(matches) > 0 {
		return strings.TrimSpace(matches[len(matches)-1][1])
	}
	lines := strings.Split(strings.TrimSpace(tail), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return strings.TrimSpace(failure)
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "[...truncated...]\n" + s[len(s)-n:]
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// isProjectInternalModule reports whether a "missing module" is really one
// of this project's own files, rather than a third-party dependency. A
// relative import always is; so is anything whose path matches a file the
// design itself declares.
func isProjectInternalModule(mod string, d *Design) bool {
	if strings.HasPrefix(mod, ".") {
		return true
	}
	if d == nil {
		return false
	}
	// "pkg.sub.mod" -> "pkg/sub/mod", the shape it would have on disk.
	asPath := strings.ReplaceAll(mod, ".", "/")
	top := topLevelModule(mod)

	for _, f := range d.Files {
		withoutExt := strings.TrimSuffix(f.Path, path.Ext(f.Path))
		if withoutExt == asPath || strings.HasSuffix(withoutExt, "/"+asPath) {
			return true
		}
		// The project's own top-level package directory counts too - a
		// build's package dir is named after the project, and that name is
		// exactly what would get installed from a registry by mistake.
		if first, _, ok := strings.Cut(f.Path, "/"); ok && first == top {
			return true
		}
	}
	return false
}

func topLevelModule(mod string) string {
	if i := strings.Index(mod, "."); i > 0 {
		return mod[:i]
	}
	return mod
}

// pypiPackageFor maps an import name to the package that actually provides
// it. These genuinely differ often enough that installing the import name
// verbatim is a real, common failure: `pip install cv2` does not exist,
// `pip install PIL` installs an abandoned Python-2 package, and
// `pip install sklearn` installs a stub whose whole purpose is to tell you
// to install scikit-learn instead.
var pypiPackages = map[string]string{
	"cv2":               "opencv-python",
	"PIL":               "pillow",
	"sklearn":           "scikit-learn",
	"yaml":              "pyyaml",
	"bs4":               "beautifulsoup4",
	"dotenv":            "python-dotenv",
	"serial":            "pyserial",
	"dateutil":          "python-dateutil",
	"attr":              "attrs",
	"OpenSSL":           "pyopenssl",
	"jwt":               "pyjwt",
	"psycopg2":          "psycopg2-binary",
	"MySQLdb":           "mysqlclient",
	"win32com":          "pywin32",
	"pkg_resources":     "setuptools",
	"google":            "google-api-python-client",
	"docx":              "python-docx",
	"pptx":              "python-pptx",
	"fitz":              "pymupdf",
	"magic":             "python-magic",
	"redis":             "redis",
	"zoneinfo":          "backports.zoneinfo",
	"Crypto":            "pycryptodome",
	"grpc":              "grpcio",
	"pandas_datareader": "pandas-datareader",
}

func pypiPackageFor(mod string) string {
	if pkg, ok := pypiPackages[mod]; ok {
		return pkg
	}
	return mod
}
