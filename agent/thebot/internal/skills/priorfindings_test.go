package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"thebot/internal/workspace"
)

// These tests pin the fix for the worst bug found in live testing: a
// multi-step plan whose steps could not see each other's results.
//
// Live run, verbatim:
//
//	step 1 "search the web for the newest Go version" -> found Go 1.27.1
//	       from go.dev/dl, with real citations
//	step 2 "write a short note about the newest Go version to
//	       go-version.md" -> wrote "the newest stable version of Go is Go
//	       1.21, released on [insert release date]"
//
// The search was real and correct; the write ignored it entirely and
// answered from training data, placeholder and all. A plan whose steps
// don't compose isn't a plan.

func TestPriorFindingsCarriesVerifiedResults(t *testing.T) {
	rc := RunContext{PriorResults: []Result{
		{
			Subtask:  Subtask{Step: "search the web for the newest Go version"},
			Output:   "The newest version of Go is 1.27.1.",
			Verified: true,
		},
	}}

	got := rc.PriorFindings()
	if !strings.Contains(got, "1.27.1") {
		t.Errorf("PriorFindings() = %q, want it to carry the finding forward", got)
	}
	if !strings.Contains(got, "search the web for the newest Go version") {
		t.Errorf("PriorFindings() = %q, want the originating step named", got)
	}
}

// A step that failed found nothing. Feeding its error text forward as a
// "finding" would be worse than feeding nothing.
func TestPriorFindingsSkipsUnverifiedAndEmpty(t *testing.T) {
	rc := RunContext{PriorResults: []Result{
		{Subtask: Subtask{Step: "search the web"}, Output: "connection refused", Verified: false},
		{Subtask: Subtask{Step: "run something"}, Output: "   ", Verified: true},
	}}

	if got := rc.PriorFindings(); got != "" {
		t.Errorf("PriorFindings() = %q, want empty - nothing here was actually found", got)
	}
}

func TestPriorFindingsIsEmptyForTheFirstStep(t *testing.T) {
	if got := (RunContext{}).PriorFindings(); got != "" {
		t.Errorf("PriorFindings() = %q, want empty when nothing has run yet", got)
	}
}

// One huge earlier result must not be able to crowd out the actual
// request, so it is truncated rather than dropped.
func TestPriorFindingsTruncatesHugeResults(t *testing.T) {
	rc := RunContext{PriorResults: []Result{{
		Subtask:  Subtask{Step: "read a giant page"},
		Output:   strings.Repeat("x", priorFindingMaxChars*3),
		Verified: true,
	}}}

	got := rc.PriorFindings()
	if len(got) > priorFindingMaxChars+300 {
		t.Errorf("PriorFindings() length = %d, want it truncated near %d", len(got), priorFindingMaxChars)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("PriorFindings() truncated silently, want it marked")
	}
}

// The direct regression: the file-write model call must actually receive
// what the earlier search found.
func TestFileWriteSendsPriorFindingsToTheModel(t *testing.T) {
	stub := &stubChatProvider{response: "PATH: go-version.md\nCONTENT:\nGo 1.27.1 is the newest release.\n"}
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatalf("workspace.New() error = %v", err)
	}

	rc := RunContext{
		Ctx: context.Background(), Provider: stub, Model: "test-model",
		Workspace: ws, OnLog: func(string) {},
		PriorResults: []Result{{
			Subtask:  Subtask{Step: "search the web for the newest Go version"},
			Output:   "The newest version of Go is 1.27.1, per go.dev/dl.",
			Verified: true,
		}},
	}

	res, err := NewFileWrite().Run(rc, Subtask{
		Step: "write a short note about the newest Go version to go-version.md",
		Type: "file-write",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}

	if len(stub.gotMessages) == 0 {
		t.Fatal("no message reached the model")
	}
	sent := stub.gotMessages[len(stub.gotMessages)-1].Content
	if !strings.Contains(sent, "1.27.1") {
		t.Errorf("the file-write prompt never mentioned what step 1 found.\nprompt was:\n%s", sent)
	}
	if !strings.Contains(sent, "override") {
		t.Error("prior findings were included but not stated as authoritative - the model will prefer its training data")
	}
}

// The live SEC regression: a web search step fetched the real filing URL
// but its own synthesis wrote "no information found" and cited nothing,
// so a naive text-scrape of its prose found no URL at all. Meta["sources"]
// is recorded independent of what the model said about it, and must
// still surface the real URL.
func TestPriorSourceURLsSurvivesBadSynthesis(t *testing.T) {
	rc := RunContext{PriorResults: []Result{{
		Subtask:  Subtask{Step: "search the web for Apple's 10-K"},
		Output:   "The search results did not provide any information about the filing.",
		Verified: true,
		Meta: map[string]any{
			"sources": []string{
				"https://investor.apple.com/sec-filings/",
				"https://www.sec.gov/Archives/edgar/data/320193/000032019325000079/aapl-20250927.htm",
			},
		},
	}}}

	urls := rc.PriorSourceURLs()
	if len(urls) != 2 {
		t.Fatalf("PriorSourceURLs() = %v, want 2 URLs recovered from Meta regardless of the prose", urls)
	}
	found := false
	for _, u := range urls {
		if strings.Contains(u, "aapl-20250927.htm") {
			found = true
		}
	}
	if !found {
		t.Errorf("PriorSourceURLs() = %v, missing the real filing URL", urls)
	}

	// The formatted PriorFindings() text must also carry it, for a skill
	// that reads plain text rather than calling PriorSourceURLs directly.
	if !strings.Contains(rc.PriorFindings(), "aapl-20250927.htm") {
		t.Errorf("PriorFindings() = %q, want the real source URL included even though the prose omitted it", rc.PriorFindings())
	}
}

