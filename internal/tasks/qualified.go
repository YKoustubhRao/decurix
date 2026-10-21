package tasks

import "fmt"

// CommitTyped records control-plane-owned work without inferring type from prose.
// Qualification tasks use a separate worker identity and cannot attach to a
// desktop's live turn. Practice must never inflate production autonomy.
func (s *Store) CommitTyped(route Route, worker Worker, text, taskType string) (Route, error) {
	if route.Kind != RouteNew || (taskType != TypePractice && taskType != TypeFiles) {
		return route, fmt.Errorf("tasks: invalid isolated task type or route")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.UpsertWorker(worker); err != nil {
		return route, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return route, err
	}
	defer tx.Rollback()
	if err := s.insertTask(tx, route, worker, text, s.now()); err != nil {
		return route, err
	}
	if _, err := tx.Exec(`UPDATE tasks SET task_type=? WHERE id=?`, taskType, route.TaskID); err != nil {
		return route, err
	}
	return route, tx.Commit()
}
