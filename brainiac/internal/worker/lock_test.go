package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// G-09: only a fully hash-locked requirement file is installed: one exact
// version and a sha256 per requirement, nothing that pulls in another
// file, an index or a URL.
func TestOnlyAHashLockedFileIsInstalled(t *testing.T) {
	hash := " --hash=sha256:" + strings.Repeat("c", 64)
	good := "# a lock\nnumpy==2.5.4 \\\n    --hash=sha256:" + strings.Repeat("a", 64) + " \\\n    --hash=sha256:" + strings.Repeat("b", 64) + "\nsix==1.17.0" + hash + "\n"
	for name, text := range map[string]string{
		"a range":           "numpy>=1.24,<3" + hash + "\n",
		"no hash":           "numpy==2.5.4\n",
		"a hash elsewhere":  "numpy==2.5.4" + hash + "\nsix==1.17.0\n",
		"another file":      "-r other.txt\n",
		"an index":          "--extra-index-url https://example.invalid/simple\nnumpy==2.5.4" + hash + "\n",
		"a URL":             "numpy @ https://example.invalid/numpy.whl" + hash + "\n",
		"a commented hash":  "numpy==2.5.4 # --hash=sha256:" + strings.Repeat("a", 64) + "\n",
		"an unpinned extra": "rapidocr[rtl]" + hash + "\n",
	} {
		p := filepath.Join(t.TempDir(), "requirements.txt")
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := CheckLocked(p); err == nil {
			t.Errorf("%s was accepted as locked", name)
		}
		// EnsureVenv refuses it before making anything.
		venv := filepath.Join(t.TempDir(), "venv")
		if _, err := EnsureVenv(context.Background(), "no-python", venv, []string{p}, nil); err == nil || !strings.Contains(err.Error(), "hash-locked") {
			t.Errorf("%s: EnsureVenv = %v", name, err)
		}
		if _, err := os.Stat(venv); err == nil {
			t.Errorf("%s: a venv was made for an unlocked file", name)
		}
	}
	p := filepath.Join(t.TempDir(), "lock.txt")
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(good, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckLocked(p); err != nil {
		t.Fatalf("a locked file was refused: %v", err)
	}
}
