// Package judge is the final check's prompt and its reading of the answer, made in soul (plan rule 3, phase 2:
// py.judge). The Python agent keeps the tools - it reads the task's files, the documents, the screen and the shared
// folder, and hands them over as data (verify.judge_evidence); this package words them into the prompt and decides
// what the model's answer says.
//
// Today's path is Python's own (agent/decurix_agent/verify.py: JUDGE_SYSTEM, judge_content, _parse), and while the
// switch (DECURIX_SOUL_JUDGE) is off by default both exist: every text here is byte for byte Python's, and the reading
// of an answer follows Python's (its str(), float(), strip() and repr(): internal/pyread, shared with
// internal/escalation; this package reads JSON with pyread.Decode and float() with pyread.FloatOf, as it always has).
// testdata/parity.json, recorded from the
// Python code, proves it (parity_test.go here, agent/tests/test_judge_parity.py there); a change to either side
// changes both and regenerates the corpus.
package judge

import (
	"encoding/json"
	"fmt"
	"strings"

	"thebot/internal/pyread"
)

// The bounds of what the judge is shown and may answer (verify.py's MAX_TASK_CHARS, MAX_LOOK, MAX_PUT_BACK).
const (
	MaxTaskChars = 20000
	MaxLook      = 6
	MaxPutBack   = 20
)

// Evidence is what the final check is shown, as data (verify.judge_evidence): the texts as the caller gave them, the
// run's values that hold something (key and value as Python writes it), the names of what the task had to choose,
// the deliverables' file names, the shared folder's note, the documents the task names and every picture as a data:
// URL. Nothing in it is worded for the model: Messages does that.
type Evidence struct {
	Task         string      `json:"task"`
	Claim        string      `json:"claim"`
	Criteria     string      `json:"criteria"`
	Values       [][2]string `json:"values"`
	Chosen       []string    `json:"chosen"`
	Steps        string      `json:"steps"`
	Observed     []string    `json:"observed"`
	Found        string      `json:"found"`
	ScreenText   string      `json:"screen_text"`
	Produced     string      `json:"produced"`
	MappingNote  string      `json:"mapping_note"`
	Deliverables []string    `json:"deliverables"`
	DocText      string      `json:"doc_text"`
	DocPictures  []string    `json:"doc_pictures"`
	LivePNG      string      `json:"live_png"`
	Screen       bool        `json:"screen"`
	ReferencePNG string      `json:"reference_png"`
}

// Part is one part of a message's content: text, or a picture as a data: URL.
type Part struct {
	Text string
	URL  string
}

// MarshalJSON writes a part as OpenAI's content part ({"type":"text","text":...} or an image_url).
func (p Part) MarshalJSON() ([]byte, error) {
	if p.URL != "" {
		return json.Marshal(map[string]any{"type": "image_url", "image_url": map[string]string{"url": p.URL}})
	}
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text", p.Text})
}

// Message is one OpenAI-shaped message: Content is a string (the system's) or a list of parts (the user's).
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Messages is the final check as the model is asked it: System, then the user's parts made of the evidence. With
// closer (the lines of the files that hold the terms the judge asked to see, look), it is the second judgement.
func Messages(ev Evidence, look []string, closer string) []Message {
	parts := Content(ev)
	if closer != "" {
		parts = append(parts, Closer(look, closer))
	}
	return []Message{{Role: "system", Content: System}, {Role: "user", Content: parts}}
}

