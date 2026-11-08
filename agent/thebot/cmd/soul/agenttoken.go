// agenttoken.go: soul's agent token from the file the desktop's entrypoint leaves for it (DECURIX_AGENT_TOKEN_FILE),
// read once and deleted, so it is in no environment the worker can read (agent/decurix-entrypoint.sh).
package main

import (
	"io"
	"os"
	"strings"
)

// agentTokenFileEnv names the file soul's token is in, when it is not in DECURIX_AGENT_TOKEN.
const agentTokenFileEnv = "DECURIX_AGENT_TOKEN_FILE"

// maxTokenBytes bounds what is read from the token file.
const maxTokenBytes = 4096

// takeTokenFile reads the token in path and deletes the file: "" for no path, a link, anything not a regular file or
// a file that cannot be read (soul then refuses every API route but its health check). The file is deleted either way.
func takeTokenFile(path string) string {
	if path == "" {
		return ""
	}
	defer os.Remove(path)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// The file opened is the one checked: a swap between the two calls is refused.
	if opened, err := f.Stat(); err != nil || !os.SameFile(info, opened) {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(f, maxTokenBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
