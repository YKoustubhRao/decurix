package atomizer

import "testing"

func TestTriageRoutesRequestsByShape(t *testing.T) {
	cases := []struct {
		prompt string
		want   RouteKind
	}{
		// Conversation: answered from the frame, no tools.
		{"hello, whats your name?", RouteChat},
		{"hi", RouteChat},
		{"thanks!", RouteChat},
		{"what's your desktop name?", RouteChat},
		{"what can you do?", RouteChat},
		{"my name is Divyanshu, remember that", RouteChat},
		{"what's my name?", RouteChat},
		{"who wrote the odyssey?", RouteChat},
		{"what is 1+1?", RouteChat},
		// Measured in a real desktop: the bare word "browser" sent this to the
		// operator, which asked the human to install xclip.
		{"browser test message one", RouteChat},
		{"ping - just reply pong", RouteChat},

		// Says how to work a screen, in an app no surface word names.
		{"On Request REQ-26-00009 the Due Date is empty. Open it, type 03-10-2026 into Due Date, press Escape, save.", RouteOperate},
		{"press ctrl+s in the editor", RouteOperate},
		{"what does the escape key do?", RouteChat},

		// On this machine: the operator works it out.
		{"what is the current kernel version?", RouteOperate},
		{"how many files are in /etc?", RouteOperate},
		{"check the disk space and report back", RouteOperate},
		{"Create a folder called notes in ~/work", RouteOperate},
		{"Summarise what's on this desktop", RouteOperate},
		{"is git installed?", RouteOperate},
		{"hello, please install jq", RouteOperate},
		{"list the running processes using the most memory", RouteOperate},
		{"create a folder called reports in ~/work and then move every csv from ~/Downloads into it", RouteOperate},

		// An email to prepare: the operator drafts it into the Outbox with
		// `office email`; the chat skill would only have written its text.
		{"Draft an email to acme@example.com with the overdue statement attached", RouteOperate},
		{"Prepare a payment reminder email for Acme and put it in the outbox", RouteOperate},
		{"write an email to billing@acme.example about invoice INV-7", RouteOperate},
		{"compose a reply to the supplier confirming delivery on Friday", RouteOperate},
		// Wording help is conversation: no address, nothing to attach.
		{"help me write an email asking my boss for Friday off", RouteChat},

		// On the screen: the operator, with the gui command - not the web
		// skill fetching the URL, and not a plan written before looking.
		{"In the Decurix Admin panel open in Chromium, add a new user: name Dana Scott, email dana@example.com, role Editor.", RouteOperate},
		{"open http://127.0.0.1:8765/settings in the browser and turn on maintenance mode", RouteOperate},
		{"LibreOffice Calc has budget.ods open. Enter the Q3 figures into column D, then save the file.", RouteOperate},
		{"In the Mousepad text editor that is open, type 'hello' and save it as ~/out/hello.txt", RouteOperate},
		{"click the Send newsletter button", RouteOperate},

		// One classified skill.
		{"run this command: `echo hi`", RouteSkill},
		{"search the web for RTX 5080 laptops", RouteSkill},
		{"write a segment tree into segment_tree.py", RouteSkill},
		{"build a FastAPI service with a SQLite store and tests", RouteSkill},
		// Two write verbs, one file: one job. Measured live as a plan whose
		// first step went to the coding harness and timed out.
		{"create a file koustubh.txt and write a poem inside it, after you are done give me the file location", RouteSkill},
		{"write a haiku and save it to haiku.md", RouteSkill},

		// Genuinely different jobs.
		{"Find out what the newest Go version is and save a short note about it to go-version.md", RoutePlan},
		{"search the web for Abhishek Gadia and download the raw html of the top 10 pages", RoutePlan},
		// Write verbs stay separate jobs across two files, or a folder.
		{"create a file a.txt and then write a summary of it to b.md", RoutePlan},
		{"create a folder called notes and then write a poem to notes/poem.txt", RoutePlan},
	}
	for _, c := range cases {
		if got := Triage(c.prompt); got.Kind != c.want {
			t.Errorf("Triage(%q) = %s (%s), want %s", c.prompt, got.Kind, got.Reason, c.want)
		}
	}
}

