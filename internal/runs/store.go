// Package runs is the run record: what was pulled off a desktop for one execution of a task (its bundle), and the
// decurix.trajectory/v1 document built from that bundle joined with the control plane's own records (the task, its
// judgements, the proxy's model calls). The control plane assembles the record because only it outlives desktops and
// sees every input (PLAN-P2 D1); a desktop never writes the trajectory itself.
//
// Three parts:
//   - Store keeps one bundle manifest per execution (run_bundles); the files themselves are blobs, by SHA-256.
//   - Puller reads a run's files out of the worker's home, as the worker, and saves them as a bundle.
//   - Build joins the bundle with the task records into a Trajectory.
//
// Nothing here deletes a row or a blob (plan rule 4: nothing that labels a run is deleted).
//
// Imports blobstore-shaped storage through the Blobs interface, plus tasks, llmrec and sqlitedb; nothing from
// httpapi.
package runs

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// ExecutionRef names one run: an execution of a task on a worker.
type ExecutionRef struct{ ExecID, TaskID, Worker string }

// File is one file of a bundle, by its path under the worker's home.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Bundle is what was pulled off a desktop for one run.
type Bundle struct {
	ExecID, TaskID, Worker string
	PulledAt               time.Time
	Files                  []File
	// Missing names every part that was looked for and not found, or not kept, with the reason: never silently empty.
	Missing    []string
	TotalBytes int64
	// ManifestSHA is the SHA-256 of the canonical JSON of Files (ManifestSHA).
	ManifestSHA string
	// History holds every file an earlier pull kept at a path whose content has changed since: a later pull never
	// takes a reference away (plan rule 4).
	History []File
}

// Blobs is the content-addressed store a bundle's files live in: *blobstore.Store satisfies it.
type Blobs interface {
	Put(r io.Reader) (string, int64, error)
	PutBytes(b []byte) (string, error)
	Open(sha string) (io.ReadCloser, error)
	Has(sha string) bool
}

// schema is the runs component's migration steps. Append only: an applied step is never edited.
var schema = []string{
	`CREATE TABLE run_bundles (
		exec_id      TEXT PRIMARY KEY,
		task_id      TEXT NOT NULL,
		worker_id    TEXT NOT NULL,
		pulled_ns    INTEGER NOT NULL,
		manifest_sha TEXT NOT NULL,
		total_bytes  INTEGER NOT NULL,
		files        TEXT NOT NULL,
		missing      TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE INDEX run_bundles_task ON run_bundles (task_id)`,
	`CREATE INDEX run_bundles_worker ON run_bundles (worker_id, pulled_ns)`,
	// Earlier contents of a path a later pull found changed (review M1): a re-pull merges, it never drops.
	`ALTER TABLE run_bundles ADD COLUMN history TEXT NOT NULL DEFAULT '[]'`,
}

// Store keeps the bundle manifests of runs.
type Store struct {
	db    *sql.DB
	blobs Blobs
	// mu makes SaveBundle's read of the earlier pull and its write one step.
	mu sync.Mutex
}

// ErrNoBlobs is a store or puller made without a blob store.
var ErrNoBlobs = errors.New("runs: no blob store")

// NewStore opens the run store on db, applying its schema ("runs" in schema_migrations); blobs holds the files.
func NewStore(db *sql.DB, blobs Blobs) (*Store, error) {
	if blobs == nil {
		return nil, ErrNoBlobs
	}
	if err := sqlitedb.Migrate(db, "runs", schema); err != nil {
		return nil, err
	}
	return &Store{db: db, blobs: blobs}, nil
}

// Blobs is the blob store the bundles' files live in.
func (s *Store) Blobs() Blobs { return s.blobs }

