package evals

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Outcome is what clients reported about a model's real answers in one field.
type Outcome struct {
	Passed   int       `json:"passed"`
	Missed   int       `json:"missed"`
	LastMiss time.Time `json:"last_miss,omitempty"`
}

// Outcomes are clients' final verdicts on real answers, by model and field.
//
// They only ever take work away. A pass here does not earn a model anything,
// because a real request has no answer key and a passing gate can be wrong;
// only checked cases earn a path. A miss sends the path back to being measured,
// though it may not be the model's fault: a false alarm costs some hosted
// tokens, and a real one that was ignored costs a wrong answer.
type Outcomes struct {
	path string
	mu   sync.Mutex
	by   map[string]map[string]Outcome
}

// OpenOutcomes reads the file at path; a missing or broken file starts empty.
func OpenOutcomes(path string) *Outcomes {
	o := &Outcomes{path: path, by: map[string]map[string]Outcome{}}
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &o.by) != nil || o.by == nil {
			o.by = map[string]map[string]Outcome{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		o.by = map[string]map[string]Outcome{}
	}
	return o
}

// Record files one verdict and saves.
func (o *Outcomes) Record(model, domain string, passed bool, at time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.by[model] == nil {
		o.by[model] = map[string]Outcome{}
	}
	c := o.by[model][domain]
	if passed {
		c.Passed++
	} else {
		c.Missed++
		if at.After(c.LastMiss) {
			c.LastMiss = at
		}
	}
	o.by[model][domain] = c
	data, err := json.MarshalIndent(o.by, "", "  ")
	if err != nil {
		return err
	}
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, o.path)
}

// Get is what has been reported for a model in a field.
func (o *Outcomes) Get(model, domain string) Outcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.by[model][domain]
}
