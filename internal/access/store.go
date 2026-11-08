// Package access is who is acting and what they may approve: users with
// roles, sign-in sessions for the gated listener, approval policies kept as
// data, and the log of every approval decision with the person who made it.
//
// The primary listener has no login: whoever reaches it is the person at this
// computer, the built-in LocalOwner, who satisfies every policy. On the gated
// listener the access code (internal/lanaccess) still decides who gets in at
// all; a user's own name and password then say which person, inside it, is
// replying. See docs/access.md.
package access

import (
	"database/sql"
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

// Role names the built-in owner role; it satisfies any approver role.
const RoleOwner = "owner"

// LocalOwnerID is the user id decisions made on the primary listener are logged under.
const LocalOwnerID = "local-owner"

// LocalOwner is the person at this computer: the actor on the primary listener.
var LocalOwner = User{ID: LocalOwnerID, Name: "Owner (this computer)", Roles: []string{RoleOwner}, Local: true}

// SessionTTL is how long a sign-in on the gated listener lasts.
var SessionTTL = 12 * time.Hour

// Errors a caller may show as they are: each has a fixed message (Message).
var (
	ErrNotFound     = errors.New("access: not found")
	ErrNameTaken    = errors.New("access: name taken")
	ErrBadName      = errors.New("access: bad name")
	ErrBadPassword  = errors.New("access: bad password")
	ErrBadRoles     = errors.New("access: bad roles")
	ErrBadPolicy    = errors.New("access: bad policy")
	ErrBadMatch     = errors.New("access: bad action_match")
	ErrSignInFailed = errors.New("access: sign-in failed")
	ErrLockedOut    = errors.New("access: locked out")
)

// messages are the client-facing words for the errors above.
var messages = map[error]string{
	ErrNotFound:     "not found",
	ErrNameTaken:    "that name is already in use",
	ErrBadName:      "a name is 1 to 64 characters, with no control characters",
	ErrBadPassword:  "a password is 8 to 256 characters",
	ErrBadRoles:     "a role is 1 to 32 lowercase letters, digits, '-' or '_'",
	ErrBadPolicy:    "a policy needs at least one approver role, min_approvers 1 or 2, and amount_over not negative",
	ErrBadMatch:     "action_match is not a valid pattern",
	ErrSignInFailed: "that name and password do not match",
	ErrLockedOut:    "too many failed sign-ins; try again in a few minutes",
}

// Message returns the fixed client-facing message for err, and whether it is one of this package's known errors.
func Message(err error) (string, bool) {
	for known, msg := range messages {
		if errors.Is(err, known) {
			return msg, true
		}
	}
	return "", false
}

// User is a person who can sign in on the gated listener (or LocalOwner).
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Roles     []string  `json:"roles"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	// Local marks the built-in owner at this computer; never stored.
	Local bool `json:"local,omitempty"`
}

// HasRole reports whether u holds role.
func (u User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Store keeps users, sessions, policies, worker roles and the approvals log.
type Store struct {
	db  *sql.DB
	now func() time.Time

	// mu serialises Decide's read-then-write on the approvals log.
	mu sync.Mutex

	// failures counts recent failed sign-ins per lowercased name.
	failMu   sync.Mutex
	failures map[string]*failState
}

type failState struct {
	count int
	first time.Time
	until time.Time
}

// migrations are the access component's schema; only ever append.
var migrations = []string{
	`CREATE TABLE access_users (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT NOT NULL,
		roles         TEXT NOT NULL DEFAULT '[]',
		disabled      INTEGER NOT NULL DEFAULT 0,
		created_ns    INTEGER NOT NULL
	)`,
	`CREATE TABLE access_sessions (
		token_hash TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL REFERENCES access_users (id) ON DELETE CASCADE,
		expires_ns INTEGER NOT NULL
	)`,
	`CREATE TABLE access_policies (
		id             TEXT PRIMARY KEY,
		worker         TEXT NOT NULL DEFAULT '*',
		worker_role    TEXT NOT NULL DEFAULT '',
		action_match   TEXT NOT NULL DEFAULT '',
		amount_over    REAL,
		approver_roles TEXT NOT NULL,
		min_approvers  INTEGER NOT NULL DEFAULT 1,
		note           TEXT NOT NULL DEFAULT '',
		created_ns     INTEGER NOT NULL
	)`,
	`CREATE TABLE access_approvals (
		id              TEXT PRIMARY KEY,
		worker          TEXT NOT NULL,
		task_id         TEXT NOT NULL DEFAULT '',
		action          TEXT NOT NULL,
		pending_key     TEXT NOT NULL,
		decided_by      TEXT NOT NULL,
		decided_by_name TEXT NOT NULL DEFAULT '',
		decision        TEXT NOT NULL,
		outcome         TEXT NOT NULL,
		policies        TEXT NOT NULL DEFAULT '[]',
		at_ns           INTEGER NOT NULL
	)`,
	`CREATE INDEX access_approvals_key ON access_approvals (pending_key, at_ns)`,
	`CREATE INDEX access_approvals_at ON access_approvals (at_ns)`,
	`CREATE TABLE access_worker_roles (
		worker TEXT PRIMARY KEY,
		roles  TEXT NOT NULL DEFAULT '[]'
	)`,
}

// NewStore opens the access component on db, applying its schema.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "access", migrations); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now, failures: map[string]*failState{}}, nil
}

var roleRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// normalizeRoles lowercases, validates, de-duplicates and sorts roles.
func normalizeRoles(roles []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		if !roleRe.MatchString(r) {
			return nil, ErrBadRoles
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out, nil
}

// validName trims name and reports whether it is acceptable as a user name.
func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", false
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	return name, true
}

func validPassword(pw string) bool {
	return len(pw) >= 8 && len(pw) <= 256
}

func encodeList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeList(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// CreateUser adds a user with a hashed password and the given roles.
func (s *Store) CreateUser(name, password string, roles []string) (User, error) {
	name, ok := validName(name)
	if !ok {
		return User{}, ErrBadName
	}
	if strings.EqualFold(name, LocalOwner.Name) {
		return User{}, ErrNameTaken
	}
	if !validPassword(password) {
		return User{}, ErrBadPassword
	}
	roles, err := normalizeRoles(roles)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}

	u := User{ID: sqlitedb.NewID("u_", 12), Name: name, Roles: roles, CreatedAt: s.now().UTC()}
	_, err = s.db.Exec(`INSERT INTO access_users (id, name, password_hash, roles, disabled, created_ns) VALUES (?, ?, ?, ?, 0, ?)`,
		u.ID, u.Name, hash, encodeList(roles), sqlitedb.ToNS(u.CreatedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return User{}, ErrNameTaken
		}
		return User{}, fmt.Errorf("access: create user: %w", err)
	}
	return u, nil
}

const userColumns = `id, name, roles, disabled, created_ns`

func scanUser(row sqlitedb.Scanner) (User, error) {
	var u User
	var roles string
	var disabled int
	var created int64
	if err := row.Scan(&u.ID, &u.Name, &roles, &disabled, &created); err != nil {
		return User{}, err
	}
	u.Roles = decodeList(roles)
	u.Disabled = disabled != 0
	u.CreatedAt = sqlitedb.FromNS(created)
	return u, nil
}

// User returns the user with id.
func (s *Store) User(id string) (User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM access_users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Users lists every user, by name.
func (s *Store) Users() ([]User, error) {
	return sqlitedb.QueryAll(s.db, scanUser, `SELECT `+userColumns+` FROM access_users ORDER BY name COLLATE NOCASE`)
}

// HasUsers reports whether any user account exists (enabled or not): from the first one on, a reply through the
// gated listener needs a sign-in (docs/access.md).
func (s *Store) HasUsers() (bool, error) {
	var any int
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM access_users)`).Scan(&any); err != nil {
		return false, fmt.Errorf("access: users: %w", err)
	}
	return any != 0, nil
}

