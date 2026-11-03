package engine

import (
	"fmt"
	"strings"

	"brainiac/internal/trace"
)

// MaxEvalSystemChars is the longest system prompt a request may carry and
// still be shaped like an eval case. `brainiac eval` sends one user turn and
// at most a short instruction; a long agent prompt is a different job.
const MaxEvalSystemChars = 600

// shapeOf is what a request looks like, in counts only: what an earned path
// was measured on is compared with it, and the trace keeps it so a later eval
// can be drawn from real traffic.
func shapeOf(req Request) trace.Shape {
	var s trace.Shape
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			s.UserTurns++
		case "assistant":
			s.AssistantTurns++
		}
		for _, p := range m.Parts {
			switch {
			case p.Kind == "text" && m.Role == "system":
				s.SystemChars += len(p.Text)
			case p.Kind == "text":
				s.TextChars += len(p.Text)
			default:
				s.Media++
			}
		}
	}
	s.Web = (req.Web != nil && *req.Web) || strings.TrimSpace(req.WebQuery) != ""
	return s
}

// likeEvalCases reports whether a request has the shape eval cases have: one
// user turn and no earlier answers, a short system prompt or none, no
// pictures or recordings, and no web lookup. Evidence of a path's accuracy
// holds only for requests like the ones it was measured on; anything else
// stays with the brain that answers it now. It says why not.
func likeEvalCases(s trace.Shape) (bool, string) {
	switch {
	case s.UserTurns != 1 || s.AssistantTurns != 0:
		return false, fmt.Sprintf("a conversation of %d turns", s.UserTurns+s.AssistantTurns)
	case s.SystemChars > MaxEvalSystemChars:
		return false, fmt.Sprintf("a %d-character system prompt", s.SystemChars)
	case s.Media > 0:
		return false, "pictures or recordings"
	case s.Web:
		return false, "a web lookup"
	}
	return true, ""
}
