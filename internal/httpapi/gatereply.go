package httpapi

import (
	"context"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
)

// openQuestions returns the questions and approvals desktopID has open, newest first. Two posted within one clock tick
// (Windows' is coarse) keep the order they were added in: the later one is the newer.
func (s *Server) openQuestions(desktopID string) []querynotes.QueryNote {
	if s.QueryNotes == nil {
		return nil
	}
	notes := s.QueryNotes.List() // oldest first
	var open []querynotes.QueryNote
	for i := len(notes) - 1; i >= 0; i-- {
		note := notes[i]
		if desktop.SanitizeID(note.DesktopID) == desktopID && (note.Kind == querynotes.KindQuery || note.Kind == querynotes.KindApproval) {
			open = append(open, note)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].CreatedAt.After(open[j].CreatedAt) })
	return open
}

// openQuestion returns the text, kind ("approval" or "query") and gate request of the newest question desktopID has
// open: the one a reply now reaches. For an approval the gate worded, the text is the gate's own words. "" when there
// is none.
func (s *Server) openQuestion(desktopID string) (text, kind, request string) {
	open := s.openQuestions(desktopID)
	if len(open) == 0 {
		return "", "", ""
	}
	if open[0].GateRequest != "" {
		return open[0].GateQuestion, open[0].Kind, open[0].GateRequest
	}
	return open[0].Content, open[0].Kind, ""
}

// replySeenKey carries when the newest entry on the person's chat was posted as they typed a reply (the desktop chat's
// seen_at): what was on their screen.
type replySeenKey struct{}

// replyBinding is what a reply about to go to desktopID may settle at its approval gate: the newest open question's
// words and kind, and its gate request only when the reply is bound to that approval. Bound means typed under its card
// (note_id, which replyStillAnswers has already held to the newest), or typed in the desktop's chat with that approval
// on screen and no other question open. Round-4 recheck: a "yes" typed in the chat went to the newest question, so a
// worker could ask something innocent, then post an approval, and the person's yes to the first landed on the second.
//
// A reply from a door that cannot say what the person saw (the requests page, the concierge, an API call) is
// delivered as a message and binds no request: it grants nothing. A chat reply that would land on an approval the
// person had not seen yet, or on one of several open questions, is refused with a reason they can act on.
func (s *Server) replyBinding(ctx context.Context, desktopID string) (asked, kind, request string, err error) {
	open := s.openQuestions(desktopID)
	if len(open) == 0 {
		return "", "", "", nil
	}
	newest := open[0]
	if newest.GateRequest == "" {
		return newest.Content, newest.Kind, "", nil
	}
	asked, kind = newest.GateQuestion, newest.Kind
	if noteID, _ := ctx.Value(replyNoteKey{}).(string); noteID != "" {
		if noteID == newest.ID {
			return asked, kind, newest.GateRequest, nil
		}
		return asked, kind, "", nil
	}
	seen, ok := ctx.Value(replySeenKey{}).(time.Time)
	if !ok {
		return asked, kind, "", nil
	}
	if seen.Before(newest.CreatedAt) {
		return "", "", "", &errUpload{http.StatusConflict,
			"This worker has just asked for your approval. Read its question, then reply."}
	}
	if len(open) > 1 {
		return "", "", "", &errUpload{http.StatusConflict,
			"This worker has more than one open question, so a reply here could answer the wrong one. " +
				"Answer the approval on its own card on the dashboard."}
	}
	return asked, kind, newest.GateRequest, nil
}

// signReplyToGate gives the desktop's approval gate the person's reply, signed, before the agent gets it: only a reply
// that came this way, bound to the gate's request (replyBinding), can grant anything (desktop.SendGateReply). A
// desktop without a gate (made before it existed) logs and goes on; it settles replies in its agent as it always did,
// and is recreated to get the boundary.
func (s *Server) signReplyToGate(ctx context.Context, desktopID, taskID, text, asked, kind, request string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := s.Desktops.SendGateReply(ctx, desktopID, taskID, text, asked, kind, request); err != nil {
		log.Printf("reply to %s: not given to its approval gate: %v", desktopID, err)
	}
}
