package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/mailtrigger"
)

// Attachments: files a person adds to a request, or to a ticket they are
// about to record, from their own computer. They travel the road an email
// trigger's attachments already do (sendlock.go): written into the desktop's
// ~/work/inbox/<id>/ before the request is handed over, and named in the
// request by where they are - so every engine, the check of a finished task
// (verify.py), a taught skill filling its values (binding.py) and a lesson
// studying a demonstration (study.py) find them the same way.

const (
	// maxAttachments bounds the files of one request.
	maxAttachments = 10

	// maxAttachmentBytes is the largest single file (an email trigger's
	// limit, mailtrigger's maxMessageBytes).
	maxAttachmentBytes = 25 << 20

	// maxUploadBytes bounds one upload as a whole.
	maxUploadBytes = 64 << 20

	// attachmentField is the form field files are sent in.
	attachmentField = "files"

	// attachmentsMark opens the lines that name a request's files. The
	// desktop's agent splits a request on it (decurix_agent/attachments.py's
	// MARK): change one and the other's test says so.
	attachmentsMark = "Files attached to this request, saved on this desktop:"

	// inboxRoot is where a desktop keeps the files it was sent.
	inboxRoot = "/home/headless/work/inbox"
)

// errUpload is an upload a person can fix: too many files, one too large.
type errUpload struct {
	status int
	reason string
}

func (e *errUpload) Error() string { return e.reason }

// limitUpload caps the body of a request that may carry files. Called before
// anything reads a form value, since the first read parses the whole body.
func limitUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
}

// readAttachments returns the files a request carries, with names safe to
// write and to name in a request; none for a request that is not multipart.
func readAttachments(r *http.Request) ([]desktop.File, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return nil, nil
	}

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &errUpload{http.StatusRequestEntityTooLarge, fmt.Sprintf("those files are too large together - at most %d MB in one request", maxUploadBytes>>20)}
		}

		return nil, &errUpload{http.StatusBadRequest, "the upload could not be read"}
	}

	if r.MultipartForm == nil {
		return nil, nil
	}

	headers := r.MultipartForm.File[attachmentField]
	if len(headers) > maxAttachments {
		return nil, &errUpload{http.StatusBadRequest, fmt.Sprintf("at most %d files can be attached to one request", maxAttachments)}
	}

	files := make([]desktop.File, 0, len(headers))
	seen := map[string]int{}

	for _, h := range headers {
		if h.Size > maxAttachmentBytes {
			return nil, &errUpload{http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is too large - at most %d MB a file", h.Filename, maxAttachmentBytes>>20)}
		}

		f, err := h.Open()
		if err != nil {
			return nil, &errUpload{http.StatusBadRequest, "the upload could not be read"}
		}

		data, err := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
		f.Close()

		if err != nil || len(data) > maxAttachmentBytes {
			return nil, &errUpload{http.StatusBadRequest, h.Filename + " could not be read"}
		}

		if len(data) == 0 {
			return nil, &errUpload{http.StatusBadRequest, h.Filename + " is empty"}
		}

		files = append(files, desktop.File{Name: uniqueAttachmentName(attachmentName(h.Filename), seen), Data: data})
	}

	return files, nil
}

var attachmentNameUnsafe = regexp.MustCompile(`[^\w.\-]+`)

// attachmentName is a file's name as it is saved: no folders, and nothing a
// path in a request cannot hold. A request names its files by path, and what
// reads those paths back (binding.PATH_RE, verify.DOCUMENT_PATH, TheBot's
// documentPathRe) stops at a space or a bracket - "PO 7731 (final).pdf" is
// saved as "PO_7731_final.pdf". Letters outside ASCII go the same way (Go's
// \w, which TheBot reads paths with, is ASCII only); a name made of nothing
// else keeps its extension ("請求書.pdf" is "file.pdf").
func attachmentName(name string) string {
	name = mailtrigger.SafeFileName(name)

	ext := path.Ext(name)
	stem := strings.Trim(attachmentNameUnsafe.ReplaceAllString(strings.TrimSuffix(name, ext), "_"), "_.")
	ext = attachmentNameUnsafe.ReplaceAllString(ext, "")

	if stem == "" {
		stem = "file"
	}
	if ext == "." {
		ext = ""
	}

	return stem + ext
}

// uniqueAttachmentName tells two files of one name apart ("scan.pdf",
// "scan_2.pdf"); seen counts the names given so far, in lower case.
func uniqueAttachmentName(name string, seen map[string]int) string {
	key := strings.ToLower(name)
	seen[key]++

	if seen[key] == 1 {
		return name
	}

	ext := path.Ext(name)

	return fmt.Sprintf("%s_%d%s", strings.TrimSuffix(name, ext), seen[key], ext)
}

// putAttachments writes files into a folder of their own in the desktop's
// inbox and returns each one's path as the desktop's agent names it. It works
// on a stopped desktop, so the files are in place before the request that
// names them wakes it.
func (s *Server) putAttachments(ctx context.Context, desktopID string, files []desktop.File) ([]string, error) {
	if len(files) == 0 {
		return nil, nil
	}

	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}

	dir := inboxRoot + "/up-" + hex.EncodeToString(raw)

	if err := s.Desktops.CopyFiles(ctx, desktopID, dir, files); err != nil {
		return nil, err
	}

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = s.Desktops.AgentPath(desktopID, dir+"/"+f.Name)
	}

	return paths, nil
}

// withAttachments is a request followed by the files it brought, one path a
// line, the way an email trigger's request lists an email's files.
func withAttachments(content string, paths []string) string {
	if len(paths) == 0 {
		return content
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(content, "\n"))
	b.WriteString("\n\n" + attachmentsMark)

	for _, p := range paths {
		b.WriteString("\n  " + p)
	}

	return b.String()
}

// splitAttachments is withAttachments undone: the request as the person
// wrote it, and the paths of the files it brought.
func splitAttachments(content string) (string, []string) {
	text, rest, found := strings.Cut(content, attachmentsMark)
	if !found {
		return content, nil
	}

	var paths []string
	for _, line := range strings.Split(rest, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}

	return strings.TrimRight(text, "\n "), paths
}

// uploadError answers a failed upload: the person's own mistake in words
// they can act on, anything else as a send that failed.
func uploadError(w http.ResponseWriter, err error) {
	var mistake *errUpload
	if errors.As(err, &mistake) {
		jsonStatusError(w, mistake.status, mistake.reason)
		return
	}

	sendRequestError(w, err)
}