// ManifestSHA is the SHA-256 of the canonical JSON of files: sorted by path, each {path, sha256, bytes}.
func ManifestSHA(files []File) string {
	sorted := sortedFiles(files)
	b, _ := json.Marshal(sorted) // a []File always marshals
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sortedFiles is a copy of files sorted by path (never nil).
func sortedFiles(files []File) []File {
	out := append(make([]File, 0, len(files)), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// SaveBundle stores b's manifest, merged with an earlier pull of the same execution by path: a file the earlier pull
// kept and this one did not find stays (the desktop may have pruned it since, W0.8); a path whose content changed
// keeps the new file, and the old one moves to History. Nothing a pull once referenced is ever dropped (plan rule 4).
// A "<part>: not present" line for a part the merged files still hold says it was kept from an earlier pull. Files
// are kept sorted by path; ManifestSHA and TotalBytes are computed here; a nil Missing is stored as [].
func (s *Store) SaveBundle(b Bundle) error {
	_, err := s.SaveBundleThen(b, nil)
	return err
}

// SaveBundleThen is SaveBundle, then then(saved) with the bundle as stored, under the same lock: the puller writes
// EXPORTED there, so two pulls of one execution can never leave the older manifest in it. A then error is returned;
// the bundle stays saved.
func (s *Store) SaveBundleThen(b Bundle, then func(saved Bundle) error) (Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveBundle(b); err != nil {
		return Bundle{}, err
	}
	saved, _, err := s.Bundle(b.ExecID)
	if err != nil {
		return Bundle{}, err
	}
	if then != nil {
		return saved, then(saved)
	}
	return saved, nil
}

// saveBundle is SaveBundle's body; the caller holds s.mu.
func (s *Store) saveBundle(b Bundle) error {
	if b.ExecID == "" || b.TaskID == "" {
		return errors.New("runs: a bundle needs its execution and task")
	}
	if old, ok, err := s.Bundle(b.ExecID); err != nil {
		return err
	} else if ok {
		b = mergeBundles(old, b)
	}
	b.Files = sortedFiles(b.Files)
	b.TotalBytes = 0
	for _, f := range b.Files {
		b.TotalBytes += f.Bytes
	}
	b.ManifestSHA = ManifestSHA(b.Files)
	if b.Missing == nil {
		b.Missing = []string{}
	}
	if b.History == nil {
		b.History = []File{}
	}
	if b.PulledAt.IsZero() {
		b.PulledAt = time.Now()
	}
	files, _ := json.Marshal(b.Files)
	missing, _ := json.Marshal(b.Missing)
	history, _ := json.Marshal(sortedFiles(b.History))
	_, err := s.db.Exec(`INSERT INTO run_bundles (exec_id, task_id, worker_id, pulled_ns, manifest_sha, total_bytes,
			files, missing, history) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(exec_id) DO UPDATE SET task_id = excluded.task_id, worker_id = excluded.worker_id,
			pulled_ns = excluded.pulled_ns, manifest_sha = excluded.manifest_sha, total_bytes = excluded.total_bytes,
			files = excluded.files, missing = excluded.missing, history = excluded.history`,
		b.ExecID, b.TaskID, b.Worker, sqlitedb.ToNS(b.PulledAt), b.ManifestSHA, b.TotalBytes, string(files),
		string(missing), string(history))
	if err != nil {
		return fmt.Errorf("runs: saving the bundle of %s: %w", b.ExecID, err)
	}
	return nil
}

// mergeBundles is next with everything old referenced kept (SaveBundle).
func mergeBundles(old, next Bundle) Bundle {
	byPath := map[string]File{}
	for _, f := range next.Files {
		byPath[f.Path] = f
	}
	history := append([]File{}, old.History...)
	seen := map[File]bool{}
	for _, h := range history {
		seen[h] = true
	}
	for _, f := range old.Files {
		cur, ok := byPath[f.Path]
		switch {
		case !ok:
			byPath[f.Path] = f // gone from the desktop since: kept from the earlier pull
		case cur.SHA256 != f.SHA256 && !seen[f]:
			history = append(history, f)
			seen[f] = true
		}
	}
	files := make([]File, 0, len(byPath))
	for _, f := range byPath {
		files = append(files, f)
	}
	held := map[string]bool{}
	for _, f := range files {
		if name := partOf(next.TaskID, f.Path); name != "" {
			held[name] = true
		}
	}
	missing := make([]string, 0, len(next.Missing))
	for _, m := range next.Missing {
		if name, ok := strings.CutSuffix(m, ": not present"); ok && held[name] {
			m = name + ": not present on the desktop now; kept from an earlier pull"
		}
		missing = append(missing, m)
	}
	next.Files, next.History, next.Missing = files, history, missing
	return next
}

// Bundle returns the stored bundle of execID; ok is false when none was pulled.
func (s *Store) Bundle(execID string) (Bundle, bool, error) {
	var b Bundle
	var pulled int64
	var files, missing, history string
	err := s.db.QueryRow(`SELECT exec_id, task_id, worker_id, pulled_ns, manifest_sha, total_bytes, files, missing,
		history FROM run_bundles WHERE exec_id = ?`, execID).Scan(&b.ExecID, &b.TaskID, &b.Worker, &pulled,
		&b.ManifestSHA, &b.TotalBytes, &files, &missing, &history)
	if err == sql.ErrNoRows {
		return Bundle{}, false, nil
	}
	if err != nil {
		return Bundle{}, false, fmt.Errorf("runs: reading the bundle of %s: %w", execID, err)
	}
	b.PulledAt = sqlitedb.FromNS(pulled)
	if err := json.Unmarshal([]byte(files), &b.Files); err != nil {
		return Bundle{}, false, fmt.Errorf("runs: the bundle of %s has unreadable files: %w", execID, err)
	}
	if err := json.Unmarshal([]byte(missing), &b.Missing); err != nil {
		return Bundle{}, false, fmt.Errorf("runs: the bundle of %s has an unreadable missing list: %w", execID, err)
	}
	if err := json.Unmarshal([]byte(history), &b.History); err != nil {
		return Bundle{}, false, fmt.Errorf("runs: the bundle of %s has an unreadable history: %w", execID, err)
	}
	if b.Files == nil {
		b.Files = []File{}
	}
	if b.History == nil {
		b.History = []File{}
	}
	if b.Missing == nil {
		b.Missing = []string{}
	}
	return b, true, nil
}

// Has reports whether a bundle of execID is stored.
func (s *Store) Has(execID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_bundles WHERE exec_id = ?`, execID).Scan(&n); err != nil {
		return false, fmt.Errorf("runs: looking up the bundle of %s: %w", execID, err)
	}
	return n > 0, nil
}
