package memory

import (
	"context"
	"os"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// judgeStub returns a canned judgement so the store/recall behaviour can
// be tested deterministically. Embeddings still come from a real Ollama
// (skipped when unavailable) because "does recall actually find the right
// fact" is only meaningful with real vectors.
type judgeStub struct{ response string }

func (j *judgeStub) Name() string { return "judge-stub" }
func (j *judgeStub) Chat(ctx context.Context, model string, messages []llm.Message, opts llm.Options) (llm.Result, error) {
	return llm.Result{Text: j.response}, nil
}
func (j *judgeStub) IsReachable(ctx context.Context) bool { return true }

func realEmbedder(t *testing.T) *llm.OllamaProvider {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = "http://127.0.0.1:11434"
	}
	p := llm.NewOllama(host)
	if !p.IsReachable(context.Background()) {
		t.Skipf("no real Ollama at %s - L1 tests need real embeddings", host)
	}
	return p
}

func TestParseJudgementYes(t *testing.T) {
	ok, fact := parseJudgement("REMEMBER: yes\nFACT: The user's name is Alex Doe.")
	if !ok {
		t.Error("parseJudgement() = false, want true")
	}
	if fact != "The user's name is Alex Doe." {
		t.Errorf("fact = %q, unexpected", fact)
	}
}

func TestParseJudgementNo(t *testing.T) {
	ok, fact := parseJudgement("REMEMBER: no\nFACT:")
	if ok {
		t.Error("parseJudgement() = true for a 'no', want false")
	}
	if fact != "" {
		t.Errorf("fact = %q, want empty", fact)
	}
}

func TestLooksExplicit(t *testing.T) {
	for _, p := range []string{
		"my name is Alex", "remember this detail", "don't forget I use Go",
		"keep in mind I prefer tabs",
	} {
		if !looksExplicit(p) {
			t.Errorf("looksExplicit(%q) = false, want true", p)
		}
	}
	if looksExplicit("what is 1+1?") {
		t.Error("looksExplicit() = true for an ordinary question, want false")
	}
}

func TestStoreAndRecallFindsTheRightFact(t *testing.T) {
	p := realEmbedder(t)
	l1, err := NewL1(t.TempDir(), &judgeStub{}, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()
	ctx := context.Background()

	if err := l1.Store(ctx, "The user's name is Alex Doe.", "explicit"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if err := l1.Store(ctx, "The user prefers Go over Python for backend work.", "inferred"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	got, err := l1.Recall(ctx, "what is my name?", 3)
	if err != nil {
		t.Fatalf("Recall() error = %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Recall() returned nothing")
	}
	if !strings.Contains(got[0].Fact, "Alex") {
		t.Errorf("top recalled fact = %q, expected the name fact to rank first", got[0].Fact)
	}
}

// The direct regression test for the duplicate found live: the same fact
// arriving twice must not create two rows.
func TestStoreDedupesNearIdenticalFacts(t *testing.T) {
	p := realEmbedder(t)
	l1, err := NewL1(t.TempDir(), &judgeStub{}, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()
	ctx := context.Background()

	if err := l1.Store(ctx, "The user's name is Alex Doe.", "explicit"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if err := l1.Store(ctx, "The user's name is Alex Doe.", "inferred"); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	n, err := l1.Count(ctx)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 1 {
		t.Errorf("Count() = %d after storing the same fact twice, want 1", n)
	}

	// An explicit statement must not be downgraded to inferred by a later
	// restatement.
	recent, err := l1.ListRecent(ctx, 1)
	if err != nil {
		t.Fatalf("ListRecent() error = %v", err)
	}
	if len(recent) != 1 || recent[0].Source != "explicit" {
		t.Errorf("entry = %+v, want source to stay explicit", recent)
	}
}

func TestStoreKeepsGenuinelyDifferentFacts(t *testing.T) {
	p := realEmbedder(t)
	l1, err := NewL1(t.TempDir(), &judgeStub{}, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()
	ctx := context.Background()

	_ = l1.Store(ctx, "The user's name is Alex Doe.", "explicit")
	_ = l1.Store(ctx, "The user is building an AI agent harness in Go.", "inferred")

	n, _ := l1.Count(ctx)
	if n != 2 {
		t.Errorf("Count() = %d for two genuinely different facts, want 2", n)
	}
}

func TestRecallEmptyStoreIsNotAnError(t *testing.T) {
	p := realEmbedder(t)
	l1, err := NewL1(t.TempDir(), &judgeStub{}, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()

	got, err := l1.Recall(context.Background(), "anything", 5)
	if err != nil {
		t.Fatalf("Recall() on an empty store errored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Recall() = %v, want nothing", got)
	}
}

// ConsiderExchange is the real entry point the server calls - this proves
// a "remember my name" exchange really does end up stored, in one go.
func TestConsiderExchangeStoresWhenJudgedImportant(t *testing.T) {
	p := realEmbedder(t)
	judge := &judgeStub{response: "REMEMBER: yes\nFACT: The user's name is Alex Doe."}
	l1, err := NewL1(t.TempDir(), judge, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()
	ctx := context.Background()

	l1.ConsiderExchange(ctx, "My name is Alex Doe. Remember this.", "Noted!", nil)

	n, _ := l1.Count(ctx)
	if n != 1 {
		t.Fatalf("Count() = %d, want 1 - the exchange should have been stored in a single go", n)
	}
	recent, _ := l1.ListRecent(ctx, 1)
	if len(recent) != 1 || recent[0].Source != "explicit" {
		t.Errorf("entry = %+v, want it recorded as explicit (the user asked)", recent)
	}
}

func TestConsiderExchangeSkipsUnimportant(t *testing.T) {
	p := realEmbedder(t)
	judge := &judgeStub{response: "REMEMBER: no\nFACT:"}
	l1, err := NewL1(t.TempDir(), judge, p, "test-model")
	if err != nil {
		t.Fatalf("NewL1() error = %v", err)
	}
	defer l1.Close()
	ctx := context.Background()

	l1.ConsiderExchange(ctx, "what is 1+1?", "2", nil)

	n, _ := l1.Count(ctx)
	if n != 0 {
		t.Errorf("Count() = %d, want 0 - trivia should not be stored", n)
	}
}
