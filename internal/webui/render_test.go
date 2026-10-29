package webui

import (
	"bytes"
	"html/template"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/dag"
	"github.com/YKoustubhRao/decurix/internal/desktop"
	"github.com/YKoustubhRao/decurix/internal/memory"
	"github.com/YKoustubhRao/decurix/internal/querynotes"
	"github.com/YKoustubhRao/decurix/internal/tasks"
)

// Every page is parsed out of the embedded FS and executed here so a broken
// template fails the build rather than the first request that reaches it: the
// renderers use template.Must, so a bad action panics at parse time, and a
// field that no longer exists errors at execute time. Neither shows up in a
// plain `go build`, since templates are data to the compiler.
func TestRenderEveryPage(t *testing.T) {
	renderer := NewRenderer()
	var readiness bytes.Buffer
	if err := renderer.RenderReadiness(&readiness, ReadinessPageData{Desktop: desktop.Desktop{ID: "worker", Name: "Worker"}, OwnerFile: "qualification-owner.token"}); err != nil {
		t.Fatalf("RenderReadiness: %v", err)
	}
	if !strings.Contains(readiness.String(), "Unlock owner controls") || !strings.Contains(readiness.String(), "Enable this scope") {
		t.Fatal("readiness page lost its owner gate or scoped enable control")
	}

	running := desktop.Desktop{
		ID:        "research-box",
		Name:      "research-box",
		Profile:   "developer",
		Harness:   desktop.HarnessTheBot,
		Status:    "running",
		URL:       "/desktop/research-box/",
		CreatedAt: time.Now().Format(time.RFC3339),
		CPUs:      2,
		MemoryMB:  4096,
	}

	stopped := desktop.Desktop{
		ID:       "spare-box",
		Name:     "spare-box",
		Profile:  "basic",
		Harness:  desktop.HarnessOllama,
		Status:   "stopped",
		URL:      "/desktop/spare-box/",
		ParentID: running.ID,
	}

	crashed := desktop.Desktop{
		ID:      "broken-box",
		Name:    "broken-box",
		Profile: "basic",
		Harness: desktop.HarnessOllama,
		Status:  "stopped",
		URL:     "/desktop/broken-box/",
		Crash:   "it exited with code 1",
	}

	entry := memory.WorkerMemoryEntry{
		ID:        "memory-1",
		DesktopID: running.ID,
		Title:     "Lead follow-up",
		Summary:   "Check the CRM before replying.",
		Category:  "workflow",
		CreatedAt: time.Now().UTC(),
	}

	page := PageData{
		Desktops:     []desktop.Desktop{running, stopped, crashed},
		WorkerMemory: []memory.WorkerMemoryEntry{entry},
		MemoryByDesktop: map[string][]memory.WorkerMemoryEntry{
			running.ID: {entry},
		},
		LinkedMemory: map[string][]string{
			running.ID: {stopped.ID},
		},
		DesktopNames: map[string]string{
			running.ID: running.Name,
			stopped.ID: stopped.Name,
		},
		TunnelURL: "https://example.trycloudflare.com",
	}

	var index bytes.Buffer

	if err := renderer.RenderIndex(&index, page); err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}

	// The dashboard's open-in-a-new-tab button is the only thing left that
	// emits ?ui=touch, now that /view/<id> is the chat alone and embeds no
	// screen. That marker is what splices the touch keyboard and pinch-zoom
	// into noVNC (httpapi.injectMobileEnhancements), and losing it fails
	// silently - nothing errors, the affordances just stop being injected -
	// so it is asserted here rather than trusted.
	if !strings.Contains(index.String(), "ui=touch") {
		t.Fatal("the dashboard no longer opens a desktop screen with ?ui=touch; mobile keyboard and pinch-zoom will not be injected")
	}

	// A crashed desktop's card is red and says why, not just "stopped".
	if !strings.Contains(index.String(), `class="status crashed"`) || !strings.Contains(index.String(), "Desktop crashed: it exited with code 1") {
		t.Fatal("a crashed desktop's card does not show that it crashed")
	}

	if err := renderer.RenderRequests(io.Discard, page); err != nil {
		t.Fatalf("RenderRequests: %v", err)
	}

	var triggers strings.Builder
	if err := renderer.RenderTriggers(&triggers, page); err != nil {
		t.Fatalf("RenderTriggers: %v", err)
	}
	for _, want := range []string{`data-kind="cron"`, `data-kind="once"`, `data-kind="email"`, `id="schedList"`, `id="ruleList"`} {
		if !strings.Contains(triggers.String(), want) {
			t.Fatalf("the triggers page is missing %s", want)
		}
	}

	if err := renderer.RenderTriggers(io.Discard, PageData{}); err != nil {
		t.Fatalf("RenderTriggers with no desktops: %v", err)
	}

	var requests strings.Builder
	if err := renderer.RenderRequests(&requests, page); err != nil {
		t.Fatalf("RenderRequests: %v", err)
	}
	if !strings.Contains(requests.String(), `id="onEmail"`) || !strings.Contains(requests.String(), `href="/triggers"`) {
		t.Fatal("the requests page no longer offers setting up an email trigger")
	}

	notes := QueryNotesPageData{
		Notes: []querynotes.QueryNote{{
			ID:        "1",
			DesktopID: running.ID,
			VMName:    running.Name,
			Filename:  "11092026120000.txt",
			Content:   "DECURIX QUERY REQUEST",
			CreatedAt: time.Now(),
		}},
	}

	if err := renderer.RenderQueryNotesDashboard(io.Discard, notes); err != nil {
		t.Fatalf("RenderQueryNotesDashboard: %v", err)
	}

	// The task pages, empty and full: the empty dashboard is a different
	// branch, and a task with a screenshot step and an unanswered approval
	// reaches every part of the record page.
	now := time.Now().UTC()
	cost := 0.12
	tokens := int64(900)

	if err := renderer.RenderTasks(io.Discard, TasksPageData{Window: WindowOption{Value: "7", Phrase: "this week"}}); err != nil {
		t.Fatalf("RenderTasks (empty): %v", err)
	}

	group := tasks.Group{Key: "claude", Label: "Claude Code", Tasks: 2, Settled: 2, Autonomous: 1, Rate: .5, TopReason: tasks.ReasonApprovalPayment, Models: []string{"claude-opus-5"}, CostUSD: &cost}
	summary := tasks.Summary{
		Task:       tasks.Task{ID: "t0123456789abcdef", WorkerID: running.ID, WorkerName: running.Name, Engine: "claude", Request: "pay invoice", Type: tasks.TypeConsequential, Status: tasks.StatusWaiting, CreatedAt: now},
		Executions: 2, ScreenActions: 1, SteppedIn: []string{tasks.ReasonApprovalPayment}, CostUSD: &cost,
	}

	full := TasksPageData{
		Window:  WindowOption{Value: "7", Label: "7 days", Phrase: "this week", Active: true},
		Windows: []WindowOption{{Value: "7", Label: "7 days", Active: true}, {Value: "all", Label: "All time"}},
		Report: tasks.Report{
			Definition: tasks.AutonomyDefinition,
			Totals:     group, ByEngine: []tasks.Group{group}, ByWorker: []tasks.Group{group}, ByType: []tasks.Group{group},
			Best: &group, StepIns: 1,
			Reasons: []tasks.Reason{{Key: tasks.ReasonApprovalPayment, Label: "Approval before a payment", Approval: true, Count: 1, Share: 1,
				ByEngine: []tasks.EngineCount{{Engine: "claude", Label: "Claude Code", Count: 1}},
				Examples: []tasks.ReasonExample{{TaskID: summary.ID, Worker: running.Name, Request: "pay invoice", Words: "Approval needed"}}}},
		},
		Tasks:  []tasks.Summary{summary},
		Filter: tasks.Filter{Engine: "claude"},
	}

	if err := renderer.RenderTasks(io.Discard, full); err != nil {
		t.Fatalf("RenderTasks: %v", err)
	}

	detail := TaskPageData{Task: tasks.Detail{
		Task:   summary.Task,
		Worker: tasks.Worker{ID: running.ID, Name: running.Name, DeletedAt: now},
		Executions: []tasks.Execution{
			{ID: "x1", Attempt: 1, Engine: "claude", Status: tasks.StatusError, FinalReply: "timed out", StartedAt: now, EndedAt: now.Add(time.Minute)},
			{ID: "x2", Attempt: 2, Engine: "claude", Model: "claude-opus-5", Status: tasks.StatusWaiting, StartedAt: now, CostUSD: &cost, InputTokens: &tokens,
				Steps: []tasks.Step{
					{Text: "$ ls", CreatedAt: now},
					{Text: "click Pay now", CreatedAt: now.Add(time.Second), Action: &tasks.Action{Kind: "click", Outcome: "needs_approval", Reason: "payment", Before: "/api/desktops/step-image?id=ab"}},
				},
				Interventions: []tasks.Intervention{{Kind: tasks.InterventionApproval, Reason: tasks.ReasonApprovalPayment, Request: "Approval needed", RequestedAt: now.Add(2 * time.Second)}},
			},
		},
	}}

	if err := renderer.RenderTask(io.Discard, detail); err != nil {
		t.Fatalf("RenderTask: %v", err)
	}

	// The trace page, from the same record plus a long output step (folded)
	// and an answered query: the request, every step with its number and
	// kind, the question and its answer, the final report and the copy/
	// download tools must all be there.
	traceDetail := detail.Task
	longOutput := "Output: line 1\n" + strings.Repeat("more\n", 12)
	traceDetail.Executions[1].Steps = append(traceDetail.Executions[1].Steps,
		tasks.Step{Text: longOutput, CreatedAt: now.Add(3 * time.Second)},
		tasks.Step{Text: "Final check: judged done", CreatedAt: now.Add(4 * time.Second)},
	)
	traceDetail.Executions[1].Interventions = append(traceDetail.Executions[1].Interventions,
		tasks.Intervention{Kind: tasks.InterventionQuery, Reason: tasks.ReasonCredentials, Request: "Which account?", Reply: "The second one", RequestedAt: now.Add(5 * time.Second), RepliedAt: now.Add(6 * time.Second)},
	)
	traceDetail.Executions[1].FinalReply = "Paid INV-1042."
	traceDetail.Verdict, traceDetail.VerdictNote, traceDetail.VerdictAt = tasks.VerdictWrong, "wrong account", now

	var trace bytes.Buffer
	if err := renderer.RenderTaskTrace(&trace, TaskTracePageData{Task: traceDetail}); err != nil {
		t.Fatalf("RenderTaskTrace: %v", err)
	}
	for _, want := range []string{
		"pay invoice", "#1", "#4", `class="step kind-command"`, `class="step kind-screen"`, `class="step kind-output"`,
		`<details class="fold">`, "13 lines · show all", "Which account?", "The second one", "Approval needed",
		"Paid INV-1042.", "timed out", `href="/tasks/t0123456789abcdef/trace.txt?download=1"`, "data-copy-trace",
		"Verdict: <b class=\"wrong\">wrong</b>", "wrong account", `href="/tasks/t0123456789abcdef"`,
	} {
		if !strings.Contains(trace.String(), want) {
			t.Errorf("the trace page is missing %q", want)
		}
	}

	// The plain-text trace holds the same, in order, as one shareable file.
	text := TraceText(traceDetail)
	for _, want := range []string{
		"Task t0123456789abcdef", "Request:\npay invoice", "==== Attempt 1: Failed", "==== Attempt 2: Waiting",
		"[1] ", "[4] ", "Command\n    $ ls", "Screen\n    click Pay now", "action: click -> needs_approval; reason: payment",
		"[?] ", "Approval asked", "Query asked (", "    Which account?", "Reply at", "    The second one", "(no reply yet)",
		"Final report (Waiting on you) --\nPaid INV-1042.", "Verdict: wrong", "Verdict note: wrong account", "Tokens: 900 in / — out", "Cost: $0.12",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the plain-text trace is missing %q in:\n%s", want, text)
		}
	}
	if idx := strings.Index(text, "[?] "); idx < 0 || idx < strings.Index(text, "[2] ") {
		t.Error("the plain-text trace does not keep the steps and the question in time order")
	}

	// The dashboard card lists the desktop's tasks newest first and links the
	// Trace button to the first; a desktop with none says so, disabled.
	page.TasksByDesktop = map[string][]tasks.Task{
		running.ID: {
			{ID: "t1111111111111111", Request: strings.Repeat("a very long request ", 10), Status: tasks.StatusRunning, CreatedAt: now},
			{ID: "t2222222222222222", Request: "older task", Status: tasks.StatusDone, CreatedAt: now.Add(-time.Hour)},
		},
	}
	index.Reset()
	if err := renderer.RenderIndex(&index, page); err != nil {
		t.Fatalf("RenderIndex with tasks: %v", err)
	}
	for _, want := range []string{
		`data-task-pick="research-box"`, `href="/tasks/t1111111111111111/trace"`, `<option value="t2222222222222222">older task · Done · `,
		`data-task-pick="spare-box"`, `<option value="">No tasks yet</option>`, "/api/desktops/tasks?",
	} {
		if !strings.Contains(index.String(), want) {
			t.Errorf("the dashboard lacks %q", want)
		}
	}
	// A running worker's card offers Simulate on this computer; the gated listener never shows it (docs/simulation.md).
	if !strings.Contains(index.String(), `href="/readiness/research-box#simulate"`) {
		t.Error("a running worker's card lacks the Simulate button")
	}
	gated := page
	gated.Gated = true
	var gatedIndex strings.Builder
	if err := renderer.RenderIndex(&gatedIndex, gated); err != nil {
		t.Fatalf("RenderIndex gated: %v", err)
	}
	if strings.Contains(gatedIndex.String(), "#simulate") {
		t.Error("the gated dashboard shows the Simulate button")
	}
	if strings.Contains(index.String(), strings.Repeat("a very long request ", 10)) {
		t.Error("the task dropdown shows the whole request instead of a short title")
	}

	// Both branches of the desktop view: a running desktop renders the chat
	// and its screen, a stopped one renders only the "start it first" state.
	for _, d := range []desktop.Desktop{running, stopped} {
		if err := renderer.RenderDesktopView(io.Discard, DesktopViewPageData{Desktop: d}); err != nil {
			t.Fatalf("RenderDesktopView(%s): %v", d.Status, err)
		}
	}

	// The recordings page, for a running desktop (recorder, list, detail) and
	// a stopped one (only the "start it" state).
	for _, d := range []desktop.Desktop{running, stopped} {
		var out bytes.Buffer
		if err := renderer.RenderRecordings(&out, RecordingsPageData{Desktop: d, Session: "demo-20260914T101500-abc123"}); err != nil {
			t.Fatalf("RenderRecordings(%s): %v", d.Status, err)
		}
		if d.Status == "running" && !strings.Contains(out.String(), `data-session="demo-20260914T101500-abc123"`) {
			t.Fatal("the recordings page lost the recording it was asked to open")
		}
	}

	// The skills page, for a running desktop (the list is read by the page's
	// own JS) and a stopped one (only the "start it" state).
	for _, d := range []desktop.Desktop{running, stopped} {
		var out bytes.Buffer
		if err := renderer.RenderSkills(&out, SkillsPageData{Desktop: d}); err != nil {
			t.Fatalf("RenderSkills(%s): %v", d.Status, err)
		}
		// The page's script reads the list into #loading's place, and only a
		// running desktop has one to read.
		if hasList := strings.Contains(out.String(), `id="loading"`); hasList != (d.Status == "running") {
			t.Fatalf("the skills page of a %s desktop has its list: %v", d.Status, hasList)
		}
	}

	// The viewer a desktop opens for an app running on the host. Its values are
	// handed to the page's script, so they must arrive as quoted JS strings and
	// never as markup or script the URL's author wrote.
	var view bytes.Buffer
	if err := renderer.RenderHostGUI(&view, HostGUIPageData{ID: "vm1-code", Title: "Code </title><script>alert(1)</script>", Target: `C:\Program Files\App\a.exe`, Args: `"x"</script>`}); err != nil {
		t.Fatalf("RenderHostGUI: %v", err)
	}
	if strings.Contains(view.String(), "<script>alert(1)") || strings.Contains(view.String(), `"x"</script>`) {
		t.Fatalf("the host app viewer let a value break out of its context:\n%s", view.String())
	}
	if !strings.Contains(view.String(), `TARGET = "C:\\Program Files\\App\\a.exe"`) {
		t.Fatalf("the launch target did not reach the page's script as a JS string:\n%s", view.String())
	}

	// The viewer for an app on a linked phone: the app's name comes from the
	// phone, so it must stay a quoted string too.
	var phoneView bytes.Buffer
	if err := renderer.RenderPhoneApp(&phoneView, PhoneAppPageData{ID: "vm1-phone-x", Title: "Evil </title><script>alert(1)</script>", Device: "d1", Package: "com.x"}); err != nil {
		t.Fatalf("RenderPhoneApp: %v", err)
	}
	if strings.Contains(phoneView.String(), "<script>alert(1)") || !strings.Contains(phoneView.String(), `PKG = "com.x"`) {
		t.Fatalf("the phone app viewer let a value break out, or lost the package:\n%s", phoneView.String())
	}

	// The screen viewer: a desktop's name is whatever a person typed and its
	// password goes into the page's script, so both must stay quoted strings.
	var screen bytes.Buffer
	if err := renderer.RenderScreen(&screen, ScreenPageData{
		ID: "terga", Name: `Ops </title><script>alert(1)</script>`, Password: `p"w</script>`,
		NoVNC: "/static/novnc/rfb-1.6.0.js", Controls: template.HTML(`<div id="decurix-ticket"></div>`),
	}); err != nil {
		t.Fatalf("RenderScreen: %v", err)
	}
	viewer := screen.String()
	if strings.Contains(viewer, "<script>alert(1)") || strings.Contains(viewer, `p"w</script>`) {
		t.Fatalf("the screen viewer let a value break out of its context:\n%s", viewer)
	}
	for _, want := range []string{
		`import { RFB, KeyTable, keysyms } from "/static/novnc/rfb-1.6.0.js"`,
		`const ID = "terga"`,
		`<div id="decurix-ticket"></div>`,
	} {
		if !strings.Contains(viewer, want) {
			t.Fatalf("the screen viewer is missing %q", want)
		}
	}

	// The workforce page: every section on the primary listener, and none of
	// the primary-only ones (queue, handoffs, access) when asked for through
	// the gated listener - neither their markup nor the script that calls them.
	var workforce bytes.Buffer
	if err := renderer.RenderWorkforce(&workforce, WorkforcePageData{HasSkillHub: true, HasKnowledge: true, HasQueue: true, HasHandoffs: true, HasAccess: true}); err != nil {
		t.Fatalf("RenderWorkforce: %v", err)
	}
	for _, want := range []string{
		`id="sec-workers"`, `id="sec-skills"`, `id="sec-queue"`, `id="sec-handoffs"`, `id="sec-knowledge"`, `id="sec-access"`,
		"/api/skillhub/sync", "/api/queue/limits", "/api/handoffs/cancel", "/api/knowledge/why", "/api/access/policies/check",
		`href="/workforce" class="active"`, "setInterval(refresh, POLL_MS)",
	} {
		if !strings.Contains(workforce.String(), want) {
			t.Errorf("the workforce page is missing %q", want)
		}
	}
	workforce.Reset()
	if err := renderer.RenderWorkforce(&workforce, WorkforcePageData{Gated: true, HasSkillHub: true, HasKnowledge: true}); err != nil {
		t.Fatalf("RenderWorkforce (gated): %v", err)
	}
	for _, gone := range []string{`id="sec-access"`, "/api/access/", `id="sec-queue"`, "/api/queue", `id="sec-handoffs"`, "/api/handoffs"} {
		if strings.Contains(workforce.String(), gone) {
			t.Errorf("the gated workforce page still has %q", gone)
		}
	}
	if err := renderer.RenderWorkforce(io.Discard, WorkforcePageData{}); err != nil {
		t.Fatalf("RenderWorkforce with nothing wired: %v", err)
	}

	// The DAGs list, empty and with one entry that has already run.
	if err := renderer.RenderDags(io.Discard, DagsPageData{}); err != nil {
		t.Fatalf("RenderDags (empty): %v", err)
	}

	dagRun := dag.Run{ID: "drun-1", DAGID: "dag-1", Status: dag.RunSucceeded, Trigger: dag.TriggerManual, StartedAt: now, EndedAt: now.Add(time.Minute)}

	if err := renderer.RenderDags(io.Discard, DagsPageData{
		Dags: []dag.Summary{{
			DAG:       dag.DAG{ID: "dag-1", Name: "nightly-report", HeadNodeID: "dnode-1", CronExpr: "0 9 * * *"},
			NodeCount: 2, EdgeCount: 1, LastRun: &dagRun,
		}},
	}); err != nil {
		t.Fatalf("RenderDags: %v", err)
	}

	// The DAG editor page itself only renders the DAG record server-side;
	// the canvas and run history are populated by the page's own JS
	// fetches (loadDag/loadRuns) against /api/dags/get and /api/dags/runs.
	if err := renderer.RenderDag(io.Discard, DagPageData{
		DAG:      dag.DAG{ID: "dag-1", Name: "nightly-report", HeadNodeID: "dnode-1", CronExpr: "0 9 * * *"},
		Desktops: []desktop.Desktop{running, stopped},
	}); err != nil {
		t.Fatalf("RenderDag: %v", err)
	}
}

