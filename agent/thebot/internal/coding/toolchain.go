package coding

import (
	"fmt"
	"path"
	"strings"
)

// Toolchain is everything language-specific in one place: how to stand a
// real project up, how to install real dependencies, how to check one file
// compiles, how to run one file's tests, and how to run everything.
//
// It exists so the build loop in build.go can be written ONCE, about
// projects in general, instead of being a pile of per-language branches.
// Adding a language is adding a Toolchain, not editing the loop.
//
// Every command returned here is a real command that gets run and checked.
// None of it is advisory.
type Toolchain struct {
	Language string

	// Ext is the source-file extension, used to sanity-check that the
	// architect's file list actually belongs to this language.
	Ext string

	// ScaffoldFiles are the project files written before any source: the
	// manifest (pyproject.toml, go.mod, package.json, CMakeLists.txt) that
	// makes the directory a real project its tooling recognises.
	ScaffoldFiles func(d *Design) []ScaffoldFile

	// SetupCommands stand the environment up once, after the scaffold
	// files exist - create the venv, init the module, install the declared
	// dependencies. Run in order, stopping at the first real failure.
	SetupCommands func(d *Design, env *EnvFacts) []string

	// InstallCmd installs additional packages discovered mid-build (a
	// missing import the architect didn't declare). One command, or "" if
	// the language has no package step.
	InstallCmd func(d *Design, env *EnvFacts, pkgs []string) string

	// SyntaxCheck is the cheapest real "does this file even parse"
	// check - run immediately after writing a file, before the far more
	// expensive test run. "" means the language has no such check.
	SyntaxCheck func(d *Design, env *EnvFacts, relPath string) string

	// UnitTestCmd runs ONE test file, so a per-file loop gets a tight
	// signal about the file it just wrote instead of the whole suite.
	UnitTestCmd func(d *Design, env *EnvFacts, testPath string) string

	// FullTestCmd runs the entire suite - the real end-to-end gate.
	FullTestCmd func(d *Design, env *EnvFacts) string

	// BuildCmd compiles/builds the whole project where that's a real step
	// ("" for interpreted languages with nothing to build).
	BuildCmd func(d *Design, env *EnvFacts) string

	// TestPathFor maps a source path to its conventional test path, so the
	// harness names test files the way each ecosystem actually expects
	// (tests/test_foo.py, foo_test.go, foo.test.js).
	TestPathFor func(srcPath string) string

	// TestFrameworkNote is injected into code-generation prompts so the
	// model writes tests in the framework that is actually installed and
	// actually going to be run, instead of whichever it likes best.
	TestFrameworkNote string
}

// ScaffoldFile is one real project file written during setup.
type ScaffoldFile struct {
	Path    string
	Content string
	// OnlyIfMissing protects a file the model may legitimately want to own
	// (a package's __init__.py) from being clobbered by the scaffold.
	OnlyIfMissing bool
}

// EnvFacts is what was actually discovered about the machine this build is
// running on - real answers from `command -v`, not assumptions. The
// difference matters: "use uv" is a good default and a wrong one on a box
// that doesn't have it, and the failure mode of assuming is a confusing
// "uv: command not found" three steps into a build.
type EnvFacts struct {
	HasUV      bool
	HasPython  bool
	PythonBin  string // python3 or python
	HasGo      bool
	HasNode    bool
	HasNPM     bool
	HasCMake   bool
	HasGXX     bool
	HasCargo   bool
	HasDocker  bool
	VenvPython string // .venv/bin/python once the venv really exists
}

// pyRun is how every Python command in this package addresses the
// interpreter: by real path into the project's own venv, never by
// activating anything. `source .venv/bin/activate && pip install x` is the
// classic way this breaks - activation is shell state, each command here
// is its own process, so the activate silently does nothing and the
// install lands in the system Python (or fails against an
// externally-managed environment). Calling .venv/bin/python directly is
// immune to that by construction.
func pyRun(env *EnvFacts) string {
	if env.VenvPython != "" {
		return env.VenvPython
	}
	if env.PythonBin != "" {
		return env.PythonBin
	}
	return "python3"
}

// Toolchains is the real registry. Language names here are exactly what
// the architect prompt is told to choose from.
var Toolchains = map[string]*Toolchain{
	"python": pythonToolchain(),
	"go":     goToolchain(),
	"node":   nodeToolchain(),
	"cpp":    cppToolchain(),
	"rust":   rustToolchain(),
}

