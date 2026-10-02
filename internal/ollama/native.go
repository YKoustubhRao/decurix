package ollama

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A native Windows control plane has no Docker to run the bundled Ollama
// container in, and must not ask the person to install one. So it runs Ollama
// as a program of its own: fetched once from Ollama's own release, started on
// a loopback port, with its models in the data folder.
//
// The Windows release is one 1.4 GB zip, of which all but 24 MB is NVIDIA's
// libraries. A zip keeps its directory at the end and each file on its own, so
// only the files wanted are downloaded, over HTTP range requests: the program
// and the CPU runtime always, the GPU libraries only on a machine with the
// GPU they are for.

const (
	// nativeRelease is the Ollama release a native control plane runs. To
	// move to a newer one, change the tag: the address below follows it.
	nativeRelease = "v0.35.0"
	nativeZipURL  = "https://github.com/ollama/ollama/releases/download/" + nativeRelease + "/ollama-windows-amd64.zip"

	// NativeURL is where the control plane's own Ollama listens. Not 11434:
	// that is where a person's own Ollama would be, and this one must not
	// fight it for the port.
	NativeURL = "http://127.0.0.1:11435"

	// ownURL is a person's own Ollama, used as it is when one is running.
	ownURL = "http://127.0.0.1:11434"
)

// EnsureNative returns the address of an Ollama on this machine, starting the
// control plane's own - downloading it first if it has never been - when
// there is none to use. dir is where it and its models are kept. say is told
// what is happening, in words for a person, while it takes time.
//
// An OLLAMA_URL a person set is honoured when it answers; so is an Ollama
// they run themselves. BundledURL, which names the container a Docker
// deployment runs, means "the one Decurix manages" here too.
func EnsureNative(ctx context.Context, dir string, say func(string)) (string, error) {
	if set := strings.TrimRight(os.Getenv("OLLAMA_URL"), "/"); set != "" && set != BundledURL && set != NativeURL {
		if answers(ctx, set) {
			return set, nil
		}
		log.Printf("Ollama: nothing answers at OLLAMA_URL=%s - using the one Decurix runs itself.", set)
	} else if set == "" && answers(ctx, ownURL) {
		return ownURL, nil
	}

	if answers(ctx, NativeURL) {
		return NativeURL, nil // still running from the last time
	}

	exe := filepath.Join(dir, nativeRelease, "ollama.exe")
	if _, err := os.Stat(exe); err != nil {
		say("Downloading the local model runtime (once)...")
		if err := fetchNative(ctx, filepath.Dir(exe), hasNvidia(ctx), say); err != nil {
			return "", fmt.Errorf("download Ollama: %w", err)
		}
	}

	say("Starting the local model runtime...")
	if err := startNative(exe, filepath.Join(dir, "models"), filepath.Join(dir, "ollama.log")); err != nil {
		return "", fmt.Errorf("start Ollama: %w", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for !answers(ctx, NativeURL) {
		if time.Now().After(deadline) {
			return "", errors.New("the local model runtime did not start; see " + filepath.Join(dir, "ollama.log"))
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}

	return NativeURL, nil
}

// hasNvidia reports whether this machine has an NVIDIA GPU with its driver:
// the only case the 1.4 GB of CUDA libraries is worth downloading for.
func hasNvidia(ctx context.Context) bool {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probe, "nvidia-smi", "-L")
	hide(cmd)
	out, err := cmd.Output()

	return err == nil && bytes.Contains(out, []byte("GPU "))
}

// wanted reports whether a file of the release is needed on this machine.
func wanted(name string, nvidia bool) bool {
	if strings.HasPrefix(name, "lib/ollama/cuda_") {
		return nvidia
	}
	return true
}

// fetchNative downloads the release's wanted files into dir. It writes into a
// folder beside dir and renames it into place, so an interrupted download is
// never mistaken for an installed runtime.
func fetchNative(ctx context.Context, dir string, nvidia bool, say func(string)) error {
	remote, err := newRangeReader(ctx, nativeZipURL)
	if err != nil {
		return err
	}

	archive, err := zip.NewReader(remote, remote.size)
	if err != nil {
		return fmt.Errorf("read the release's file list: %w", err)
	}

	var files []*zip.File
	var total, done uint64
	for _, f := range archive.File {
		if f.FileInfo().IsDir() || !wanted(f.Name, nvidia) {
			continue
		}
		// A zip from elsewhere names its own paths; none may leave dir.
		if !filepath.IsLocal(filepath.FromSlash(f.Name)) {
			return fmt.Errorf("the release holds an unsafe path: %q", f.Name)
		}
		files = append(files, f)
		total += f.CompressedSize64
	}
	if len(files) == 0 {
		return errors.New("the release holds no files")
	}

	partial := dir + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return err
	}

	lastSaid := time.Now()
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}

		target := filepath.Join(partial, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		// Open checks the file's CRC-32 as it is read to the end.
		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		dst, err := os.Create(target)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}

		done += f.CompressedSize64
		if time.Since(lastSaid) > 2*time.Second {
			lastSaid = time.Now()
			say(fmt.Sprintf("Downloading the local model runtime: %d of %d MB", done>>20, total>>20))
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}

	return os.Rename(partial, dir)
}

