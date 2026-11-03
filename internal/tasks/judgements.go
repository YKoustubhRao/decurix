package tasks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
	"github.com/YKoustubhRao/decurix/internal/withhold"
)

// The judgements table (plan R5) holds every human input with its author, append-only. Nothing in this package (or
// any other) updates or deletes a row: what an input changed is a later row citing it, and deleting a worker leaves
// its judgements served. insertJudgement is the one statement that writes the table.

// ErrNoActor is a judgement offered without an author: no human input is stored without one (plan rule 5).
var ErrNoActor = errors.New("tasks: a judgement needs its actor")

// judgementIDPattern is the shape of a judgement id this store issues: what a desktop is told and may cite back.
var judgementIDPattern = regexp.MustCompile(`^j_[0-9a-f]{16}$`)

// NewJudgementID is a fresh judgement id ("j_" and 16 hex digits).
func NewJudgementID() string { return sqlitedb.NewID("j_", 8) }

// ValidJudgementID reports whether id has the shape of a judgement id this store issues.
func ValidJudgementID(id string) bool { return judgementIDPattern.MatchString(id) }

// validJudgementKind reports whether k is one of the judgement kinds.
func validJudgementKind(k string) bool {
	switch k {
	case JudgementAnswer, JudgementApproval, JudgementVerdict, JudgementCorrection, JudgementRunAnswer,
		JudgementLesson, JudgementKnowledge:
		return true
	}
	return false
}

// execer is a *sql.DB or a *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertJudgement writes one judgement and returns its id (j.ID when it has a valid one, a new one otherwise). It
// refuses one without an actor (ErrNoActor) or of an unknown kind.
func insertJudgement(db execer, j Judgement) (string, error) {
	if strings.TrimSpace(j.Actor.ID) == "" || strings.TrimSpace(j.Actor.Kind) == "" {
		return "", ErrNoActor
	}
	if !validJudgementKind(j.Kind) {
		return "", fmt.Errorf("tasks: unknown judgement kind %q", j.Kind)
	}
	if !ValidJudgementID(j.ID) {
		j.ID = NewJudgementID()
	}
	if j.At.IsZero() {
		j.At = time.Now().UTC()
	}
	roles := j.Actor.Roles
	if roles == nil {
		roles = []string{}
	}
	rolesJSON, _ := json.Marshal(roles)
	payload, changed := jsonOrEmpty(j.Payload), jsonOrEmpty(j.Changed)
	if j.Secret {
		j.Text = withhold.Secret
	}
	_, err := db.Exec(`INSERT INTO judgements (id, at_ns, actor_id, actor_name, actor_roles, actor_kind, door, kind,
		worker_id, task_id, execution_id, intervention_id, skill_name, skill_version, text, secret, payload, changed,
		derived_from) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, sqlitedb.ToNS(j.At), j.Actor.ID, j.Actor.Name, string(rolesJSON), j.Actor.Kind, j.Door, j.Kind,
		j.WorkerID, j.TaskID, j.ExecutionID, j.InterventionID, j.SkillName, j.SkillVersion, j.Text, j.Secret,
		payload, changed, j.DerivedFrom)
	if err != nil {
		return "", fmt.Errorf("tasks: recording a judgement: %w", err)
	}
	return j.ID, nil
}

// jsonOrEmpty is raw when it is a JSON object, "{}" otherwise.
func jsonOrEmpty(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return "{}"
	}
	return string(raw)
}

// AddJudgement stores one judgement, append-only, and returns its id. Its text is withheld here (whole when it is
// secret) whatever the caller did. It refuses one without an actor (ErrNoActor).
func (s *Store) AddJudgement(j Judgement) (string, error) {
	if !j.Secret {
		j.Text = withhold.Text(j.Text)
	}
	if j.At.IsZero() {
		j.At = s.now()
	}
	return insertJudgement(s.db, j)
}

const judgementColumns = `id, at_ns, actor_id, actor_name, actor_roles, actor_kind, door, kind, worker_id, task_id,
	execution_id, intervention_id, skill_name, skill_version, text, secret, payload, changed, derived_from`

// scanJudgement reads one judgements row.
func scanJudgement(row sqlitedb.Scanner) (Judgement, error) {
	var j Judgement
	var at int64
	var roles, payload, changed string
	if err := row.Scan(&j.ID, &at, &j.Actor.ID, &j.Actor.Name, &roles, &j.Actor.Kind, &j.Door, &j.Kind, &j.WorkerID,
		&j.TaskID, &j.ExecutionID, &j.InterventionID, &j.SkillName, &j.SkillVersion, &j.Text, &j.Secret, &payload,
		&changed, &j.DerivedFrom); err != nil {
		return j, err
	}
	j.At = sqlitedb.FromNS(at).UTC()
	_ = json.Unmarshal([]byte(roles), &j.Actor.Roles)
	if j.Actor.Roles == nil {
		j.Actor.Roles = []string{}
	}
	j.Payload, j.Changed = json.RawMessage(jsonOrEmpty([]byte(payload))), json.RawMessage(jsonOrEmpty([]byte(changed)))
	return j, nil
}

// Judgement is the judgement id, and false when there is none.
func (s *Store) Judgement(id string) (Judgement, bool, error) {
	j, err := scanJudgement(s.db.QueryRow(`SELECT `+judgementColumns+` FROM judgements WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Judgement{}, false, nil
	}
	if err != nil {
		return Judgement{}, false, fmt.Errorf("tasks: reading a judgement: %w", err)
	}
	return j, true, nil
}