// A coding request is never planned or split, however compound it reads.
func TestTriageKeepsCodingWhole(t *testing.T) {
	r := Triage("build a FastAPI service with a SQLite store, a REST layer and tests, each in its own file")
	if r.Kind != RouteSkill || r.TaskType != "coding" {
		t.Errorf("Triage = %+v, want the coding skill, whole", r)
	}
}

// Measured live: "content-type" read as a request to type sent a terminal job
// to the browser extension, which was not running, for 16 minutes.
func TestTriageSendsOnlyRealPageWorkToTheBrowser(t *testing.T) {
	notBrowser := []string{
		"run curl -sI https://example.com in the terminal and tell me the content-type header",
		"check the content-type example.com returns",
		"from the shell, search example.com's robots.txt for sitemap lines",
		// An address is not a website (live: waited on the browser extension).
		"Create invoices.xlsx in my work folder with three invoices. Then draft an email to ap@example.com with the workbook attached.",
	}
	for _, prompt := range notBrowser {
		if got := Triage(prompt); got.Kind == RouteBrowser {
			t.Errorf("Triage(%q) = browser, want anything else", prompt)
		}
	}
	browser := []string{
		"open google.com and search for addis ababa",
		"go to amazon.com and add a kettle to the cart",
		"browse to example.com then click More information",
		"on github.com type decurix into the search box",
		"email sam@acme.com, then open acme.com and click Pricing",
	}
	for _, prompt := range browser {
		if got := Triage(prompt); got.Kind != RouteBrowser {
			t.Errorf("Triage(%q) = %s, want browser", prompt, got.Kind)
		}
	}
}

func TestTriageSendsAWebAppsQuestionsToTheOperator(t *testing.T) {
	// Found live: answered by the chat skill from the model's own knowledge.
	for _, p := range []string{
		`chatgpt "what is 3+3?", "what colour is the sky?"`,
		`ChatGPT: who is jesus`,
		`ask chatgpt what can you do`,
		`search google for pizza near me`,
		`google "best laptop"`,
	} {
		if got := Triage(p); got.Kind != RouteOperate {
			t.Errorf("Triage(%q) = %v (%s), want operate", p, got.Kind, got.Reason)
		}
	}
	for _, p := range []string{"what does google say about x?", "is chatgpt down?", "who founded google?"} {
		if got := Triage(p); got.Reason == "something to ask a web app - the operator uses it with gui ask" {
			t.Errorf("Triage(%q) took it for a web app's question", p)
		}
	}
}

// Found live on a native Windows worker: a request that was only a command in
// backticks named an app and "open", went to the operator as screen work, and
// the small model answered "DONE" without running it.
func TestTriageRunsACommandNamedInTheRequest(t *testing.T) {
	for _, p := range []string{
		"run the command: `gui open \"character map\"`",
		"Run `gui click 'button \"Select\"'`.",
		"please execute this command `gui open https://example.com`",
		"run the command: `gui observe`",
	} {
		got := Triage(p)
		if got.Kind != RouteSkill || got.TaskType != "terminal" {
			t.Errorf("Triage(%q) = %v/%s (%s), want the terminal skill", p, got.Kind, got.TaskType, got.Reason)
		}
	}
	// A command that is only part of the request is still a goal for a loop.
	for _, p := range []string{
		"run `gui observe` and then click the Save button in the editor window",
		"open the settings window and click `Apply`",
	} {
		if got := Triage(p); got.Kind == RouteSkill && got.TaskType == "terminal" {
			t.Errorf("Triage(%q) ran it as a bare command (%s)", p, got.Reason)
		}
	}
}

