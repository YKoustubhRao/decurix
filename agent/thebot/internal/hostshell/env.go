package hostshell

import (
	"bytes"
	"os"
	"strings"
)

// ChildEnv is the environment a command the model wrote starts with: soul's own, without the secrets soul holds for
// itself, then extra (a later duplicate key wins). Kept out: the desktop's agent token (soul's API and the agent's
// listen on it), the hosted brain's Anthropic key, and any other *_TOKEN or *_API_KEY - so `env` or a traceback never
// prints them into the transcript, the job log or the stream (review T-16). Not a boundary: the same user can read
// /proc. OPENAI_API_KEY and OLLAMA_API_KEY stay, because the worker's own gui tool reads them for its model calls
// (agent/decurix_agent/ollama_client.py).
func ChildEnv(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if withheldFromChildren(name) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// childKeys are the key variables a child keeps (ChildEnv).
var childKeys = map[string]bool{"OPENAI_API_KEY": true, "OLLAMA_API_KEY": true}

// withheldFromChildren reports whether a variable is one of soul's secrets.
func withheldFromChildren(name string) bool {
	upper := strings.ToUpper(name)
	if childKeys[upper] {
		return false
	}
	return upper == "DECURIX_AGENT_TOKEN" || strings.HasSuffix(upper, "_TOKEN") || strings.HasSuffix(upper, "_API_KEY")
}

// CappedBuffer keeps the first Limit bytes written to it (MaxCapture when 0) and counts the rest: what one command can
// hold in memory however much it prints (review T-14).
type CappedBuffer struct {
	Limit   int
	buf     bytes.Buffer
	Dropped int
}

// MaxCapture is the default bound on one command's output held in memory.
const MaxCapture = 256 * 1024

// Write keeps what fits and counts what does not; it never fails, so the command is never stopped by its own output.
func (c *CappedBuffer) Write(p []byte) (int, error) {
	limit := c.Limit
	if limit <= 0 {
		limit = MaxCapture
	}
	room := limit - c.buf.Len()
	if room <= 0 {
		c.Dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.Dropped += len(p) - room
		return len(p), nil
	}
	return c.buf.Write(p)
}

// String is what was kept.
func (c *CappedBuffer) String() string { return c.buf.String() }

// Bytes is what was kept.
func (c *CappedBuffer) Bytes() []byte { return c.buf.Bytes() }
