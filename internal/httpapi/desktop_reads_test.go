package httpapi

import "testing"

// desktopReads says, for every route a desktop may call (containerRoutes), why what it answers is that desktop's to
// read: the handler binds the caller (it answers for the calling desktop only, whatever the request names), or what it
// serves is the same for every desktop (org-public), or a token signed for one view gates it. A route added to the
// allowlist without a line here fails TestEveryDesktopRouteSaysWhoseItReads (review 2026-11-03, C-09: knowledge read
// any worker's and any task's scope).
var desktopReads = map[string]string{
	"/api/llm":                 "org-public: which hosted models are set up, never a key",
	"/api/llm/":                "binds the caller: the proxy answers the calling desktop's own request",
	"/api/native-ollama/":      "binds the caller: the proxy answers the calling desktop's own request",
	"/api/grounder":            "org-public: whether a grounder is configured",
	"/api/grounder/locate":     "binds the caller: answers about the screenshot the caller sent",
	"/api/agent/events":        "binds the caller: actingAs",
	"/api/querytext":           "binds the caller: actingAs",
	"/api/desktops/memory":     "binds the caller: a desktop reads its own memory (requestDesktop)",
	"/api/desktops/learned":    "binds the caller: only its own tasks, citing a person's judgement there (desktopLearnedHandler)",
	"/api/desktops/judgements": "binds the caller: only its own tasks' judgements, a secret one's text never (desktopJudgementsHandler)",
	"/api/outbox":              "binds the caller: drafts are filed as the caller",
	"/api/knowledge":           "binds the caller: worker forced to the caller, task only one of its own (knowledge.Handler.Caller)",
	"/api/skillhub":            "org-public: the organisation's skill catalogue",
	"/api/handoffs":            "binds the caller: handoff.Handler.Caller",
	"/api/handoffs/get":        "binds the caller: only a handoff it sent or was sent",
	"/api/handoffs/cancel":     "binds the caller: only the sender cancels",
	"/api/handoffs/ask":        "binds the caller: handoff.Handler.Caller",
	"/api/host-files/stat":     "binds the caller: only a path its own open task names",
	"/api/host-files/read":     "binds the caller: only a path its own open task names",
	"/api/host-files/release":  "binds the caller: only a path its own open task names",
	"/api/host-bridge/exec":    "token: each command signed for the caller (winhost.ExecToken)",
	"/api/host-bridge/open":    "token: each link signed for a launcher written for the caller",
	"/api/host-gui/start":      "token: signed for one app of the caller",
	"/api/host-gui/stop":       "token: signed for one app of the caller",
	"/api/host-gui/ws":         "token: signed for one view",
	"/host-gui/view":           "token: signed for one view",
	"/phone-app/view":          "token: signed for one view",
	"/api/phone-app/ws":        "token: signed for one view",
	noVNCPath:                  "org-public: the viewer's static files",
	"/favicon.svg":             "org-public: static",
}

func TestEveryDesktopRouteSaysWhoseItReads(t *testing.T) {
	for _, route := range containerRoutes {
		if desktopReads[route.path] == "" {
			t.Errorf("%s is callable from a desktop but desktopReads does not say whose data it serves", route.path)
		}
	}
	for path := range desktopReads {
		found := false
		for _, route := range containerRoutes {
			found = found || route.path == path
		}
		if !found {
			t.Errorf("desktopReads names %s, which a desktop cannot call: drop it", path)
		}
	}
}