// rangeBlock is how much one request reads ahead. archive/zip reads a file
// in small pieces; fetching each as its own request would take minutes.
const rangeBlock = 4 << 20

// rangeReader reads a file on a web server piece by piece (HTTP Range).
type rangeReader struct {
	ctx  context.Context
	url  string
	size int64

	mu    sync.Mutex
	block []byte
	at    int64
}

func newRangeReader(ctx context.Context, url string) (*rangeReader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return nil, fmt.Errorf("%s cannot be downloaded in parts", url)
	}

	// The address the redirects ended at: every part then goes straight there.
	return &rangeReader{ctx: ctx, url: resp.Request.URL.String(), size: resp.ContentLength}, nil
}

// ReadAt implements io.ReaderAt, reading ahead a block at a time.
func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}

		if pos < r.at || pos >= r.at+int64(len(r.block)) {
			if err := r.fill(pos, len(p)-n); err != nil {
				return n, err
			}
		}

		n += copy(p[n:], r.block[pos-r.at:])
	}

	return n, nil
}

// fill reads the block that starts at pos: at least need bytes, and a whole
// rangeBlock when the file has that much left.
func (r *rangeReader) fill(pos int64, need int) error {
	length := int64(max(need, rangeBlock))
	if pos+length > r.size {
		length = r.size - pos
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-r.ctx.Done():
				return r.ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", pos, pos+length-1))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d for a part of the download", resp.StatusCode)
			continue
		}

		block, err := io.ReadAll(io.LimitReader(resp.Body, length))
		resp.Body.Close()
		if err != nil || int64(len(block)) != length {
			lastErr = firstError(err, errors.New("a part of the download came back short"))
			continue
		}

		r.block, r.at = block, pos

		return nil
	}

	return lastErr
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// EnsureModels makes sure the Ollama at baseURL has each named model, pulling
// the ones it lacks. say is told how far a pull is.
func EnsureModels(ctx context.Context, baseURL string, say func(string), names ...string) error {
	installed, err := listInstalled(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("ask Ollama what it has: %w", err)
	}

	have := map[string]bool{}
	for _, m := range installed {
		have[normName(m.Name)] = true
	}
	// listInstalled leaves embedding models out; ask for those by name.
	for _, name := range names {
		if !have[normName(name)] && modelExists(ctx, baseURL, name) {
			have[normName(name)] = true
		}
	}

	for _, name := range names {
		if name == "" || have[normName(name)] {
			continue
		}
		if err := pullModel(ctx, baseURL, name, say); err != nil {
			return fmt.Errorf("download the model %s: %w", name, err)
		}
	}

	return nil
}

// modelExists asks Ollama for one model by name.
func modelExists(ctx context.Context, baseURL, name string) bool {
	body, _ := json.Marshal(map[string]string{"model": name})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// pullModel downloads one model, following Ollama's own progress stream.
func pullModel(ctx context.Context, baseURL, name string, say func(string)) error {
	body, _ := json.Marshal(map[string]any{"model": name, "stream": true})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	lastSaid := time.Time{}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for lines.Scan() {
		var event struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
		}
		if json.Unmarshal(lines.Bytes(), &event) != nil {
			continue
		}
		if event.Error != "" {
			return errors.New(event.Error)
		}
		if event.Status == "success" {
			return nil
		}
		// The model's weights, not the small files beside them: those are a
		// few KB each and would read "0 of 0 MB".
		if event.Total > 8<<20 && time.Since(lastSaid) > 2*time.Second {
			lastSaid = time.Now()
			say(fmt.Sprintf("Downloading the model %s: %d of %d MB", name, event.Completed>>20, event.Total>>20))
		}
	}

	if err := lines.Err(); err != nil {
		return err
	}

	return errors.New("the download ended before it finished")
}
