// Package skillhub is the control plane's registry of skills shared between
// workers. A skill is learned on one desktop (agent/decurix_agent/computer/
// skills.py keeps it there as one JSON file); publishing copies it here as a
// numbered version, and installing copies a version onto another desktop.
// Skills can be assigned to roles, and a worker carries roles, so a worker
// that should have a skill and lacks it - or holds an older version - is
// reported by Outdated and brought up to date by a sync.
package skillhub

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// BundleFormat is the format tag a desktop writes on an exported skill.
const BundleFormat = "decurix-skill/1"

// MaxBundleBytes bounds one skill bundle: a skill carries its own steps, never
// recordings or screenshots. It stays under the desktop agent's 1 MiB request
// cap (http_api.MAX_BODY) with room for the install call's own fields, since a
// bundle the registry took but no desktop can receive would be useless.
const MaxBundleBytes = 1000 << 10

// maxRoles bounds how many roles one worker carries.
const maxRoles = 32

// maxNote bounds a version's note.
const maxNote = 2000

// NamePattern is the skill-name rule the desktop's skills.py enforces too.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// rolePattern keeps a role a short plain label.
var rolePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)

// workerPattern keeps a worker id safe to store and to show; the desktop
// manager resolves the id itself when a request reaches the desktop.
var workerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// volatileKeys are top-level skill fields one desktop changes by using a skill
// (run counts, its own run history, where it was installed from). They are
// left out of a bundle's sha, so republishing an unchanged skill after a few
// runs is not a new version.
var volatileKeys = []string{"runs", "experience", "installed_from"}

// ErrInvalid marks input the store refused; its text is safe to show.
var ErrInvalid = errors.New("invalid")

// ErrNotFound marks a skill or version the registry does not have.
var ErrNotFound = errors.New("not found")

// invalid builds an ErrInvalid with words a person can act on.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

var schema = []string{
	`CREATE TABLE skillhub_versions (
		name           TEXT NOT NULL,
		version        INTEGER NOT NULL,
		sha            TEXT NOT NULL,
		bundle         BLOB NOT NULL,
		published_from TEXT NOT NULL,
		note           TEXT NOT NULL,
		published_ns   INTEGER NOT NULL,
		PRIMARY KEY (name, version)
	)`,
	`CREATE TABLE skillhub_installs (
		worker       TEXT NOT NULL,
		name         TEXT NOT NULL,
		version      INTEGER NOT NULL,
		installed_ns INTEGER NOT NULL,
		PRIMARY KEY (worker, name)
	)`,
	`CREATE TABLE skillhub_role_skills (
		role TEXT NOT NULL,
		name TEXT NOT NULL,
		PRIMARY KEY (role, name)
	)`,
	`CREATE TABLE skillhub_worker_roles (
		worker TEXT NOT NULL,
		role   TEXT NOT NULL,
		PRIMARY KEY (worker, role)
	)`,
}

// Version is one published version of a skill. Bundle is filled only by Get.
type Version struct {
	Name          string          `json:"name"`
	Version       int             `json:"version"`
	SHA           string          `json:"sha"`
	Size          int             `json:"size"`
	PublishedFrom string          `json:"published_from"`
	Note          string          `json:"note"`
	PublishedAt   time.Time       `json:"published_at"`
	Bundle        json.RawMessage `json:"bundle,omitempty"`
}

// Install records which version of a skill a worker holds.
type Install struct {
	Worker      string    `json:"worker"`
	Name        string    `json:"name"`
	Version     int       `json:"version"`
	InstalledAt time.Time `json:"installed_at"`
}

// Entry is one skill as List shows it: its versions (newest first), the roles
// it is assigned to and the workers holding it.
type Entry struct {
	Name     string    `json:"name"`
	Latest   int       `json:"latest"`
	Versions []Version `json:"versions"`
	Roles    []string  `json:"roles"`
	Installs []Install `json:"installs"`
}

// Outdated is a skill a worker should be brought up to: Installed is 0 when it
// lacks the skill.
type Outdated struct {
	Name      string `json:"name"`
	Installed int    `json:"installed"`
	Latest    int    `json:"latest"`
	Reason    string `json:"reason"` // "behind" or "missing"
	Role      string `json:"role,omitempty"`
}

// Store keeps the registry in SQLite.
type Store struct {
	db *sql.DB
	mu sync.Mutex // read-then-write steps (Publish, SetWorkerRoles)
}

// NewStore opens the registry on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "skillhub", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

// ValidName reports whether name is a skill name a desktop accepts.
func ValidName(name string) bool { return NamePattern.MatchString(name) }

