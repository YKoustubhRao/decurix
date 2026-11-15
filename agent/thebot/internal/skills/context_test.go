package skills

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"thebot/internal/llm"
)

// notesProvider answers every call with the same notes.
type notesProvider struct{ calls int }

func (p *notesProvider) Name() string                         { return "notes" }
func (p *notesProvider) IsReachable(ctx context.Context) bool { return true }
func (p *notesProvider) Chat(_ context.Context, _ string, _ []llm.Message, _ llm.Options) (llm.Result, error) {
	p.calls++
	return llm.Result{Text: "NOTES: GSTR-1 saved; B2CS 254237.29 from COMPUTATION!B72; next the HSN table."}, nil
}

func longGoal(n int) *operatorState {
	st := &operatorState{Hosted: true, Request: "file September", PrefixLen: 2}
	st.Messages = []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "contract: file September"}}
	for i := 0; i < n; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: fmt.Sprintf("COMMAND:\ngui click e%d", i)})
		content := fmt.Sprintf("Command execution result:\n\nexit_code: 0\n\noutput:\nclicked e%d", i)
		if i == 3 {
			content = "The human replied: the rent is paid in cash\n\nContinue the task."
		}
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: content})
		st.noteLedger(fmt.Sprintf("gui click e%d", i), shellResult{Output: fmt.Sprintf("Clicked e%d", i)})
	}
	return st
}

func TestALongGoalIsShortenedToItsPrefixNotesRecordAndNewestSteps(t *testing.T) {
	st := longGoal(30)
	st.ScreenMsgs = []int{5, len(st.Messages) - 1}
	provider := &notesProvider{}
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: provider, OnLog: func(string) {}}, st, "test")

	if provider.calls != 1 {
		t.Fatalf("notes asked %d times", provider.calls)
	}
	if st.Messages[0].Content != "frame" || st.Messages[1].Content != "contract: file September" {
		t.Error("the prefix changed")
	}
	note := st.Messages[2].Content
	for _, want := range []string{"PROGRESS SO FAR", "B2CS 254237.29 from COMPUTATION!B72", "$ gui click e0 -> exit 0 · Clicked e0",
		"The human replied: the rent is paid in cash"} {
		if !strings.Contains(note, want) {
			t.Errorf("progress note lacks %q", want)
		}
	}
	if st.Messages[3].Role != "assistant" || len(st.Messages) != 3+keepRecentMessages {
		t.Errorf("kept %d messages, first kept %q", len(st.Messages)-3, st.Messages[3].Role)
	}
	if len(st.ScreenMsgs) != 1 || st.ScreenMsgs[0] != len(st.Messages)-1 {
		t.Errorf("screen indexes not moved with their messages: %v", st.ScreenMsgs)
	}
	if st.Compactions != 1 {
		t.Error("not counted")
	}
}

// bigGoal is a goal whose every output is as long as a hosted brain's limit and whose ledger is long.
func bigGoal(t *testing.T) *operatorState {
	t.Helper()
	st := &operatorState{Hosted: true, Request: "file September", PrefixLen: 2, OutputsDir: t.TempDir()}
	st.Messages = []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "contract: file September"}}
	output := strings.Repeat("a line of a long listing\n", 640) // 16,000 characters
	for i := 0; i < 40; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: fmt.Sprintf("COMMAND:\noffice read book%d.xlsx", i)})
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "Command execution result:\n\nexit_code: 0\n\noutput:\n" + output})
		st.noteLedger(fmt.Sprintf("office read book%d.xlsx %s", i, strings.Repeat("--range A1:Z9 ", 20)), shellResult{Output: fmt.Sprintf("row %d\n", i) + output})
	}
	return st
}

func TestACompactionFreesRealRoomHoweverLongTheKeptOutputs(t *testing.T) {
	st := bigGoal(t)
	if transcriptChars(st) <= contextBudgetChars || !st.contextOverBudget() {
		t.Fatalf("not over budget: %d", transcriptChars(st))
	}
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	if got := transcriptChars(st); got > contextBudgetChars/2 {
		t.Errorf("after a compaction %d characters are still re-sent", got)
	}
	if st.CompactedTo != transcriptChars(st) {
		t.Errorf("CompactedTo %d", st.CompactedTo)
	}
}

func TestACompactionThatCouldNotFreeRoomIsNotTriedAgainAtOnce(t *testing.T) {
	st := bigGoal(t)
	st.CompactedTo = transcriptChars(st) - 1000
	if st.contextOverBudget() {
		t.Error("compacted again with 1,000 characters of growth")
	}
	st.CompactedTo = transcriptChars(st) - compactRegrowth
	if !st.contextOverBudget() {
		t.Error("not compacted after growing by a quarter of the budget")
	}
}

