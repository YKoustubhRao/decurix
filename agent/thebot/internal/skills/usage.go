package skills

import (
	"fmt"

	"thebot/internal/llm"
)

// promptParts is what one operator turn sends, by part, in characters: the system frame (and any session history),
// the contract with its rules, the request (the stage's prompt), the model's earlier replies, the earlier command
// results and notes, the newest message, and how many pictures go with it all.
type promptParts struct {
	System, Contract, Request, Replies, Earlier, Latest, Pictures int
}

// measurePrompt splits st.Messages into promptParts. The contract message is the first user message: its request
// part is st.Request's length, the rest is the contract.
func measurePrompt(st *operatorState) promptParts {
	var p promptParts
	contractSeen := false
	for i, m := range st.Messages {
		p.Pictures += len(m.Images)
		n := len(m.Content)
		switch {
		case i == len(st.Messages)-1:
			p.Latest += n
		case m.Role == "system" || (!contractSeen && m.Role != "user"):
			p.System += n
		case m.Role == "user" && !contractSeen:
			contractSeen = true
			req := len(st.Request)
			if req > n {
				req = n
			}
			p.Request += req
			p.Contract += n - req
		case m.Role == "assistant":
			p.Replies += n
		default:
			p.Earlier += n
		}
	}
	return p
}

// turnUsage is the log line that says what one turn cost and what it was spent on (characters / 4 is the estimate
// of tokens by part; the provider's own count and its cached share are the measurement). The agent keeps it in its
// log and out of the chat (thebot_harness.py).
func turnUsage(turn int, st *operatorState, res llm.Result) string {
	p := measurePrompt(st)
	est := func(chars int) int { return (chars + 3) / 4 }
	return fmt.Sprintf("usage: turn %d prompt %d tokens (cached %d), reply %d; est. system %d, contract %d, request %d, "+
		"earlier replies %d, earlier outputs/notes %d, newest %d, pictures %d",
		turn, res.PromptTokens, res.CachedTokens, res.ResponseTokens, est(p.System), est(p.Contract), est(p.Request),
		est(p.Replies), est(p.Earlier), est(p.Latest), p.Pictures)
}
