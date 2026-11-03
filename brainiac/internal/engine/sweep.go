package engine

import (
	"path/filepath"
	"strings"
)

// orphanServer reports whether a process is a llama-server Brainiac started
// that has outlived it: its arguments name a socket in a brainiac-llama-*
// directory, and its parent is no longer a Brainiac (a crash or a SIGKILL
// left it to init). A server another running Brainiac owns is left alone.
func orphanServer(args []string, parentName string) bool {
	sock := false
	for _, a := range args {
		if strings.HasPrefix(filepath.Base(filepath.Dir(a)), "brainiac-llama-") && filepath.Base(a) == "s.sock" {
			sock = true
		}
	}
	return sock && parentName != "brainiac"
}
