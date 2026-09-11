package llm

import "strings"

// Models is deterministic model routing by task type - never asked of a
// model itself, the same discipline the atomizer applies to task
// classification.
//
// Routing by task type is not a micro-optimisation. Writing a complete
// source file and answering "hello" are genuinely different jobs, and the
// model that is best at one is routinely not best at the other. On a
// single-GPU box the constraint is physical as well as qualitative: a big
// coding model and a big chat model cannot both stay resident, so which
// one is loaded for which kind of work is a real decision with real
// consequences for both quality and speed.
type Models struct {
	Default string
	// Judgment drives the small structured calls - classification,
	// planning, reasoning about a failure. Cheap, frequent, short.
	Judgment string
	// Coding drives the coding harness: the architecture, every generated
	// file, every test, every repair. These are by far the longest prompts
	// and longest answers the agent produces (the whole architecture, the
	// content of every file written so far, the last compiler error, and
	// then a complete file in reply), so this is where a model with a
	// genuinely large context window earns its place even if it is a worse
	// conversationalist.
	//
	// Empty means "use Default" - nothing changes for a deployment that
	// never configures it.
	Coding string
}

func DefaultModels(chatModel string) Models {
	return Models{Default: chatModel, Judgment: chatModel}
}

// WithCoding returns a copy routed to a dedicated coding model. An empty
// value leaves routing alone rather than silently pointing every build at
// a model name that does not exist.
func (m Models) WithCoding(model string) Models {
	if trimmed := strings.TrimSpace(model); trimmed != "" {
		m.Coding = trimmed
	}
	return m
}

// ForTaskType picks the real model for one kind of work. The task types
// here are exactly the ones atomizer.Classify produces.
func (m Models) ForTaskType(taskType string) string {
	if taskType == "coding" && m.Coding != "" {
		return m.Coding
	}
	return m.Default
}
