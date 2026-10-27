package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Found live (2026-10-24): an invoice fetched outside the browser was a web page saved as .pdf.
func TestAFileACommandWroteThatIsNotItsKindIsSaidAtOnce(t *testing.T) {
	dir := t.TempDir()
	started := time.Now()
	page := filepath.Join(dir, "197_invoice.pdf")
	real := filepath.Join(dir, "198_invoice.pdf")
	os.WriteFile(page, []byte("<!doctype html><html>sign in</html>"), 0o644)
	os.WriteFile(real, []byte("%PDF-1.4\n..."), 0o644)
	note := wrongKindNote(dir, `curl -s -o 197_invoice.pdf "http://x/f/197" && curl -s -o '`+real+`' http://x/f/198`, started)
	if !strings.Contains(note, "197_invoice.pdf is named as a PDF file but holds a web page") || strings.Contains(note, "198_invoice") {
		t.Fatalf("note = %q", note)
	}
	script := "python3 - <<'PY'\nopen('" + page + "', 'wb').write(b'x')\nPY"
	if note := wrongKindNote(dir, script, started); !strings.Contains(note, "197_invoice.pdf") {
		t.Errorf("a file named in a script was not checked: %q", note)
	}
	// A file the command did not write (older than it) is not this command's to report.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(page, old, old)
	if note := wrongKindNote(dir, "ls 197_invoice.pdf", time.Now()); note != "" {
		t.Errorf("an older file was reported: %q", note)
	}
}

// Found live (2026-10-25): a script built its paths (os.path.join) and saved sign-in pages under the bills' names.
func TestAFileAScriptWroteIntoANamedFolderIsCheckedToo(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	started := time.Now()
	os.WriteFile(filepath.Join(work, "Invoice_451.pdf"), []byte("<html>Sign in to continue</html>"), 0o644)
	script := "python3 - <<'PY'\nimport os, urllib.request\nfolder = '" + work + "'\nfor n in ['Invoice_451.pdf']:\n    urllib.request.urlretrieve('http://x/f/' + n, os.path.join(folder, n))\nPY"
	if note := wrongKindNote(dir, script, started); !strings.Contains(note, "Invoice_451.pdf is named as a PDF file but holds a web page") {
		t.Fatalf("note = %q", note)
	}
}