// Content is the user message's parts, in order (verify.judge_content).
func Content(ev Evidence) []Part {
	// The task is the stage's instruction AND what the caller adds after it - the person's binding answers, the
	// system's own read-back, the code checks' evidence, the other stages' context (cut at 1,500 characters once,
	// everything after a long instruction never reached the judge).
	body := pyread.Strip(ev.Task)
	if n := pyread.Len(body); n > MaxTaskChars {
		body = pyread.Head(body, MaxTaskChars) + fmt.Sprintf("\n[cut here: %d more characters not shown]", n-MaxTaskChars)
	}
	lines := []string{"The task as asked: " + body}
	if len(ev.Values) > 0 {
		chosen := map[string]bool{}
		for _, c := range ev.Chosen {
			chosen[c] = true
		}
		var typed, lookedUp []string
		for _, kv := range ev.Values {
			name, _, _ := strings.Cut(kv[0], "__")
			if chosen[name] {
				lookedUp = append(lookedUp, "for "+kv[0]+", the document says: "+kv[1])
			} else {
				typed = append(typed, kv[0]+" = "+kv[1])
			}
		}
		if len(typed) > 0 {
			lines = append(lines, "This run's values: "+pyread.Head(strings.Join(typed, "; "), 3000))
		}
		if len(lookedUp) > 0 {
			// Apart from the values to compare, and never as "k = v": with "item = Office Mesh Chairs" among the run's
			// values, the check refused a correct record "Ergonomic Office Chair" as not the value.
			lines = append(lines, "Things this run had to find in the application's own lists and choose (NOT values to "+
				"compare by name - the worker searched and took the existing record of each thing, and a "+
				"record has its own name; judge only whether the record taken is that kind of thing): "+
				pyread.Head(strings.Join(lookedUp, "; "), 2000))
		}
	}
	if ev.Criteria != "" {
		lines = append(lines, "What the person who taught this task said about it (and what done looks like): "+
			pyread.Head(pyread.Strip(ev.Criteria), 6000))
	}
	if ev.Steps != "" {
		// The demonstration's steps, not "what the worker did": their clicks on list entries name the demonstration's
		// own choices.
		lines = append(lines, "The steps of the person's demonstration, which this run followed with its own values (a click "+
			"on a list entry here names the DEMONSTRATION's choice, not this run's - what this run chose "+
			"is in the tools' record below): "+pyread.Head(pyread.Strip(ev.Steps), 2000))
	}
	if len(ev.Observed) > 0 {
		lines = append(lines, "What the worker's tools did and read off the screen during this task, as it happened (observed, "+
			"not claimed - a report that matches these is backed by them; a look at the screen listed here "+
			"shows what was on it at that moment, a page since closed included):\n"+
			pyread.Tail(strings.Join(ev.Observed, "\n"), 24000))
	}
	if ev.Found != "" {
		// Facts of their own, never inside the person's words (there they read as one more claim).
		lines = append(lines, "Facts the harness checked in code on this desktop just now (exact; not the worker's claim and not "+
			"the person's words - a part of the result these settle IS settled, judge it from them):\n"+
			pyread.Head(pyread.Strip(ev.Found), 4000))
	}
	if ev.ScreenText != "" {
		lines = append(lines, "The text the fields on the screen hold NOW, read from the apps themselves (exact - where it and "+
			"the picture's small print disagree, this is right; a report that says a text was changed is "+
			"backed only if this shows it):\n"+ev.ScreenText)
	}
	claim := ev.Claim
	if claim == "" {
		claim = "(nothing)"
	}
	lines = append(lines, "What the worker reports: "+pyread.Head(pyread.Strip(claim), 4000))
	if ev.Produced != "" {
		lines = append(lines, "The files the worker wrote during this task, as they read NOW (the result itself - the report "+
			"must agree with them):\n"+ev.Produced)
	}
	if ev.MappingNote != "" {
		// A path under the shared folder is that folder, never "the wrong folder"; where a file is, code decides.
		lines = append(lines, ev.MappingNote+" Do not judge a file's location by how its path is spelled: a file the "+
			"request asks to be saved in that folder and found under that path IS saved where asked.")
	}
	if len(ev.Deliverables) > 0 {
		// A correct, saved register was refused once for a mistake the screen still showed and the file no longer had.
		lines = append(lines, "The file(s) the request asks for - "+strings.Join(ev.Deliverables, ", ")+
			" - are saved, and as they read above they ARE that part of the result. Decide that part from "+
			"the saved file(s) alone. A mistake the action log shows and the saved file no longer has, or an "+
			"unsaved change still on the screen, is not a reason for not_done: mention it in 'seen'. Judge "+
			"not_done for what is wrong or missing in the saved file(s), for another part of the request "+
			"not done, or for a report the files contradict.")
	}
	if ev.DocText != "" {
		lines = append(lines, "The documents the task names, as they read (the values must come from these):\n"+ev.DocText)
	}

	content := []Part{{Text: strings.Join(lines, "\n")}}
	for _, picture := range ev.DocPictures {
		content = append(content, Part{Text: "A page of a document the task names:"}, Part{URL: picture})
	}
	switch {
	case ev.LivePNG != "":
		content = append(content, Part{Text: "The screen NOW:"}, Part{URL: ev.LivePNG})
	case !ev.Screen:
		content = append(content, Part{Text: "(This task did not work on the screen: judge it on the files and on " +
			"what the tools read.)"})
	default:
		content = append(content, Part{Text: "(The screen now could not be read.)"})
	}
	if ev.ReferencePNG != "" {
		content = append(content, Part{Text: "The screen at the end of the person's own demonstration (other values):"},
			Part{URL: ev.ReferencePNG})
	}
	return content
}

