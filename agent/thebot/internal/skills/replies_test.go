package skills

import (
	"strings"
	"testing"
)

// A person's reply to the harness's own question is read for what it does to what was asked (2026-10-23).

func TestAPlainYesOrNoNeedsNoModel(t *testing.T) {
	st := &operatorState{Hosted: true}
	chat := &scriptedChat{}
	for _, r := range []string{"yes", "ok", "k", "Yes, correct.", "use your judgement", "fine"} {
		if a, known := (&OperatorSkill{}).replyAccepts(weighRC(chat), st, "where does 900 come from?", r); !a || !known {
			t.Errorf("%q not taken as accepting", r)
		}
	}
	for _, r := range []string{"no", "nope", "don't", "wrong", "no, don't"} {
		if a, known := (&OperatorSkill{}).replyAccepts(weighRC(chat), st, "where does 900 come from?", r); a || !known {
			t.Errorf("%q not taken as refusing", r)
		}
	}
	if len(chat.calls) != 0 {
		t.Errorf("%d model call(s) for plain replies", len(chat.calls))
	}
}

func TestAReplyThatSaysWhereToLookDoesNotVouchForTheValue(t *testing.T) {
	st := groundingState("Prepare the return from the workbook.")
	st.Hosted, st.AskedUntraced, st.LastAsked = true, []string{"31200"}, `Where does "31200" come from?`
	chat := &scriptedChat{replies: []string{`{"does": "points"}`}}
	rc := weighRC(chat)
	rc.HumanReply = "31200 for which field? dont type anything u cant point to - add up the rows of the sales sheet"
	(&OperatorSkill{}).resume(rc, Subtask{}, st)
	if groundingNudge(st, `gui type e5 "31200"`) == "" {
		t.Fatal("a reply that only said where to look made the value traced")
	}
	if len(chat.calls) != 1 || !strings.Contains(chat.calls[0][1].Content, "31200") {
		t.Fatalf("the reply was not read against what was asked: %d call(s)", len(chat.calls))
	}
}

func TestAYesInThePersonsOwnWordsVouches(t *testing.T) {
	st := groundingState("Prepare the return from the workbook.")
	st.Hosted, st.AskedUntraced = true, []string{"31200"}
	rc := weighRC(&scriptedChat{replies: []string{`{"does": "accepts"}`}})
	rc.HumanReply = "haan thats right, its the total of the cashew rows"
	(&OperatorSkill{}).resume(rc, Subtask{}, st)
	if groundingNudge(st, `gui type e5 "31200"`) != "" {
		t.Fatal("a yes in their own words left the value untraced")
	}
}

// Found live (2026-10-23): told "not yet - tell me which invoice you treated as cancelled and what it shows", the worker
// was told the reply settled its question and asked again, three times.
func TestAReplyThatAsksSomethingOfTheWorkerIsAnsweredFirst(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file the return", LastAsked: "May I file it now?"}
	rc := weighRC(&scriptedChat{replies: []string{`{"does": "asks", "wants": "which invoice was treated as cancelled, and what that bill shows"}`}})
	rc.HumanReply = "not yet. tell me in 2-3 lines which inv u treated as cancelled n what the bill shows, then i'll ok"
	st, _ = (&OperatorSkill{}).resume(rc, Subtask{}, st)
	last := st.Messages[len(st.Messages)-1].Content
	if !strings.Contains(last, "They want this from you before anything else: which invoice was treated as cancelled") ||
		!strings.Contains(last, "HELP:") || strings.Contains(last, "That settles what you asked") {
		t.Fatalf("the model was told: %s", last)
	}
	for _, r := range []*Reading{{Decision: "no", Wants: "what table 13 shows now"}, {Decision: "unclear", Wants: "what table 13 shows now"}} {
		if note := notApprovedNote(RunContext{HumanReading: r}, "gui click e9", "not yet, what does table 13 show"); !strings.Contains(note, "They want this from you before they decide: what table 13 shows now") {
			t.Errorf("an approval's note did not carry what they want (%s): %s", r.Decision, note)
		}
	}
}

// The person (2026-10-24): if the CA says "u know how we treat cancelled ones", how will the worker know the way? It
// finds it where such things are kept and says where, or asks - never guesses.
func TestAPracticeTheReplyDoesNotSpellOutIsFoundOrAskedForNeverGuessed(t *testing.T) {
	st := &operatorState{Hosted: true, Request: "file the return", LastAsked: "How do I show bill 455?"}
	rc := weighRC(&scriptedChat{replies: []string{`{"does": "points", "wants": "", "refers": "how cancelled bills are treated"}`}})
	rc.HumanReply = "u know how we treat cancelled ones, same as always"
	st, _ = (&OperatorSkill{}).resume(rc, Subtask{}, st)
	last := st.Messages[len(st.Messages)-1].Content
	if !strings.Contains(last, "They rely on something they do not spell out: how cancelled bills are treated") ||
		!strings.Contains(last, "quote where you found it") || !strings.Contains(last, "never guess it") {
		t.Fatalf("the model was told: %s", last)
	}
	plain := &operatorState{Hosted: true, Request: "file the return"}
	rc2 := weighRC(&scriptedChat{})
	rc2.HumanReply = "ok"
	plain, _ = (&OperatorSkill{}).resume(rc2, Subtask{}, plain)
	if strings.Contains(plain.Messages[len(plain.Messages)-1].Content, "They rely on") {
		t.Error("a plain ok was taken as relying on something")
	}
}

// On a local brain no side call reads the reply: only a plain yes vouches. Anything else is not known, and not known
// is not vouched - the values stay untraced (T-11).
func TestOnALocalBrainOnlyAPlainYesVouches(t *testing.T) {
	for _, c := range []struct {
		reply   string
		vouched bool
	}{
		{"use your judgement", true},
		{"yes", true},
		{"Use your judgement and carry on.", false},
		{"900 for which field?", false},
	} {
		st := groundingState("Prepare the return from the workbook.")
		st.AskedUntraced = []string{"900"}
		chat := &scriptedChat{}
		rc := weighRC(chat)
		rc.HumanReply = c.reply
		(&OperatorSkill{}).resume(rc, Subtask{}, st)
		if traced := groundingNudge(st, `gui type e5 "900"`) == ""; traced != c.vouched {
			t.Errorf("%q: traced = %v, want %v", c.reply, traced, c.vouched)
		}
		if len(chat.calls) != 0 {
			t.Errorf("%q: a local brain's reply was read by a side call", c.reply)
		}
	}
}

// A web page the person was asked to vouch for stays a lookup when their reply cannot be read (T-11).
func TestAnUnreadReplyDoesNotTrustAPage(t *testing.T) {
	st := &operatorState{Lookups: []webLookup{{Text: "the rate is 18 percent"}}, AskedSources: []int{0}}
	rc := weighRC(&scriptedChat{})
	rc.HumanReply = "hmm, is that the official page?"
	(&OperatorSkill{}).resume(rc, Subtask{}, st)
	if strings.Contains(st.Seen, "18 percent") {
		t.Fatal("an unread reply made a web page a source")
	}
}
