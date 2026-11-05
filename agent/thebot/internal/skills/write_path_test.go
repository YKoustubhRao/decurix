package skills

import (
	"os"
	"path/filepath"
	"testing"

	"thebot/internal/workspace"
)

// file-write never writes into a hidden folder of the home, nor into the workspace's own stores; a link inside the
// workspace is judged by where it leads (review T-15).
func TestFileWriteRefusesTheAgentsOwnFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, ".decurix", "ai")
	agentDir := filepath.Join(home, ".decurix", "command")
	for _, d := range []string{root, agentDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	rc := RunContext{Workspace: ws, WorkDir: root}
	if _, err := resolveWritePath(rc, filepath.Join(agentDir, "next.txt")); err == nil {
		t.Error("a write into a hidden folder of the home was allowed")
	}
	if _, err := resolveWritePath(rc, filepath.Join(root, ".memory", "l1.db")); err == nil {
		t.Error("a write into the workspace's own store was allowed")
	}
	if _, err := resolveWritePath(rc, filepath.Join(root, "notes.txt")); err != nil {
		t.Errorf("a write into the workspace was refused: %v", err)
	}
	if err := os.Symlink(agentDir, filepath.Join(root, "inbox")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if _, err := resolveWritePath(rc, filepath.Join(root, "inbox", "next.txt")); err == nil {
		t.Error("a link inside the workspace to the agent's own folder was followed")
	}
	if _, err := ws.ResolveSafe("inbox/next.txt"); err == nil {
		t.Error("ResolveSafe followed a link out of the workspace")
	}
}
