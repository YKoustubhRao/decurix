package hostshell

import (
	"strings"
	"testing"
)

// A command the model wrote never sees soul's own secrets in its environment (review T-16).
func TestChildEnvWithholdsSoulsSecrets(t *testing.T) {
	t.Setenv("DECURIX_AGENT_TOKEN", "tok")
	t.Setenv("ANTHROPIC_API_KEY", "key")
	t.Setenv("SOME_SERVICE_TOKEN", "x")
	t.Setenv("OPENAI_API_KEY", "kept")
	t.Setenv("PLAIN_SETTING", "1")
	env := strings.Join(ChildEnv("EXTRA=1"), "\n")
	for _, gone := range []string{"DECURIX_AGENT_TOKEN=", "ANTHROPIC_API_KEY=", "SOME_SERVICE_TOKEN="} {
		if strings.Contains(env, gone) {
			t.Errorf("%s reached the child", gone)
		}
	}
	for _, kept := range []string{"OPENAI_API_KEY=kept", "PLAIN_SETTING=1", "EXTRA=1"} {
		if !strings.Contains(env, kept) {
			t.Errorf("%s did not reach the child", kept)
		}
	}
}

func TestCappedBufferKeepsTheFirstBytes(t *testing.T) {
	c := &CappedBuffer{Limit: 4}
	n, err := c.Write([]byte("abcdef"))
	if n != 6 || err != nil || c.String() != "abcd" || c.Dropped != 2 {
		t.Fatalf("wrote %d %v kept %q dropped %d", n, err, c.String(), c.Dropped)
	}
}
