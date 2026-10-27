package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// These are variables so tests can shorten them.
var (
	// idleTimeout is how long a download may receive nothing before it is
	// treated as stalled and resumed on a fresh connection.
	idleTimeout = 60 * time.Second
	// retryWait is the pause before attempt n (1-based) of a download.
	retryWait = func(attempt int) time.Duration { return time.Duration(attempt*attempt) * time.Second }
)

const maxAttempts = 4

var (
	errStalled      = errors.New("download stalled: no data received for a while")
	errRangeRestart = errors.New("the server refused to resume; starting the file again")
)

// defaultHTTPClient has connection timeouts but no overall one: a model file
// can take an hour on a slow line, and stalls are caught by idleTimeout.
func defaultHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSHandshakeTimeout = 20 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: t}
}

// safeClient is hc (or a default client) whose redirects never carry
// credentials to another host.
func safeClient(hc *http.Client) *http.Client {
	if hc == nil {
		hc = defaultHTTPClient()
	}
	c := *hc
	c.CheckRedirect = withoutAuthAcrossHosts(hc.CheckRedirect)
	return &c
}

// withoutAuthAcrossHosts wraps a redirect policy so the credentials set on the
// first request are dropped when a redirect goes to another host or port, or
// from https to http. Go's own rule keeps Authorization for subdomains and
// ignores ports; a Hub token has no business at a CDN, which is sent a signed
// URL instead.
func withoutAuthAcrossHosts(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0].URL
		if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
			for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
				req.Header.Del(h)
			}
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

// DownloadURL fetches url into dest: through dest+".part", resuming it with a
// Range request when one is left over, retrying a broken or stalled transfer,
// and renaming it into place only once its size and sha256 (when given) are
// right. A dest that is already complete is left alone.
func DownloadURL(ctx context.Context, hc *http.Client, rawURL, dest string, size int64, sha string, headers map[string]string, progress func(done, total int64)) error {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha != "" && !sha256Re.MatchString(sha) {
		return fmt.Errorf("%q is not a sha256", sha)
	}
	if progress == nil {
		progress = func(int64, int64) {}
	}
	done, err := complete(dest, size, sha)
	if err != nil {
		return err
	}
	if done {
		if st, err := os.Stat(dest); err == nil {
			progress(st.Size(), st.Size())
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	client := safeClient(hc)
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryWait(attempt)):
			}
		}
		err := downloadOnce(ctx, client, rawURL, dest, size, sha, headers, progress)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", maxAttempts, lastErr)
}

// complete reports whether dest already holds the wanted file.
func complete(dest string, size int64, sha string) (bool, error) {
	st, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("%s exists and is not a file", dest)
	}
	if size > 0 && st.Size() != size {
		return false, nil
	}
	if sha == "" {
		return true, nil
	}
	got, err := fileSHA256(dest)
	if err != nil {
		return false, err
	}
	return got == sha, nil
}

