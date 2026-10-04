//go:build windows

package hostshell

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTools puts a decurix_agent package on PYTHONPATH whose gui and office
// print what they were given: their arguments, and what was piped to them.
func fakeTools(t *testing.T) []string {
	t.Helper()

	python, err := exec.LookPath("python.exe")
	if err != nil {
		t.Skip("no python.exe on this machine to stand in for the worker's")
	}

	root := t.TempDir()
	program := `import json, os, sys
passed = os.environ.get("DECURIX_CLI_ARGS")
args = passed.split("\x1f") if passed else []
print(json.dumps({"args": args, "stdin": sys.stdin.read()}))
sys.exit(3 if args[:1] == ["refuse"] else 0)
`
	for _, module := range []string{"computer", filepath.Join("tools", "office")} {
		dir := filepath.Join(root, "decurix_agent", module)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "__main__.py"), []byte(program), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return append(os.Environ(), "DECURIX_PYTHON="+python, "PYTHONPATH="+root, "PYTHONUTF8=1")
}

type toolCall struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

func runTool(t *testing.T, env []string, line string) (toolCall, int) {
	t.Helper()

	cmd := Command(line)
	cmd.Env = env
	out, err := cmd.Output()

	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", line, err)
	}

	var call toolCall
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &call); err != nil {
		t.Fatalf("%s printed %q: %v", line, out, err)
	}

	return call, code
}

// The reason gui and office are functions: an argument with double quotes in
// it must arrive whole, which PowerShell 5.1 does not manage for a program.
func TestTheWorkersCommandsReceiveTheirArgumentsWhole(t *testing.T) {
	env := fakeTools(t)

	for line, want := range map[string][]string{
		`gui click 'button "Save"'`:                 {"click", `button "Save"`},
		`gui type e12 "Acme & Sons, 50% off"`:       {"type", "e12", "Acme & Sons, 50% off"},
		`gui type e3 'it''s here'`:                  {"type", "e3", "it's here"},
		`gui do add-a-customer --param name="Acme"`: {"do", "add-a-customer", "--param", "name=Acme"},
		`gui observe`: {"observe"},
		`gui`:         {},
	} {
		call, code := runTool(t, env, line)
		if strings.Join(call.Args, "|") != strings.Join(want, "|") || code != 0 {
			t.Errorf("%s -> %q (exit %d), want %q", line, call.Args, code, want)
		}
	}
}

func TestWhatIsPipedToAWorkersCommandReachesIt(t *testing.T) {
	env := fakeTools(t)

	call, _ := runTool(t, env, `'{"edits": [{"cell": "B2", "value": 5}]}' | office update book.xlsx -`)
	if strings.Join(call.Args, "|") != "update|book.xlsx|-" || !strings.Contains(call.Stdin, `"cell": "B2"`) {
		t.Errorf("office got args %q and stdin %q", call.Args, call.Stdin)
	}
	// Exactly what was written, from its first character: a byte-order mark
	// in front of it (PowerShell's own UTF-8 encoding adds one) made the
	// JSON invalid, live.
	if !strings.HasPrefix(call.Stdin, "{") {
		t.Errorf("what was piped starts with %q, want the JSON's own first character", []rune(call.Stdin)[:1])
	}
	// And beyond ASCII it is still what was written.
	call, _ = runTool(t, env, `'{"name": "Zoë – café ₹500"}' | office create book.xlsx -`)
	if !strings.Contains(call.Stdin, "Zoë – café ₹500") {
		t.Errorf("non-ASCII text arrived as %q", call.Stdin)
	}

	// A here-string is how a body of several lines is written in PowerShell.
	call, _ = runTool(t, env, "@'\nDear Ana,\n\nThe invoice is attached.\n'@ | office email --to ana@example.com --subject \"Invoice\"")
	if !strings.Contains(call.Stdin, "Dear Ana,") || !strings.Contains(call.Stdin, "The invoice is attached.") {
		t.Errorf("the email body arrived as %q", call.Stdin)
	}
}

// gui's exit codes are a contract the operator reads: 3 is "needs approval".
func TestAWorkersCommandsExitCodeIsTheCommands(t *testing.T) {
	env := fakeTools(t)

	if _, code := runTool(t, env, `gui refuse`); code != 3 {
		t.Errorf("exit code %d, want gui's own 3", code)
	}
}

// python and python3 are the worker's own Python, not Windows' link to the
// Microsoft Store ("Python was not found", exit 9009).
func TestPythonIsTheWorkersOwn(t *testing.T) {
	env := fakeTools(t)
	want := ""
	for _, e := range env {
		if strings.HasPrefix(e, "DECURIX_PYTHON=") {
			want = strings.TrimPrefix(e, "DECURIX_PYTHON=")
		}
	}

	for _, name := range []string{"python", "python3"} {
		cmd := Command(name + ` -c 'import sys; print(sys.executable)'`)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(out)), want) {
			t.Errorf("%s ran %q (%v), want %s", name, strings.TrimSpace(string(out)), err, want)
		}
	}
}

func TestACommandsOwnExitCodeAndFailureAreReported(t *testing.T) {
	for line, want := range map[string]int{
		`$env:COMPUTERNAME`:                   0,
		`cmd /c exit 7`:                       7,
		`Get-Item C:\no\such\file-decurix`:    1,
		`Write-Output hello # a comment last`: 0,
	} {
		err := Command(line).Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		if code != want {
			t.Errorf("%s exited %d, want %d", line, code, want)
		}
	}
}