// For returns the toolchain for a language, falling back to Python (by far
// the most common target, and the one whose tooling is most likely to be
// present) with a clear signal that the fallback happened.
func For(language string) (*Toolchain, bool) {
	tc, ok := Toolchains[strings.ToLower(strings.TrimSpace(language))]
	if ok {
		return tc, true
	}
	return Toolchains["python"], false
}

// ---------------------------------------------------------------- Python

func pythonToolchain() *Toolchain {
	return &Toolchain{
		Language:          "python",
		Ext:               ".py",
		TestFrameworkNote: "Use pytest. Plain `def test_*()` functions with bare `assert` statements - no unittest.TestCase classes.",

		ScaffoldFiles: func(d *Design) []ScaffoldFile {
			deps := append([]string{"pytest"}, d.Dependencies...)
			var quoted []string
			for _, dep := range deps {
				quoted = append(quoted, fmt.Sprintf("%q", dep))
			}
			return []ScaffoldFile{
				{
					Path: "pyproject.toml",
					Content: fmt.Sprintf(`[project]
name = %q
version = "0.1.0"
description = %q
requires-python = ">=3.9"
dependencies = [%s]

[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[tool.pytest.ini_options]
testpaths = ["tests"]
pythonpath = ["."]
`, pyProjectName(d.ProjectName), oneLine(d.Summary), strings.Join(quoted, ", ")),
				},
				{Path: "tests/__init__.py", Content: "", OnlyIfMissing: true},
			}
		},

		SetupCommands: func(d *Design, env *EnvFacts) []string {
			// uv when it's really there (an order of magnitude faster, and
			// it sidesteps externally-managed-environment entirely by
			// always working inside a venv); otherwise the stdlib venv,
			// which every Python 3 install has. Both end at the same place:
			// a real .venv the rest of the build addresses by path.
			if env.HasUV {
				return []string{
					"uv venv --python " + env.PythonBin + " .venv || uv venv .venv",
					".venv/bin/python -m ensurepip --upgrade 2>/dev/null || true",
					"uv pip install --python .venv/bin/python pytest",
					depInstall("uv pip install --python .venv/bin/python", d.Dependencies),
				}
			}
			return []string{
				env.PythonBin + " -m venv .venv",
				".venv/bin/python -m pip install --upgrade pip",
				".venv/bin/python -m pip install pytest",
				depInstall(".venv/bin/python -m pip install", d.Dependencies),
			}
		},

		InstallCmd: func(d *Design, env *EnvFacts, pkgs []string) string {
			if len(pkgs) == 0 {
				return ""
			}
			if env.HasUV {
				return "uv pip install --python .venv/bin/python " + strings.Join(pkgs, " ")
			}
			return ".venv/bin/python -m pip install " + strings.Join(pkgs, " ")
		},

		SyntaxCheck: func(d *Design, env *EnvFacts, relPath string) string {
			return fmt.Sprintf("%s -m py_compile %q", pyRun(env), relPath)
		},

		UnitTestCmd: func(d *Design, env *EnvFacts, testPath string) string {
			return fmt.Sprintf("%s -m pytest %q -q 2>&1", pyRun(env), testPath)
		},

		FullTestCmd: func(d *Design, env *EnvFacts) string {
			return fmt.Sprintf("%s -m pytest -q 2>&1", pyRun(env))
		},

		BuildCmd: func(d *Design, env *EnvFacts) string { return "" },

		TestPathFor: func(srcPath string) string {
			base := strings.TrimSuffix(path.Base(srcPath), ".py")
			return path.Join("tests", "test_"+base+".py")
		},
	}
}

func pyProjectName(name string) string {
	n := strings.ToLower(name)
	n = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return '-'
		default:
			return '-'
		}
	}, n)
	n = strings.Trim(n, "-")
	if n == "" {
		return "project"
	}
	return n
}

// -------------------------------------------------------------------- Go

