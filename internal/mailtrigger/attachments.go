package mailtrigger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path"
	"regexp"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Attachments: an emailed PO, a supplier's invoice or price sheet is the
// whole point of many back-office triggers, so a matching email's files go
// to the desktop with the request. They are kept with the job until it is
// sent (mail_job_files), written into the desktop's ~/work/inbox/<job id>/
// just before the request (FileSubmitter), and listed in the request.

// maxAttachments bounds the files taken from one email.
const maxAttachments = 20

// InboxDir is where a job's files land in a desktop.
func InboxDir(jobID string) string { return "/home/headless/work/inbox/" + jobID }

// FileSubmitter is a Submitter that can also put files into the desktop
// before sending the request (*httpapi.Server). A job with files sent through
// a plain Submitter goes out without them.
type FileSubmitter interface {
	SubmitTrackedWithFiles(ctx context.Context, desktopID, content, dir string, files []Attachment, origin tasks.Origin) (string, error)
}

// mayHaveAttachments reports whether a message's header says it is a
// container that can carry files - the cheap check before fetching it whole.
func mayHaveAttachments(header []byte) bool {
	msg, err := mail.ReadMessage(bytes.NewReader(ensureHeaderEnd(header)))
	if err != nil {
		return false
	}

	mediaType, _, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))

	return err == nil && (mediaType == "multipart/mixed" || mediaType == "multipart/related" || mediaType == "multipart/report")
}

// ExtractAttachments returns the files of a whole message: parts marked as
// attachments, and inline parts that carry a file name and are not the
// message's own text. A part cut short (a message over the fetch limit) is
// left out rather than delivered broken, and so is every file past
// maxAttachments; dropped names each file left out and why, for the request
// to say (review 2026-11-03, B-28).
func ExtractAttachments(raw []byte) (files []Attachment, dropped []string) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil
	}

	walkAttachments(msg.Header, msg.Body, 0, &files, &dropped)

	return files, dropped
}

// maxDropped bounds how many left-out files a request names.
const maxDropped = 20

func walkAttachments(h partHeader, body io.Reader, depth int, out *[]Attachment, dropped *[]string) {
	mediaType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		mediaType = "application/octet-stream"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		if params["boundary"] == "" || depth >= 6 {
			return
		}

		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, err := reader.NextRawPart()
			if err != nil {
				return
			}
			walkAttachments(part.Header, part, depth+1, out, dropped)
		}
	}

	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	name := decodeHeader(dparams["filename"])
	if name == "" {
		name = decodeHeader(params["name"])
	}

	isFile := disposition == "attachment" || (name != "" && mediaType != "text/plain" && mediaType != "text/html")
	if !isFile {
		return
	}

	shown := SafeFileName(name)
	if name == "" {
		shown = "an unnamed " + mediaType + " file"
	}

	drop := func(why string) {
		if len(*dropped) < maxDropped {
			*dropped = append(*dropped, shown+" ("+why+")")
		}
	}

	if len(*out) >= maxAttachments {
		drop(fmt.Sprintf("only the first %d files of an email are kept", maxAttachments))
		return
	}

	data, err := decodeAll(h.Get("Content-Transfer-Encoding"), body)
	if err != nil {
		drop("it was cut short or could not be decoded")
		return
	}
	if len(data) == 0 {
		drop("it is empty")
		return
	}

	if name == "" {
		name = "attachment" + extensionFor(mediaType)
	}

	*out = append(*out, Attachment{Name: SafeFileName(name), ContentType: mediaType, Data: data})
}

// decodeAll undoes a part's transfer encoding in full, failing on a part
// that ends early.
func decodeAll(encoding string, body io.Reader) ([]byte, error) {
	var r io.Reader

	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &stripNewlines{r: body})
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	default:
		r = body
	}

	data, err := io.ReadAll(io.LimitReader(r, maxMessageBytes+1))
	if err == nil && len(data) > maxMessageBytes {
		// Cut at the limit, it would be delivered broken.
		return nil, fmt.Errorf("larger than %d MB", maxMessageBytes>>20)
	}

	return data, err
}

