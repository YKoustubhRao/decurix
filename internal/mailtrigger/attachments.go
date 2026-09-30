package mailtrigger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path"
	"regexp"
	"strings"
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
	SubmitTrackedWithFiles(ctx context.Context, desktopID, content, dir string, files []Attachment) (string, error)
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
// left out rather than delivered broken.
func ExtractAttachments(raw []byte) []Attachment {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}

	var out []Attachment
	walkAttachments(msg.Header, msg.Body, 0, &out)

	return out
}

func walkAttachments(h partHeader, body io.Reader, depth int, out *[]Attachment) {
	if len(*out) >= maxAttachments {
		return
	}

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
			walkAttachments(part.Header, part, depth+1, out)
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

	data, err := decodeAll(h.Get("Content-Transfer-Encoding"), body)
	if err != nil || len(data) == 0 {
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

	return io.ReadAll(io.LimitReader(r, maxMessageBytes))
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

var unsafeName = regexp.MustCompile(`[^\w.\- ()&+,@]`)

// SafeFileName makes an attachment's name safe to write into a folder: no
// directories, no control characters, a bounded length.
func SafeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimSpace(path.Base(name))
	name = unsafeName.ReplaceAllString(name, "_")
	if len(name) > 150 {
		ext := path.Ext(name)
		name = name[:150-len(ext)] + ext
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// uniqueNames renames duplicates ("invoice.pdf", "invoice (2).pdf").
func uniqueNames(files []Attachment) []Attachment {
	seen := map[string]int{}
	for i := range files {
		name := files[i].Name
		seen[strings.ToLower(name)]++
		if n := seen[strings.ToLower(name)]; n > 1 {
			ext := path.Ext(name)
			files[i].Name = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
		}
	}
	return files
}

// JobFiles returns the files kept with a job.
func (s *Store) JobFiles(jobID string) ([]Attachment, error) {
	rows, err := s.db.Query(`SELECT name, content_type, data FROM mail_job_files WHERE job_id = ? ORDER BY position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("mailtrigger: job files: %w", err)
	}
	defer rows.Close()

	var out []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.Name, &a.ContentType, &a.Data); err != nil {
			return nil, fmt.Errorf("mailtrigger: job files: %w", err)
		}
		out = append(out, a)
	}

	return out, rows.Err()
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