// retryable reports whether trying the download again could cure err.
func retryable(err error) bool {
	if errors.Is(err, errStalled) || errors.Is(err, errRangeRestart) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status >= 500 || se.Status == http.StatusTooManyRequests || se.Status == http.StatusRequestTimeout
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// downloadOnce is one attempt: resume or start dest+".part", then finish it.
func downloadOnce(ctx context.Context, client *http.Client, rawURL, dest string, size int64, sha string, headers map[string]string, progress func(done, total int64)) error {
	part := dest + ".part"
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}
	if size > 0 && offset > size {
		if err := os.Remove(part); err != nil {
			return err
		}
		offset = 0
	}
	if size > 0 && offset == size {
		return finish(part, dest, size, sha)
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(idleTimeout, func() { cancel(errStalled) })
	defer timer.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := client.Do(req)
	if err != nil {
		return stallCause(ctx, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0 // a fresh file, or a server that ignored the Range
	case http.StatusPartialContent:
		start, total, ok := contentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			_ = os.Remove(part)
			return errRangeRestart
		}
		if size > 0 && total > 0 && total != size {
			return fmt.Errorf("%w: the server has %d bytes, the listing said %d", ErrChecksum, total, size)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(part)
		return errRangeRestart
	default:
		return statusError(resp)
	}

	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if offset > 0 {
		flags = os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := size
	if total <= 0 && resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	written, copyErr := copyWatched(f, resp.Body, timer, offset, total, progress)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return stallCause(ctx, copyErr)
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return io.ErrUnexpectedEOF
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return finish(part, dest, size, sha)
}

// stallCause turns the cancellation done by the idle timer back into errStalled.
func stallCause(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errStalled) {
		return errStalled
	}
	return err
}

// copyWatched copies body into f, resetting the idle timer on every read and
// reporting progress; it returns the bytes copied from body.
func copyWatched(f io.Writer, body io.Reader, timer *time.Timer, offset, total int64, progress func(done, total int64)) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		n, err := body.Read(buf)
		if n > 0 {
			timer.Reset(idleTimeout)
			if _, werr := f.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)
			progress(offset+written, total)
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

// finish checks a complete .part and renames it to dest; a wrong one is removed.
func finish(part, dest string, size int64, sha string) error {
	st, err := os.Stat(part)
	if err != nil {
		return err
	}
	if size > 0 && st.Size() < size {
		return io.ErrUnexpectedEOF // the connection ended early: keep the part and resume it
	}
	if size > 0 && st.Size() > size {
		_ = os.Remove(part)
		return fmt.Errorf("%w: got %d bytes, want %d", ErrChecksum, st.Size(), size)
	}
	if sha != "" {
		got, err := fileSHA256(part)
		if err != nil {
			return err
		}
		if got != sha {
			_ = os.Remove(part)
			return fmt.Errorf("%w: sha256 %s, want %s", ErrChecksum, got, sha)
		}
	}
	return os.Rename(part, dest)
}

// contentRange parses "bytes start-end/total"; total is -1 when "*".
func contentRange(h string) (start, total int64, ok bool) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(h, "bytes ") {
		return 0, 0, false
	}
	spec, tot, found := strings.Cut(strings.TrimPrefix(h, "bytes "), "/")
	if !found {
		return 0, 0, false
	}
	s, _, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	total = -1
	if tot != "*" {
		if total, err = strconv.ParseInt(tot, 10, 64); err != nil {
			return 0, 0, false
		}
	}
	return start, total, true
}

// fileSHA256 is the hex sha256 of a file's content.
func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hexSum(h), nil
}

// hexSum is a hash's sum in lower-case hex.
func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// LocalPath joins a repo file path onto dir, refusing one that would land
// outside it ("../x", an absolute path, a drive letter).
func LocalPath(dir, repoFile string) (string, error) {
	p := strings.ReplaceAll(repoFile, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') {
		return "", fmt.Errorf("hub: unsafe file path %q", repoFile)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("hub: unsafe file path %q", repoFile)
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), nil
}

// ProgressPrinter returns a progress func that writes one updating line to w,
// at most twice a second, and ends it with a newline when the file is done.
func ProgressPrinter(w io.Writer, label string) func(done, total int64) {
	var (
		mu       sync.Mutex
		last     time.Time
		finished bool
	)
	return func(done, total int64) {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		full := total > 0 && done >= total
		now := time.Now()
		if !full && now.Sub(last) < 500*time.Millisecond {
			return
		}
		last = now
		if total > 0 {
			fmt.Fprintf(w, "\r%s  %.1f / %.1f MB  %3.0f%%", label, float64(done)/1e6, float64(total)/1e6, 100*float64(done)/float64(total))
		} else {
			fmt.Fprintf(w, "\r%s  %.1f MB", label, float64(done)/1e6)
		}
		if full {
			fmt.Fprintln(w)
			finished = true
		}
	}
}
