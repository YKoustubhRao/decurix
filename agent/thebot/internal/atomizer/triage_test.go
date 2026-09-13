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

		// One classified skill.
		{"run this command: `echo hi`", RouteSkill},
		{"search the web for RTX 5080 laptops", RouteSkill},
		{"write a segment tree into segment_tree.py", RouteSkill},
		{"build a FastAPI service with a SQLite store and tests", RouteSkill},

		// Genuinely different jobs.
		{"Find out what the newest Go version is and save a short note about it to go-version.md", RoutePlan},
		{"search the web for Abhishek Gadia and download the raw html of the top 10 pages", RoutePlan},
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