func TestWorkerResourceDefaultsInDashboard(t *testing.T) {
	t.Setenv("DECURIX_DEFAULT_DESKTOP_CPUS", "3")
	t.Setenv("DECURIX_DEFAULT_DESKTOP_MEMORY_MB", "6144")
	var out bytes.Buffer
	if err := NewRenderer().RenderIndex(&out, PageData{}); err != nil {
		t.Fatal(err)
	}
	for _, checked := range []string{
		"name=\"cpus\" value=\"3\" checked",
		"name=\"memory_mb\" value=\"6144\" checked",
	} {
		if !strings.Contains(out.String(), checked) {
			t.Fatalf("configured default missing: %s", checked)
		}
	}
	if strings.Contains(out.String(), "name=\"cpus\" value=\"1\" checked") {
		t.Fatal("hardcoded CPU default overrides configuration")
	}
}

// A worker made with a shared folder says which folder on the card, and the create form can set it: the person must be
// able to see what the worker can touch.
func TestTheDashboardShowsAndTakesTheSharedFolder(t *testing.T) {
	shared := desktop.Desktop{
		ID: "pc-box", Name: "pc-box", Profile: "basic", Harness: desktop.HarnessTheBot, Status: "running",
		URL: "/desktop/pc-box/", SharedFolder: `C:\Users\Administrator\Documents\Decurix demo`,
	}
	plain := desktop.Desktop{ID: "plain-box", Name: "plain-box", Profile: "basic", Harness: desktop.HarnessTheBot, Status: "running", URL: "/desktop/plain-box/"}

	var out bytes.Buffer
	if err := NewRenderer().RenderIndex(&out, PageData{Desktops: []desktop.Desktop{shared, plain}}); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	for _, want := range []string{
		`name="shared_folder"`,        // the create form's field
		`data-shared-folder="pc-box"`, // the card of the shared worker
		`C:\Users\Administrator\Documents\Decurix demo`,
		"shared_folder: sharedFolder", // sent with the create request
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the dashboard lacks %q", want)
		}
	}
	if strings.Contains(page, `data-shared-folder="plain-box"`) {
		t.Error("a worker with no shared folder says it shares one")
	}
}

