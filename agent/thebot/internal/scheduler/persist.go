// persist.go: a paused job (Checkpoint) as data, so soul can keep it across a restart (cmd/soul/paused.go). The
// checkpoint's own secret values (Checkpoint.Secrets) are never part of it: there is no field for them here.
package scheduler

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"thebot/internal/atomizer"
	"thebot/internal/llm"
	"thebot/internal/skills"
)

// checkpointVersion is the shape of an encoded checkpoint; one of another version is not decoded.
const checkpointVersion = 1

// checkpointDisk is a Checkpoint as EncodeCheckpoint writes it. It has no Secrets field, by design.
type checkpointDisk struct {
	V               int            `json:"v"`
	Prompt          string         `json:"prompt"`
	Context         string         `json:"context,omitempty"`
	WorkDir         string         `json:"workdir,omitempty"`
	Route           atomizer.Route `json:"route"`
	System          llm.Message    `json:"system"`
	Kind            string         `json:"kind"`
	Action          string         `json:"action,omitempty"`
	Question        string         `json:"question,omitempty"`
	PausedID        int            `json:"pausedId,omitempty"`
	Replans         int            `json:"replans,omitempty"`
	UsedSkill       string         `json:"usedSkill,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	GateRequest     string         `json:"gateRequest,omitempty"`
	GateFingerprint string         `json:"gateFingerprint,omitempty"`
	PlanAgreed      bool           `json:"planAgreed,omitempty"`
	OnScreen        bool           `json:"onScreen,omitempty"`
	Nodes           []nodeDisk     `json:"nodes"`
	Seq             int            `json:"seq"`
}

// nodeDisk is one step of a paused job. The step's grant is not kept: a checkpoint never carries an approval.
type nodeDisk struct {
	Step       atomizer.Step         `json:"step"`
	State      StepState             `json:"state"`
	Result     skills.Result         `json:"result"`
	ResultCP   skills.CheckpointBlob `json:"resultCheckpoint"`
	HasResult  bool                  `json:"hasResult,omitempty"`
	Superseded bool                  `json:"superseded,omitempty"`
	// ResumeIsResult says the step resumes from its result's own checkpoint (the same value, not a copy of it).
	ResumeIsResult bool                  `json:"resumeIsResult,omitempty"`
	Resume         skills.CheckpointBlob `json:"resume"`
	Reply          string                `json:"reply,omitempty"`
	Reading        *skills.Reading       `json:"reading,omitempty"`
	Started        time.Time             `json:"started"`
	DurationNs     int64                 `json:"durationNs,omitempty"`
	Seq            int                   `json:"seq,omitempty"`
}

// EncodeCheckpoint is cp as data, without its secret values: an error when a step's own checkpoint is of a kind
// that cannot be kept (skills.EncodeCheckpoint) or its result does not encode.
func EncodeCheckpoint(cp *Checkpoint) ([]byte, error) {
	if cp == nil {
		return nil, fmt.Errorf("no checkpoint")
	}
	d := checkpointDisk{
		V: checkpointVersion, Prompt: cp.Prompt, Context: cp.Context, WorkDir: cp.WorkDir, Route: cp.Route,
		System: cp.System, Kind: cp.Kind, Action: cp.Action, Question: cp.Question, PausedID: cp.PausedID,
		Replans: cp.Replans, UsedSkill: cp.UsedSkill, CreatedAt: cp.CreatedAt, GateRequest: cp.GateRequest,
		GateFingerprint: cp.GateFingerprint, PlanAgreed: cp.PlanAgreed, OnScreen: cp.OnScreen, Seq: cp.seq,
	}
	for i := range cp.nodes {
		n := &cp.nodes[i]
		nd := nodeDisk{Step: n.step, State: n.state, Result: n.result, HasResult: n.hasResult, Superseded: n.superseded,
			Reply: n.reply, Reading: n.reading, Started: n.started, DurationNs: int64(n.duration), Seq: n.seq}
		nd.Result.Checkpoint = nil
		var err error
		if nd.ResultCP, err = skills.EncodeCheckpoint(n.result.Checkpoint); err != nil {
			return nil, fmt.Errorf("step %d: %w", n.step.ID, err)
		}
		if n.resume != nil && samePointer(n.resume, n.result.Checkpoint) {
			nd.ResumeIsResult = true
		} else if nd.Resume, err = skills.EncodeCheckpoint(n.resume); err != nil {
			return nil, fmt.Errorf("step %d: %w", n.step.ID, err)
		}
		d.Nodes = append(d.Nodes, nd)
	}
	return json.Marshal(d)
}

// DecodeCheckpoint is the checkpoint EncodeCheckpoint wrote as raw, with no secret values (the caller decides what a
// checkpoint that had some may do).
func DecodeCheckpoint(raw []byte) (*Checkpoint, error) {
	var d checkpointDisk
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.V != checkpointVersion {
		return nil, fmt.Errorf("checkpoint version %d, want %d", d.V, checkpointVersion)
	}
	cp := &Checkpoint{
		Prompt: d.Prompt, Context: d.Context, WorkDir: d.WorkDir, Route: d.Route, System: d.System, Kind: d.Kind,
		Action: d.Action, Question: d.Question, PausedID: d.PausedID, Replans: d.Replans, UsedSkill: d.UsedSkill,
		CreatedAt: d.CreatedAt, GateRequest: d.GateRequest, GateFingerprint: d.GateFingerprint,
		PlanAgreed: d.PlanAgreed, OnScreen: d.OnScreen, seq: d.Seq,
	}
	for _, nd := range d.Nodes {
		n := node{step: nd.Step, state: nd.State, result: nd.Result, hasResult: nd.HasResult, superseded: nd.Superseded,
			reply: nd.Reply, reading: nd.Reading, started: nd.Started, duration: time.Duration(nd.DurationNs), seq: nd.Seq}
		n.result.Meta = skills.RestoreMeta(n.result.Meta)
		var err error
		if n.result.Checkpoint, err = skills.DecodeCheckpoint(nd.ResultCP); err != nil {
			return nil, fmt.Errorf("step %d: %w", nd.Step.ID, err)
		}
		if nd.ResumeIsResult {
			n.resume = n.result.Checkpoint
		} else if n.resume, err = skills.DecodeCheckpoint(nd.Resume); err != nil {
			return nil, fmt.Errorf("step %d: %w", nd.Step.ID, err)
		}
		cp.nodes = append(cp.nodes, n)
	}
	return cp, nil
}

// samePointer reports whether a and b hold the same pointer (never comparing values, which may not be comparable).
func samePointer(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Kind() == reflect.Pointer && vb.Kind() == reflect.Pointer && va.Type() == vb.Type() &&
		va.Pointer() == vb.Pointer()
}
