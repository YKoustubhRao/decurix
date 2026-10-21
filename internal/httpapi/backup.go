package httpapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// A backup is what a firm cannot get back once it is gone: what its workers
// were taught (each desktop's skills), what they learned (worker memory),
// optionally the demonstrations themselves, and the control plane's own
// records (task history, chats, triggers, the saved model settings) with the
// key files that open its sealed secrets. rebuild.sh deletes the database
// after one prompt and a desktop's volume goes with the desktop; before this
// nothing kept a copy.
//
// It is one .tar.gz streamed as it is built:
//
//	manifest.json                    when, which desktops, what was left out and why
//	data/decurix.db                  a consistent snapshot (VACUUM INTO), not a live copy
//	data/<file>                      the data dir's other files: *.key, hostgui-apps.json
//	desktops/<id>/skills/...         ~/.decurix/skills
//	desktops/<id>/memory/...         ~/.decurix/memory
//	desktops/<id>/recordings/...     ~/.decurix/recordings, only with ?recordings=1
//
// It holds the keys to the saved secrets, so it is served only on the
// primary listener and only to a person (RegisterLocalRoutes, humansOnly).

// backupParts are the folders of a desktop's ~/.decurix a backup always takes.
//
// playbooks and tools are what a desktop worked out for itself: the way
// through a kind of request, and the scripts it wrote from work it had done by
// hand (the agent's playbooks.py). They are know-how like a taught skill, and
// a worker that takes over from another should not have to find it out again.
var backupParts = []string{"skills", "memory", "playbooks", "tools"}

// backupMaxDataFile bounds a data-dir file copied beside the database; the
// database itself is never skipped.
const backupMaxDataFile = 64 << 20

// backupManifest is manifest.json.
type backupManifest struct {
	Version    int             `json:"version"`
	Created    time.Time       `json:"created"`
	Desktops   []backupDesktop `json:"desktops"`
	Recordings bool            `json:"recordings"`
}

// What could not be read goes into problems.json, written last since it is
// only known at the end of the stream, so a backup never looks more complete
// than it is.

type backupDesktop struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Brain string `json:"brain,omitempty"`
}

// backupHandler streams the backup (GET /api/backup[?recordings=1]).
func (s *Server) backupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.DB == nil {
		http.Error(w, "backups are not available in this build", http.StatusServiceUnavailable)
		return
	}

	withRecordings := r.URL.Query().Get("recordings") == "1"

	// Take the snapshot before the first byte goes out: a failure here is
	// still a proper error response, not a truncated download.
	snap, err := snapshotDB(r.Context(), s.DB)
	if err != nil {
		jsonError(w, err)
		return
	}
	defer os.Remove(snap)

	list, err := s.Desktops.List(r.Context())
	if err != nil {
		jsonError(w, err)
		return
	}

	name := "decurix-backup-" + time.Now().Format("20060102-1504") + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	var problems []string

	man := backupManifest{Version: 1, Created: time.Now().UTC(), Recordings: withRecordings}
	for _, d := range list {
		man.Desktops = append(man.Desktops, backupDesktop{ID: d.ID, Name: d.Name, Brain: d.Brain})
	}
	if err := writeJSONEntry(tw, "manifest.json", man); err != nil {
		log.Printf("backup: %v", err)
		return
	}

	if err := addFile(tw, snap, "data/"+sqlitedb.FileName); err != nil {
		log.Printf("backup: database: %v", err)
		return
	}

	problems = append(problems, addDataDir(tw, sqlitedb.DataDir())...)

	parts := backupParts
	if withRecordings {
		parts = append(append([]string{}, parts...), "recordings")
	}
	for _, d := range list {
		for _, part := range parts {
			if err := s.addDesktopFolder(r.Context(), tw, d.ID, part); err != nil {
				problems = append(problems, fmt.Sprintf("desktop %s: %s: %v", d.ID, part, err))
			}
		}
	}

	if problems == nil {
		problems = []string{}
	}
	if len(problems) > 0 {
		log.Printf("backup: %d part(s) not included: %s", len(problems), strings.Join(problems, "; "))
	}
	if err := writeJSONEntry(tw, "problems.json", map[string]any{"not_included": problems}); err != nil {
		log.Printf("backup: %v", err)
		return
	}

	if err := tw.Close(); err != nil {
		log.Printf("backup: %v", err)
		return
	}
	if err := gz.Close(); err != nil {
		log.Printf("backup: %v", err)
	}
}

