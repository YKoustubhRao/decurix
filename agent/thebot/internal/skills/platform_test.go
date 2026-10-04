package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"thebot/internal/hostshell"
)

// forShell picks the command a test sends for the shell this build's workers
// run commands in: bash on a Linux desktop, PowerShell on a native Windows
// worker. The tests below it are about the loop around the command, so each
// is written once and names the same job in both shells.
func forShell(bash, powershell string) string {
	if hostshell.Windows {
		return powershell
	}
	return bash
}

// stubProgram puts a stand-in for a program first on PATH: a shell script on
// Linux, a .cmd on Windows.
func stubProgram(t *testing.T, name, shBody, cmdBody string) {
	t.Helper()
	dir := t.TempDir()
	file, body := name, "#!/bin/sh\n"+shBody
	if hostshell.Windows {
		file, body = name+".cmd", "@echo off\r\n"+cmdBody
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubWorkerTools stands in for the worker's own gui and office commands.
//
// On Linux they are programs on the PATH, so each is a script (shGui,
// shOffice). On Windows they are PowerShell functions that run the worker's
// Python with `-m decurix_agent.computer` or `-m decurix_agent.tools.office`
// and the arguments in DECURIX_CLI_ARGS (hostshell), so the stand-in is that
// Python: one .cmd, whose second argument says which of the two it is.
func stubWorkerTools(t *testing.T, shGui, shOffice, cmdPython string) {
	t.Helper()
	dir := t.TempDir()
	if hostshell.Windows {
		python := filepath.Join(dir, "python.cmd")
		if err := os.WriteFile(python, []byte("@echo off\r\n"+cmdPython), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DECURIX_PYTHON", python)
		return
	}
	for name, body := range map[string]string{"gui": shGui, "office": shOffice} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// workerPython makes python3 mean a real Python in the commands a test runs.
// On Linux it already does. On Windows the worker's own is named by
// DECURIX_PYTHON (python3 is otherwise Windows' link to the Microsoft Store),
// so the test borrows whichever real python.exe this machine has.
func workerPython(t *testing.T) {
	t.Helper()
	if !hostshell.Windows {
		return
	}
	python, err := exec.LookPath("python.exe")
	if err == nil {
		// The Store link is a python.exe too; a real one runs code.
		err = exec.Command(python, "-c", "pass").Run()
	}
	if err != nil {
		t.Skip("no python.exe on this machine to stand in for the worker's")
	}
	t.Setenv("DECURIX_PYTHON", python)
}
