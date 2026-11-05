package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brainiac/internal/isolate"
)

// pip and venv run with no API key or token in their environment: a
// package's build script runs there. pip's own settings and proxies stay.
func TestPipRunsWithoutSecrets(t *testing.T) {
	for k, v := range map[string]string{"AZURE_KEY": "s1", "HF_TOKEN": "s2", "GITHUB_TOKEN": "s3", "BRAINIAC_API_KEY": "s4",
		"PIP_INDEX_URL": "https://mirror.example/simple", "HTTPS_PROXY": "http://proxy.example:3128"} {
		t.Setenv(k, v)
	}
	var cmds []*exec.Cmd
	saved := command
	defer func() { command = saved }()
	command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// The test binary itself, running no test, stands in for python.
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
		cmds = append(cmds, c)
		return c
	}
	if err := run(context.Background(), func(string, ...any) {}, "python", "-m", "pip", "install"); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmds[0].Env, "\n")
	for _, name := range []string{"AZURE_KEY", "HF_TOKEN", "GITHUB_TOKEN", "BRAINIAC_API_KEY"} {
		if strings.Contains(env, name+"=") {
			t.Errorf("pip's environment holds %s", name)
		}
	}
	if !strings.Contains(env, "PIP_INDEX_URL=") || !strings.Contains(env, "HTTPS_PROXY=") {
		t.Errorf("pip lost its own settings:\n%s", env)
	}
}

// A confined worker still answers over its pipes. Where the system allows
// namespaces it has no network and sees only its own files, and says so.
func TestAConfinedWorkerAnswers(t *testing.T) {
	w := newTestWorker(t)
	script := scriptPath(t)
	pol := isolate.ForProgram(w.python, []string{filepath.Dir(script)}, []string{t.TempDir()})
	w.Sandbox, w.Required = &pol, true
	ok, _ := isolate.CanConfine()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := w.Hello(ctx)
	if !ok {
		// sandbox: required refuses a worker that cannot be confined.
		if err == nil || !strings.Contains(err.Error(), "required") {
			t.Fatalf("an unconfined worker started under sandbox: required: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if open := w.Open(); len(open) != 0 {
		t.Fatalf("the confined worker can still reach %v", open)
	}
}
