// Package dag stores user-authored DAGs of nodes (a desktop plus request
// text) and edges, and the runs that execute them - a chain of tickets (the
// same internal/tasks request a schedule or a chat message submits) fired
// one after another as each node's task succeeds, entirely wired up by
// whoever built the DAG: nothing here infers a node or an edge on its own.
//
// A run starts at the DAG's one head node, triggered by its own cron (see
// SetCron/DueCron, which reuse internal/schedule's Parse/Expr rather than
// duplicating a cron parser) or a manual "run now", and follows edges
// outward: a node dispatches once every one of its parents has succeeded,
// and a node's failure skips everything reachable only through it - the
// same default trigger rule Airflow and Argo Workflows both start from. See
// runner.go for the Runner that actually drives a run forward, and
// internal/httpapi's dags.go for how a person edits and triggers one.
package dag

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/flowera/internal/schedule"
	"github.com/YKoustubhRao/flowera/internal/sqlitedb"
)

// Run statuses.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// Trigger kinds.
const (
	TriggerCron   = "cron"
	TriggerManual = "manual"
)

// RunNode statuses.
const (
	NodePending   = "pending"
	NodeRunning   = "running"
	NodeSucceeded = "succeeded"
	NodeFailed    = "failed"
	NodeSkipped   = "skipped"
)

// ErrNotFound is returned for a DAG, node, edge or run that does not exist.
var ErrNotFound = errors.New("dag: not found")

// nodeNamePattern is what a node's Name must match: charset-restricted so
// {{nodes.<name>.reply}} always parses unambiguously.
var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// DAG is a user-authored template: a set of nodes and edges, one of which
// (HeadNodeID) is where every run of it begins.
type DAG struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	HeadNodeID string    `json:"head_node_id"`
	CronExpr   string    `json:"cron_expr"`
	NextRunAt  time.Time `json:"next_run_at"`
	LastRunAt  time.Time `json:"last_run_at"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Node is one step: a desktop to send a request to, and the request text
// (which may reference an upstream node's reply - see runner.go's
// substitute). PosX/PosY are the canvas position the editor last saved.
type Node struct {
	ID        string    `json:"id"`
	DAGID     string    `json:"dag_id"`
	Name      string    `json:"name"`
	DesktopID string    `json:"desktop_id"`
	Content   string    `json:"content"`
	PosX      float64   `json:"pos_x"`
	PosY      float64   `json:"pos_y"`
	CreatedAt time.Time `json:"created_at"`
}

// Edge is a directed dependency: To dispatches once From has succeeded (and
// every other parent To has, too).
type Edge struct {
	ID         string    `json:"id"`
	DAGID      string    `json:"dag_id"`
	FromNodeID string    `json:"from_node_id"`
	ToNodeID   string    `json:"to_node_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Run is one executed instance of a DAG.