// stripNewlines feeds base64 its text without line breaks or stray spaces,
// which some mailers leave in.
type stripNewlines struct{ r io.Reader }

func (s *stripNewlines) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	j := 0
	for i := 0; i < n; i++ {
		switch p[i] {
		case '\r', '\n', ' ', '\t':
		default:
			p[j] = p[i]
			j++
		}
	}
	if j == 0 && err == nil && n > 0 {
		return s.Read(p)
	}
	return j, err
}

func extensionFor(mediaType string) string {
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// unsafeName is every character a file name keeps out: letters and digits of
// any script stay (an invoice named in Hindi keeps its name), as do a few
// marks; control characters, slashes, quotes and shell characters go.
var unsafeName = regexp.MustCompile(`[^\p{L}\p{M}\p{N}_.\- ()&+,@]`)

// SafeFileName makes an attachment's name safe to write into a folder: no
// directories, no control characters, a bounded length, and no leading "-"
// or "." (a name that a command the worker runs would read as an option, or
// that hides the file; review 2026-11-03, B-37).
func SafeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(path.Base(name))
	name = unsafeName.ReplaceAllString(name, "_")
	if name != "." && name != ".." && (strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".")) {
		name = "_" + name[1:]
	}
	if len(name) > 150 {
		// An extension that long is not one: cutting the name to make room for it panicked and crash-looped the
		// control plane on one email (review 2026-11-02).
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = strings.ToValidUTF8(name[:150-len(ext)], "") + ext
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// uniqueNames renames duplicates ("invoice.pdf", "invoice (2).pdf"),
// comparing without case, and never to a name another file already has
// (review 2026-11-03, B-37: "a.pdf", "a.pdf", "a (2).pdf" made two of them).
func uniqueNames(files []Attachment) []Attachment {
	taken := map[string]bool{}
	for _, f := range files {
		taken[strings.ToLower(f.Name)] = true
	}

	seen := map[string]bool{}
	for i := range files {
		name := files[i].Name
		if !seen[strings.ToLower(name)] {
			seen[strings.ToLower(name)] = true
			continue
		}

		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
			if key := strings.ToLower(candidate); !taken[key] && !seen[key] {
				files[i].Name = candidate
				seen[key] = true
				taken[key] = true
				break
			}
		}
	}
	return files
}

// JobFiles returns the files kept with a job.
func (s *Store) JobFiles(jobID string) ([]Attachment, error) {
	out, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (a Attachment, err error) {
		err = r.Scan(&a.Name, &a.ContentType, &a.Data)
		return a, err
	}, `SELECT name, content_type, data FROM mail_job_files WHERE job_id = ? ORDER BY position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("mailtrigger: job files: %w", err)
	}

	return out, nil
}

func insertJobFiles(tx *sql.Tx, jobID string, files []Attachment) error {
	for i, f := range files {
		if _, err := tx.Exec(`INSERT INTO mail_job_files (job_id, position, name, content_type, data) VALUES (?, ?, ?, ?, ?)`,
			jobID, i, f.Name, f.ContentType, f.Data); err != nil {
			return fmt.Errorf("mailtrigger: job files: %w", err)
		}
	}
	return nil
}

func (s *Store) dropJobFiles(jobID string) {
	_, _ = s.db.Exec(`DELETE FROM mail_job_files WHERE job_id = ?`, jobID)
}

// dropOrphanFiles deletes the files kept for jobs that no longer exist: a
// job pruned from the history or deleted with its rule, mailbox or desktop
// takes its files with it, so untrusted mail cannot grow the database for
// good (review 2026-11-03, B-17).
func dropOrphanFiles(db interface {
	Exec(query string, args ...any) (sql.Result, error)
}) error {
	if _, err := db.Exec(`DELETE FROM mail_job_files WHERE job_id NOT IN (SELECT id FROM mail_jobs)`); err != nil {
		return fmt.Errorf("mailtrigger: drop job files: %w", err)
	}
	return nil
}
