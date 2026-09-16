package webui

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/flowera/internal/dag"
	"github.com/YKoustubhRao/flowera/internal/desktop"
	"github.com/YKoustubhRao/flowera/internal/memory"
	"github.com/YKoustubhRao/flowera/internal/querynotes"
	"github.com/YKoustubhRao/flowera/internal/tasks"
)

// Every page is parsed out of the embedded FS and executed here so a broken
// template fails the build rather than the first request that reaches it: the
// renderers use template.Must, so a bad action panics at parse time, and a
// field that no longer exists errors at execute time. Neither shows up in a
// plain `go build`, since templates are data to the compiler.
func TestRenderEveryPage(t *testing.T) {
	renderer := NewRenderer()

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
		ID:      "spare-box",
		Name:    "spare-box",
		Profile: "basic",
		Harness: desktop.HarnessOllama,
		Status:  "stopped",
		URL:     "/desktop/spare-box/",
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
		Desktops:     []desktop.Desktop{running, stopped},
		WorkerMemory: []memory.WorkerMemoryEntry{entry},
		MemoryByDesktop: map[string][]memory.WorkerMemoryEntry{
			running.ID: {entry},
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

	if err := renderer.RenderRequests(io.Discard, page); err != nil {
		t.Fatalf("RenderRequests: %v", err)
	}

	notes := QueryNotesPageData{
		Notes: []querynotes.QueryNote{{
			ID:        "1",
			DesktopID: running.ID,
			VMName:    running.Name,
			Filename:  "11092026120000.txt",
			Content:   "FLOWERA QUERY REQUEST",
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

	// The DAG editor, with a head node, a child wired to it by an edge (its
	// content using the named output placeholder), and one recent run - the
	// canvas, the inspector's hints and the run history all reach real data.
	dagDetail := dag.Detail{
		DAG: dag.DAG{ID: "dag-1", Name: "nightly-report", HeadNodeID: "dnode-1", CronExpr: "0 9 * * *"},
		Nodes: []dag.Node{
			{ID: "dnode-1", DAGID: "dag-1", Name: "fetch", DesktopID: running.ID, Content: "fetch the data", PosX: 40, PosY: 40},
			{ID: "dnode-2", DAGID: "dag-1", Name: "summarize", DesktopID: running.ID, Content: "summarize {{nodes.fetch.reply}}", PosX: 260, PosY: 40},
		},
		Edges:      []dag.Edge{{ID: "dedge-1", DAGID: "dag-1", FromNodeID: "dnode-1", ToNodeID: "dnode-2"}},
		RecentRuns: []dag.Run{dagRun},
	}

	if err := renderer.RenderDag(io.Discard, DagPageData{Detail: dagDetail, Desktops: []desktop.Desktop{running, stopped}}); err != nil {
		t.Fatalf("RenderDag: %v", err)
	}
}