// snapshotDB writes a consistent copy of the database to a temporary file
// with VACUUM INTO, which reads one transaction's view of it while the
// control plane keeps writing.
func snapshotDB(ctx context.Context, db *sql.DB) (string, error) {
	f, err := os.CreateTemp("", "decurix-backup-*.db")
	if err != nil {
		return "", err
	}
	p := f.Name()
	f.Close()
	os.Remove(p) // VACUUM INTO refuses a file that exists

	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, p); err != nil {
		os.Remove(p)
		return "", fmt.Errorf("snapshot the database: %w", err)
	}

	return p, nil
}

// addDataDir adds the data dir's own small files (key files, the kept host
// apps list) - not the database and its WAL, which the snapshot covers.
func addDataDir(tw *tar.Writer, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("data dir: %v", err)}
	}

	var problems []string
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(n, sqlitedb.FileName) || n == QualificationOwnerFile {
			continue
		}
		info, err := e.Info()
		if err != nil {
			problems = append(problems, fmt.Sprintf("data/%s: %v", n, err))
			continue
		}
		if info.Size() > backupMaxDataFile {
			problems = append(problems, fmt.Sprintf("data/%s: %d bytes, over the %d MB a data file may be", n, info.Size(), backupMaxDataFile>>20))
			continue
		}
		if err := addFile(tw, filepath.Join(dir, n), "data/"+n); err != nil {
			problems = append(problems, fmt.Sprintf("data/%s: %v", n, err))
		}
	}

	return problems
}

// addDesktopFolder copies one ~/.decurix/<part> of a desktop into the backup
// under desktops/<id>/. A folder the desktop does not have yet is not a
// problem: a new desktop has no skills.
func (s *Server) addDesktopFolder(ctx context.Context, tw *tar.Writer, id, part string) error {
	rc, err := s.Desktops.CopyOut(ctx, id, "/home/headless/.decurix/"+part)
	if errors.Is(err, desktop.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Docker names entries from the folder's own name down ("skills/x.json").
		rel := path.Clean(strings.TrimPrefix(h.Name, "/"))
		if rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			continue
		}

		out := *h
		out.Name = path.Join("desktops", desktop.SanitizeID(id), rel)
		if h.Typeflag == tar.TypeDir {
			out.Name += "/"
		}
		out.Uname, out.Gname = "", ""
		if err := tw.WriteHeader(&out); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			// The header has promised h.Size bytes. A stream that breaks
			// part way is padded to that size, so the entries after it -
			// the rest of the backup - stay readable, and the file is
			// named as broken.
			n, err := io.CopyN(tw, tr, h.Size)
			if err != nil {
				if _, perr := io.CopyN(tw, zeros{}, h.Size-n); perr != nil {
					return perr
				}
				return fmt.Errorf("%s cut short after %d of %d bytes: %w", out.Name, n, h.Size, err)
			}
		}
	}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// addFile copies one file from disk into the archive as name.
func addFile(tw *tar.Writer, src, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: info.Size(), Mode: 0o600, ModTime: info.ModTime(),
	}); err != nil {
		return err
	}

	_, err = io.Copy(tw, f)
	return err
}

// writeJSONEntry adds v as an indented JSON file.
func writeJSONEntry(tw *tar.Writer, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: name, Size: int64(len(b)), Mode: 0o600, ModTime: time.Now(),
	}); err != nil {
		return err
	}
	_, err = tw.Write(b)
	return err
}