func TestAShortenedOutputCanBeReadAgainAndNoLaterOneOverwritesIt(t *testing.T) {
	st := bigGoal(t)
	first := st.Ledger[0]
	i := strings.Index(first, "(full output: ")
	if i < 0 {
		t.Fatalf("no pointer in %q", first[:200])
	}
	path := strings.TrimSuffix(first[i+len("(full output: "):], ")")
	before, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(before), "row 0\n") {
		t.Fatalf("%v, %q", err, string(before)[:20])
	}
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "a later screen"})
	st.ScreenMsgs = []int{len(st.Messages) - 1}
	st.digestStaleScreens()
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Error("the earlier output was overwritten")
	}
	if !strings.Contains(st.Messages[len(st.Messages)-1].Content, "(full output: ") {
		t.Error("the digest lost its pointer")
	}
}

func TestACompactionCarriesTheDeclaredRecordsOverFromTheHarness(t *testing.T) {
	saved := declaredRecords
	defer func() { declaredRecords = saved }()
	declaredRecords = func(RunContext, *operatorState) string { return "invoice 455: confirmed\ninvoice 456: differs" }
	st := longGoal(30)
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	if !strings.Contains(st.Messages[2].Content, "invoice 456: differs") {
		t.Error("the declared records were not carried over")
	}
}

func TestAShortOutputNeedsNoFile(t *testing.T) {
	st := &operatorState{OutputsDir: t.TempDir()}
	st.noteLedger("gui click e1", shellResult{Output: "Clicked e1"})
	if strings.Contains(st.Ledger[0], "full output") || st.OutputSeq != 0 {
		t.Errorf("%q", st.Ledger[0])
	}
}

func TestKeptOutputsAreBounded(t *testing.T) {
	st := &operatorState{OutputsDir: t.TempDir(), OutputBytes: maxKeptOutputBytes}
	if p := st.keepOutput("more"); p != "" {
		t.Errorf("kept past the limit: %s", p)
	}
}

func TestAShortGoalIsLeftAsItIs(t *testing.T) {
	st := longGoal(4)
	before := len(st.Messages)
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	if len(st.Messages) != before {
		t.Error("a short goal was shortened")
	}
}

func TestOnlyAWindowThatMadeProgressGetsAnotherBudget(t *testing.T) {
	st := &operatorState{Hosted: true, CommandsSucceeded: 40}
	if !st.mayContinue(30) {
		t.Error("ten commands worked in the window: should go on")
	}
	if st.mayContinue(38) {
		t.Error("two commands worked: should not go on")
	}
	st.Continuations = maxContinuations
	if st.mayContinue(0) {
		t.Error("no budgets left")
	}
	if (&operatorState{CommandsSucceeded: 40}).mayContinue(0) {
		t.Error("a local model is left as it was")
	}
}

// maskGoal is a goal of n steps whose outputs are each size characters long; step 3 is the person's reply.
func maskGoal(t *testing.T, n, size int) *operatorState {
	st := &operatorState{Hosted: true, Request: "file September", PrefixLen: 2, OutputsDir: t.TempDir()}
	st.Messages = []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "contract: file September"}}
	for i := 0; i < n; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: fmt.Sprintf("COMMAND:\noffice read bill%d.pdf", i)})
		content := fmt.Sprintf("Command execution result:\n\nexit_code: 0\n\noutput:\nbill %d: ", i) + strings.Repeat("x", size)
		if i == 3 {
			content = "The human replied: the rent is paid in cash. " + strings.Repeat("y", size)
		}
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: content})
	}
	return st
}

func TestOlderOutputsAreMaskedInOnePassKeepingTheirStartAndFile(t *testing.T) {
	st := maskGoal(t, 30, 5_000)
	if !st.maintenanceDue() {
		t.Fatalf("not due at %d characters with %d removable", transcriptChars(st), st.removableChars())
	}
	before := transcriptChars(st)
	n := st.maskOldOutputs()
	if n == 0 || transcriptChars(st) >= before/2 {
		t.Fatalf("masked %d, %d -> %d characters", n, before, transcriptChars(st))
	}
	last := len(st.Messages) - maskKeepNewest
	for i, m := range st.Messages {
		masked := strings.Contains(m.Content, maskMarker)
		switch {
		case i < 2 && masked:
			t.Errorf("the prefix was masked (%d)", i)
		case i >= last && masked:
			t.Errorf("a newest message was masked (%d)", i)
		case strings.HasPrefix(m.Content, "The human replied") && masked:
			t.Error("the person's words were masked")
		case masked:
			if !strings.HasPrefix(m.Content, "Command execution result") || !strings.Contains(m.Content, "bill ") {
				t.Errorf("a masked output lost its start: %q", m.Content[:60])
			}
			path := m.Content[strings.Index(m.Content, "output is in ")+len("output is in "):]
			path = path[:strings.Index(path, " - cat")]
			if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), strings.Repeat("x", 5_000)) {
				t.Errorf("the whole output is not in %s: %v", path, err)
			}
		}
	}
}