func TestTriageSendsATaughtTaskToTheOperator(t *testing.T) {
	// Found live: "a question - answering directly", by a skill with no tools.
	p := `Do the task you were taught, "Ask ChatGPT" (skill ask-chatgpt) with typed_text="what is 2+5?".`
	if got := Triage(p); got.Kind != RouteOperate {
		t.Fatalf("Triage(%q) = %v (%s), want operate", p, got.Kind, got.Reason)
	}
}

// An Office document or PDF is the operator's to make (office create/update),
// never file-write's, which writes text: live, a workbook request saved the
// model's refusal as totals.xlsx and was reported done.
func TestTriageSendsOfficeDocumentsToTheOperator(t *testing.T) {
	for _, prompt := range []string{
		"Create totals.xlsx in my work folder with two rows: Rent 1500 and Power 220, and a Total row that sums them.",
		"make report.docx with a heading and the three findings",
		"update budget.xlsx: set March to 1200",
		"combine a.pdf and b.pdf into all.pdf",
	} {
		if got := Triage(prompt); got.Kind != RouteOperate {
			t.Errorf("Triage(%q) = %s, want operate", prompt, got.Kind)
		}
	}
	// Text files stay with file-write.
	if got := Triage("write hello world to notes.txt"); got.Kind != RouteSkill || got.TaskType != "file-write" {
		t.Errorf("a text file: %+v, want the file-write skill", got)
	}
}

// Without the browser extension, web work goes to the operator: found live,
// a step sent to the browser skill waited 30 s for a listener that was never
// there and failed the task.
func TestWebPageWorkGoesToTheOperatorWithoutTheExtension(t *testing.T) {
	was := BrowserAvailable
	defer func() { BrowserAvailable = was }()

	req := "Open https://opensource-demo.orangehrmlive.com, log in and add the new employee"
	BrowserAvailable = true
	if r := Triage(req); r.Kind != RouteBrowser {
		t.Fatalf("with the extension: %+v", r)
	}
	BrowserAvailable = false
	if r := Triage(req); r.Kind != RouteOperate || r.TaskType != "operator" {
		t.Fatalf("without the extension: %+v", r)
	}
	if typ := shapeStep("browser", "Open the project portal and create the request").Type; typ != "operator" {
		t.Errorf("a planned [browser] step without the extension runs as %q", typ)
	}
}

// Found live: `search "sundar pichai"` was a "question" for the chat skill,
// which has no tools and answered that a web search needed approval.
func TestTriageSendsAThingToLookUpToTheWebSkill(t *testing.T) {
	for _, prompt := range []string{
		`search "sundar pichai"`,
		"search sundar pichai",
		"look up the height of the Eiffel tower",
		"please google decurix",
	} {
		got := Triage(prompt)
		if got.Kind != RouteSkill || got.TaskType != "web" {
			t.Errorf("Triage(%q) = %s/%s (%s), want the web skill", prompt, got.Kind, got.TaskType, got.Reason)
		}
	}

	// Something on this machine is the operator's, and a named web app keeps its own route.
	for _, prompt := range []string{
		"search the logs folder for errors",
		"search ~/work for files named report.csv",
		`search google for "decurix"`,
	} {
		if got := Triage(prompt); got.Kind == RouteSkill && got.TaskType == "web" && got.Reason == "something to look up - searching the web" {
			t.Errorf("Triage(%q) went to the web skill as a bare search", prompt)
		}
	}
}

// A request a person attached files to is the operator's, whatever it asks:
// only the operator can read a file, and the chat skill would answer about a
// document it never opened.
func TestTriageSendsARequestWithAttachedFilesToTheOperator(t *testing.T) {
	p := "what does this say?\n\n" + attachedFilesMark + "\n  /home/headless/work/inbox/up-1/notes.txt"
	if got := Triage(p); got.Kind != RouteOperate {
		t.Fatalf("Triage(%q) = %v (%s), want operate", p, got.Kind, got.Reason)
	}
	if got := Triage("what does this say?"); got.Kind == RouteOperate {
		t.Errorf("the same question with no file went to the operator (%s)", got.Reason)
	}
}
