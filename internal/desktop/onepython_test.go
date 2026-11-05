package desktop

import (
	"os"
	"path/filepath"
	"regexp"
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
	// pip never shadows or replaces an apt-owned package: its report is checked against apt's packages.
	if !strings.Contains(install, "--report /tmp/pip-report.json") || !strings.Contains(install, "/usr/lib/python3/dist-packages") {
		t.Error("the pip RUN does not check what it installed against apt's packages")
	}

	// Every library the pip RUN installs, and the apt ones a worker reaches for, are imported at build time by the
	// python3 of exactly the PATH a desktop gets (review RES-CP-1: the owed live check, made a build step).
	module := map[string]string{"python-pptx": "pptx", "pandas": "pandas", "requests": "requests", "beautifulsoup4": "bs4",
		"pdfplumber": "pdfplumber", "pymupdf": "fitz", "matplotlib": "matplotlib", "python-dateutil": "dateutil",
		"xlrd": "xlrd", "tabulate": "tabulate", "reportlab": "reportlab"}
	pins := regexp.MustCompile(`"?([A-Za-z0-9][A-Za-z0-9._-]*)(==|>=)`).FindAllStringSubmatch(install, -1)
	if len(pins) < len(module) {
		t.Fatalf("read %d pins from the pip RUN, want at least %d", len(pins), len(module))
	}
	var check string
	for _, run := range runInstructions(string(raw)) {
		if strings.Contains(run, "python3 -c \"import ") && strings.HasPrefix(run, "env -i PATH=") {
			check = run
		}
	}
	m := regexp.MustCompile(`^env -i PATH=(\S+) python3 -c "import ([^"]+)"$`).FindStringSubmatch(check)
	if m == nil {
		t.Fatalf("no build-time import of the worker libraries under a desktop's PATH: %q", clipRun(check))
	}
	if m[1] != desktopPath {
		t.Fatalf("the import check runs under PATH %q, not desktopPath %q", m[1], desktopPath)
	}
	imported := map[string]bool{}
	for _, name := range strings.Split(m[2], ",") {
		imported[strings.TrimSpace(name)] = true
	}
	for _, pin := range pins {
		mod, ok := module[pin[1]]
		if !ok {
			t.Errorf("the pip RUN installs %s, which the import check does not know: add it to both", pin[1])
			continue
		}
		if !imported[mod] {
			t.Errorf("%s (import %s) is installed but not imported by the build-time check", pin[1], mod)
		}
		if n := strings.Count(string(raw), pin[1]+pin[2]); n != 1 {
			t.Errorf("%s is installed %d times, want once (one environment)", pin[1], n)
		}
	}
	for _, aptOwned := range []string{"openpyxl", "docx"} {
		if !imported[aptOwned] {
			t.Errorf("the apt library %s is not imported by the build-time check", aptOwned)
		}
	}
}

// The agent is compiled at image build after its last COPY, and before the tree is made root's, so the .pyc are
// there, read-only, for every import (resource plan P1 #1: a gui call imported the agent from source, about 1 s).
func TestTheAgentIsCompiledAfterItsLastCopy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker", "desktop.basic.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	lastCopy, compile, chown := -1, -1, -1
	for i, line := range lines {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "COPY ") && strings.Contains(l, "/opt/decurix_agent"):
			lastCopy = i
		case strings.Contains(l, "python3 -m compileall") && strings.Contains(l, "/opt/decurix_agent"):
			compile = i
		case strings.Contains(l, "chown -R root:root /opt/decurix_agent"):
			chown = i
		}
	}
	if lastCopy < 0 || compile < 0 || chown < 0 {
		t.Fatalf("copy %d, compile %d, chown %d", lastCopy, compile, chown)
	}
	if compile < lastCopy || chown < compile {
		t.Fatalf("the agent is compiled at line %d, its last COPY is at %d and it is made root's at %d", compile+1, lastCopy+1, chown+1)
	}
}
