package llm

// Models is deterministic model routing by task type - never asked of a
// model itself, same discipline the atomizer applies to task
// classification. Single default for now since only the terminal skill
// exists; ForTaskType is the seam where e.g. a dedicated coder model gets
// routed for "code-fix" once that skill is ported.
type Models struct {
	Default  string
	Judgment string
}

func DefaultModels(chatModel string) Models {
	return Models{Default: chatModel, Judgment: chatModel}
}

func (m Models) ForTaskType(taskType string) string {
	return m.Default
}
