// Package workspace enforces the one hard rule every part of TheBot
// answers to: the AI works inside a single root directory, always - never
// outside it. Every skill/tool resolves paths through this package
// instead of doing its own filepath.Join, so the sandboxing guarantee
// lives in exactly one place.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Workspace struct {
	root string
}

// New creates (if needed) and returns a Workspace rooted at root. Fails
// loudly if the root can't actually be created - a workspace that
// silently falls back to somewhere else is worse than a hard failure at
// startup.
func New(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: create root %q: %w", abs, err)
	}
	return &Workspace{root: abs}, nil
}

func (w *Workspace) Root() string { return w.root }

// ResolveSafe resolves relPath against the workspace root, refusing to
// let it escape (e.g. "../../etc/passwd"). Every skill trusts this
// instead of joining paths itself.
func (w *Workspace) ResolveSafe(relPath string) (string, error) {
	// Treat relPath as rooted at "/" first so filepath.Clean collapses any
	// ".." components before it ever touches the real root - a relPath of
	// "../../etc/passwd" becomes "/etc/passwd", which then joins harmlessly
	// under the real root as "<root>/etc/passwd" rather than escaping it.
	cleaned := filepath.Clean(string(filepath.Separator) + relPath)
	abs := filepath.Join(w.root, cleaned)
	if abs != w.root && !strings.HasPrefix(abs, w.root+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace: path %q escapes workspace root", relPath)
	}
	return abs, nil
}
