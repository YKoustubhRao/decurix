package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real and practice desktops run one Python (resource plan B1): the PATH a practice desktop gets is the one a real
// desktop gets, whatever the image's own PATH says, and it names no venv. The image installs the worker libraries
// into the Python that PATH finds, /usr/bin/python3, by that Python's own pip. Before, the image put a venv first on
// its PATH, the "system" pip3 installed into the venv, practice desktops (which kept the image's PATH) had pandas,
// requests and pymupdf, and real desktops had none of them.
//
// Owed live, once the image is rebuilt: docker run it with a real desktop's env and import pandas, requests, fitz,
// openpyxl, pptx and docx in python3 (docs/follow-ups.md).
func TestRealAndPracticeDesktopsRunOnePython(t *testing.T) {
	real := envValue(buildDesktopEnv("box", "thebot", "", "", Brain{}), "PATH")
	// The image's own PATH, as an older image had it: the venv first.
	practice := envValue(practiceEnvironment([]string{"PATH=/opt/office-venv/bin:/usr/local/bin:/usr/bin:/bin"},
		PracticeSpec{RunID: "r1", Harness: "thebot", Brain: "ollama"}, "pw"), "PATH")
	if real == "" || practice != real {
		t.Fatalf("a practice desktop's PATH %q is not a real desktop's %q", practice, real)
	}
	if strings.Contains(real, "venv") {
		t.Fatalf("the desktop PATH names a venv: %q", real)
	}
	dirs := strings.Split(real, ":")
	usrBin := -1
	for i, d := range dirs {
		if d == "/usr/bin" {
			usrBin = i
		}
	}
	if usrBin < 0 {
		t.Fatalf("the desktop PATH has no /usr/bin: %q", real)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "docker", "desktop.basic.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var install string
	for _, run := range runInstructions(string(raw)) {
		if strings.Contains(run, " -m venv") || strings.Contains(run, "virtualenv") {
			t.Errorf("the image still makes a venv: %s", clipRun(run))
		}
		if strings.Contains(run, "pandas==") {
			install = run
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if t2 := strings.TrimSpace(line); strings.HasPrefix(t2, "ENV PATH=") && strings.Contains(t2, "venv") {
			t.Errorf("the image puts a venv on its PATH: %s", t2)
		}
	}
	if !strings.Contains(install, "/usr/bin/python3 -m pip install") {
		t.Fatalf("the worker libraries are not installed by /usr/bin/python3's own pip: %s", clipRun(install))
	}
	for _, lib := range []string{"pandas==", "requests==", "pymupdf==", "python-pptx==", "pdfplumber==", "matplotlib=="} {
		if strings.Count(string(raw), lib) != 1 {
			t.Errorf("%s is installed %d times, want once (one environment)", lib, strings.Count(string(raw), lib))
		}
	}
}
