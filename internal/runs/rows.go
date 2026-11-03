package runs

import (
	"encoding/json"
	"time"
)

// CallRow is one row of the proxy's call log as Build reads it (llmrec.Row, CP-1). Until CP-1 lands it is declared
// here with the fields Build uses; then it becomes an alias of llmrec.Row.
type CallRow struct {
	ID                string
	At                time.Time
	Ms                int64
	Provider          string
	Deployment        string
	Status            int
	DesktopID         string
	TaskID            string
	Turn              string
	Purpose           string
	Caller            string
	Messages          []string // message blob SHAs, in order
	PrefixSHA         string
	PromptSHA         string
	ResponseSHA       string
	InputTokens       *int64
	CachedInputTokens *int64
	OutputTokens      *int64
	Problems          []string
}

// JudgementActor is who made a judgement (tasks.Actor, CP-3).
type JudgementActor struct {
	ID    string
	Name  string
	Roles []string
	Kind  string
}

// JudgementRow is one row of the judgements table as Build reads it (tasks.Judgement, CP-3). Until CP-3 lands it is
// declared here with the fields Build uses; then it becomes an alias of tasks.Judgement.
type JudgementRow struct {
	ID             string
	At             time.Time
	Actor          JudgementActor
	Door           string
	Kind           string
	WorkerID       string
	TaskID         string
	ExecutionID    string
	InterventionID string
	SkillName      string
	SkillVersion   int
	Text           string
	Secret         bool
	Payload        json.RawMessage
	Changed        json.RawMessage
	DerivedFrom    string
}
