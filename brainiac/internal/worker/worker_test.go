package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testPython is an interpreter that runs, or "" (BRAINIAC_TEST_PYTHON wins,
// so a venv with rapidocr/sherpa-onnx can be pointed at).
func testPython(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("BRAINIAC_TEST_PYTHON")}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, "python", "python3")
	} else {
		candidates = append(candidates, "python3", "python")
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = exec.CommandContext(ctx, path, "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)").Run()
		cancel()
		if err == nil {
			return path
		}
	}
	t.Skip("no Python 3.8+ found (set BRAINIAC_TEST_PYTHON)")
	return ""
}

func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "workers", "python", "brainiac_worker.py"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestWorker(t *testing.T) *Worker {
	t.Helper()
	w := New(testPython(t), scriptPath(t), []string{"BRAINIAC_WORKER_TEST=1"})
	t.Cleanup(func() { _ = w.Stop() })
	return w
}

func TestHelloAndBadOp(t *testing.T) {
	w := newTestWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := w.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Worker != "brainiac-python" || h.Protocol != 1 || h.Python == "" {
		t.Fatalf("hello = %+v", h)
	}
	for _, key := range []string{"rapidocr", "sherpa_onnx", "numpy", "tesseract", "ffmpeg"} {
		if _, ok := h.Have[key]; !ok {
			t.Errorf("hello.have lacks %q: %v", key, h.Have)
		}
	}

	err = w.Call(ctx, "no-such-op", nil, nil)
	if !IsWorkerError(err) || !strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("bad op error = %v", err)
	}
	// The worker keeps serving after a refusal.
	if _, err := w.Hello(ctx); err != nil {
		t.Fatalf("hello after a refusal: %v", err)
	}
}

func TestDeadlineKillsThenNextCallRestarts(t *testing.T) {
	w := newTestWorker(t)
	ctx := context.Background()
	if _, err := w.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	first := w.PID()

	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	err := w.Call(short, "sleep", map[string]any{"seconds": 30}, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sleep past deadline: err = %v", err)
	}
	if w.Running() {
		t.Fatal("worker still running after its call outlived the deadline")
	}

	long, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := w.Hello(long); err != nil {
		t.Fatalf("hello after restart: %v", err)
	}
	if pid := w.PID(); pid == 0 || pid == first {
		t.Fatalf("pid after restart = %d (first %d)", pid, first)
	}
}

func TestStopEndsTheProcess(t *testing.T) {
	w := newTestWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := w.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	if !w.Running() || w.PID() == 0 {
		t.Fatal("not running after hello")
	}
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	if w.Running() || w.PID() != 0 {
		t.Fatal("still running after Stop")
	}
}

func TestLargePayloadIsAnsweredNotDropped(t *testing.T) {
	w := newTestWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	junk := make([]byte, 12<<20) // 16 MB of base64: far past any default line limit
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	_, err := w.OCR(ctx, junk, "auto")
	if !IsWorkerError(err) {
		t.Fatalf("OCR of junk: want the worker's refusal, got %v", err)
	}
	if !w.Running() {
		t.Fatal("worker died on a large request")
	}
}

func TestOCRReadsRenderedText(t *testing.T) {
	w := newTestWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	h, err := w.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Have["rapidocr"] {
		t.Skip("RapidOCR not installed in this Python (set BRAINIAC_TEST_PYTHON to a venv with requirements-ocr.txt)")
	}
	img := filepath.Join(t.TempDir(), "invoice.png")
	render := `import sys, cv2, numpy as np
img = np.full((120, 900, 3), 255, np.uint8)
cv2.putText(img, "Invoice 4417 Total 12,345.00", (20, 75), cv2.FONT_HERSHEY_SIMPLEX, 1.4, (0, 0, 0), 3)
cv2.imwrite(sys.argv[1], img)`
	if out, err := exec.CommandContext(ctx, testPython(t), "-c", render, img).CombinedOutput(); err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	r, err := w.OCR(ctx, data, "rapidocr")
	if err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, l := range r.Lines {
		text = append(text, l.Text)
		if l.Box[2] <= 0 || l.Box[3] <= 0 || l.Conf <= 0 || l.Conf > 1 {
			t.Errorf("odd line %+v", l)
		}
	}
	joined := strings.Join(text, " ")
	if r.Width != 900 || r.Height != 120 || !strings.Contains(joined, "4417") || !strings.Contains(joined, "345") {
		t.Fatalf("OCR = %+v (text %q)", r, joined)
	}
}