// A requested host app the desktop was made without is on its card with the reason, and one still being tried says so:
// never a silent skip. A desktop that has everything it asked for shows nothing.
func TestTheCardSaysWhichRequestedAppsAreMissingAndWhy(t *testing.T) {
	short := desktop.Desktop{
		ID: "pc", Name: "pc", Profile: "basic", Harness: desktop.HarnessTheBot, Status: "running", URL: "/desktop/pc/",
		MissingApps: []desktop.MissingApp{
			{ID: "win_paint", Name: "Paint", Reason: "This is a Microsoft Store app."},
			{ID: "win_word", Name: "Word", Reason: desktop.UnverifiedAppReason, Pending: true},
		},
	}
	whole := desktop.Desktop{ID: "whole", Name: "whole", Profile: "basic", Harness: desktop.HarnessTheBot, Status: "running", URL: "/desktop/whole/"}

	var out bytes.Buffer
	if err := NewRenderer().RenderIndex(&out, PageData{Desktops: []desktop.Desktop{short, whole}}); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	for _, want := range []string{
		"<strong>Paint</strong> was not added: This is a Microsoft Store app.",
		"<strong>Word</strong> is being added: Not added yet",
		`data-missing-apps="whole" hidden`, // there, for the poll to fill, but nothing shown
		"paintMissingApps(card.querySelector(\"[data-missing-apps]\")",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the dashboard lacks %q", want)
		}
	}
	if strings.Contains(page, `data-missing-apps="pc" hidden`) {
		t.Error("the card of a desktop missing apps hides them")
	}
}