func goToolchain() *Toolchain {
	return &Toolchain{
		Language:          "go",
		Ext:               ".go",
		TestFrameworkNote: "Use Go's standard `testing` package - `func TestXxx(t *testing.T)` with t.Errorf/t.Fatalf. No third-party assertion libraries.",

		ScaffoldFiles: func(d *Design) []ScaffoldFile { return nil },

		SetupCommands: func(d *Design, env *EnvFacts) []string {
			mod := goModuleName(d.ProjectName)
			cmds := []string{
				fmt.Sprintf("test -f go.mod || go mod init %s", mod),
			}
			for _, dep := range d.Dependencies {
				if dep = strings.TrimSpace(dep); dep != "" {
					cmds = append(cmds, "go get "+dep)
				}
			}
			return cmds
		},

		InstallCmd: func(d *Design, env *EnvFacts, pkgs []string) string {
			if len(pkgs) == 0 {
				return ""
			}
			return "go get " + strings.Join(pkgs, " ")
		},

		// `go build` on one file can't see its package siblings, so a
		// single-file check would fail on any real multi-file package. `go
		// vet` on the whole package is the honest per-file gate here: it
		// type-checks everything, which is what actually matters after
		// writing one file of a package.
		SyntaxCheck: func(d *Design, env *EnvFacts, relPath string) string {
			return "go vet ./... 2>&1"
		},

		UnitTestCmd: func(d *Design, env *EnvFacts, testPath string) string {
			dir := path.Dir(testPath)
			if dir == "." || dir == "" {
				dir = "."
			}
			return fmt.Sprintf("go test ./%s/... -run . -count=1 2>&1", strings.TrimPrefix(dir, "./"))
		},

		FullTestCmd: func(d *Design, env *EnvFacts) string {
			return "go test ./... -count=1 2>&1"
		},

		BuildCmd: func(d *Design, env *EnvFacts) string {
			return "go build ./... 2>&1"
		},

		TestPathFor: func(srcPath string) string {
			return strings.TrimSuffix(srcPath, ".go") + "_test.go"
		},
	}
}

func goModuleName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, " ", "-")
	if n == "" {
		n = "project"
	}
	return "example.com/" + n
}

// ------------------------------------------------------------------ Node

func nodeToolchain() *Toolchain {
	return &Toolchain{
		Language: "node",
		Ext:      ".js",
		// node:test is in the standard library from Node 18 on, so tests
		// run with zero install and zero network. A project that pulls in
		// jest/vitest can't run its own tests until an install succeeds,
		// which turns every offline or registry-flaky box into a red suite
		// for reasons that have nothing to do with the code.
		TestFrameworkNote: "Use Node's built-in test runner: `import test from 'node:test'` and `import assert from 'node:assert/strict'`. No jest, no vitest, no mocha.",

		ScaffoldFiles: func(d *Design) []ScaffoldFile {
			return []ScaffoldFile{{
				Path: "package.json",
				Content: fmt.Sprintf(`{
  "name": %q,
  "version": "0.1.0",
  "description": %q,
  "type": "module",
  "scripts": {
    "test": "node --test"
  }
}
`, pyProjectName(d.ProjectName), oneLine(d.Summary)),
			}}
		},

		SetupCommands: func(d *Design, env *EnvFacts) []string {
			if len(d.Dependencies) == 0 {
				return nil
			}
			return []string{"npm install " + strings.Join(d.Dependencies, " ")}
		},

		InstallCmd: func(d *Design, env *EnvFacts, pkgs []string) string {
			if len(pkgs) == 0 {
				return ""
			}
			return "npm install " + strings.Join(pkgs, " ")
		},

		SyntaxCheck: func(d *Design, env *EnvFacts, relPath string) string {
			return fmt.Sprintf("node --check %q", relPath)
		},

		UnitTestCmd: func(d *Design, env *EnvFacts, testPath string) string {
			return fmt.Sprintf("node --test %q 2>&1", testPath)
		},

		FullTestCmd: func(d *Design, env *EnvFacts) string {
			return "node --test 2>&1"
		},

		BuildCmd: func(d *Design, env *EnvFacts) string { return "" },

		TestPathFor: func(srcPath string) string {
			ext := path.Ext(srcPath)
			return strings.TrimSuffix(srcPath, ext) + ".test" + ext
		},
	}
}

// -------------------------------------------------------------------- C++

