// purge.go is the chat's part of purging a leaked secret by value (internal/purge): the stored entries and steps, and
// the threads held in memory, are rewritten with the value replaced by a mark. Ids, order and verdicts stay, so the
// chat still links to its task records.

package conversation

import (
	"encoding/json"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// PurgeValue rewrites every stored chat entry and step holding any of forms, each replaced by mark, and the same in
// every thread held in memory (whose watchers are woken), and returns how many stored rows changed. It holds the
// store's lock throughout, so no append lands between the database and memory.
func (s *Store) PurgeValue(forms []string, mark string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := sqlitedb.ReplaceInColumns(s.db, "chat_entries", []string{"content"}, forms, mark)
	if err != nil {
		return 0, err
	}
	steps, err := sqlitedb.ReplaceInColumns(s.db, "chat_steps", []string{"text", "action"}, forms, mark)
	if err != nil {
		return entries, err
	}
	for _, t := range s.threads {
		changed := false
		for i := range t.entries {
			e := t.entries[i]
			content, c1 := sqlitedb.PurgeText(e.Content, forms, mark)
			newSteps, c2 := purgeSteps(e.Steps, forms, mark)
			if c1 || c2 {
				e.Content, e.Steps = content, newSteps
				t.entries[i] = e
				changed = true
			}
		}
		if steps, c := purgeSteps(t.steps, forms, mark); c {
			t.steps = steps
			changed = true
		}
		if changed {
			t.touch()
		}
	}
	return entries + steps, nil
}

// purgeSteps is steps with forms replaced by mark in each one's text and action, as a new slice (a snapshot taken
// before shares the old one), and whether anything changed.
func purgeSteps(steps []Step, forms []string, mark string) ([]Step, bool) {
	var out []Step
	for i, st := range steps {
		text, c1 := sqlitedb.PurgeText(st.Text, forms, mark)
		action, c2 := purgeAction(st.Action, forms, mark)
		if !c1 && !c2 {
			continue
		}
		if out == nil {
			out = make([]Step, len(steps))
			copy(out, steps)
		}
		st.Text, st.Action = text, action
		out[i] = st
	}
	if out == nil {
		return steps, false
	}
	return out, true
}

// purgeAction is a with forms replaced by mark in every field, as a new Action, and whether anything changed.
func purgeAction(a *Action, forms []string, mark string) (*Action, bool) {
	if a == nil {
		return nil, false
	}
	b, err := json.Marshal(a)
	if err != nil {
		return a, false
	}
	purged, changed := sqlitedb.PurgeText(string(b), forms, mark)
	if !changed {
		return a, false
	}
	var out Action
	if json.Unmarshal([]byte(purged), &out) != nil {
		return a, false
	}
	return &out, true
}
