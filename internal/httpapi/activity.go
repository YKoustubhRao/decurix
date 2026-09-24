package httpapi

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/conversation"
	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// maxActivityIDs bounds one activity request: the dashboard asks for every
// card it shows in one call.
const maxActivityIDs = 200

// maxActivityRunes bounds each line a card shows; the chat has the whole text.
const maxActivityRunes = 160

// Activity is what one desktop's dashboard card says it is doing right now:
// the request its running turn was opened for and the latest step it
// reported. It is read from the chat transcript, so a card can never say
// something different from what the desktop's chat shows.
type Activity struct {
	Running bool `json:"running"`
	// Ticket is a person recording a task on the screen ("/record"), as
	// opposed to an agent working one.
	Ticket    bool      `json:"ticket,omitempty"`
	Request   string    `json:"request,omitempty"`
	Step      string    `json:"step,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`

	// Status and Crash are the desktop's own (desktop.Desktop's), so a card
	// rendered before its desktop crashed or was started turns the right
	// colour on the next poll instead of on the next page load.
	Status string `json:"status,omitempty"`
	Crash  string `json:"crash,omitempty"`
}

// activityHandler answers GET /api/desktops/activity?ids=a,b,c with each
// named desktop's Activity, keyed by ID: one request per dashboard poll,
// however many cards it shows.
func (s *Server) activityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	out := map[string]Activity{}

	// List is cached and shared across callers, so this poll does not add a
	// Docker call per card. A failure only leaves the cards' own status.
	known := map[string]desktop.Desktop{}
	if s.Desktops != nil {
		if list, err := s.Desktops.List(r.Context()); err == nil {
			for _, d := range list {
				known[d.ID] = d
			}
		}
	}

	for i, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if i >= maxActivityIDs {
			break
		}

		id := desktop.SanitizeID(strings.TrimSpace(raw))
		if id == "" {
			continue
		}

		a := activityOf(s.Chat.Snapshot(id))
		if d, ok := known[id]; ok {
			a.Status, a.Crash = d.Status, d.Crash
		}
		out[id] = a
	}

	jsonResponse(w, out)
}

// activityOf reads a desktop's Activity off its chat snapshot.
func activityOf(thread conversation.Thread) Activity {
	if !thread.Running {
		return Activity{}
	}

	activity := Activity{Running: true, StartedAt: thread.StartedAt}

	// The request that opened this turn is the last thing the person sent.
	for i := len(thread.Entries) - 1; i >= 0; i-- {
		if entry := thread.Entries[i]; entry.Role == conversation.RoleUser {
			activity.Request = entry.Content
			break
		}
	}

	if title, ok := strings.CutPrefix(activity.Request, "/record"); ok && (title == "" || title[0] == ' ') {
		activity.Ticket = true
		activity.Request = strings.TrimSpace(title)
	}

	if n := len(thread.Steps); n > 0 {
		activity.Step = thread.Steps[n-1].Text
	}

	activity.Request = clipRunes(activity.Request, maxActivityRunes)
	activity.Step = clipRunes(activity.Step, maxActivityRunes)

	return activity
}

// clipRunes is text on one line, cut to at most n runes with an ellipsis.
func clipRunes(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= n {
		return text
	}

	return string([]rune(text)[:n-1]) + "…"
}