type Run struct {
	ID        string    `json:"id"`
	DAGID     string    `json:"dag_id"`
	Status    string    `json:"status"`
	Trigger   string    `json:"trigger"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
}

// RunNode is one node's execution within a run: the task its ticket became,
// and - once terminal - the reply it ended with.
type RunNode struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id"`
	NodeID     string    `json:"node_id"`
	Status     string    `json:"status"`
	TaskID     string    `json:"task_id"`
	FinalReply string    `json:"final_reply"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
}

// Summary is one DAG plus what the list page needs.
type Summary struct {
	DAG       DAG  `json:"dag"`
	NodeCount int  `json:"node_count"`
	EdgeCount int  `json:"edge_count"`
	LastRun   *Run `json:"last_run,omitempty"`
}

// Detail is everything the editor page needs in one call.
type Detail struct {
	DAG        DAG    `json:"dag"`
	Nodes      []Node `json:"nodes"`
	Edges      []Edge `json:"edges"`
	RecentRuns []Run  `json:"recent_runs"`
}

// RunDetail is one run's per-node state, for the run-detail graph view.
type RunDetail struct {
	Run      Run       `json:"run"`
	Nodes    []Node    `json:"nodes"`
	Edges    []Edge    `json:"edges"`
	RunNodes []RunNode `json:"run_nodes"`
}

var schema = []string{
	`CREATE TABLE dags (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		head_node_id TEXT NOT NULL DEFAULT '',
		cron_expr    TEXT NOT NULL DEFAULT '',
		next_run_ns  INTEGER NOT NULL DEFAULT 0,
		last_run_ns  INTEGER NOT NULL DEFAULT 0,
		created_ns   INTEGER NOT NULL,
		updated_ns   INTEGER NOT NULL
	)`,
	`CREATE INDEX dags_due ON dags (next_run_ns)`,
	`CREATE TABLE dag_nodes (
		id         TEXT PRIMARY KEY,
		dag_id     TEXT NOT NULL REFERENCES dags (id) ON DELETE CASCADE,
		name       TEXT NOT NULL,
		desktop_id TEXT NOT NULL,
		content    TEXT NOT NULL DEFAULT '',
		pos_x      REAL NOT NULL DEFAULT 0,
		pos_y      REAL NOT NULL DEFAULT 0,
		created_ns INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX dag_nodes_name ON dag_nodes (dag_id, name)`,
	`CREATE INDEX dag_nodes_dag ON dag_nodes (dag_id)`,
	`CREATE TABLE dag_edges (
		id           TEXT PRIMARY KEY,
		dag_id       TEXT NOT NULL REFERENCES dags (id) ON DELETE CASCADE,
		from_node_id TEXT NOT NULL REFERENCES dag_nodes (id) ON DELETE CASCADE,
		to_node_id   TEXT NOT NULL REFERENCES dag_nodes (id) ON DELETE CASCADE,
		created_ns   INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX dag_edges_pair ON dag_edges (from_node_id, to_node_id)`,
	`CREATE INDEX dag_edges_from ON dag_edges (from_node_id)`,
	`CREATE INDEX dag_edges_to ON dag_edges (to_node_id)`,
	// trigger_kind, not "trigger" - a bare SQLite keyword some drivers are
	// stricter about accepting unquoted as a column name than others.
	`CREATE TABLE dag_runs (
		id           TEXT PRIMARY KEY,
		dag_id       TEXT NOT NULL REFERENCES dags (id) ON DELETE CASCADE,
		status       TEXT NOT NULL,
		trigger_kind TEXT NOT NULL,
		started_ns   INTEGER NOT NULL,
		ended_ns     INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX dag_runs_dag ON dag_runs (dag_id, started_ns)`,
	`CREATE INDEX dag_runs_status ON dag_runs (status)`,
	// node_id is deliberately not a foreign key: a run's history must
	// outlive a node that gets edited or deleted later, the same
	// soft-reference precedent executions.worker_id already sets.
	`CREATE TABLE dag_run_nodes (
		id          TEXT PRIMARY KEY,
		run_id      TEXT NOT NULL REFERENCES dag_runs (id) ON DELETE CASCADE,
		node_id     TEXT NOT NULL,
		status      TEXT NOT NULL,
		task_id     TEXT NOT NULL DEFAULT '',
		final_reply TEXT NOT NULL DEFAULT '',
		started_ns  INTEGER NOT NULL DEFAULT 0,
		ended_ns    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX dag_run_nodes_run_node ON dag_run_nodes (run_id, node_id)`,
	`CREATE INDEX dag_run_nodes_task ON dag_run_nodes (task_id)`,
	// Edges(dagID) - called on every Runner tick for every active run, via
	// advance() - had no index with dag_id as its leading column; the three
	// above are all keyed by node ID instead. Appended, not merged into the
	// dag_edges block above: a schema change is always a new step, never an
	// edit to one already applied (see sqlitedb.Migrate).
	`CREATE INDEX dag_edges_dag ON dag_edges (dag_id)`,
	// DeleteNode's live-run guard filters dag_run_nodes by node_id alone;
	// dag_run_nodes_run_node above can't seek on that (run_id, its leading
	// column, isn't part of the WHERE clause there).
	`CREATE INDEX dag_run_nodes_node ON dag_run_nodes (node_id)`,
}

// Store is the SQLite-backed collection of DAGs, their nodes/edges, and the
// runs executed against them.
type Store struct {
	db *sql.DB

	// now is the test seam - defaults to time.Now().UTC.
	now func() time.Time
}

// NewStore opens the dag Store on db, creating its tables if needed.
func NewStore(db *sql.DB) (*Store, error) {
	if err := sqlitedb.Migrate(db, "dag", schema); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now().UTC()
}

// newID returns a random identifier with a prefix naming what it is.
func newID(prefix string) string {
	raw := make([]byte, 8)

	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("dag: generating id: %v", err))
	}

	return prefix + hex.EncodeToString(raw)
}

func validateNodeName(name string) (string, error) {
	name = strings.TrimSpace(name)

	if !nodeNamePattern.MatchString(name) {
		return "", fmt.Errorf("dag: node name must be 1-64 characters of letters, digits, - or _")
	}

	return name, nil
}

func scanDAG(scanner interface{ Scan(...any) error }) (DAG, error) {
	var d DAG
	var nextRun, lastRun, created, updated int64

	if err := scanner.Scan(&d.ID, &d.Name, &d.HeadNodeID, &d.CronExpr, &nextRun, &lastRun, &created, &updated); err != nil {
		return DAG{}, err
	}

	d.NextRunAt = sqlitedb.FromNS(nextRun)
	d.LastRunAt = sqlitedb.FromNS(lastRun)
	d.CreatedAt = sqlitedb.FromNS(created)
	d.UpdatedAt = sqlitedb.FromNS(updated)

	return d, nil
}

const dagColumns = `id, name, head_node_id, cron_expr, next_run_ns, last_run_ns, created_ns, updated_ns`

// ---------------------------------------------------------------------------
// DAG
// ---------------------------------------------------------------------------

// Create persists a new, empty DAG - no head, no nodes, no cron.
func (s *Store) Create(name string) (DAG, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return DAG{}, fmt.Errorf("dag: name is required")
	}

	now := s.clock()
	d := DAG{ID: newID("dag-"), Name: name, CreatedAt: now, UpdatedAt: now}

	if _, err := s.db.Exec(
		`INSERT INTO dags (id, name, created_ns, updated_ns) VALUES (?, ?, ?, ?)`,
		d.ID, d.Name, sqlitedb.ToNS(now), sqlitedb.ToNS(now),
	); err != nil {
		return DAG{}, fmt.Errorf("dag: create: %w", err)
	}

	return d, nil
}

// Get returns one DAG by ID.
func (s *Store) Get(id string) (DAG, error) {
	d, err := scanDAG(s.db.QueryRow(`SELECT `+dagColumns+` FROM dags WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return DAG{}, ErrNotFound
	}
	if err != nil {
		return DAG{}, fmt.Errorf("dag: get %s: %w", id, err)
	}

	return d, nil
}

// List returns every DAG, most recently created first, with its node/edge
// counts and last run for the list page. Counts and last runs are each one
// query across every DAG (grouped by dag_id) rather than three queries per
// DAG, so the list page's cost stays flat as the number of DAGs grows.
func (s *Store) List() ([]Summary, error) {
	rows, err := s.db.Query(`SELECT ` + dagColumns + ` FROM dags ORDER BY created_ns DESC`)
	if err != nil {
		return nil, fmt.Errorf("dag: list: %w", err)
	}

	dags := make([]DAG, 0)

	for rows.Next() {
		d, err := scanDAG(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("dag: list: %w", err)
		}

		dags = append(dags, d)
	}

	rows.Close()

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dag: list: %w", err)
	}

	nodeCounts, err := s.countsByDAG(`SELECT dag_id, COUNT(*) FROM dag_nodes GROUP BY dag_id`)
	if err != nil {
		return nil, fmt.Errorf("dag: list: %w", err)
	}

	edgeCounts, err := s.countsByDAG(`SELECT dag_id, COUNT(*) FROM dag_edges GROUP BY dag_id`)
	if err != nil {
		return nil, fmt.Errorf("dag: list: %w", err)
	}

	lastRuns, err := s.lastRuns()
	if err != nil {
		return nil, fmt.Errorf("dag: list: %w", err)
	}

	summaries := make([]Summary, 0, len(dags))

	for _, d := range dags {
		summary := Summary{DAG: d, NodeCount: nodeCounts[d.ID], EdgeCount: edgeCounts[d.ID]}

		if run, ok := lastRuns[d.ID]; ok {
			runCopy := run
			summary.LastRun = &runCopy
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

// countsByDAG runs a "SELECT dag_id, COUNT(*) ... GROUP BY dag_id" query and
// returns it as a map, for List to load every DAG's node/edge count in one
// round trip instead of one query per DAG.
func (s *Store) countsByDAG(query string) (map[string]int, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)

	for rows.Next() {
		var dagID string
		var count int

		if err := rows.Scan(&dagID, &count); err != nil {
			return nil, err
		}

		counts[dagID] = count
	}

	return counts, rows.Err()
}

// Detail returns one DAG with its nodes, edges and recent run history.
func (s *Store) Detail(id string) (Detail, error) {
	d, err := s.Get(id)
	if err != nil {
		return Detail{}, err
	}

	nodes, err := s.Nodes(id)
	if err != nil {
		return Detail{}, err
	}

	edges, err := s.Edges(id)
	if err != nil {
		return Detail{}, err
	}

	runs, err := s.Runs(id, recentRunsLimit)
	if err != nil {
		return Detail{}, err
	}

	return Detail{DAG: d, Nodes: nodes, Edges: edges, RecentRuns: runs}, nil
}

// Delete removes a DAG and, via cascade, its nodes, edges, runs and run-node
// records. A no-op if the ID is not present.
func (s *Store) Delete(id string) {
	if _, err := s.db.Exec(`DELETE FROM dags WHERE id = ?`, id); err != nil {
		log.Printf("dag: delete %s: %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// Nodes
// ---------------------------------------------------------------------------

// Nodes returns a DAG's nodes, oldest first.
func (s *Store) Nodes(dagID string) ([]Node, error) {
	rows, err := s.db.Query(
		`SELECT id, dag_id, name, desktop_id, content, pos_x, pos_y, created_ns
		 FROM dag_nodes WHERE dag_id = ? ORDER BY created_ns`, dagID,
	)
	if err != nil {
		return nil, fmt.Errorf("dag: nodes: %w", err)
	}
	defer rows.Close()

	nodes := make([]Node, 0)

	for rows.Next() {
		var n Node
		var created int64

		if err := rows.Scan(&n.ID, &n.DAGID, &n.Name, &n.DesktopID, &n.Content, &n.PosX, &n.PosY, &created); err != nil {
			return nil, fmt.Errorf("dag: nodes: %w", err)
		}

		n.CreatedAt = sqlitedb.FromNS(created)
		nodes = append(nodes, n)
	}

	return nodes, rows.Err()
}

func (s *Store) getNode(id string) (Node, error) {
	var n Node
	var created int64

	err := s.db.QueryRow(
		`SELECT id, dag_id, name, desktop_id, content, pos_x, pos_y, created_ns FROM dag_nodes WHERE id = ?`, id,
	).Scan(&n.ID, &n.DAGID, &n.Name, &n.DesktopID, &n.Content, &n.PosX, &n.PosY, &created)
	if err == sql.ErrNoRows {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("dag: get node %s: %w", id, err)
	}

	n.CreatedAt = sqlitedb.FromNS(created)

	return n, nil
}

// AddNode adds a node to a DAG. The first node ever added to a DAG
// automatically becomes its head, so a freshly created DAG converges to a
// valid single-head state without any extra step.
func (s *Store) AddNode(dagID, name, desktopID, content string) (Node, error) {
	name, err := validateNodeName(name)
	if err != nil {
		return Node{}, err
	}

	desktopID = strings.TrimSpace(desktopID)
	if desktopID == "" {
		return Node{}, fmt.Errorf("dag: desktop id is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Node{}, fmt.Errorf("dag: add node: %w", err)
	}
	defer tx.Rollback()

	var head string
	if err := tx.QueryRow(`SELECT head_node_id FROM dags WHERE id = ?`, dagID).Scan(&head); err != nil {
		if err == sql.ErrNoRows {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("dag: add node: %w", err)
	}

	var dup int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM dag_nodes WHERE dag_id = ? AND name = ?`, dagID, name).Scan(&dup); err != nil {
		return Node{}, fmt.Errorf("dag: add node: %w", err)
	}
	if dup > 0 {
		return Node{}, fmt.Errorf("dag: a node named %q already exists in this DAG", name)
	}

	now := s.clock()
	node := Node{ID: newID("dnode-"), DAGID: dagID, Name: name, DesktopID: desktopID, Content: content, CreatedAt: now}

	if _, err := tx.Exec(
		`INSERT INTO dag_nodes (id, dag_id, name, desktop_id, content, created_ns) VALUES (?, ?, ?, ?, ?, ?)`,
		node.ID, node.DAGID, node.Name, node.DesktopID, node.Content, sqlitedb.ToNS(now),
	); err != nil {
		return Node{}, fmt.Errorf("dag: add node: %w", err)
	}

	if head == "" {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM dag_nodes WHERE dag_id = ?`, dagID).Scan(&count); err != nil {
			return Node{}, fmt.Errorf("dag: add node: %w", err)
		}

		if count == 1 {
			if _, err := tx.Exec(
				`UPDATE dags SET head_node_id = ?, updated_ns = ? WHERE id = ?`, node.ID, sqlitedb.ToNS(now), dagID,
			); err != nil {
				return Node{}, fmt.Errorf("dag: add node: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return Node{}, fmt.Errorf("dag: add node: %w", err)
	}

	return node, nil
}

// UpdateNode changes a node's name, desktop or content.
func (s *Store) UpdateNode(id, name, desktopID, content string) (Node, error) {
	name, err := validateNodeName(name)
	if err != nil {
		return Node{}, err
	}

	desktopID = strings.TrimSpace(desktopID)
	if desktopID == "" {
		return Node{}, fmt.Errorf("dag: desktop id is required")
	}

	var dagID string
	if err := s.db.QueryRow(`SELECT dag_id FROM dag_nodes WHERE id = ?`, id).Scan(&dagID); err != nil {
		if err == sql.ErrNoRows {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("dag: update node: %w", err)
	}

	var dup int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM dag_nodes WHERE dag_id = ? AND name = ? AND id != ?`, dagID, name, id,
	).Scan(&dup); err != nil {
		return Node{}, fmt.Errorf("dag: update node: %w", err)
	}
	if dup > 0 {
		return Node{}, fmt.Errorf("dag: a node named %q already exists in this DAG", name)
	}

	if _, err := s.db.Exec(
		`UPDATE dag_nodes SET name = ?, desktop_id = ?, content = ? WHERE id = ?`, name, desktopID, content, id,
	); err != nil {
		return Node{}, fmt.Errorf("dag: update node: %w", err)
	}

	return s.getNode(id)
}

// SetNodePosition records where the editor's canvas last placed a node.
func (s *Store) SetNodePosition(id string, x, y float64) error {
	result, err := s.db.Exec(`UPDATE dag_nodes SET pos_x = ?, pos_y = ? WHERE id = ?`, x, y, id)
	if err != nil {
		return fmt.Errorf("dag: set node position %s: %w", id, err)
	}

	if changed, _ := result.RowsAffected(); changed == 0 {
		return ErrNotFound
	}

	return nil
}

// DeleteNode removes a node (cascading its edges) and clears the DAG's head
// if the head itself was deleted. Refused while a run still in progress
// depends on this node - a run can never be orphaned mid-flight by an edit.
func (s *Store) DeleteNode(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("dag: delete node: %w", err)
	}
	defer tx.Rollback()

	var dagID string
	if err := tx.QueryRow(`SELECT dag_id FROM dag_nodes WHERE id = ?`, id).Scan(&dagID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("dag: delete node: %w", err)
	}

	var live int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM dag_run_nodes rn JOIN dag_runs r ON r.id = rn.run_id
		 WHERE rn.node_id = ? AND r.status = ? AND rn.status IN (?, ?)`,
		id, RunRunning, NodePending, NodeRunning,
	).Scan(&live); err != nil {
		return fmt.Errorf("dag: delete node: %w", err)
	}
	if live > 0 {
		return fmt.Errorf("dag: a run in progress is still using this node - wait for it to finish before deleting it")
	}

	if _, err := tx.Exec(`DELETE FROM dag_nodes WHERE id = ?`, id); err != nil {
		return fmt.Errorf("dag: delete node: %w", err)
	}

	if _, err := tx.Exec(`UPDATE dags SET head_node_id = '' WHERE id = ? AND head_node_id = ?`, dagID, id); err != nil {
		return fmt.Errorf("dag: delete node: %w", err)
	}

	return tx.Commit()
}

// SetHead promotes a node with no incoming edges to be the DAG's head. The
// previous head simply becomes an ordinary node.
func (s *Store) SetHead(dagID, nodeID string) (DAG, error) {
	var owner string
	if err := s.db.QueryRow(`SELECT dag_id FROM dag_nodes WHERE id = ?`, nodeID).Scan(&owner); err != nil {
		if err == sql.ErrNoRows {
			return DAG{}, ErrNotFound
		}
		return DAG{}, fmt.Errorf("dag: set head: %w", err)
	}
	if owner != dagID {
		return DAG{}, fmt.Errorf("dag: that node does not belong to this DAG")
	}

	var incoming int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM dag_edges WHERE to_node_id = ?`, nodeID).Scan(&incoming); err != nil {
		return DAG{}, fmt.Errorf("dag: set head: %w", err)
	}
	if incoming > 0 {
		return DAG{}, fmt.Errorf("dag: a node with an incoming edge cannot be the head - remove that edge first")
	}

	if _, err := s.db.Exec(
		`UPDATE dags SET head_node_id = ?, updated_ns = ? WHERE id = ?`, nodeID, sqlitedb.ToNS(s.clock()), dagID,
	); err != nil {
		return DAG{}, fmt.Errorf("dag: set head: %w", err)
	}

	return s.Get(dagID)
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

// Edges returns a DAG's edges, oldest first.
func (s *Store) Edges(dagID string) ([]Edge, error) {
	rows, err := s.db.Query(
		`SELECT id, dag_id, from_node_id, to_node_id, created_ns FROM dag_edges WHERE dag_id = ? ORDER BY created_ns`, dagID,
	)
	if err != nil {
		return nil, fmt.Errorf("dag: edges: %w", err)
	}
	defer rows.Close()

	edges := make([]Edge, 0)

	for rows.Next() {
		var e Edge
		var created int64

		if err := rows.Scan(&e.ID, &e.DAGID, &e.FromNodeID, &e.ToNodeID, &created); err != nil {
			return nil, fmt.Errorf("dag: edges: %w", err)
		}

		e.CreatedAt = sqlitedb.FromNS(created)
		edges = append(edges, e)
	}

	return edges, rows.Err()
}

// AddEdge adds a directed dependency from -> to: reject a self-loop, an edge
// into the head (the head must always have zero incoming edges), a
// duplicate, or anything that would create a cycle. The whole check-then-act
// sequence runs in one transaction, same as AddNode/DeleteNode - two
// concurrent AddEdge calls on the same DAG (a double-submit, two tabs) must
// not each read a cycle-free edge set and then together introduce a cycle
// the check was supposed to prevent.
func (s *Store) AddEdge(dagID, from, to string) (Edge, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)

	if from == "" || to == "" {
		return Edge{}, fmt.Errorf("dag: both ends of an edge are required")
	}

	if from == to {
		return Edge{}, fmt.Errorf("dag: a node cannot depend on itself")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Edge{}, fmt.Errorf("dag: add edge: %w", err)
	}
	defer tx.Rollback()

	var head string
	if err := tx.QueryRow(`SELECT head_node_id FROM dags WHERE id = ?`, dagID).Scan(&head); err != nil {
		if err == sql.ErrNoRows {
			return Edge{}, ErrNotFound
		}
		return Edge{}, fmt.Errorf("dag: add edge: %w", err)
	}

	if to == head {
		return Edge{}, fmt.Errorf("dag: the head node cannot have an incoming edge")
	}

	for _, nodeID := range [2]string{from, to} {
		var owner string
		if err := tx.QueryRow(`SELECT dag_id FROM dag_nodes WHERE id = ?`, nodeID).Scan(&owner); err != nil {
			if err == sql.ErrNoRows {
				return Edge{}, ErrNotFound
			}
			return Edge{}, fmt.Errorf("dag: add edge: %w", err)
		}
		if owner != dagID {
			return Edge{}, fmt.Errorf("dag: both nodes must belong to this DAG")
		}
	}

	rows, err := tx.Query(`SELECT from_node_id, to_node_id FROM dag_edges WHERE dag_id = ?`, dagID)
	if err != nil {
		return Edge{}, fmt.Errorf("dag: add edge: %w", err)
	}

	var edges []Edge
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.FromNodeID, &e.ToNodeID); err != nil {
			rows.Close()
			return Edge{}, fmt.Errorf("dag: add edge: %w", err)
		}
		edges = append(edges, e)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return Edge{}, fmt.Errorf("dag: add edge: %w", scanErr)
	}

	for _, e := range edges {
		if e.FromNodeID == from && e.ToNodeID == to {
			return Edge{}, fmt.Errorf("dag: that edge already exists")
		}
	}

	if reaches(edges, to, from) {
		return Edge{}, fmt.Errorf("dag: that edge would create a cycle")
	}

	now := s.clock()
	edge := Edge{ID: newID("dedge-"), DAGID: dagID, FromNodeID: from, ToNodeID: to, CreatedAt: now}

	if _, err := tx.Exec(
		`INSERT INTO dag_edges (id, dag_id, from_node_id, to_node_id, created_ns) VALUES (?, ?, ?, ?, ?)`,
		edge.ID, edge.DAGID, edge.FromNodeID, edge.ToNodeID, sqlitedb.ToNS(now),
	); err != nil {
		return Edge{}, fmt.Errorf("dag: add edge: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Edge{}, fmt.Errorf("dag: add edge: %w", err)
	}

	return edge, nil
}

// reaches reports whether target is reachable from start by following edges
// forward. AddEdge uses it to reject from->to when to already reaches from,
// which is exactly when adding it would create a cycle.
func reaches(edges []Edge, start, target string) bool {
	if start == target {
		return true
	}

	children := make(map[string][]string, len(edges))
	for _, e := range edges {
		children[e.FromNodeID] = append(children[e.FromNodeID], e.ToNodeID)
	}

	seen := map[string]bool{start: true}
	stack := []string{start}

	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		for _, next := range children[n] {
			if next == target {
				return true
			}

			if !seen[next] {
				seen[next] = true
				stack = append(stack, next)
			}
		}
	}

	return false
}

// DeleteEdge removes an edge by ID. Refused while a run still in progress
// has the edge's to-node pending or running - the same "a run can never be
// orphaned mid-flight by an edit" rule DeleteNode enforces: pulling a
// dependency out from under a live join would let that node dispatch
// without waiting for (or after the failure of) a parent it was still
// counting on. A no-op if the ID is not present.
func (s *Store) DeleteEdge(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("dag: delete edge %s: %w", id, err)
	}
	defer tx.Rollback()

	var toNodeID string
	if err := tx.QueryRow(`SELECT to_node_id FROM dag_edges WHERE id = ?`, id).Scan(&toNodeID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("dag: delete edge %s: %w", id, err)
	}

	var live int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM dag_run_nodes rn JOIN dag_runs r ON r.id = rn.run_id
		 WHERE rn.node_id = ? AND r.status = ? AND rn.status IN (?, ?)`,
		toNodeID, RunRunning, NodePending, NodeRunning,
	).Scan(&live); err != nil {
		return fmt.Errorf("dag: delete edge %s: %w", id, err)
	}
	if live > 0 {
		return fmt.Errorf("dag: a run in progress is still waiting on this edge's node - wait for it to finish before deleting it")
	}

	if _, err := tx.Exec(`DELETE FROM dag_edges WHERE id = ?`, id); err != nil {
		return fmt.Errorf("dag: delete edge %s: %w", id, err)
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Cron
// ---------------------------------------------------------------------------

// SetCron sets or clears a DAG's cron trigger. An empty cronExpr clears it -
// a DAG has no separate enabled/disabled flag, so this is what disables it.
// A schedule never fires immediately: the first run is the next time the
// expression matches, same as internal/schedule.Store.Create.
func (s *Store) SetCron(dagID, cronExpr string) (DAG, error) {
	cronExpr = strings.TrimSpace(cronExpr)

	if _, err := s.Get(dagID); err != nil {
		return DAG{}, err
	}

	now := s.clock()

	if cronExpr == "" {
		if _, err := s.db.Exec(
			`UPDATE dags SET cron_expr = '', next_run_ns = 0, updated_ns = ? WHERE id = ?`, sqlitedb.ToNS(now), dagID,
		); err != nil {
			return DAG{}, fmt.Errorf("dag: set cron: %w", err)
		}

		return s.Get(dagID)
	}

	expr, err := schedule.Parse(cronExpr)
	if err != nil {
		return DAG{}, err
	}

	next, ok := expr.Next(now)
	if !ok {
		return DAG{}, fmt.Errorf("cron expression %q never matches", cronExpr)
	}

	if _, err := s.db.Exec(
		`UPDATE dags SET cron_expr = ?, next_run_ns = ?, updated_ns = ? WHERE id = ?`,
		cronExpr, sqlitedb.ToNS(next), sqlitedb.ToNS(now), dagID,
	); err != nil {
		return DAG{}, fmt.Errorf("dag: set cron: %w", err)
	}

	return s.Get(dagID)
}

// DueCron returns DAGs whose cron trigger has arrived by now.
func (s *Store) DueCron(now time.Time) ([]DAG, error) {
	rows, err := s.db.Query(
		`SELECT `+dagColumns+` FROM dags WHERE cron_expr != '' AND next_run_ns > 0 AND next_run_ns <= ?`,
		sqlitedb.ToNS(now),
	)
	if err != nil {
		return nil, fmt.Errorf("dag: due cron: %w", err)
	}
	defer rows.Close()

	due := make([]DAG, 0)

	for rows.Next() {
		d, err := scanDAG(rows)
		if err != nil {
			return nil, fmt.Errorf("dag: due cron: %w", err)
		}

		due = append(due, d)
	}

	return due, rows.Err()
}

// MarkCronRun records that dagID's cron fired at ranAt and advances it to
// its next occurrence. An expression that can no longer match anything
// clears the cron instead of leaving the DAG stuck re-checking a run that
// will never come - the same self-disabling behavior
// internal/schedule.Store.MarkRun already has.
func (s *Store) MarkCronRun(dagID string, ranAt time.Time) error {
	var cronExpr string

	if err := s.db.QueryRow(`SELECT cron_expr FROM dags WHERE id = ?`, dagID).Scan(&cronExpr); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("dag: mark cron run %s: %w", dagID, err)
	}

	if cronExpr == "" {
		return nil
	}

	expr, err := schedule.Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("dag: mark cron run %s: %w", dagID, err)
	}

	next, ok := expr.Next(ranAt)

	if !ok {
		_, err = s.db.Exec(
			`UPDATE dags SET cron_expr = '', next_run_ns = 0, last_run_ns = ? WHERE id = ?`, sqlitedb.ToNS(ranAt), dagID,
		)
	} else {
		_, err = s.db.Exec(
			`UPDATE dags SET next_run_ns = ?, last_run_ns = ? WHERE id = ?`, sqlitedb.ToNS(next), sqlitedb.ToNS(ranAt), dagID,
		)
	}

	if err != nil {
		return fmt.Errorf("dag: mark cron run %s: %w", dagID, err)
	}

	return nil
}