// SetRoles replaces a user's roles.
func (s *Store) SetRoles(id string, roles []string) error {
	roles, err := normalizeRoles(roles)
	if err != nil {
		return err
	}
	return s.updateUser(`UPDATE access_users SET roles = ? WHERE id = ?`, encodeList(roles), id)
}

// SetDisabled turns a user's sign-in off or on; turning it off ends their sessions.
func (s *Store) SetDisabled(id string, disabled bool) error {
	flag := 0
	if disabled {
		flag = 1
	}
	if err := s.updateUser(`UPDATE access_users SET disabled = ? WHERE id = ?`, flag, id); err != nil {
		return err
	}
	if disabled {
		_, err := s.db.Exec(`DELETE FROM access_sessions WHERE user_id = ?`, id)
		return err
	}
	return nil
}

// SetPassword replaces a user's password and ends their sessions.
func (s *Store) SetPassword(id, password string) error {
	if !validPassword(password) {
		return ErrBadPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.updateUser(`UPDATE access_users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM access_sessions WHERE user_id = ?`, id)
	return err
}

// DeleteUser removes a user and their sessions; the approvals log keeps their name.
func (s *Store) DeleteUser(id string) error {
	res, err := s.db.Exec(`DELETE FROM access_users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("access: delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) updateUser(query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("access: update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// failure limits for Authenticate.
const (
	maxFailures   = 5
	failureWindow = 15 * time.Minute
	lockoutFor    = 15 * time.Minute
)

// Authenticate checks name and password and returns the user; a disabled
// user, an unknown name and a wrong password fail alike. Repeated failures
// for one name lock it out for a while.
func (s *Store) Authenticate(name, password string) (User, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if s.lockedOut(key) {
		return User{}, ErrLockedOut
	}

	var u User
	var hash, roles string
	var disabled int
	var created int64
	err := s.db.QueryRow(`SELECT id, name, password_hash, roles, disabled, created_ns FROM access_users WHERE name = ?`,
		strings.TrimSpace(name)).Scan(&u.ID, &u.Name, &hash, &roles, &disabled, &created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Spend the same work as a real check, so timing does not tell which names exist.
		CheckPassword(dummyHash(), password)
		s.recordFailure(key)
		return User{}, ErrSignInFailed
	case err != nil:
		return User{}, fmt.Errorf("access: authenticate: %w", err)
	}

	if !CheckPassword(hash, password) || disabled != 0 {
		s.recordFailure(key)
		return User{}, ErrSignInFailed
	}

	s.failMu.Lock()
	delete(s.failures, key)
	s.failMu.Unlock()

	u.Roles = decodeList(roles)
	u.CreatedAt = sqlitedb.FromNS(created)
	return u, nil
}

func (s *Store) lockedOut(key string) bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	f := s.failures[key]
	return f != nil && s.now().Before(f.until)
}

func (s *Store) recordFailure(key string) {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	now := s.now()
	for k, f := range s.failures {
		if now.Sub(f.first) > failureWindow && now.After(f.until) {
			delete(s.failures, k)
		}
	}
	f := s.failures[key]
	if f == nil || now.Sub(f.first) > failureWindow {
		f = &failState{first: now}
		s.failures[key] = f
	}
	f.count++
	if f.count >= maxFailures {
		f.until = now.Add(lockoutFor)
		f.count = 0
		f.first = now
	}
}

// NewSession starts a sign-in session for userID and returns its token; only the token's hash is stored.
func (s *Store) NewSession(userID string) (string, time.Time, error) {
	token := randomToken()
	expires := s.now().Add(SessionTTL)
	now := sqlitedb.ToNS(s.now())
	if _, err := s.db.Exec(`DELETE FROM access_sessions WHERE expires_ns <= ?`, now); err != nil {
		return "", time.Time{}, fmt.Errorf("access: prune sessions: %w", err)
	}
	if _, err := s.db.Exec(`INSERT INTO access_sessions (token_hash, user_id, expires_ns) VALUES (?, ?, ?)`,
		tokenHash(token), userID, sqlitedb.ToNS(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("access: new session: %w", err)
	}
	return token, expires, nil
}

// SessionUser returns the user a session token belongs to, if it is unexpired and the user is enabled.
func (s *Store) SessionUser(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	var userID string
	var expires int64
	err := s.db.QueryRow(`SELECT user_id, expires_ns FROM access_sessions WHERE token_hash = ?`, tokenHash(token)).Scan(&userID, &expires)
	if err != nil || !s.now().Before(sqlitedb.FromNS(expires)) {
		return User{}, false
	}
	u, err := s.User(userID)
	if err != nil || u.Disabled {
		return User{}, false
	}
	return u, true
}

// EndSession forgets a session token.
func (s *Store) EndSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM access_sessions WHERE token_hash = ?`, tokenHash(token))
	return err
}

// SetWorkerRoles replaces the roles a worker (desktop id) has, which policies' worker_role match against.
func (s *Store) SetWorkerRoles(worker string, roles []string) error {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return ErrBadPolicy
	}
	roles, err := normalizeRoles(roles)
	if err != nil {
		return err
	}
	if len(roles) == 0 {
		_, err = s.db.Exec(`DELETE FROM access_worker_roles WHERE worker = ?`, worker)
		return err
	}
	_, err = s.db.Exec(`INSERT INTO access_worker_roles (worker, roles) VALUES (?, ?)
		ON CONFLICT(worker) DO UPDATE SET roles = excluded.roles`, worker, encodeList(roles))
	return err
}

// WorkerRoles returns the roles set for a worker, empty when none.
func (s *Store) WorkerRoles(worker string) []string {
	var roles string
	if err := s.db.QueryRow(`SELECT roles FROM access_worker_roles WHERE worker = ?`, worker).Scan(&roles); err != nil {
		return []string{}
	}
	return decodeList(roles)
}

// AllWorkerRoles returns every worker that has roles set.
func (s *Store) AllWorkerRoles() (map[string][]string, error) {
	type pair struct {
		worker string
		roles  []string
	}
	rows, err := sqlitedb.QueryAll(s.db, func(r sqlitedb.Scanner) (pair, error) {
		var p pair
		var roles string
		err := r.Scan(&p.worker, &roles)
		p.roles = decodeList(roles)
		return p, err
	}, `SELECT worker, roles FROM access_worker_roles ORDER BY worker`)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(rows))
	for _, p := range rows {
		out[p.worker] = p.roles
	}
	return out, nil
}
