// store_test.go checks that Brainiac's home is owner-only: a new root is 0700,
// JSON it writes is 0600, an existing Brainiac home and its private folders
// are tightened, and a directory that is not Brainiac's is left as it is.
package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func skipModesOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores Unix file modes")
	}
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestModeNewRootIsPrivateAndJSONIs0600(t *testing.T) {
	skipModesOnWindows(t)
	root := filepath.Join(t.TempDir(), "home")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if m := perm(t, root); m != 0o700 {
		t.Fatalf("new root %o, want 700", m)
	}
	// A leftover temp file from a crash must not pass its mode on.
	if err := os.WriteFile(s.Path("plan.json.tmp"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteJSON("plan.json", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, s.Path("plan.json")); m != 0o600 {
		t.Fatalf("plan.json %o, want 600", m)
	}
}

func TestModeAnExistingHomeIsTightened(t *testing.T) {
	skipModesOnWindows(t)
	root := t.TempDir()
	for _, d := range []string{"cache", "traces", "logs", "tmp", "models"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(root, d), 0o755)
	}
	os.Chmod(root, 0o755)
	if err := os.WriteFile(filepath.Join(root, "lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".", "cache", "traces", "logs", "tmp"} {
		if m := perm(t, filepath.Join(root, d)); m != 0o700 {
			t.Fatalf("%s %o, want 700", d, m)
		}
	}
	// Public artefacts keep their modes; the root hides them.
	if m := perm(t, filepath.Join(root, "models")); m != 0o755 {
		t.Fatalf("models %o, want it left at 755", m)
	}
}

func TestModeADirectoryThatIsNotBrainiacsIsLeftAlone(t *testing.T) {
	skipModesOnWindows(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "tmp"), 0o755)
	os.Chmod(filepath.Join(root, "tmp"), 0o755)
	os.Chmod(root, 0o755)
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, root); m != 0o755 {
		t.Fatalf("a person's directory was chmodded to %o", m)
	}
	if m := perm(t, filepath.Join(root, "tmp")); m != 0o755 {
		t.Fatalf("a person's tmp was chmodded to %o", m)
	}
}

func TestModeASymlinkedPrivateFolderIsNotFollowed(t *testing.T) {
	skipModesOnWindows(t)
	root, other := t.TempDir(), t.TempDir()
	os.Chmod(other, 0o755)
	if err := os.Symlink(other, filepath.Join(root, "cache")); err != nil {
		t.Skip("no symlinks here")
	}
	os.WriteFile(filepath.Join(root, "census.json"), []byte("{}"), 0o600)
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if m := perm(t, other); m != 0o755 {
		t.Fatalf("the folder a symlink points at was chmodded to %o", m)
	}
}
