package httpapi

import (
	"context"
	"log"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
)

// openQuestion returns the text and kind ("approval" or "query") of the newest question desktopID has open: the one a
// reply now reaches. "" when there is none.
func (s *Server) openQuestion(desktopID string) (text, kind string) {
	if s.QueryNotes == nil {
		return "", ""
	}
	var newest *querynotes.QueryNote
	for _, note := range s.QueryNotes.List() {
		if desktop.SanitizeID(note.DesktopID) != desktopID || (note.Kind != querynotes.KindQuery && note.Kind != querynotes.KindApproval) {
			continue
		}
		if newest == nil || note.CreatedAt.After(newest.CreatedAt) {
			n := note
			newest = &n
		}
	}
	if newest == nil {
		return "", ""
	}
	return newest.Content, newest.Kind
}

// signReplyToGate gives the desktop's approval gate the person's reply, signed, before the agent gets it: only a reply
// that came this way can grant anything (desktop.SendGateReply). A desktop without a gate (made before it existed)
// logs and goes on; it settles replies in its agent as it always did, and is recreated to get the boundary.
func (s *Server) signReplyToGate(ctx context.Context, desktopID, taskID, text string) {
	asked, kind := s.openQuestion(desktopID)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
	defer cancel()
	if err := s.Desktops.SendGateReply(ctx, desktopID, taskID, text, asked, kind); err != nil {
		log.Printf("reply to %s: not given to its approval gate: %v", desktopID, err)
	}
}