func TestMaskingWaitsForRegrowthAndNeverMasksTwice(t *testing.T) {
	st := maskGoal(t, 30, 5_000)
	st.maskOldOutputs()
	if st.maintenanceDue() {
		t.Fatal("due again at once: every turn would break the prompt cache")
	}
	once := st.Messages[5].Content // (the second step's output: 0 frame, 1 contract, then command and output in turn)
	st.maskOldOutputs()
	if st.Messages[5].Content != once || strings.Count(once, maskMarker) != 1 {
		t.Error("an output was masked twice")
	}
	small := maskGoal(t, 30, 100)
	if small.maintenanceDue() || small.maskOldOutputs() != 0 {
		t.Error("a short transcript, or short outputs, were masked")
	}
}

// shotGoal is a hosted goal of n listings, each with a screenshot, tracked as screen outputs.
func shotGoal(t *testing.T, n int) *operatorState {
	st := &operatorState{Hosted: true, Request: "file September", PrefixLen: 2, OutputsDir: t.TempDir()}
	st.Messages = []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "contract: file September"}}
	for i := 0; i < n; i++ {
		st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: "COMMAND:\ngui observe"},
			llm.Message{Role: "user", Content: fmt.Sprintf("Command execution result:\n\nexit_code: 0\n\noutput:\nScreen %d\ne1 button \"Save\"", i) + screenshotNote,
				Images: [][]byte{[]byte("png")}})
		st.ScreenMsgs = append(st.ScreenMsgs, len(st.Messages)-1)
	}
	return st
}

func TestATranscriptIsOnlyAppendedToUntilAPassTakesEnoughOff(t *testing.T) {
	st := shotGoal(t, 8) // seven older screenshots, but a transcript too short for a pass to pay back
	if st.maintenanceDue() {
		t.Fatalf("a pass is due at %d characters with %d older screenshots", transcriptChars(st), st.oldScreenshots())
	}
	long := shotGoal(t, 12) // the same pictures beside a contract they are a fifth of: not worth the cache either
	long.Messages[1].Content = strings.Repeat("rule ", 30_000)
	if long.maintenanceDue() {
		t.Fatalf("a pass is due for %d removable of %d characters", long.removableChars(), transcriptChars(long))
	}
	st = shotGoal(t, 12) // eleven older screenshots that are most of the transcript: worth it
	if !st.maintenanceDue() {
		t.Fatalf("no pass due for %d removable of %d characters", st.removableChars(), transcriptChars(st))
	}
	did := st.maintain()
	shots, digested := 0, 0
	for i, m := range st.Messages {
		if len(m.Images) > 0 {
			shots++
			if i != len(st.Messages)-1 {
				t.Errorf("an older screenshot stayed at %d", i)
			}
		}
		if strings.Contains(m.Content, staleNote) {
			digested++
		}
	}
	if shots != 1 || digested != 11 || len(st.ScreenMsgs) != 1 || st.ScreenMsgs[0] != len(st.Messages)-1 {
		t.Errorf("after %q: %d screenshots, %d digested, screens %v", did, shots, digested, st.ScreenMsgs)
	}
	if st.maintenanceDue() {
		t.Error("due again right after a pass")
	}
}

// capturingNotes answers like notesProvider and keeps the messages it was asked with.
type capturingNotes struct{ asked [][]llm.Message }

func (p *capturingNotes) Name() string                         { return "notes" }
func (p *capturingNotes) IsReachable(ctx context.Context) bool { return true }
func (p *capturingNotes) Chat(_ context.Context, _ string, m []llm.Message, _ llm.Options) (llm.Result, error) {
	p.asked = append(p.asked, append([]llm.Message(nil), m...))
	return llm.Result{Text: "NOTES: saved the first page.\nUNVERIFIED: the second page is the last - the scroll bar ended there"}, nil
}

