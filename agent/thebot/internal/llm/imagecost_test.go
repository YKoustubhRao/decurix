package llm

// imagecost_test.go: a picture's price is read off the provider's own prompt counts, per route, and only when the
// reading can be trusted.

import (
	"context"
	"strings"
	"testing"
)

// pricingProvider prices a prompt as a real provider does: text at tokensPerByte, each picture at pictureTokens.
// cachedOnly reports only what follows the previous call's prompt, as a server reusing its cached prefix may.
type pricingProvider struct {
	tokensPerByte float64
	pictureTokens int
	cachedOnly    bool
	last          int
}

func (p *pricingProvider) Name() string                         { return "pricing" }
func (p *pricingProvider) IsReachable(ctx context.Context) bool { return true }
func (p *pricingProvider) Endpoint() string                     { return "http://brain" }
func (p *pricingProvider) Chat(ctx context.Context, model string, messages []Message, opts Options) (Result, error) {
	total := 0.0
	for _, m := range messages {
		total += p.tokensPerByte*float64(len(m.Content)) + float64(p.pictureTokens*len(m.Images))
	}
	prompt := int(total)
	if p.cachedOnly && p.last > 0 {
		prompt, p.last = prompt-p.last, prompt
	} else {
		p.last = prompt
	}
	return Result{Text: "COMMAND: gui observe", PromptTokens: prompt, ResponseTokens: 4, FinishReason: "stop"}, nil
}

// screenGoal runs turns of a goal that looks at the screen, a picture each turn, through p.
func screenGoal(t *testing.T, p Provider, model string, turns int) []Message {
	t.Helper()
	msgs := []Message{{Role: "system", Content: strings.Repeat("You are a worker. ", 200)}, {Role: "user", Content: "Fill the form."}}
	shot := []byte("\x89PNG\r\n\x1a\n-screen-")
	for i := 0; i < turns; i++ {
		res, err := p.Chat(context.Background(), model, msgs, Options{Purpose: PurposeOperatorTurn})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, Message{Role: "assistant", Content: res.Text},
			Message{Role: "user", Content: "Command execution result:\n" + strings.Repeat("Field: value\n", 60+i), Images: [][]byte{append(shot, byte(i))}})
	}
	return msgs
}

func TestAPicturesPriceIsLearnedFromUsagePerRoute(t *testing.T) {
	imageCosts.forget()
	defer imageCosts.forget()
	p := Metered(&pricingProvider{tokensPerByte: 0.3, pictureTokens: 1100})
	screenGoal(t, p, "vision-a", 8)
	route := RouteKey{Provider: "pricing", Model: "vision-a", Endpoint: "http://brain"}
	got, ok := ImageTokens(route)
	if !ok || got < 1000 || got > 1200 {
		t.Fatalf("learned %d (%v), want about 1100", got, ok)
	}
	if active, ok := ActiveImageTokens(); !ok || active != got {
		t.Fatalf("active %d (%v), want the route just learned", active, ok)
	}
	if _, ok := ImageTokens(RouteKey{Provider: "pricing", Model: "vision-b", Endpoint: "http://brain"}); ok {
		t.Fatal("another model inherited the price")
	}
}

func TestAReadingThatCannotBeTrustedTeachesNothing(t *testing.T) {
	imageCosts.forget()
	defer imageCosts.forget()
	// A server that counts only what it had not cached: every reading falls below its anchor.
	screenGoal(t, Metered(&pricingProvider{tokensPerByte: 0.3, pictureTokens: 1100, cachedOnly: true}), "local", 6)
	if n, ok := ActiveImageTokens(); ok {
		t.Fatalf("learned %d from counts that leave out the cached prefix", n)
	}

	// A transcript rewritten between calls (a compaction): the anchor no longer matches, so no reading.
	p := Metered(&pricingProvider{tokensPerByte: 0.3, pictureTokens: 1100})
	msgs := screenGoal(t, p, "hosted", 2) // the second call taught one reading
	before, ok := ActiveImageTokens()
	if !ok {
		t.Fatal("no reading from a trustworthy call")
	}
	msgs[3].Content = "PROGRESS SO FAR - shortened."
	msgs[3].Images = nil
	msgs = append(msgs, Message{Role: "user", Content: "look", Images: [][]byte{[]byte("\x89PNG\r\n\x1a\nnew")}})
	if _, err := p.Chat(context.Background(), "hosted", msgs, Options{Purpose: PurposeOperatorTurn}); err != nil {
		t.Fatal(err)
	}
	if after, _ := ActiveImageTokens(); after != before {
		t.Fatalf("learned %d -> %d across a rewritten transcript", before, after)
	}
}