// JudgementsForTask is every judgement on a task, oldest first.
func (s *Store) JudgementsForTask(taskID string) ([]Judgement, error) {
	return sqlitedb.QueryAll(s.db, scanJudgement,
		`SELECT `+judgementColumns+` FROM judgements WHERE task_id = ? ORDER BY at_ns, rowid`, taskID)
}

// JudgementsForWorker is every judgement on a worker's tasks, oldest first: kept when the worker is deleted.
func (s *Store) JudgementsForWorker(workerID string) ([]Judgement, error) {
	return sqlitedb.QueryAll(s.db, scanJudgement,
		`SELECT `+judgementColumns+` FROM judgements WHERE worker_id = ? ORDER BY at_ns, rowid`, workerID)
}

// SetVerdict records a person's verdict on a finished task (correct or wrong, or "" to clear it), and the verdict
// judgement by actor beside it, in one transaction. It refuses a verdict without an actor (ErrNoActor).
func (s *Store) SetVerdict(taskID, verdict, note string, actor Actor) (Task, error) {
	switch verdict {
	case VerdictCorrect, VerdictWrong, "":
	default:
		return Task{}, fmt.Errorf("tasks: unknown verdict %q", verdict)
	}
	if strings.TrimSpace(actor.ID) == "" || strings.TrimSpace(actor.Kind) == "" {
		return Task{}, ErrNoActor
	}

	now := s.now()
	verdictAt := int64(0)
	if verdict != "" {
		verdictAt = sqlitedb.ToNS(now)
	} else {
		note = ""
	}
	note = withhold.Text(strings.TrimSpace(note))

	s.mu.Lock()
	err := func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("tasks: recording verdict: %w", err)
		}
		defer tx.Rollback()

		result, err := tx.Exec(`UPDATE tasks SET verdict = ?, verdict_note = ?, verdict_ns = ? WHERE id = ?`,
			verdict, note, verdictAt, taskID)
		if err != nil {
			return fmt.Errorf("tasks: recording verdict: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			return ErrNotFound
		}

		var workerID, executionID string
		_ = tx.QueryRow(`SELECT worker_id FROM tasks WHERE id = ?`, taskID).Scan(&workerID)
		_ = tx.QueryRow(`SELECT id FROM executions WHERE task_id = ? ORDER BY attempt DESC LIMIT 1`, taskID).Scan(&executionID)
		payload, _ := json.Marshal(map[string]string{"verdict": verdict, "note": note})
		if _, err := insertJudgement(tx, Judgement{At: now, Actor: actor, Door: "task_page", Kind: JudgementVerdict,
			WorkerID: workerID, TaskID: taskID, ExecutionID: executionID, Text: note, Payload: payload}); err != nil {
			return err
		}
		return tx.Commit()
	}()
	s.mu.Unlock()
	if err != nil {
		return Task{}, err
	}

	detail, err := s.Get(taskID)
	return detail.Task, err
}
