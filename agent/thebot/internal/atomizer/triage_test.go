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
