package worker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
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