// TestTranscribe runs a real speech model, named by BRAINIAC_TEST_ASR (an
// ASRModel as JSON), on BRAINIAC_TEST_WAV, with BRAINIAC_TEST_VAD if set.
func TestTranscribe(t *testing.T) {
	spec, wav := os.Getenv("BRAINIAC_TEST_ASR"), os.Getenv("BRAINIAC_TEST_WAV")
	if spec == "" || wav == "" {
		t.Skip("set BRAINIAC_TEST_ASR and BRAINIAC_TEST_WAV to run a real speech model")
	}
	var m ASRModel
	if err := json.Unmarshal([]byte(spec), &m); err != nil {
		t.Fatal(err)
	}
	audio, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	w := newTestWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	tr, err := w.Transcribe(ctx, audio, "wav", m, os.Getenv("BRAINIAC_TEST_VAD"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(tr.Text) == "" || tr.Duration <= 0 || len(tr.Segments) == 0 {
		t.Fatalf("transcript = %+v", tr)
	}
	t.Logf("transcript (%d ms for %.1f s): %q", tr.Ms, tr.Duration, tr.Text)
}

func TestEnsureVenvSkipsAnInstalledSet(t *testing.T) {
	dir := t.TempDir()
	req := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(req, []byte("example==1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	venv := filepath.Join(dir, "venv")
	python := VenvPython(venv)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	digest, err := requirementsDigest([]string{req})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, stampFile), []byte(digest+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A base interpreter that does not exist proves nothing was run.
	got, err := EnsureVenv(context.Background(), filepath.Join(dir, "no-python"), venv, []string{req}, nil)
	if err != nil || got != python {
		t.Fatalf("EnsureVenv = %q, %v", got, err)
	}

	// A changed requirement file is a different set: it must reinstall (and
	// fails here, since the stand-in interpreter is not a real one).
	if err := os.WriteFile(req, []byte("example==2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureVenv(context.Background(), filepath.Join(dir, "no-python"), venv, []string{req}, nil); err == nil {
		t.Fatal("a changed requirement set was not reinstalled")
	}
}

// TestEnsureVenvCreatesARealOne makes a real venv (tens of seconds, so opt-in:
// BRAINIAC_TEST_VENV=1) and checks that a second call does nothing.
func TestEnsureVenvCreatesARealOne(t *testing.T) {
	if os.Getenv("BRAINIAC_TEST_VENV") != "1" {
		t.Skip("set BRAINIAC_TEST_VENV=1 to create a real venv")
	}
	base := testPython(t)
	dir := t.TempDir()
	req := filepath.Join(dir, "requirements.txt")
	if err := os.WriteFile(req, []byte("# nothing to install\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	venv := filepath.Join(dir, "venv")
	python, err := EnsureVenv(ctx, base, venv, []string{req}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, python, "-c", "import sys; print(sys.prefix)").Output(); err != nil ||
		!strings.EqualFold(filepath.Clean(strings.TrimSpace(string(out))), filepath.Clean(venv)) {
		t.Fatalf("venv python prefix = %q, %v", out, err)
	}
	started := time.Now()
	if again, err := EnsureVenv(ctx, base, venv, []string{req}, t.Logf); err != nil || again != python {
		t.Fatalf("second EnsureVenv = %q, %v", again, err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("second EnsureVenv took %v: it reinstalled", took)
	}
}

func TestRequirementsDigestDependsOnContentAndOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	_ = os.WriteFile(a, []byte("x==1\n"), 0o644)
	_ = os.WriteFile(b, []byte("y==1\n"), 0o644)
	ab, _ := requirementsDigest([]string{a, b})
	ba, _ := requirementsDigest([]string{b, a})
	again, _ := requirementsDigest([]string{a, b})
	if ab == ba || ab != again || len(ab) != 64 {
		t.Fatalf("digests: ab=%s ba=%s again=%s", ab, ba, again)
	}
}