// ValidWorker reports whether id is acceptable as a worker id.
func ValidWorker(id string) bool { return workerPattern.MatchString(id) }

// ValidRole reports whether role is acceptable as a role label.
func ValidRole(role string) bool { return rolePattern.MatchString(role) }

// BundleSHA checks a bundle's shape and returns the sha256 of its skill with
// the volatile fields left out, as canonical JSON (sorted keys at every
// depth), so neither key order nor run counts make a new version.
func BundleSHA(name string, bundle []byte) (string, error) {
	if len(bundle) > MaxBundleBytes {
		return "", invalid("the skill is larger than %d bytes", MaxBundleBytes)
	}

	var parsed struct {
		Format string                     `json:"format"`
		Skill  map[string]json.RawMessage `json:"skill"`
	}
	if err := json.Unmarshal(bundle, &parsed); err != nil {
		return "", invalid("the skill bundle is not JSON")
	}
	if parsed.Format != BundleFormat {
		return "", invalid("not a %s bundle", BundleFormat)
	}
	if parsed.Skill == nil {
		return "", invalid("the bundle holds no skill")
	}

	var skillName string
	if err := json.Unmarshal(parsed.Skill["name"], &skillName); err != nil || skillName != name {
		return "", invalid("the bundle holds a different skill than %s", name)
	}

	for _, key := range volatileKeys {
		delete(parsed.Skill, key)
	}

	canonical, err := canonicalJSON(parsed.Skill)
	if err != nil {
		return "", invalid("the skill bundle is not JSON")
	}

	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON encodes v with sorted keys at every depth: decoding into
// generic values and encoding again sorts every object's keys.
func canonicalJSON(v map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}

	return json.Marshal(generic)
}

