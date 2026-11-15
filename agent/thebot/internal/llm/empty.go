package llm

// empty.go: an empty answer that will stay empty is not asked for again and again.
//
// A model that answers nothing - a clean reply, no text, no output tokens - is asked again by its caller (the
// operator nudges an unusable reply up to maxInvalidReplies times), and every ask re-bills the whole prompt, about 30k
// tokens on a hosted goal. When two answers in a row in one conversation come back empty with the provider's usage
// saying it generated nothing (output tokens 0), from the same route and with the same finish reason, the emptiness
// is the route's, not chance: the second is returned as an error, ErrEmptyAnswer, and the caller's error path reports
// that the model could not be asked. Anything less certain fails open and is left to the caller as before: no usage
// reported, any output token (reasoning counts), a different finish reason, or a real answer in between.
// After Hermes agent/empty_response_guard.py (deterministic-empty detection).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrEmptyAnswer is a call whose model answered nothing, twice in a row, in the same way: it could not be asked.
var ErrEmptyAnswer = errors.New("the model could not be asked")

// EmptyAnswerError says which route answered nothing, and how it said it stopped.
type EmptyAnswerError struct {
	Route        RouteKey
	FinishReason string
}

// Error is the failure in words a person can read.
func (e *EmptyAnswerError) Error() string {
	reason := e.FinishReason
	if reason == "" {
		reason = "none given"
	}
	return fmt.Sprintf("%v: %s answered with nothing (no text, no output tokens, finish reason %s) twice in a row, "+
		"so asking again would only pay for the prompt again", ErrEmptyAnswer, e.Route.Model, reason)
}

// Is lets errors.Is(err, ErrEmptyAnswer) find it.
func (e *EmptyAnswerError) Is(target error) bool { return target == ErrEmptyAnswer }

// zeroOutput reports whether res is an answer the provider's own usage proves empty: no text, a prompt counted, and no
// token generated.
func zeroOutput(res Result) bool {
	return strings.TrimSpace(res.Text) == "" && res.PromptTokens > 0 && res.ResponseTokens == 0
}

// maxConversations bounds the conversations tracked at once; past it the table starts again (every check fails open
// on a conversation it does not know).
const maxConversations = 512

// conversation is what is kept about one conversation between calls: its empty streak and its usage anchor.
type conversation struct {
	empties     int    // consecutive zero-output answers
	emptyReason string // the finish reason they gave
	anchor      *usageAnchor
}

// conversations is the per-conversation state the Metered wrapper keeps.
type conversations struct {
	mu   sync.Mutex
	byID map[string]*conversation
}

// get is the state of conversation id, made when new. The caller holds c.mu.
func (c *conversations) get(id string) *conversation {
	if c.byID == nil || len(c.byID) >= maxConversations && c.byID[id] == nil {
		c.byID = map[string]*conversation{}
	}
	conv := c.byID[id]
	if conv == nil {
		conv = &conversation{}
		c.byID[id] = conv
	}
	return conv
}

// conversationID names the conversation a call belongs to: its route, the job and purpose it was made for, and its
// fixed start - the first message and the first user message after it, the part a goal's calls of one kind share.
func conversationID(route RouteKey, job, purpose string, messages []Message) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", route, job, purpose)
	if len(messages) > 0 {
		fmt.Fprintf(h, "%s\x00%s\x00", messages[0].Role, messages[0].Content)
		for _, m := range messages[1:] {
			if m.Role == "user" {
				fmt.Fprintf(h, "%s\x00", m.Content)
				break
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// noteAnswer records a successful call's answer in its conversation and returns ErrEmptyAnswer (an
// *EmptyAnswerError) when it is the second zero-output answer in a row with the same finish reason. The streak then
// starts again. The caller holds the conversations' lock.
func (conv *conversation) noteAnswer(route RouteKey, res Result) error {
	if !zeroOutput(res) {
		conv.empties, conv.emptyReason = 0, ""
		return nil
	}
	if conv.empties > 0 && conv.emptyReason == res.FinishReason {
		conv.empties, conv.emptyReason = 0, ""
		return &EmptyAnswerError{Route: route, FinishReason: res.FinishReason}
	}
	conv.empties, conv.emptyReason = 1, res.FinishReason
	return nil
}
