//go:build windows

package hostshell

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Name is what the shell is called, for messages and prompts.
const Name = "PowerShell"

// Windows reports whether commands are PowerShell rather than bash.
const Windows = true

// before makes PowerShell write UTF-8, not the console's code page, keeps its
// progress bars out of the output a model reads, and gives the command the
// worker's own two commands, gui and office (the agent's computer-use layer
// and document tools; /usr/local/bin/gui and office on a Linux desktop).
//
// They are functions, not programs on the PATH, because of what Windows
// PowerShell 5.1 does to an argument with double quotes in it on its way to a
// program: `gui click 'button "Save"'` arrived as `button Save`. A function
// receives its arguments as PowerShell parsed them, and hands them on in
// DECURIX_CLI_ARGS, separated by a character no argument contains
// (decurix_agent/cliargs.py). Text piped to one - a JSON spec, an email's
// body - is piped on to the program.
//
// The worker's own Python goes first on the PATH, and python3 names it too.
// Windows ships a `python`/`python3` that is only a link to the Microsoft
// Store: run without this, `python3 -c "..."` printed "Python was not found;
// run without arguments to install from the Microsoft Store" (exit 9009), and
// python is what every prompt tells a model it can always fall back on.
//
// What is piped to a program must not start with a byte-order mark. Found
// live: `'{"sheets": ...}' | office create book.xlsx -` was refused as "not
// valid JSON" - the first character of the spec was the mark. Measured in
// PowerShell 5.1: setting $OutputEncoding to UTF-8 without a mark is not
// enough (the mark was still sent); it goes only when the console's *input*
// encoding is also UTF-8 without one, which is what the piped program's stdin
// is opened with.
const before = "$ProgressPreference='SilentlyContinue'; $decurixUtf8=New-Object System.Text.UTF8Encoding($false); " +
	"[Console]::InputEncoding=$decurixUtf8; [Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=$decurixUtf8\n" +
	"if ($env:DECURIX_PYTHON) { $env:PATH = (Split-Path -Parent $env:DECURIX_PYTHON) + ';' + $env:PATH; Set-Alias python3 python.exe }\n" +
	"function Invoke-DecurixTool($module, $words, $piped) { $env:DECURIX_CLI_ARGS = [string]::Join([string][char]31, [string[]]$words); " +
	"try { if ($piped) { $piped | & $env:DECURIX_PYTHON -m $module } else { & $env:DECURIX_PYTHON -m $module } } finally { $env:DECURIX_CLI_ARGS = $null } }\n" +
	"function gui { Invoke-DecurixTool 'decurix_agent.computer' $args @($input) }\n" +
	"function office { Invoke-DecurixTool 'decurix_agent.tools.office' $args @($input) }\n"

// after gives the command an exit code a caller can read the way it reads
// bash's: a native program's own code, 1 for a cmdlet that failed, else 0.
// On lines of their own, so a command ending in a comment does not swallow it.
const after = "\n$decurixOk = $?\nif ($LASTEXITCODE) { exit $LASTEXITCODE } elseif (-not $decurixOk) { exit 1 }"

func argv(line string) (string, []string) {
	return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", before + line + after}
}

const createNewProcessGroup = 0x00000200

// prepare keeps the shell's console window off the worker's screen: soul has
// no console of its own, so every console program it starts would open one.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

// OwnGroup keeps cmd out of this process's console group. Call before Start.
func OwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup}
}

// KillTree ends cmd and every process it started. Windows has no process
// group to signal, so taskkill walks the tree.
func KillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if kill.Run() != nil {
		_ = cmd.Process.Kill()
	}
}
