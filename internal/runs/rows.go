package runs

import (
	"github.com/YKoustubhRao/decurix/internal/llmrec"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// CallRow is one row of the proxy's call log as Build reads it.
type CallRow = llmrec.Row

// JudgementRow is one row of the judgements table as Build reads it.
type JudgementRow = tasks.Judgement