// Publish stores bundle as the next version of name, unless the latest
// version already has the same sha; created says which. from is the worker it
// came from.
func (s *Store) Publish(name string, bundle []byte, from, note string) (Version, bool, error) {
	if !ValidName(name) {
		return Version{}, false, invalid("not a skill name: %q", name)
	}
	if from != "" && !ValidWorker(from) {
		return Version{}, false, invalid("not a worker id")
	}
	note = strings.TrimSpace(note)
	if len(note) > maxNote {
		return Version{}, false, invalid("the note is longer than %d bytes", maxNote)
	}

	sha, err := BundleSHA(name, bundle)
	if err != nil {
		return Version{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	latest, err := s.latest(name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Version{}, false, err
	}
	if err == nil && latest.SHA == sha {
		return latest, false, nil
	}

	v := Version{
		Name:          name,
		Version:       latest.Version + 1,
		SHA:           sha,
		Size:          len(bundle),
		PublishedFrom: from,
		Note:          note,
		PublishedAt:   time.Now().UTC(),
	}

	if _, err := s.db.Exec(
		`INSERT INTO skillhub_versions (name, version, sha, bundle, published_from, note, published_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.Name, v.Version, v.SHA, bundle, v.PublishedFrom, v.Note, sqlitedb.ToNS(v.PublishedAt),
	); err != nil {
		return Version{}, false, fmt.Errorf("skillhub: publish %s: %w", name, err)
	}

	return v, true, nil
}

// latest returns a skill's newest version, without its bundle.
func (s *Store) latest(name string) (Version, error) {
	v, err := scanVersion(s.db.QueryRow(
		`SELECT name, version, sha, length(bundle), published_from, note, published_ns
		 FROM skillhub_versions WHERE name = ? ORDER BY version DESC LIMIT 1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}

	return v, err
}

// scanVersion reads a version row without its bundle.
func scanVersion(row sqlitedb.Scanner) (Version, error) {
	var v Version
	var ns int64
	if err := row.Scan(&v.Name, &v.Version, &v.SHA, &v.Size, &v.PublishedFrom, &v.Note, &ns); err != nil {
		return Version{}, err
	}
	v.PublishedAt = sqlitedb.FromNS(ns)

	return v, nil
}

// scanInstall reads an install row.
func scanInstall(row sqlitedb.Scanner) (Install, error) {
	var in Install
	var ns int64
	if err := row.Scan(&in.Worker, &in.Name, &in.Version, &ns); err != nil {
		return Install{}, err
	}
	in.InstalledAt = sqlitedb.FromNS(ns)

	return in, nil
}

// scanString reads a one-column text row.
func scanString(row sqlitedb.Scanner) (string, error) {
	var s string
	err := row.Scan(&s)
	return s, err
}

// Get returns one version of a skill with its bundle; version 0 is the latest.
func (s *Store) Get(name string, version int) (Version, error) {
	if !ValidName(name) || version < 0 {
		return Version{}, ErrNotFound
	}

	query := `SELECT name, version, sha, bundle, published_from, note, published_ns
		FROM skillhub_versions WHERE name = ? AND version = ?`
	args := []any{name, version}
	if version == 0 {
		query = `SELECT name, version, sha, bundle, published_from, note, published_ns
			FROM skillhub_versions WHERE name = ? ORDER BY version DESC LIMIT 1`
		args = args[:1]
	}

	var v Version
	var bundle []byte
	var ns int64
	err := s.db.QueryRow(query, args...).Scan(&v.Name, &v.Version, &v.SHA, &bundle, &v.PublishedFrom, &v.Note, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("skillhub: get %s: %w", name, err)
	}

	v.PublishedAt = sqlitedb.FromNS(ns)
	v.Size = len(bundle)
	v.Bundle = json.RawMessage(bundle)

	return v, nil
}

// List returns every published skill by name, each with its versions (newest
// first, without bundles), roles and installs.
func (s *Store) List() ([]Entry, error) {
	versions, err := sqlitedb.QueryAll(s.db, scanVersion,
		`SELECT name, version, sha, length(bundle), published_from, note, published_ns
		 FROM skillhub_versions ORDER BY name, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: list: %w", err)
	}

	installs, err := sqlitedb.QueryAll(s.db, scanInstall,
		`SELECT worker, name, version, installed_ns FROM skillhub_installs ORDER BY name, worker`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: list installs: %w", err)
	}

	roles, err := sqlitedb.QueryAll(s.db, scanPair, `SELECT name, role FROM skillhub_role_skills ORDER BY name, role`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: list roles: %w", err)
	}

	byName := map[string]*Entry{}
	order := []string{}
	for _, v := range versions {
		e, ok := byName[v.Name]
		if !ok {
			e = &Entry{Name: v.Name, Latest: v.Version, Versions: []Version{}, Roles: []string{}, Installs: []Install{}}
			byName[v.Name] = e
			order = append(order, v.Name)
		}
		e.Versions = append(e.Versions, v)
	}
	for _, in := range installs {
		if e, ok := byName[in.Name]; ok {
			e.Installs = append(e.Installs, in)
		}
	}
	for _, r := range roles {
		if e, ok := byName[r[0]]; ok {
			e.Roles = append(e.Roles, r[1])
		}
	}

	out := make([]Entry, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}

	return out, nil
}

// scanPair reads a two-column text row.
func scanPair(row sqlitedb.Scanner) ([2]string, error) {
	var p [2]string
	err := row.Scan(&p[0], &p[1])
	return p, err
}

// RecordInstall notes that worker now holds version of name.
func (s *Store) RecordInstall(worker, name string, version int) error {
	if !ValidWorker(worker) {
		return invalid("not a worker id")
	}
	if !ValidName(name) || version < 1 {
		return invalid("not a skill version")
	}

	if _, err := s.db.Exec(
		`INSERT INTO skillhub_installs (worker, name, version, installed_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT(worker, name) DO UPDATE SET version = excluded.version, installed_ns = excluded.installed_ns`,
		worker, name, version, sqlitedb.ToNS(time.Now()),
	); err != nil {
		return fmt.Errorf("skillhub: record install: %w", err)
	}

	return nil
}

// Installs returns what worker holds, by skill name.
func (s *Store) Installs(worker string) ([]Install, error) {
	return sqlitedb.QueryAll(s.db, scanInstall,
		`SELECT worker, name, version, installed_ns FROM skillhub_installs WHERE worker = ? ORDER BY name`, worker)
}

// ForgetWorker drops a worker's installs and roles, for when its desktop is deleted.
func (s *Store) ForgetWorker(worker string) error {
	for _, q := range []string{
		`DELETE FROM skillhub_installs WHERE worker = ?`,
		`DELETE FROM skillhub_worker_roles WHERE worker = ?`,
	} {
		if _, err := s.db.Exec(q, worker); err != nil {
			return fmt.Errorf("skillhub: forget worker: %w", err)
		}
	}

	return nil
}

// Assign makes role carry the skill name; the skill must have been published.
func (s *Store) Assign(name, role string) error {
	role = strings.TrimSpace(role)
	if !ValidName(name) {
		return invalid("not a skill name: %q", name)
	}
	if !ValidRole(role) {
		return invalid("not a role: %q", role)
	}
	if _, err := s.latest(name); err != nil {
		return err
	}

	if _, err := s.db.Exec(
		`INSERT INTO skillhub_role_skills (role, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, role, name,
	); err != nil {
		return fmt.Errorf("skillhub: assign: %w", err)
	}

	return nil
}

// Unassign takes the skill name off role. Workers keep what they installed.
func (s *Store) Unassign(name, role string) error {
	if _, err := s.db.Exec(`DELETE FROM skillhub_role_skills WHERE role = ? AND name = ?`, strings.TrimSpace(role), name); err != nil {
		return fmt.Errorf("skillhub: unassign: %w", err)
	}

	return nil
}

// SetWorkerRoles replaces the roles worker carries and returns them, sorted
// and without duplicates.
func (s *Store) SetWorkerRoles(worker string, roles []string) ([]string, error) {
	if !ValidWorker(worker) {
		return nil, invalid("not a worker id")
	}
	if len(roles) > maxRoles {
		return nil, invalid("at most %d roles", maxRoles)
	}

	seen := map[string]bool{}
	clean := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if !ValidRole(role) {
			return nil, invalid("not a role: %q", role)
		}
		if !seen[role] {
			seen[role] = true
			clean = append(clean, role)
		}
	}
	sort.Strings(clean)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("skillhub: set roles: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM skillhub_worker_roles WHERE worker = ?`, worker); err != nil {
		return nil, fmt.Errorf("skillhub: set roles: %w", err)
	}
	for _, role := range clean {
		if _, err := tx.Exec(`INSERT INTO skillhub_worker_roles (worker, role) VALUES (?, ?)`, worker, role); err != nil {
			return nil, fmt.Errorf("skillhub: set roles: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("skillhub: set roles: %w", err)
	}

	return clean, nil
}

// WorkerRoles returns the roles worker carries, sorted. It satisfies
// knowledge.RoleSource, so scoped knowledge reads roles from here.
func (s *Store) WorkerRoles(worker string) ([]string, error) {
	return sqlitedb.QueryAll(s.db, scanString, `SELECT role FROM skillhub_worker_roles WHERE worker = ? ORDER BY role`, worker)
}

// AllWorkerRoles returns every worker's roles.
func (s *Store) AllWorkerRoles() (map[string][]string, error) {
	rows, err := sqlitedb.QueryAll(s.db, scanPair, `SELECT worker, role FROM skillhub_worker_roles ORDER BY worker, role`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: worker roles: %w", err)
	}

	out := map[string][]string{}
	for _, r := range rows {
		out[r[0]] = append(out[r[0]], r[1])
	}

	return out, nil
}

// Outdated returns the skills worker should be brought up to: the ones it
// holds behind the latest version, and the ones a role of it carries that it
// lacks. Sorted by name.
func (s *Store) Outdated(worker string) ([]Outdated, error) {
	installs, err := s.Installs(worker)
	if err != nil {
		return nil, fmt.Errorf("skillhub: outdated: %w", err)
	}
	held := map[string]int{}
	for _, in := range installs {
		held[in.Name] = in.Version
	}

	latest, err := sqlitedb.QueryAll(s.db, func(sc sqlitedb.Scanner) (Version, error) {
		var v Version
		err := sc.Scan(&v.Name, &v.Version)
		return v, err
	}, `SELECT name, MAX(version) FROM skillhub_versions GROUP BY name`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: outdated: %w", err)
	}
	newest := map[string]int{}
	for _, v := range latest {
		newest[v.Name] = v.Version
	}

	wanted, err := sqlitedb.QueryAll(s.db, scanPair,
		`SELECT rs.name, rs.role FROM skillhub_role_skills rs
		 JOIN skillhub_worker_roles wr ON wr.role = rs.role
		 WHERE wr.worker = ? ORDER BY rs.name, rs.role`, worker)
	if err != nil {
		return nil, fmt.Errorf("skillhub: outdated: %w", err)
	}

	out := make([]Outdated, 0)
	listed := map[string]bool{}
	for name, version := range held {
		if top, ok := newest[name]; ok && version < top {
			out = append(out, Outdated{Name: name, Installed: version, Latest: top, Reason: "behind"})
			listed[name] = true
		}
	}
	for _, w := range wanted {
		name, role := w[0], w[1]
		if _, ok := held[name]; ok || listed[name] {
			continue
		}
		top, ok := newest[name]
		if !ok {
			continue
		}
		out = append(out, Outdated{Name: name, Latest: top, Reason: "missing", Role: role})
		listed[name] = true
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}
