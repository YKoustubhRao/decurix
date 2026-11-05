package tasks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The record types of phase 2 (PLAN-P2 contracts C5, C12; plan R5-R7): where an execution came from, who a human
// input is by, and the structured verdict of a run's final check. Other packages (runs, eval, practice) build
// against these; the store-side code that writes them sits next to the tables that hold them.

// Origin is what started an execution: the door or producer (Kind), its own reference (a schedule id, a DAG
// run/node, a queue item, an eval case), and the person who started it when one did.
type Origin struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref,omitempty"`
	ActorID string `json:"actor_id,omitempty"`
}

// Origin kinds (C12).
const (
	OriginPerson    = "person"
	OriginConcierge = "concierge"
	OriginSchedule  = "schedule"
	OriginMail      = "mail"
	OriginDAG       = "dag"
	OriginQueue     = "queue"
	OriginHandoff   = "handoff"
	OriginRecording = "recording"
	OriginPractice  = "practice"
	OriginEval      = "eval"
	OriginUnknown   = "unknown"
)

// ErrNoOrigin is an execution offered with an origin that names no kind: every producer says what started its work.
var ErrNoOrigin = errors.New("tasks: an execution needs its origin")

// ValidOriginKind reports whether k is one of the origin kinds.
func ValidOriginKind(k string) bool {
	switch k {
	case OriginPerson, OriginConcierge, OriginSchedule, OriginMail, OriginDAG, OriginQueue, OriginHandoff,
		OriginRecording, OriginPractice, OriginEval, OriginUnknown:
		return true
	}
	return false
}

// Actor is who made a human input: a signed-in person (or the local owner on the primary listener), another worker
// answering through a handoff, the eval's scripted expert, or a program acting on a person's standing order. It is
// decided by the server from the door a request came through, never read from a request body.
type Actor struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles"`
	Kind  string   `json:"kind"`
}

// Actor kinds.
const (
	ActorPerson   = "person"
	ActorWorker   = "worker"
	ActorScripted = "scripted"
	ActorProgram  = "program"
)

// LocalOwner is the actor of a request on the primary listener with access control off: the person at this
// machine, who owns it.
var LocalOwner = Actor{ID: "local-owner", Name: "Owner (this computer)", Roles: []string{"owner"}, Kind: ActorPerson}

// Judgement kinds (plan R5).
const (
	JudgementAnswer     = "answer"
	JudgementApproval   = "approval"
	JudgementVerdict    = "verdict"
	JudgementCorrection = "correction"
	JudgementRunAnswer  = "run_answer"
	JudgementLesson     = "lesson"
	JudgementKnowledge  = "knowledge"
	// JudgementFieldAnswer is what a desktop kept of a person's answer for one field of an application's list: JSON
	// {"label", "host", "option"}, derived from that person's judgement, whose text names the option (H-PY-18).
	JudgementFieldAnswer = "field_answer"
)

// Judgement is one human input, stored append-only with its author: an answer to a question, an approval, a verdict
// on a finished task, or something a desktop kept from a person's reply (a correction, a run answer, a lesson), which
// names the judgement it was derived from.
type Judgement struct {
	ID             string          `json:"id"`
	At             time.Time       `json:"at"`
	Actor          Actor           `json:"actor"`
	Door           string          `json:"door"`
	Kind           string          `json:"kind"`
	WorkerID       string          `json:"worker_id"`
	TaskID         string          `json:"task_id"`
	ExecutionID    string          `json:"execution_id"`
	InterventionID string          `json:"intervention_id"`
	SkillName      string          `json:"skill_name"`
	SkillVersion   int             `json:"skill_version"`
	Text           string          `json:"text"`
	Secret         bool            `json:"secret"`
	Payload        json.RawMessage `json:"payload"`
	Changed        json.RawMessage `json:"changed"`
	DerivedFrom    string          `json:"derived_from"`
}

// Skill names the taught skill that drove an execution, and its version (C5). Empty when none did.
type Skill struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// VerdictSchema is the schema name of a structured gate verdict.
const VerdictSchema = "decurix.verdict/v1"

// Gate verdicts (PLAN-P2 §5.2).
const (
	GateDone      = "done"
	GateNotDone   = "not_done"
	GateUnchecked = "unchecked"
	GateStopped   = "stopped"
)

// GateVerdict is a run's final check, structured (decurix.verdict/v1): the verdict, which gate gave it, why, the
// kinds of check that passed, each check with its result, and the evidence lines a person is shown.
type GateVerdict struct {
	Schema   string        `json:"schema"`
	Verdict  string        `json:"verdict"`
	By       string        `json:"by"`
	Why      string        `json:"why"`
	Basis    []string      `json:"basis"`
	Checks   []CheckResult `json:"checks"`
	Evidence []string      `json:"evidence"`

	// extra holds the fields this version does not know, so a v1.1 verdict read here is written back whole.
	extra map[string]json.RawMessage
}

// gateVerdictFields is GateVerdict without its methods, for encoding.
type gateVerdictFields GateVerdict

// verdictKeys are the fields GateVerdict knows by name.
var verdictKeys = map[string]bool{"schema": true, "verdict": true, "by": true, "why": true, "basis": true,
	"checks": true, "evidence": true}

// UnmarshalJSON reads a verdict, keeping the fields it does not know.
func (v *GateVerdict) UnmarshalJSON(b []byte) error {
	var known gateVerdictFields
	if err := json.Unmarshal(b, &known); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	*v = GateVerdict(known)
	v.extra = nil
	for k, raw := range all {
		if !verdictKeys[k] {
			if v.extra == nil {
				v.extra = map[string]json.RawMessage{}
			}
			v.extra[k] = raw
		}
	}
	return nil
}

// MarshalJSON writes a verdict, with the fields it did not know put back.
func (v GateVerdict) MarshalJSON() ([]byte, error) {
	known, err := json.Marshal(gateVerdictFields(v))
	if err != nil || len(v.extra) == 0 {
		return known, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(known, &all); err != nil {
		return nil, err
	}
	for k, raw := range v.extra {
		if !verdictKeys[k] {
			all[k] = raw
		}
	}
	return json.Marshal(all)
}

// CheckResult is one check of a gate verdict: its id, pass / fail / not_made, the kind of evidence it rests on, a
// detail and references (files, frames, records).
type CheckResult struct {
	ID     string   `json:"id"`
	Result string   `json:"result"`
	Basis  string   `json:"basis"`
	Detail string   `json:"detail"`
	Refs   []string `json:"refs"`
}

// TaskWorker is the worker a task belongs to, and false when there is no such task.
func (s *Store) TaskWorker(taskID string) (string, bool) {
	var worker string
	if err := s.db.QueryRow(`SELECT worker_id FROM tasks WHERE id = ?`, taskID).Scan(&worker); err != nil {
		return "", false
	}
	return worker, true
}

// TaskOfExecution is the task an execution is an attempt at, or ErrNotFound.
func (s *Store) TaskOfExecution(execID string) (string, error) {
	var task string
	err := s.db.QueryRow(`SELECT task_id FROM executions WHERE id = ?`, execID).Scan(&task)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tasks: reading an execution: %w", err)
	}
	return task, nil
}
