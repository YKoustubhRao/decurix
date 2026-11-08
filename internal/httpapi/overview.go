package httpapi

import (
	"net/http"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/resourcemon"
	"github.com/YKoustubhRao/decurix/internal/tasks"
	"github.com/YKoustubhRao/decurix/internal/webui"
)

// The parts of a card GET /api/desktops/overview can answer (?parts=, every one when absent).
const (
	overviewActivity = "activity"
	overviewTasks    = "tasks"
	overviewMemory   = "memory"
	overviewAlerts   = "alerts"
	overviewNotes    = "notes"
)

// maxOverviewIDs bounds one overview: the cards a page shows, as the activity and task polls were bounded.
const maxOverviewIDs = maxActivityIDs

// OverviewCard is what one desktop's dashboard card polls, in one place: what /api/desktops/activity,
// /api/desktops/tasks, /api/desktops/memory and /api/desktops/resource-alerts say about it. A part the request did
// not ask for is left out; one asked for is always there (an empty list when there is nothing).
type OverviewCard struct {
	Activity *Activity                   `json:"activity,omitempty"`
	Tasks    *[]TaskPick                 `json:"tasks,omitempty"`
	Memory   *[]memory.WorkerMemoryEntry `json:"memory,omitempty"`
	Alert    *resourcemon.Alert          `json:"alert,omitempty"`
	// RestReason and RestNote are why the control plane stopped this desktop to save memory, when it did (the
	// desktop's own, desktop.Desktop.RestReason): a stopped card with one says it is resting and starts when given work.
	RestReason string `json:"rest_reason,omitempty"`
	RestNote   string `json:"rest_note,omitempty"`
}

// Overview is the answer of GET /api/desktops/overview: every card's parts keyed by desktop ID, the open query notes
// the page's rail shows (as /api/querynotes), and whether older memory entries were left out (as /api/desktops/memory
// ?limit=1000 says it).
type Overview struct {
	Desktops        map[string]OverviewCard `json:"desktops"`
	Notes           *[]queryNoteView        `json:"notes,omitempty"`
	MemoryTruncated bool                    `json:"memory_truncated,omitempty"`
}

// overviewParts is the set of parts raw (?parts=) names, every part when it names none it knows.
func overviewParts(raw string) map[string]bool {
	parts := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		switch p = strings.TrimSpace(p); p {
		case overviewActivity, overviewTasks, overviewMemory, overviewAlerts, overviewNotes:
			parts[p] = true
		}
	}
	if len(parts) == 0 {
		for _, p := range []string{overviewActivity, overviewTasks, overviewMemory, overviewAlerts, overviewNotes} {
			parts[p] = true
		}
	}
	return parts
}

// overviewHandler answers GET /api/desktops/overview[?ids=a,b][&parts=activity,tasks,memory,alerts,notes]: what every
// dashboard card polls, in one request, built from one (cached) desktop list and one read of each store, however many
// cards there are (W-14). Without ids it answers every listed desktop. It reads nothing the routes it stands for do
// not, and is served where they are (both listeners; never to a desktop, which GuardPrimary holds to its callbacks).
func (s *Server) overviewHandler(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	parts := overviewParts(r.URL.Query().Get("parts"))
	out := Overview{Desktops: map[string]OverviewCard{}}

	var ids []string
	seen := map[string]bool{}
	add := func(raw string) {
		id := desktop.SanitizeID(strings.TrimSpace(raw))
		if id == "" || seen[id] || len(ids) >= maxOverviewIDs {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if raw := r.URL.Query().Get("ids"); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			add(id)
		}
	}
	named := len(ids) > 0
	cardParts := parts[overviewActivity] || parts[overviewTasks] || parts[overviewMemory] || parts[overviewAlerts]

	// One desktop list (cached and shared, like the activity poll's) for the status, the default set of cards and
	// each one's rest reason. A failure only leaves the cards' own status.
	known := map[string]desktop.Desktop{}
	if cardParts && s.Desktops != nil && (parts[overviewActivity] || !named) {
		if list, err := s.Desktops.List(r.Context()); err == nil {
			for _, d := range list {
				known[d.ID] = d
				if !named {
					add(d.ID)
				}
			}
		}
	}

	if cardParts {
		for _, id := range ids {
			out.Desktops[id] = OverviewCard{}
		}
	}

	if parts[overviewActivity] {
		for _, id := range ids {
			card := out.Desktops[id]
			var a Activity
			if s.Chat != nil {
				a = activityOf(s.Chat.Snapshot(id))
			}
			if d, ok := known[id]; ok {
				a.Status, a.Crash = d.Status, d.Crash
				a.CreatedAt, a.CPUs, a.MemoryMB = d.CreatedAt, d.CPUs, d.MemoryMB
				a.MissingApps = d.MissingApps
				card.RestReason, card.RestNote = d.RestReason, d.RestNote
			}
			card.Activity = &a
			out.Desktops[id] = card
		}
	}

	if parts[overviewTasks] && len(ids) > 0 {
		var byWorker map[string][]tasks.Task
		if s.Tasks != nil {
			var err error
			if byWorker, err = s.Tasks.RecentForWorkers(ids, maxTaskPicks); err != nil {
				jsonError(w, err)
				return
			}
		}
		for _, id := range ids {
			picks := taskPicks(byWorker[id])
			card := out.Desktops[id]
			card.Tasks = &picks
			out.Desktops[id] = card
		}
	}

	if parts[overviewMemory] && len(ids) > 0 {
		// The newest entries of every worker, as many as the card's own poll asked for (at most memoryListMax).
		byDesktop := map[string][]memory.WorkerMemoryEntry{}
		if s.Memory != nil {
			entries := s.Memory.List("")
			if len(entries) > memoryListMax {
				entries, out.MemoryTruncated = entries[:memoryListMax], true
			}
			for _, e := range entries {
				if seen[e.DesktopID] {
					byDesktop[e.DesktopID] = append(byDesktop[e.DesktopID], e)
				}
			}
		}
		for _, id := range ids {
			list := byDesktop[id]
			if list == nil {
				list = []memory.WorkerMemoryEntry{}
			}
			card := out.Desktops[id]
			card.Memory = &list
			out.Desktops[id] = card
		}
	}

	if parts[overviewAlerts] && s.ResourceMon != nil {
		for _, alert := range s.ResourceMon.ListAlerts() {
			id := desktop.SanitizeID(alert.DesktopID)
			if card, ok := out.Desktops[id]; ok {
				card.Alert = alert
				out.Desktops[id] = card
			}
		}
	}

	if parts[overviewNotes] {
		var notes []queryNoteView
		if s.QueryNotes != nil {
			notes = viewQueryNotes(s.QueryNotes.List())
		}
		if notes == nil {
			notes = []queryNoteView{}
		}
		out.Notes = &notes
	}

	jsonResponse(w, out)
}

// taskPicks is a card's task dropdown from a worker's recent tasks, as desktopTasksHandler builds it.
func taskPicks(list []tasks.Task) []TaskPick {
	picks := make([]TaskPick, 0, len(list))
	for _, t := range list {
		picks = append(picks, TaskPick{
			ID:          t.ID,
			Title:       tasks.ShortTitle(t.Request, webui.ShortTitleRunes),
			Request:     t.Request,
			Status:      t.Status,
			StatusLabel: t.Outcome.Label,
			Checked:     t.Checked,
			Outcome:     t.Outcome,
			CreatedAt:   t.CreatedAt,
		})
	}
	return picks
}