// The request for notes asks for uncertain claims as UNVERIFIED lines, in the newest message only: the cached frame
// and the request are the same bytes before, during and after a compaction.
func TestTheNotesRequestAsksForUnverifiedClaimsOutsideTheFrame(t *testing.T) {
	st := longGoal(30)
	frame, request := st.Messages[0].Content, st.Messages[1].Content
	provider := &capturingNotes{}
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: provider, OnLog: func(string) {}}, st, "test")
	if len(provider.asked) != 1 {
		t.Fatalf("notes asked %d times", len(provider.asked))
	}
	asked := provider.asked[0]
	if asked[0].Content != frame || asked[1].Content != request || st.Messages[0].Content != frame || st.Messages[1].Content != request {
		t.Fatal("the prefix changed")
	}
	if last := asked[len(asked)-1].Content; !strings.Contains(last, "UNVERIFIED: the claim - what it rests on") {
		t.Errorf("the notes request does not ask for UNVERIFIED lines: %q", last)
	}
	if strings.Contains(frame, "UNVERIFIED") {
		t.Error("the instruction went into the frame")
	}
	if !strings.Contains(st.Messages[2].Content, "UNVERIFIED: the second page is the last") {
		t.Error("the notes' UNVERIFIED line was not kept")
	}
}

func TestTheProgressNoteListsExactIdentifiersMostFrequentFirst(t *testing.T) {
	output := func(s string) llm.Message {
		return llm.Message{Role: "user", Content: "Command execution result:\n" + s}
	}
	region := []llm.Message{
		{Role: "assistant", Content: "COMMAND:\noffice read /home/worker/Documents/ledger.xlsx"},
		output("INV-2026-0042 total 1,250.00 due 2026-11-12, mail ops@example.com, see https://example.com/a?b=1"),
		output("INV-2026-0042 again; 1,250.00 again; saved to C:\\Users\\worker\\book.xlsx on 12/11/2026"),
		output("INV-2026-0042 a third time; account 4471; small numbers 12 and 345 and words h1 abc; SEP-2026 is in the request"),
		output("a line of a listing 3.75"),
	}
	prefix := []llm.Message{{Role: "system", Content: "frame"}, {Role: "user", Content: "file SEP-2026"}}
	got := exactIdentifiers(region, prefix)
	joined := strings.Join(got, " | ")
	if len(got) == 0 || got[0] != "INV-2026-0042" || got[1] != "1,250.00" {
		t.Fatalf("not ranked by frequency: %s", joined)
	}
	for _, want := range []string{"/home/worker/Documents/ledger.xlsx", `C:\Users\worker\book.xlsx`, "ops@example.com",
		"https://example.com/a?b=1", "2026-11-12", "12/11/2026", "4471", "3.75"} {
		if !strings.Contains(" | "+joined+" | ", " | "+want+" | ") {
			t.Errorf("lacks %q: %s", want, joined)
		}
	}
	for _, noise := range []string{"12", "345", "h1", "abc", "SEP-2026", "again"} {
		for _, s := range got {
			if s == noise {
				t.Errorf("kept %q, which is not an identifier or is in the prefix: %s", noise, joined)
			}
		}
	}
	if again := strings.Join(exactIdentifiers(region, prefix), " | "); again != joined {
		t.Error("the same region gave a different list")
	}

	var many strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&many, "REF%05d ", i)
	}
	capped := exactIdentifiers([]llm.Message{output(many.String())}, prefix)
	if n := len(strings.Join(capped, "  ")); n > maxAnchorChars || len(capped) < 100 {
		t.Errorf("list of %d strings, %d characters: want it capped near %d", len(capped), n, maxAnchorChars)
	}
}

// A compaction's note carries the list, and only from the part it shortened.
func TestACompactionNoteCarriesTheIdentifierList(t *testing.T) {
	st := longGoal(30)
	st.Messages[3].Content = "Command execution result:\n\nexit_code: 0\n\noutput:\nposted voucher JV-0981 for 18,400.50"
	(&OperatorSkill{}).compact(RunContext{Ctx: context.Background(), Provider: &notesProvider{}, OnLog: func(string) {}}, st, "test")
	note := st.Messages[2].Content
	if !strings.Contains(note, "Exact strings from the shortened part") || !strings.Contains(note, "JV-0981") ||
		!strings.Contains(note, "18,400.50") {
		t.Errorf("the note lacks the identifier list: %s", note)
	}
}

// A picture counts at its learned price when that is higher than the flat one, never lower: a picture priced low
// would let a local transcript past its window.
func TestAPicturesLearnedPriceRaisesItsCountNeverLowersIt(t *testing.T) {
	saved := learnedPictureTokens
	defer func() { learnedPictureTokens = saved }()
	shot := []llm.Message{{Role: "user", Content: "", Images: [][]byte{{1}}}}
	for _, c := range []struct {
		tokens int
		known  bool
		want   int
	}{{0, false, pictureChars}, {2000, true, 2000 * localCharsPerToken}, {100, true, pictureChars}} {
		learnedPictureTokens = func() (int, bool) { return c.tokens, c.known }
		if got := messagesChars(shot); got != c.want {
			t.Errorf("learned %d (%v): a picture counts %d, want %d", c.tokens, c.known, got, c.want)
		}
	}
}
