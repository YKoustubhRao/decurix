// checkpoint_codec.go: a skill's own checkpoint (Result.Checkpoint, RunContext.Resume) as data, so soul can keep a
// paused job across a restart (cmd/soul/paused.go). Only the kinds of checkpoint listed here are encoded; any other is
// an error, and the caller keeps that paused job in memory only, as before.
package skills

import (
	"encoding/json"
	"fmt"
	"math"
)

// CheckpointBlob is a skill's checkpoint as data: which kind it is and its JSON.
type CheckpointBlob struct {
	Kind string          `json:"kind,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

const (
	checkpointOperator = "operator"
	checkpointCoding   = "coding"
	checkpointText     = "text"
)

// operatorDisk is an operator's state with the unexported parts a resumed goal reads again: the look before a commit
// and the folder facts. What is only a cache (the reply's reading, the review's last work points), the job's secret
// values and the turn's ids are set again when the goal resumes (Run, loop), and the document reader is made anew.
type operatorDisk struct {
	State        *operatorState `json:"state"`
	LookShot     []byte         `json:"lookShot,omitempty"`
	LookShotStep int            `json:"lookShotStep,omitempty"`
	FolderFacts  []string       `json:"folderFacts,omitempty"`
}

// EncodeCheckpoint is cp as data: the zero blob for nil, an error for a kind it does not know.
func EncodeCheckpoint(cp any) (CheckpointBlob, error) {
	var kind string
	var v any
	switch c := cp.(type) {
	case nil:
		return CheckpointBlob{}, nil
	case *operatorState:
		if c == nil {
			return CheckpointBlob{}, nil
		}
		kind = checkpointOperator
		v = operatorDisk{State: c, LookShot: c.lookShot, LookShotStep: c.lookShotStep, FolderFacts: c.folderFacts}
	case *codingCheckpoint:
		if c == nil {
			return CheckpointBlob{}, nil
		}
		kind, v = checkpointCoding, c
	case string:
		kind, v = checkpointText, c
	default:
		return CheckpointBlob{}, fmt.Errorf("a %T checkpoint cannot be kept on disk", cp)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return CheckpointBlob{}, err
	}
	return CheckpointBlob{Kind: kind, Data: raw}, nil
}

// DecodeCheckpoint is the checkpoint b holds: nil for the zero blob, an error for a kind it does not know.
func DecodeCheckpoint(b CheckpointBlob) (any, error) {
	switch b.Kind {
	case "":
		return nil, nil
	case checkpointOperator:
		var d operatorDisk
		if err := json.Unmarshal(b.Data, &d); err != nil {
			return nil, err
		}
		if d.State == nil {
			return nil, fmt.Errorf("an operator checkpoint without its state")
		}
		d.State.lookShot, d.State.lookShotStep, d.State.folderFacts = d.LookShot, d.LookShotStep, d.FolderFacts
		return d.State, nil
	case checkpointCoding:
		var c codingCheckpoint
		if err := json.Unmarshal(b.Data, &c); err != nil {
			return nil, err
		}
		return &c, nil
	case checkpointText:
		var s string
		if err := json.Unmarshal(b.Data, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, fmt.Errorf("unknown checkpoint kind %q", b.Kind)
}

// RestoreMeta is a result's Meta as JSON gave it back, with the shapes Go code reads it in again: a whole number is
// an int (Meta "exitCode") and a list of strings a []string (Meta "commands"). Other values stay as JSON decoded them.
func RestoreMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		out[k] = restoreMetaValue(v)
	}
	return out
}

// restoreMetaValue is one Meta value in the shape RestoreMeta gives it.
func restoreMetaValue(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int(t)
		}
	case []any:
		strs := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return t
			}
			strs = append(strs, s)
		}
		return strs
	}
	return v
}
