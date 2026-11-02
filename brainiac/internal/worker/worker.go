// Package worker runs Brainiac's Python perception worker
// (workers/python/brainiac_worker.py) as a child process and calls it over
// JSON lines on stdin/stdout: OCR of images and speech-to-text of audio.
//
// The worker holds every model it loads, so stopping the process is how that
// memory is given back; a call that outlives its deadline kills the process
// and the next call starts a fresh one.
//
// Depends on nothing internal.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"brainiac/internal/isolate"
)

// stopGrace is how long Stop waits for the worker to exit on EOF before killing it.
const stopGrace = 3 * time.Second

// stderrKeep is how much of the worker's stderr is kept for error messages.
const stderrKeep = 4096

// Worker is one Python worker process, started on first use.
type Worker struct {
	python string
	script string
	env    []string

	// Logf, when set, receives each line the worker writes to stderr.
	Logf func(format string, args ...any)
	// Sandbox, when set and the kernel can, is what the worker may touch.
	Sandbox *isolate.Policy

	callMu sync.Mutex // one call at a time

	mu      sync.Mutex // guards the fields below
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	exited  chan struct{}
	running bool
	nextID  int64
	stderr  *tail
}

// New describes a worker run as `python -I -u script` with env added to the
// reduced environment a model process gets (none of Brainiac's secrets).
// Nothing starts until Start or the first Call.
func New(python, script string, env []string) *Worker {
	return &Worker{python: python, script: script, env: env, stderr: &tail{max: stderrKeep}}
}

// Start launches the worker process if it is not running. The process outlives
// ctx: ctx only stops a start that has not happened yet.
func (w *Worker) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.startLocked()
}

// startLocked launches the process; w.mu must be held.
func (w *Worker) startLocked() error {
	if w.running {
		return nil
	}
	// -I: isolated mode, so modules in the working directory or the user's
	// site-packages are never imported; -u: unbuffered replies.
	cmd := exec.Command(w.python, "-I", "-u", w.script)
	cmd.Env = isolate.Env(w.env...)
	if w.Sandbox != nil && isolate.CanSandbox() {
		if err := isolate.Wrap(cmd, *w.Sandbox); err != nil {
			if w.Logf != nil {
				w.Logf("no file sandbox: %v", err)
			}
			cmd = exec.Command(w.python, "-I", "-u", w.script)
			cmd.Env = isolate.Env(w.env...)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("worker: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("worker: stdout: %w", err)
	}
	w.stderr.reset()
	cmd.Stderr = &lineSink{tail: w.stderr, logf: w.Logf}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("worker: start %s: %w", w.python, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
		w.mu.Lock()
		if w.cmd == cmd {
			w.running = false
		}
		w.mu.Unlock()
	}()
	w.cmd, w.stdin, w.reader, w.exited, w.running = cmd, stdin, bufio.NewReaderSize(stdout, 1<<20), exited, true
	return nil
}

// Running reports whether the worker process is alive.
func (w *Worker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// PID is the worker process's id, or 0 when it is not running. On Windows a
// venv's python.exe is a launcher and the interpreter (which holds the
// memory) is its child; killing the launcher ends the child too (checked on
// Python 3.11), so stopping by this PID still returns the memory, but a
// memory reading must add the child.
func (w *Worker) PID() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running || w.cmd == nil || w.cmd.Process == nil {
		return 0
	}
	return w.cmd.Process.Pid
}

// Stop ends the worker: it closes stdin (the worker exits on EOF) and kills
// the process if it has not exited within a few seconds.
func (w *Worker) Stop() error {
	w.mu.Lock()
	cmd, stdin, exited, running := w.cmd, w.stdin, w.exited, w.running
	w.mu.Unlock()
	if !running || cmd == nil {
		return nil
	}
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(stopGrace):
		_ = cmd.Process.Kill()
		<-exited
	}
	w.mu.Lock()
	if w.cmd == cmd {
		w.running = false
	}
	w.mu.Unlock()
	return nil
}

// kill ends the process at once (a call that outlived its deadline, or a broken pipe).
func (w *Worker) kill() {
	w.mu.Lock()
	cmd, exited, running := w.cmd, w.exited, w.running
	w.running = false
	w.mu.Unlock()
	if running && cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		<-exited
	}
}