// A single-page read (web.go's readPage path) records Meta["url"] instead
// of Meta["sources"] - both shapes must be recognised.
func TestPriorSourceURLsRecognisesSingleURLShape(t *testing.T) {
	rc := RunContext{PriorResults: []Result{{
		Subtask: Subtask{Step: "read https://example.com/report"}, Verified: true, Output: "ok",
		Meta: map[string]any{"url": "https://example.com/report"},
	}}}
	urls := rc.PriorSourceURLs()
	if len(urls) != 1 || urls[0] != "https://example.com/report" {
		t.Errorf("PriorSourceURLs() = %v, want the single url shape recognised", urls)
	}
}

// Unverified steps found nothing real - their URLs must not leak forward
// as if they were confirmed.
func TestPriorSourceURLsSkipsUnverified(t *testing.T) {
	rc := RunContext{PriorResults: []Result{{
		Subtask: Subtask{Step: "search"}, Verified: false,
		Meta: map[string]any{"sources": []string{"https://example.com/decoy"}},
	}}}
	if urls := rc.PriorSourceURLs(); len(urls) != 0 {
		t.Errorf("PriorSourceURLs() = %v, want empty for an unverified step", urls)
	}
}

// The full pipeline: download must find the SEC filing URL even when the
// prior search step's own prose was wrong about what it found.
func TestDownloadRecoversURLWhenSearchSynthesisWasWrong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("%PDF real filing bytes"))
	}))
	defer srv.Close()

	rc := newDownloadRC(t)
	rc.PriorResults = []Result{{
		Subtask:  Subtask{Step: "search the web for the filing"},
		Output:   "No information was found about the filing.", // wrong, cites nothing
		Verified: true,
		Meta:     map[string]any{"sources": []string{srv.URL + "/filing.pdf"}},
	}}

	res, err := NewDownload().Run(rc, Subtask{Step: "download the PDF files from the search results URL", Type: "download"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.Verified {
		t.Fatalf("Run() not verified: %s", res.Output)
	}
}

// The general skill is the other place a later step lands, so it needs
// the same carry-forward.
func TestGeneralSendsPriorFindingsToTheModel(t *testing.T) {
	stub := &stubChatProvider{response: "Go 1.27.1."}
	rc := RunContext{
		Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {},
		PriorResults: []Result{{
			Subtask:  Subtask{Step: "search the web for the newest Go version"},
			Output:   "The newest version of Go is 1.27.1.",
			Verified: true,
		}},
	}

	if _, err := NewGeneral().Run(rc, Subtask{Step: "summarise the finding", Type: "general"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var joined strings.Builder
	for _, m := range stub.gotMessages {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	if !strings.Contains(joined.String(), "1.27.1") {
		t.Errorf("general skill never saw the earlier finding.\nmessages:\n%s", joined.String())
	}
}

// Without earlier results nothing extra should be injected - a plain
// single-step request must look exactly as it did before.
func TestGeneralWithoutPriorResultsInjectsNothingExtra(t *testing.T) {
	stub := &stubChatProvider{response: "hi"}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "test-model", OnLog: func(string) {}}

	if _, err := NewGeneral().Run(rc, Subtask{Step: "hello", Type: "general"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, m := range stub.gotMessages {
		if strings.Contains(m.Content, "Earlier steps of this task") {
			t.Error("injected a findings block when there were no prior results")
		}
	}
}

// Measured: a 3B model asked to write a note forgot the PATH/CONTENT
// format, and the step failed with a note already written in its reply.
func TestFileWriteSavesAReplyThatForgotTheFormat(t *testing.T) {
	stub := &stubChatProvider{response: "Here is the note:\n# Go\n\nThe newest Go release is 1.27.1.\n"}
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "m", Workspace: ws, OnLog: func(string) {}}

	res, _ := NewFileWrite().Run(rc, Subtask{Step: "write a short note about the newest Go version to go-version.md", Type: "file-write"})
	if !res.Verified {
		t.Fatalf("result = %+v", res)
	}
	data, _ := os.ReadFile(res.Meta["path"].(string))
	if string(data) != "# Go\n\nThe newest Go release is 1.27.1.\n" {
		t.Errorf("file content = %q", data)
	}
}

func TestFileWriteAcceptsDecoratedMarkers(t *testing.T) {
	stub := &stubChatProvider{response: "**PATH:** notes/today.md\n**Content:**\nremember the milk\n"}
	ws, _ := workspace.New(t.TempDir())
	rc := RunContext{Ctx: context.Background(), Provider: stub, Model: "m", Workspace: ws, OnLog: func(string) {}}
	res, _ := NewFileWrite().Run(rc, Subtask{Step: "write a reminder about milk", Type: "file-write"})
	if !res.Verified || !strings.HasSuffix(res.Meta["path"].(string), "notes/today.md") {
		t.Fatalf("result = %+v", res)
	}
}
