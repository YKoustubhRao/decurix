package llm

import "testing"

// Routing exists so a build and a chat turn can use genuinely different
// models - which on a single GPU also decides which one is resident.
func TestForTaskTypeRoutesCodingSeparately(t *testing.T) {
	m := DefaultModels("chat-model").WithCoding("coder-model")

	if got := m.ForTaskType("coding"); got != "coder-model" {
		t.Errorf("ForTaskType(coding) = %q, want the dedicated coder", got)
	}
	for _, taskType := range []string{"general", "web", "terminal", "file-write", "download", "rag-query"} {
		if got := m.ForTaskType(taskType); got != "chat-model" {
			t.Errorf("ForTaskType(%s) = %q, want the default chat model", taskType, got)
		}
	}
}

// Unset must mean "use the default", so a single-model deployment behaves
// exactly as it did before routing existed.
func TestWithCodingIgnoresEmptyValues(t *testing.T) {
	base := DefaultModels("only-model")
	for _, empty := range []string{"", "   ", "\n\t"} {
		if got := base.WithCoding(empty).ForTaskType("coding"); got != "only-model" {
			t.Errorf("WithCoding(%q) routed coding to %q, want the default", empty, got)
		}
	}
	if got := base.WithCoding("  spaced-model  ").ForTaskType("coding"); got != "spaced-model" {
		t.Errorf("WithCoding did not trim: got %q", got)
	}
}