// envelope is the part of every reply Call reads itself.
type envelope struct {
	ID    *int64 `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Call sends one request (req plus "op" and a fresh "id") and decodes the
// matching reply into out (which may be nil). A reply with ok=false is an
// error carrying the worker's message. If ctx ends first, the worker is killed
// so the next Call starts a fresh one.
func (w *Worker) Call(ctx context.Context, op string, req map[string]any, out any) error {
	w.callMu.Lock()
	defer w.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	w.mu.Lock()
	if err := w.startLocked(); err != nil {
		w.mu.Unlock()
		return err
	}
	w.nextID++
	id := w.nextID
	stdin, reader := w.stdin, w.reader
	w.mu.Unlock()

	msg := make(map[string]any, len(req)+2)
	for k, v := range req {
		msg[k] = v
	}
	msg["op"] = op
	msg["id"] = id
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("worker: encode %s request: %w", op, err)
	}
	line = append(line, '\n')

	type answer struct {
		raw []byte
		err error
	}
	done := make(chan answer, 1)
	go func() {
		if _, err := stdin.Write(line); err != nil {
			done <- answer{err: err}
			return
		}
		for {
			raw, err := reader.ReadBytes('\n')
			if err != nil {
				done <- answer{err: err}
				return
			}
			var env envelope
			if json.Unmarshal(bytes.TrimSpace(raw), &env) != nil || env.ID == nil || *env.ID != id {
				continue // not this call's reply
			}
			done <- answer{raw: raw}
			return
		}
	}()

	select {
	case <-ctx.Done():
		w.kill()
		return fmt.Errorf("worker: %s: %w", op, ctx.Err())
	case a := <-done:
		if a.err != nil {
			w.kill()
			return fmt.Errorf("worker: %s: the worker stopped (%v)%s", op, a.err, w.stderrNote())
		}
		var env envelope
		if err := json.Unmarshal(a.raw, &env); err != nil {
			return fmt.Errorf("worker: %s: bad reply: %w", op, err)
		}
		if !env.OK {
			return &Error{Op: op, Message: env.Error}
		}
		if out != nil {
			if err := json.Unmarshal(a.raw, out); err != nil {
				return fmt.Errorf("worker: %s: decode reply: %w", op, err)
			}
		}
		return nil
	}
}

// stderrNote is the tail of the worker's stderr for an error message, or "".
func (w *Worker) stderrNote() string {
	s := strings.TrimSpace(w.stderr.String())
	if s == "" {
		return ""
	}
	return "; stderr: " + s
}

// Error is a request the worker answered with ok=false.
type Error struct {
	Op      string
	Message string
}

// Error is the worker's message, prefixed with the op.
func (e *Error) Error() string { return "worker: " + e.Op + ": " + e.Message }

// IsWorkerError reports whether err is a refusal by the worker (as opposed to
// a dead process or a deadline).
func IsWorkerError(err error) bool {
	var we *Error
	return errors.As(err, &we)
}

// HelloInfo is what the worker says it can do.
type HelloInfo struct {
	Worker   string          `json:"worker"`
	Protocol int             `json:"protocol"`
	Python   string          `json:"python"`
	Have     map[string]bool `json:"have"`
}

// Hello asks the worker what it can do; it loads no model.
func (w *Worker) Hello(ctx context.Context) (HelloInfo, error) {
	var h HelloInfo
	err := w.Call(ctx, "hello", nil, &h)
	return h, err
}

// OCRLine is one line of text and the box around it, in image pixels.
type OCRLine struct {
	Text string  `json:"text"`
	Box  [4]int  `json:"box"` // x, y, width, height
	Conf float64 `json:"conf"`
}

// OCRResult is an image's text lines in reading order.
type OCRResult struct {
	Engine string    `json:"engine"`
	Model  string    `json:"model,omitempty"`
	Width  int       `json:"width"`
	Height int       `json:"height"`
	Lines  []OCRLine `json:"lines"`
	Ms     int64     `json:"ms"`
}

// OCR reads the text of a PNG, JPEG or WebP image whose text is upright (a
// screen, a rendered page). engine is "auto", "rapidocr" or "tesseract".
func (w *Worker) OCR(ctx context.Context, image []byte, engine string) (OCRResult, error) {
	return w.ocr(ctx, image, engine, true)
}

// OCRAnyAngle reads an image whose lines may be upside down (a photo, a
// scan fed in reversed): RapidOCR's angle classifier then turns them. It is
// off in OCR because on upright text it only adds mistakes.
func (w *Worker) OCRAnyAngle(ctx context.Context, image []byte, engine string) (OCRResult, error) {
	return w.ocr(ctx, image, engine, false)
}

// ocr is one OCR call.
func (w *Worker) ocr(ctx context.Context, image []byte, engine string, upright bool) (OCRResult, error) {
	if engine == "" {
		engine = "auto"
	}
	var r OCRResult
	err := w.Call(ctx, "ocr", map[string]any{
		"image_b64": base64.StdEncoding.EncodeToString(image),
		"engine":    engine,
		"upright":   upright,
	}, &r)
	return r, err
}

// ASRModel is a speech model on disk, described for sherpa-onnx.
type ASRModel struct {
	// Kind is transducer | whisper | moonshine | moonshine_v2 | sense_voice |
	// nemo_ctc | nemo_canary | omnilingual_ctc | qwen3_asr.
	Kind string `json:"kind"`
	// Files maps each file role the kind needs to an absolute path:
	//   transducer: encoder, decoder, joiner, tokens
	//   whisper, moonshine_v2, nemo_canary: encoder, decoder, tokens
	//   moonshine: preprocessor, encoder, uncached_decoder, cached_decoder, tokens
	//   sense_voice, nemo_ctc, omnilingual_ctc: model, tokens
	//   qwen3_asr: conv_frontend, encoder, decoder, tokenizer (a directory)
	Files map[string]string `json:"files"`
	// Language is "auto" or a code such as "en" or "hi".
	Language string `json:"language,omitempty"`
	Threads  int    `json:"threads,omitempty"`
	// ModelType overrides sherpa-onnx's model_type for a transducer
	// (default "nemo_transducer", for NeMo Parakeet TDT).
	ModelType string `json:"model_type,omitempty"`
	// Options are extra constructor arguments (max_new_tokens, hotwords...);
	// ones the installed sherpa-onnx does not take are dropped.
	Options map[string]any `json:"options,omitempty"`
}

// Segment is one stretch of speech.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Transcript is what was said in a clip.
type Transcript struct {
	Text     string    `json:"text"`
	Language string    `json:"language"`
	Duration float64   `json:"duration"`
	Segments []Segment `json:"segments"`
	Ms       int64     `json:"ms"`
}

// Transcribe turns an audio clip (format: wav, mp3, m4a, ogg, webm, flac) into
// text with model m. vad is a silero VAD model path, or "" to cut fixed 30 s
// pieces instead of speech segments.
func (w *Worker) Transcribe(ctx context.Context, audio []byte, format string, m ASRModel, vad string) (Transcript, error) {
	req := map[string]any{
		"audio_b64": base64.StdEncoding.EncodeToString(audio),
		"format":    format,
		"model":     m,
	}
	if vad != "" {
		req["vad"] = map[string]string{"model": vad}
	}
	var t Transcript
	err := w.Call(ctx, "asr", req, &t)
	return t, err
}

// Unload asks the worker to drop every engine it holds.
func (w *Worker) Unload(ctx context.Context) error {
	return w.Call(ctx, "unload", nil, nil)
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

// Write appends p, keeping only the last max bytes.
func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append([]byte(nil), t.buf[over:]...)
	}
	return len(p), nil
}

// String is what is kept.
func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// reset forgets what was kept.
func (t *tail) reset() {
	t.mu.Lock()
	t.buf = nil
	t.mu.Unlock()
}

// lineSink copies stderr into the tail and, line by line, to logf.
type lineSink struct {
	tail    *tail
	logf    func(string, ...any)
	pending []byte
}

// Write records p and logs every complete line.
func (s *lineSink) Write(p []byte) (int, error) {
	_, _ = s.tail.Write(p)
	if s.logf == nil {
		return len(p), nil
	}
	s.pending = append(s.pending, p...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(s.pending[:i]), "\r"); line != "" {
			s.logf("[worker] %s", line)
		}
		s.pending = s.pending[i+1:]
	}
	return len(p), nil
}