// Closer is the part a second judgement adds: the lines of the files that hold what the judge asked to see.
func Closer(look []string, lines string) Part {
	return Part{Text: "You were unsure and asked to see: " + strings.Join(look, "; ") + ". The lines of the files that hold " +
		"them, looked up for you (exact):\n" + lines + "\nJudge again with these. Do not ask to look again."}
}

// System is the judge's instructions (verify.JUDGE_SYSTEM, byte for byte).
const System = "You check a computer worker's work before anyone is told it succeeded. You are shown the task as it was " +
	"asked, what the person who taught it said about it, what the worker reports, the screen as it is NOW, and " +
	"often the screen at the end of the person's own demonstration of the same task - which was done with other " +
	"values, so compare the kind of result, not the exact words.\n" +
	"Answer \"done\" only when what you are shown clearly proves the task was carried out for THIS request: the " +
	"right page or record, with this request's values, saved or sent or answered as the task asks. Evidence is " +
	"the screen now AND what the tools observed during the task (earlier screens are gone, but what was read off " +
	"them is listed). For a task that asks for information, each answer in the report must match what the tools " +
	"read, and each must answer what was asked.\n" +
	"A report that says the worker did not check a result the request asks for is not proof of it: unless the " +
	"screen or the observations show that result, answer \"unsure\".\n" +
	"A screen left over from before is not this task's result: when it shows the demonstration's own values (or " +
	"any others) where this run's should be, it is \"not_done\". A taught task's details that belonged to the " +
	"demonstration's own result (a person's city, a company, a date, an amount) are examples, not requirements: " +
	"never answer not_done or unsure because such a detail differs.\n" +
	"Read the report's own evidence against the request: when a figure the report or the files give contradicts " +
	"what the request asks, the answer is \"not_done\" however the report sums itself up - say what was asked and " +
	"what the evidence shows.\n" +
	"When the request names the app to do the work in, the tools' record must show that app's own window opened " +
	"(its name in the window's title): work done in another app in its place - another program of the same kind " +
	"- is not what was asked; answer \"cannot\" and say which app was used instead, unless the report says the " +
	"named app was not available and the person agreed. A file whose content was made with tools (office, " +
	"python) and that was then opened in the named app's window, checked there and saved from it IS done in that " +
	"app; made with tools and never opened and saved in that app, it is not. An app named only as a file's " +
	"format (\"the template (<app>)\", \"a <app> file\") or offered as a choice (\"if you use <app> there\") is not an " +
	"app the work must be done in.\n" +
	"Go through the request part by part before you answer. Each thing it names - which company, account, person " +
	"or record, a date, a reference, the state to leave the result in (a draft, not submitted) - must be shown, " +
	"or reported, with that value; ANOTHER value for a part of the request is \"not_done\" however complete the " +
	"rest is: say which part, what was asked and what was done instead.\n" +
	"A value of the request or its document that was left out, cleared, shortened or replaced because the " +
	"application refused it is never \"done\": the record now says something the document does not - name the " +
	"value. A value the record has a place for, left out because the worker did not find its field, is " +
	"\"not_done\" too; only a reason that stands (no place for it at all, the person said to leave it) lets it " +
	"pass. A record made from a document carries that document's own reference (its number, and its date) where " +
	"the record has a field for it.\n" +
	"A period the document gives (valid for N days) is counted from the record's own date, never taken from an " +
	"application's default.\n" +
	"A result must be where the request said to put it: a file saved in another folder is \"not_done\" - say where " +
	"it is. A request to save in a folder of the person's own computer (C:\\...) is met only by a file saved " +
	"there.\n" +
	"Answer \"not_done\" when you can see it was not: an error, a validation message, the wrong page or record, " +
	"another value than asked, a half-filled form, a sign-in page, a CAPTCHA, nothing changed, fewer items than " +
	"were asked for.\n" +
	"When the task names a document and you are shown what it says, the values entered or reported must be the " +
	"document's: a value that is not in it, or differs from it, is \"not_done\" - except that a thing the document " +
	"describes is entered as the application's own record of the same thing, whose name is usually shorter or " +
	"worded differently (a document's \"Premium Ruled Notebook A5, 200 Pages (Pack of 6)\" is the record \"A5 " +
	"Notebook\"): that is the value entered correctly. Only a record for a different thing is a mismatch. The " +
	"same value written in its field's format (a date in the field's own order, thousands separators, a " +
	"currency) is not a difference: judge the value, not its format.\n" +
	"A rule about HOW to work (look for an existing record first, compare before saving) is a step of the " +
	"method: judge it from what the tools observed and the report, never as missing from the final screen. What " +
	"makes the task not done is the RESULT showing the rule was broken (a second record with the same reference, " +
	"a record submitted where a draft was asked).\n" +
	"Answer \"cannot\" only when the worker's own report says it stopped and gives the reason (something the task " +
	"needs is not there, a rule it was given forbids the only way forward, the application refused): that is the " +
	"worker being honest, and the person is told what it said. A report that claims success is never \"cannot\"; " +
	"nor is one that names no obstacle - that is \"not_done\".\n" +
	"When you are shown the files the worker wrote, they are the result, exact as they read now. Every figure, " +
	"status and statement the report makes about a file must be what that file shows; a report the file " +
	"contradicts is \"not_done\" - say which file and what differs. Work out yourself every total, count or sum " +
	"the report or an email states, from the figures the files show. When the request gives rules for working " +
	"figures out and you are shown the sources, recompute at least two rows from the sources by those rules - " +
	"the first, and one a rule changes - and compare. A figure the result presents as a source's own must equal " +
	"what that source states. When rules sort the source's rows into places, every source row must be where the " +
	"rules put it, exactly once (a copy shown as its own row marked as the duplicate, with its amounts in no " +
	"total, is counted once). Every rule applies to every entry it covers, in the request's order. A total of " +
	"something the request defines counts everything the definition covers. A record the worker made during this " +
	"task is the task's result, not one that \"already existed\". A file shown cut short is judged on the part " +
	"shown. Text taken from a web page reads as the page shows it (no markup or character codes), and in a " +
	"workbook a figure the task asks for is a number in a cell of its own.\n" +
	"A refusal about how an item is classified or decided (its category, priority, status, approval) must rest " +
	"on the item's own source - its message, document or record - and say the source's own words that require " +
	"the change. The worker's working notes are not a source: a note that disagrees with the item's " +
	"classification is a fault in the notes - say so, and that the notes are to be corrected, never the answer " +
	"the source supports. When the worker kept an answer against an earlier check and quotes the source, judge " +
	"the quote against the source.\n" +
	"Answer \"unsure\" whenever you cannot tell from what you are shown. Never guess \"done\". When you are unsure " +
	"only because a part of a file you need is not shown (a row, a person, a section), add \"look\": up to 6 exact " +
	"words to find in the files (a row's reference, a name, a heading) - you will be shown the lines that hold " +
	"them.\n" +
	"When the facts checked in code say that a file the request gives changed during the task, decide from the " +
	"request's words whether it asked for that file itself to change. When it did not, add \"put_back\": [that " +
	"file's name] - the original, kept before the task, is put back in its place; the rest of the work is judged " +
	"as usual. Never name a file whose change the request asks for, or one the facts do not say changed.\n" +
	"Reply with JSON only: {\"verdict\": \"done\"|\"not_done\"|\"cannot\"|\"unsure\", \"confidence\": 0.0-1.0, \"seen\": \"what " +
	"the screen now shows (or, with no screen, what the files show), one sentence\", \"why\": \"one sentence\", " +
	"\"look\": [optional], \"put_back\": [optional]}"
