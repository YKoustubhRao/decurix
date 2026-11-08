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
// records (task history, chats, triggers, the saved model settings).
// rebuild.sh deletes the database after one prompt and a desktop's volume goes
// with the desktop; before this nothing kept a copy.
//
// It holds no secret that opens anything (review 2026-11-02: a stolen backup
// carried llm.key, and with it every saved API key). The key files that seal
// the saved secrets (llm.key, mail.key, cloudflare.key, hostgui.key, the
// qualification owner token: every *.key, *.token and *.pem) are left out, so
// the sealed values in the snapshot stay sealed; a secret saved unsealed (a key
// file that could not be written) and any Claude Code login an older build kept
// (claude_session, emptied since the claude harness was removed: it carried a
// live refresh token) are taken out of the snapshot itself (scrubSnapshot). Restored on the same
// install, whose key files are still in place, the sealed secrets open again;
// anywhere else they are entered again, and the restore says so.
//
// It is one .tar.gz streamed as it is built:
//
//	manifest.json                    when, which desktops, which secrets were left out
//	data/decurix.db                  a consistent snapshot (VACUUM INTO), not a live copy, scrubbed
//	data/<file>                      the data dir's other files (hostgui-apps.json), never a key
//	desktops/<id>/skills/...         ~/.decurix/skills
//	desktops/<id>/memory/...         ~/.decurix/memory
//	desktops/<id>/recordings/...     ~/.decurix/recordings, only with ?recordings=1
//
// It is still the firm's whole history, so it is served only on the primary
// listener and only to a person (RegisterLocalRoutes, humansOnly).

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

// backupManifest is manifest.json. SecretsLeftOut names what a restore has to
// have entered again (cmd/decurix/restore.go reads it); a backup from before
// it existed carried its key files.
type backupManifest struct {
	Version        int             `json:"version"`
	Created        time.Time       `json:"created"`
	Desktops       []backupDesktop `json:"desktops"`
	Recordings     bool            `json:"recordings"`
	SecretsLeftOut []string        `json:"secrets_left_out"`
}

// backupSecretsLeftOut is what every backup leaves out, in the words the
// restore repeats to the person.
var backupSecretsLeftOut = []string{
	"the model API keys (Claude, Azure, OpenAI): set them up again on the dashboard",
	"mailbox passwords: enter them again on the Triggers page",
	"the Cloudflare tunnel on your own domain: set it up again",
	"the Claude Code login: sign in again",
	"the key that signs host-app launchers and desktop tokens: recreate the desktops made before the restore",
}

// isSecretDataFile reports whether a data-dir file is a key or token a backup
// never carries: a name ending in .key, .token or .pem, or the qualification
// owner's. By pattern, so a key file added later is left out without anyone
// remembering a list.
func isSecretDataFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".token") ||
		strings.HasSuffix(lower, ".pem") || name == QualificationOwnerFile
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
	if !allowMethod(w, r, http.MethodGet) {
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

	// A snapshot that could not be scrubbed is not sent: a backup with a
	// secret in it is worse than none.
	if err := scrubSnapshot(r.Context(), snap); err != nil {
		jsonError(w, err)
		return
	}

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

	man := backupManifest{Version: 1, Created: time.Now().UTC(), Recordings: withRecordings, SecretsLeftOut: backupSecretsLeftOut}
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

// backupScrub is what scrubSnapshot runs on the snapshot, per table: a
// secret that was saved unsealed (sealed.Seal with no key file) would open
// for anyone holding the backup, and a Claude Code login is taken out even
// sealed (a live refresh token is not worth carrying). A sealed value ("enc1:...") stays: without its key file, which the
// backup leaves out, it is noise, and on the same install it opens again.
var backupScrub = map[string][]string{
	"llm_providers":   {`DELETE FROM llm_providers WHERE api_key NOT LIKE 'enc1:%'`},
	"cftunnel_config": {`DELETE FROM cftunnel_config WHERE api_token NOT LIKE 'enc1:%' OR connector_token NOT LIKE 'enc1:%'`},
	"mail_accounts": {`UPDATE mail_accounts SET password = '', needs_password = 1,
		last_error = 'The password was not in the backup: enter it again.' WHERE password NOT LIKE 'enc1:%'`},
	"claude_session": {`DELETE FROM claude_session`},
}

// legacyClaudeSession is the migration of the claude_session table, the shared
// Claude Code login that claude-harness desktops were seeded from. The harness
// and its login store (internal/claudeauth) were removed on 2026-11-07; the
// steps stay, append-only, under the store's component name, and the one
// appended then empties the table, so an old database keeps no refresh token
// nothing reads any more.
var legacyClaudeSession = []string{
	`CREATE TABLE claude_session (
		id               INTEGER PRIMARY KEY CHECK (id = 1),
		credentials_json TEXT NOT NULL,
		email            TEXT NOT NULL DEFAULT '',
		updated_ns       INTEGER NOT NULL
	)`,
	`DELETE FROM claude_session`,
}

// MigrateLegacyClaudeSession brings the old claude_session table to its last step (legacyClaudeSession): made, and
// emptied of any login an older build kept.
func MigrateLegacyClaudeSession(db *sql.DB) error {
	return sqlitedb.Migrate(db, "claudeauth", legacyClaudeSession)
}

// scrubSnapshot takes the secrets that would open without a key file out of
// the snapshot at p (backupScrub), with secure_delete on so the freed pages
// are zeroed rather than left holding them.
func scrubSnapshot(ctx context.Context, p string) error {
	db, err := sql.Open("sqlite", p)
	if err != nil {
		return fmt.Errorf("open the snapshot: %w", err)
	}
	defer func() {
		db.Close()
		for _, side := range []string{"-journal", "-wal", "-shm"} {
			os.Remove(p + side)
		}
	}()
	db.SetMaxOpenConns(1) // the pragmas below hold for this one connection

	for _, pragma := range []string{`PRAGMA journal_mode = DELETE`, `PRAGMA secure_delete = ON`} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("scrub the snapshot: %w", err)
		}
	}

	for table, statements := range backupScrub {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return fmt.Errorf("scrub the snapshot: %w", err)
		}
		if n == 0 {
			continue // that part of the control plane never ran here
		}
		for _, q := range statements {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("scrub the snapshot: %s: %w", table, err)
			}
		}
	}

	return nil
}

// addDataDir adds the data dir's own small files (the kept host apps list) -
// not the database and its WAL, which the snapshot covers, and never a key
// file (isSecretDataFile).
func addDataDir(tw *tar.Writer, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("data dir: %v", err)}
	}

	var problems []string
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(n, sqlitedb.FileName) || isSecretDataFile(n) {
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