func cppToolchain() *Toolchain {
	return &Toolchain{
		Language: "cpp",
		Ext:      ".cpp",
		// No gtest/catch2: both need a real dependency fetch before a
		// single test can run. A test that is its own main() with <cassert>
		// compiles and runs with nothing but a compiler, which is the only
		// thing guaranteed to be present.
		TestFrameworkNote: "Each test file is a standalone program with its own `int main()` using `<cassert>` assertions, printing \"OK\" and returning 0 on success. No gtest, no catch2.",

		ScaffoldFiles: func(d *Design) []ScaffoldFile {
			return []ScaffoldFile{{
				Path: "CMakeLists.txt",
				Content: fmt.Sprintf(`cmake_minimum_required(VERSION 3.16)
project(%s CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

include_directories(${CMAKE_SOURCE_DIR})
enable_testing()

# Every tests/*.cpp is its own standalone test executable - see the
# harness's C++ toolchain note: a test is a real main() with <cassert>,
# so it needs no test framework to be fetched before it can run.
file(GLOB TEST_SOURCES ${CMAKE_SOURCE_DIR}/tests/*.cpp)
foreach(test_src ${TEST_SOURCES})
    get_filename_component(test_name ${test_src} NAME_WE)
    add_executable(${test_name} ${test_src})
    add_test(NAME ${test_name} COMMAND ${test_name})
endforeach()
`, pyProjectName(d.ProjectName)),
			}}
		},

		SetupCommands: func(d *Design, env *EnvFacts) []string {
			if !env.HasCMake {
				return nil // compile tests directly with g++ instead
			}
			return []string{"cmake -S . -B build -DCMAKE_BUILD_TYPE=Debug 2>&1"}
		},

		InstallCmd: func(d *Design, env *EnvFacts, pkgs []string) string { return "" },

		SyntaxCheck: func(d *Design, env *EnvFacts, relPath string) string {
			// -fsyntax-only parses and type-checks without emitting an
			// object file: exactly the "does this even compile" signal,
			// at a fraction of a full build.
			return fmt.Sprintf("g++ -std=c++20 -fsyntax-only -I. %q 2>&1", relPath)
		},

		UnitTestCmd: func(d *Design, env *EnvFacts, testPath string) string {
			bin := "build/" + strings.TrimSuffix(path.Base(testPath), path.Ext(testPath))
			return fmt.Sprintf("g++ -std=c++20 -I. -o %q %q 2>&1 && %q", bin, testPath, bin)
		},

		FullTestCmd: func(d *Design, env *EnvFacts) string {
			// Compile and run every test program in turn, failing loudly on
			// the first one that doesn't exit 0. Written as a real shell
			// loop rather than ctest so it works identically whether or not
			// cmake is installed.
			return `set -e; mkdir -p build; for t in tests/*.cpp; do ` +
				`b="build/$(basename "${t%.*}")"; ` +
				`echo "== $t"; g++ -std=c++20 -I. -o "$b" "$t" 2>&1; "$b" 2>&1; ` +
				`done; echo "ALL C++ TESTS PASSED"`
		},

		BuildCmd: func(d *Design, env *EnvFacts) string {
			if !env.HasCMake {
				return ""
			}
			return "cmake --build build 2>&1"
		},

		TestPathFor: func(srcPath string) string {
			base := strings.TrimSuffix(path.Base(srcPath), path.Ext(srcPath))
			return path.Join("tests", "test_"+base+".cpp")
		},
	}
}

// ------------------------------------------------------------------ Rust

func rustToolchain() *Toolchain {
	return &Toolchain{
		Language:          "rust",
		Ext:               ".rs",
		TestFrameworkNote: "Use Rust's built-in test support: `#[cfg(test)] mod tests { use super::*; #[test] fn ... }` with assert!/assert_eq!.",

		ScaffoldFiles: func(d *Design) []ScaffoldFile { return nil },

		SetupCommands: func(d *Design, env *EnvFacts) []string {
			cmds := []string{fmt.Sprintf("test -f Cargo.toml || cargo init --name %s", pyProjectName(d.ProjectName))}
			for _, dep := range d.Dependencies {
				if dep = strings.TrimSpace(dep); dep != "" {
					cmds = append(cmds, "cargo add "+dep)
				}
			}
			return cmds
		},

		InstallCmd: func(d *Design, env *EnvFacts, pkgs []string) string {
			if len(pkgs) == 0 {
				return ""
			}
			return "cargo add " + strings.Join(pkgs, " ")
		},

		SyntaxCheck: func(d *Design, env *EnvFacts, relPath string) string {
			return "cargo check 2>&1"
		},

		UnitTestCmd: func(d *Design, env *EnvFacts, testPath string) string {
			return "cargo test 2>&1"
		},

		FullTestCmd: func(d *Design, env *EnvFacts) string {
			return "cargo test 2>&1"
		},

		BuildCmd: func(d *Design, env *EnvFacts) string {
			return "cargo build 2>&1"
		},

		// Rust convention puts unit tests in the same file as the code, so
		// there is no separate test path - the builder treats an empty
		// result as "tests live inside the source file itself".
		TestPathFor: func(srcPath string) string { return "" },
	}
}

// ---------------------------------------------------------------- helpers

func depInstall(prefix string, deps []string) string {
	var real []string
	for _, d := range deps {
		if d = strings.TrimSpace(d); d != "" {
			real = append(real, d)
		}
	}
	if len(real) == 0 {
		// Still a real command so the setup sequence stays uniform - `true`
		// succeeds and costs nothing.
		return "true"
	}
	return prefix + " " + strings.Join(real, " ")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, `"`, `'`)
	return strings.TrimSpace(s)
}
