package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"time"

	"thebot/internal/llm"
	"thebot/internal/prompt"
	"thebot/internal/safety"
)

// OperatorSkill works a goal out on this machine, one shell command at a
// time: the model picks a command, sees what it printed, and picks the next
// one until it can answer.
//
// It exists because TheBot could not do the most ordinary thing a desktop
// agent is asked. The terminal skill runs a command someone already wrote
// down; asked "what is the current kernel version?" with no command in the
// request, the old planner had to invent a plan up front, before seeing
// anything - and on a 3B model that plan was "search the web for the
// current weather in New York City". A plan written before the first
// observation cannot adapt to it. A loop can.
//
// The loop's rules are Decurix's own Ollama harness's rules, measured there
// against the same model (decurix_agent/task_runner.py): one marker per
// reply, one push back on a HELP before anything was tried, a repeated
// command answered with its own output instead of being run again, and a
// reply with no marker taken as the answer it almost always is. The
// transcript only ever grows at the end, so every turn after the first
// reuses Ollama's cached prefix and reads only what the last command added.
type OperatorSkill struct {
	// MaxTurns bounds one goal. Zero means defaultOperatorTurns.
	MaxTurns int
	// CommandTimeout bounds one command. Zero means OPERATOR_COMMAND_TIMEOUT_SECONDS
	// from the environment, else defaultCommandTimeout.
	CommandTimeout time.Duration
}

const (
	defaultOperatorTurns  = 16
	defaultCommandTimeout = 180 * time.Second
	// maxCommandOutputChars is how much of one command's output a local
	// model is shown (llm.Capabilities.CommandOutputChars; a hosted one sees
	// more). One `apt install` or `find /` would otherwise inflate every
	// later turn of the same goal.
	maxCommandOutputChars = 4000
	maxInvalidReplies     = 3
)

func NewOperator() *OperatorSkill { return &OperatorSkill{} }

// logReplies puts each raw model reply into the job log. Off by default -
// it doubles the log - and the first thing to turn on when the operator
// does something its commands don't explain.
var logReplies = os.Getenv("THEBOT_LOG_MODEL_REPLIES") != ""

func (o *OperatorSkill) Name() string { return "operator" }
func (o *OperatorSkill) Description() string {
	return "Works a goal out on this machine by running shell commands one at a time and reading what they print."
}

func (o *OperatorSkill) Matches(subtask Subtask) bool { return subtask.Type == "operator" }

// operatorContract opens the task turn. It lives in the user turn rather
// than the system frame so the frame stays identical for every other kind
// of call in the job.
const operatorContract = `Work on the task below on this computer by running shell commands, one at a time.

Every reply is exactly one of these, marker first:

COMMAND:
<one shell command>

DONE:
<your final answer for the human>

HELP:
<the one thing you need from the human>

- DONE when the task is finished, or when you can already answer it (a greeting, a fact you were given). Base it on output you actually saw - never report work you did not verify.
- COMMAND when you need to look at or change something. You get the exit code and output back. If it fails, read the error and try something different; never resend a command that just failed.
- HELP only when no command can unblock you: the human has to choose, give you a secret, or do something outside this machine. Not knowing yet is not being blocked - run a command and find out.
- Before a payment, sending a message as the human, publishing, or deleting something that cannot be recovered, reply HELP saying exactly what you would do. Drafting an email with office email is not sending it (a person sends it from the Outbox): do that without asking.
- Commands run in %s with no terminal attached: never open editors or pagers, and pass -y to installers.
- For anything in a window (a web page in the browser, LibreOffice, a text editor) use the gui command: gui observe lists the screen's elements with refs like e12, then gui click e12, gui type e12 "text", gui key ctrl+s, gui select e12 "option", gui open <url|file|app>, gui scroll down 5 (or up; 5 is wheel notches, about 100 pixels each - over the part of the page to move, gui scroll x,y down 5). Every gui action prints what changed. gui read e12 prints the whole text of a field or document with line numbers - gui observe shows only its first 60 characters - so read a text before you change it and again after, and report a change as made only when gui read shows it: \"replace every X\" is done when no X is left in the text, so use the app's Find and Replace with its replace-all option (a button, or a checkbox to tick first), not one Replace at a time. gui type on a field that holds several lines is refused, because it would replace the whole document; change part of it through the app, or with --append where the cursor is. gui recordings <words> finds a recorded demonstration of a task, and gui replay <id> runs it again. gui skills <words> finds a task this desktop was taught: gui skill <name> says what it needs and gui do <name> --param name=value does it with no model - prefer it to working the task out yourself, but only in the app or site it works in (gui skill says where): the same kind of work in another system is done there with gui. A value gui skill calls a list takes every item of the document in one run, in the document's order, separated by \"; \" (--param product=\"Desk; Chair\" --param quantity=\"3; 1\") - never only the first item. gui ask chatgpt "QUESTION" (or google, duckduckgo, wikipedia, youtube) puts one question to that service in the browser and prints its answer - run it once per question.
- To read a document (PDF, scan, Word, Excel, email, picture) run office read <file> [pages]; never guess at a file's contents.
- To change an existing spreadsheet, never rewrite it with a script (that drops its charts): run office update <file.xlsx> - with a JSON spec on stdin, e.g. office update model.xlsx - <<'EOF' then {"edits": [{"sheet": "Data", "cell": "B14", "value": 1250}], "replace_refs": [{"from": "Data!$B$2:$B$13", "to": "Data!$B$2:$B$14"}]} then EOF. replace_refs makes charts and totals take in rows you added. office create <file.xlsx> - makes a new one from {"sheets": [{"name": "Invoices", "rows": [["Invoice", "Amount"], ["INV-1", 1200]], "cells": {"B3": "=SUM(B2:B2)"}}]} (rows fill from A1; cells set one address each).
- To prepare an email, run office email --to <address> --subject "<subject>" [--attach <file>] with the body on stdin (a heredoc). It goes to the Outbox for the human to review and send - it is never sent, so drafting one needs no approval. There is no way for you to send email yourself.
%s%s
Task: %s`

// visionRule is added to the contract for a brain that reads images: the
// screenshot comes with every gui command's result.
const visionRule = `- The pages of a scanned document that office read finds are shown to you as pictures: read them yourself.
- Windows: gui observe names the active window and the open ones. Before working in a window that is not the active one, bring it forward with gui front "a few words of its title" (for the browser: gui front browser) - never click the taskbar at the bottom of the screen for it, and alt+tab does nothing here. Close one you no longer need with gui key alt+f4 once it is in front, unless the task wants it left open.
- A page that asks whoever is there to prove they are a person (a CAPTCHA, "I'm not a robot", "unusual traffic") is for the person, never for you: reply HELP asking them to complete it on this worker's screen and to reply when done. Do not click at it.
- After every gui command you are also shown a screenshot of the screen as it is now. Use it to check what really happened and to find what the element list does not name (a picture, a canvas, an unlabelled icon, a message painted on the page). Still act by ref from gui observe wherever there is one; use x,y coordinates only for something that has no ref (and where gui observe offers gui click --see, describe the thing with that instead of reading a position). The screenshot has a ruler drawn on it: dotted pink lines every 100 pixels, each numbered with its real screen position at the top and left edges and again half way across. Read a position off those numbers (a point between the 300 and 400 lines is 300-and-something) - never estimate it from the picture's own size, which is not the screen's. After a click by coordinates, check in the next screenshot that it landed; if nothing changed, read the position again more carefully before trying another.
`

// taskRules adds source grounding and application verification for hosted brains.
const taskRules = `- Use documents only when their names, identifiers and contents belong to the person, organization or task requested. Leave unrelated sources out, finish the rest, and report what you excluded.
- Match records using identifiers and relevant attributes from the request and sources. Capitalization, punctuation or spacing alone need not make a different record. Similar names alone are not enough; if several candidates fit, ask the person with the distinguishing evidence.
- When comparing documents, compare the requested fields, ignoring only capitals and spacing unless the task permits other normalization. Report differences with both values and their sources.
- Two lists may describe the same entry differently: compare descriptions, identifiers, attributes and units before calling an entry missing or new. When each of two lists has an entry left over that agrees on who, when and how much but whose reference is written differently (a leading zero, punctuation, a prefix), report the pair as a probable match with both spellings - not as matched, and not as two missing entries. Whatever you say about a file you wrote must be what the file holds: read it back before DONE.
- A table of entries on a page shows its fields for one row at a time, and gui observe says which (textbox "Quantity" [row 2]). Tab out of a row's last field moves to the next row, making it if there is none: look before adding a row, fill each row's fields by that row's own refs, and delete a row left empty before saving. A form opens with defaults already chosen (a company, an account, a date, a tax): set every one the request names, and compare the rest with the request before saving.
- Use the source's values when making a draft from it, and report differences from application defaults. Every figure has a unit and basis: read them, convert to what was asked, and check the result.
- In an application, scroll and observe before deciding a field is missing. Never click the screen's bottom edge or a scrollbar to move a page. A form is saved only when the application says so; fix field errors and save again. On a page with several forms, use Save within the form you filled. Set dropdowns and suggestion fields with gui select REF "value", then verify the selected entry; typing and Tab may leave the field unset. Complete any configuration dialog using the requested attributes and verify the result. Type dates in the format the field shows, then check that they hold.
- Before DONE, check every part of the request in the application or output files. List what was completed, how you verified it, and anything left undone with its reason.
`

// successQueryNudge answers a HELP that arrives straight after a command
// succeeded, with that command's output in front of the model again.
//
// Measured on qwen2.5-coder:3b through this loop: asked "how many files are
// in /etc?", it ran `ls -l /etc | wc -l`, got exit 0 and "118", and replied
// HELP "It seems there was an error. Please try running the command again."
// Asked the kernel version, it ran `uname -r` and delivered the correct
// answer - under HELP. Both paged a human about a task that was already
// done. A HELP that neither follows a failure nor asks for anything a human
// alone can give is a mislabelled answer far more often than a real block.
const successQueryNudge = `Nothing failed: your last command finished with exit code 0 and printed:

%s

If that answers the task, reply now with DONE: followed by the answer.
If there is more to do, send the next COMMAND.
Only reply HELP again if the human really has to act - to choose between options, give you something only they have, or approve a consequential action.`

// humanOnlyRe recognises a HELP that is genuinely asking for something only
// a person can give. Such a HELP is always delivered, never pushed back on.
var humanOnlyRe = regexp.MustCompile(`(?i)\b(approv|permission|consent|confirm|authori[sz]|captcha|not\s+a\s+robot|human\s+check|a\s+person\s+is\s+there|password|passphrase|token|api[ -]?key|credential|secret|log ?in|sign ?in|2fa|otp|which\s+(one|of)|choose|pick|prefer|decide|should\s+i|do\s+you\s+want|payment|pay\b|purchase|send(ing)?\s+(an?\s+)?(e-?mail|message|sms)|publish|push)`)

// failureQueryNudge answers a HELP that follows a failed command - the one
// case successQueryNudge and prematureQueryNudge leave open, and the one that
// paged a human live: `chatgpt "where is addis ababa"` became
// `... | xargs curl -s https://www.google.com/search?q=`, which failed with
// "xargs: curl: No such file or directory", and the person was sent
// "Install curl using sudo apt-get install curl before running the command."
// A failure is something to try another way, not a job for the person, until
// the loop has tried twice (maxFailureNudges, the same as the Ollama
// harness's MAX_FAILURE_NUDGES).
const failureQueryNudge = `Your last command did not work: it finished with exit code %d and printed:

%s

Do not ask the human yet, and never ask them to run or install something for you. Try a DIFFERENT command that gets the same result - a simpler one, without the filter or pipe that may have hidden the output, or a different tool. Send it now with COMMAND.
Only reply HELP if the human really has to act - to choose between options, give you something only they have, or approve a consequential action.`

const maxFailureNudges = 2

// ungroundedDoneNudge answers a DONE sent when every command so far failed.
const ungroundedDoneNudge = `Every command you have run so far FAILED, so nothing on this machine has shown you the answer. Do not answer from memory.

Send a DIFFERENT command that gets the real answer with COMMAND.
If the failure itself is the answer (for example, the file really does not exist), reply DONE: again and quote the error that shows it.`

// missingProgramRe finds the program a failed command could not find:
// "bash: curl: command not found", "xargs: curl: No such file or directory",
// "sh: 1: jq: not found".
//
// "No such file or directory" names a program only when a launcher says it
// (xargs, env, a shell); from anything else it is a file the program was
// given ("cat: notes.txt: No such file or directory").
var missingProgramRe = regexp.MustCompile(`(?m)(?:^|:\s)([A-Za-z0-9_.+-]+): (?:command not found|not found)\s*$|(?:^|\b)(?:xargs|env|nohup|timeout|sudo|exec|nice|bash|sh|dash|zsh)(?:: line \d+)?: ([A-Za-z0-9_.+-]+): No such file or directory\s*$`)

// missingModuleRe finds a Python module that is not installed.
var missingModuleRe = regexp.MustCompile(`No module named '([\w.]+)'`)

// missingProgramHint tells the model what to use instead of a program that
// is not installed. It cannot install anything (no sudo), and every desktop
// image has python3 - which also fetches a web page, the job curl and wget
// are most often reached for.
func missingProgramHint(output, command string) string {
	if m := missingModuleRe.FindStringSubmatch(output); m != nil {
		return fmt.Sprintf("\n\nThe Python module `%s` is not installed, and you cannot install it here. Use Python's standard library instead: urllib.request for web requests (urllib.request.urlopen(url).headers, .status, .read()), json, csv, subprocess.", m[1])
	}
	for _, m := range missingProgramRe.FindAllStringSubmatch(output, -1) {
		program := m[1]
		if program == "" {
			program = m[2]
		}
		if !regexp.MustCompile(`(^|[\s|;&(])` + regexp.QuoteMeta(program) + `(\s|$)`).MatchString(command) {
			continue // a missing file the program was given, not the program
		}
		hint := fmt.Sprintf("\n\n`%s` is not installed on this machine, and you cannot install software here. Use a program that is here instead.", program)
		switch program {
		case "curl", "wget", "http", "lynx", "links", "w3m":
			hint += " To fetch a page use python3, for example:\nCOMMAND:\npython3 -c \"import urllib.request; print(urllib.request.urlopen('https://example.com', timeout=20).read().decode('utf-8', 'replace')[:3000])\""
		case "jq":
			hint += " To read JSON use python3 -c \"import json,sys; ...\"."
		}
		return hint
	}
	return ""
}

// prematureQueryNudge answers a HELP sent before any command ran. Its last
// paragraph matters: HELP is also the approval brake, and this must never
// talk the model out of asking permission.
const prematureQueryNudge = `You asked for help before running a single command, so nothing has been tried yet.

If you can already answer - including from the facts you were given - answer now with DONE.
If you could find out by running something, run it with COMMAND.
Only reply HELP again if the human really has to act before you can continue.

If you were pausing for approval before a payment, an outgoing message, or another real-world consequence: when it is a click in an app or a web page, do not ask - do the work up to that click and make it with gui, which stops such a click itself and asks the human with the details in front of them. For anything else (a shell command, a script), reply HELP again with the same request and it will go to the human.`

var (
	controlRe      = regexp.MustCompile(`\b(COMMAND|DONE|HELP):[*_` + "`" + `]*`)
	looseControlRe = regexp.MustCompile("(?im)^[\\s*_`#>-]*(COMMAND|DONE|HELP)\\s*:[*_`]*")
	fenceWrapRe    = regexp.MustCompile("(?s)^```[a-zA-Z]*\\n(.*?)\\n?```$")
	// bareMarkerRe is a reply that is a marker word and nothing else
	// ("DONE", "**Done.**"): no colon, so not a control signal, and no
	// content, so not an answer either.
	bareMarkerRe = regexp.MustCompile("(?i)^[\\s*_`#>-]*(COMMAND|DONE|HELP)[\\s*_`.!:]*$")
)

// parseControl finds the reply's control signal and its value, bounded at
// the next marker - a model that writes COMMAND: and then a predicted DONE:
// for output it has not seen yet must not have that prediction folded into
// the command it runs.
func parseControl(reply string) (action, value string) {
	reply = strings.TrimSpace(reply)
	matches := controlRe.FindAllStringSubmatchIndex(reply, -1)
	if len(matches) == 0 {
		matches = looseControlRe.FindAllStringSubmatchIndex(reply, -1)
	}
	if len(matches) == 0 {
		return "invalid", reply
	}
	first := matches[0]
	end := len(reply)
	if len(matches) > 1 {
		end = matches[1][0]
	}
	action = strings.ToLower(reply[first[2]:first[3]])
	value = strings.TrimSpace(reply[first[1]:end])
	if action == "command" {
		if m := fenceWrapRe.FindStringSubmatch(value); m != nil {
			value = strings.TrimSpace(m[1])
		}
		value = strings.TrimSpace(strings.Trim(value, "`"))
		value = strings.TrimSpace(strings.TrimPrefix(value, "$ "))
		value = withoutLeakedCall(value)
	}
	return action, value
}

// leakedCallRe is the trace a hosted model leaves when part of its own
// tool-call framing spills into the text of a reply: " to=<channel>" followed
// by the fields of a call or of its result.
var (
	leakedToRe     = regexp.MustCompile(`\s+to=[A-Za-z_][\w.:]*`)
	leakedFieldsRe = regexp.MustCompile(`^(?:\s+\S+)*?\s+(?:code=\d+|stdout=|stderr=|code_block\b|format=[a-z]+)`)
)

// leakedCall is where a spill of call framing starts in a line, or -1: a
// " to=<channel>" that is not inside a quoted argument and is either followed
// by a call's fields or comes straight after text in another script glued on
// by the model (`"65000.00" <Thai>assistant to=all:final code`).
func leakedCall(line string) int {
	for _, loc := range leakedToRe.FindAllStringIndex(line, -1) {
		before := line[:loc[0]]
		if strings.Count(before, `"`)%2 == 1 || strings.Count(before, `'`)%2 == 1 {
			continue
		}
		if leakedFieldsRe.MatchString(line[loc[1]:]) {
			return loc[0]
		}
		words := strings.Fields(before)
		if len(words) == 0 {
			continue
		}
		last := words[len(words)-1]
		if strings.IndexFunc(last, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
			return loc[0]
		}
		// A channel's name before it and a payload's kind after it: both, or
		// it is an ordinary argument ("./final to=backup:/srv").
		if leakedRoleRe.MatchString(last) && leakedKindRe.MatchString(line[loc[1]:]) {
			return loc[0]
		}
	}
	return -1
}

// leakedRoleRe is a word that ends in the name of one of the model's own
// channels, as the word before a spilled " to=" does ("...analysis
// to=commentary").
var (
	leakedRoleRe = regexp.MustCompile(`(?i)(analysis|assistant|commentary|final)$`)
	leakedKindRe = regexp.MustCompile(`^\s+(?:code|code_block|json)\b`)
)

// withoutLeakedCall cuts a command back to what the model meant when its
// reply ran on into leaked call framing. Found live, several times a day on
// one hosted model:
//
//	gui key enter<Thai text> to=gui.key  <Chinese text> code=0 stdout="Pressed enter. ...
//	gui key Tab【<Thai text>analysis to=final code_block format=plaintext ...
//
// The whole line went to the shell: the key was not pressed (or the wrong
// keys were), the field after it was never filled, and the task carried on
// as if it had been. The cut is made at the framing, and then any trailing
// characters outside ASCII glued to the command's last word are dropped with
// it - they are the start of the spill, never part of a key or a ref. A
// command with no such framing is returned untouched, whatever script its
// text is in.
func withoutLeakedCall(command string) string {
	lines := strings.Split(command, "\n")
	for i, line := range lines {
		at := leakedCall(line)
		if at < 0 {
			continue
		}
		kept := strings.Fields(line[:at])
		for len(kept) > 0 {
			// The word the spill began in: its channel name goes, then
			// everything from its first character outside ASCII; a word
			// left with no letter or digit was all spill.
			last := leakedRoleRe.ReplaceAllString(kept[len(kept)-1], "")
			spilled := last != kept[len(kept)-1]
			if cut := strings.IndexFunc(last, func(r rune) bool { return r > unicode.MaxASCII }); cut >= 0 {
				last, spilled = last[:cut], true
			}
			if !spilled {
				break
			}
			if strings.IndexFunc(last, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
				kept = kept[:len(kept)-1]
				continue
			}
			kept[len(kept)-1] = last
			break
		}
		lines[i] = strings.Join(kept, " ")
		// What follows the spill on later lines is more of it.
		lines = lines[:i+1]
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// unmarkedCommandRe is a reply that is one command and nothing else, in a
// shape no sentence has: the worker's own gui / office commands, a PowerShell
// cmdlet (Verb-Noun), or an expression that starts as PowerShell does
// ($env:..., (Get-...)).
var unmarkedCommandRe = regexp.MustCompile(`^(?:gui\s+(?:observe|click|type|key|select|open|scroll|drag|verify|upload|ask|recordings?|replay|skills?|do)\b.*` +
	`|office\s+(?:read|update|create|combine|email)\b.*|\(?[A-Z][a-z]+-[A-Z][A-Za-z]+\b.*|\$env:[A-Za-z_]+.*)$`)

// unmarkedCommand is the command a marker-less reply is, or "" when it is
// anything else. Found live on a 1.5B model: asked how many files were in
// its folder it replied `Get-ChildItem C:\...\work -FileCount` with no
// COMMAND: in front, was told three times to add the marker, and its fourth
// identical reply was delivered to the person as the answer. A command is to
// be run - and if it is wrong (that one is), its error is what teaches the
// next try. Deliberately narrow: one line, in a shape prose never takes, so a
// sentence is never run as a command.
func unmarkedCommand(reply string) string {
	text := strings.TrimSpace(reply)
	if m := fenceWrapRe.FindStringSubmatch(text); m != nil {
		text = strings.TrimSpace(m[1])
	}
	text = strings.TrimSpace(strings.Trim(text, "`"))
	if text == "" || strings.ContainsAny(text, "\r\n") || len(text) > 400 {
		return ""
	}
	if !unmarkedCommandRe.MatchString(text) {
		return ""
	}
	return text
}

// operatorState is the operator's checkpoint: everything needed to carry
// on from exactly where it paused.
type operatorState struct {
	Messages      []llm.Message
	Pending       string // a command the approval gate stopped
	CommandsRun   int
	Nudged        bool
	SuccessNudged bool
	FailureNudges int
	// CommandsSucceeded counts commands that exited 0; UngroundedNudged is
	// set once a DONE with none of them has been pushed back.
	CommandsSucceeded int
	UngroundedNudged  bool
	EarlyDoneNudged   bool // a DONE before any command was pushed back
	ScreenNudged      bool // a DONE for typing or clicking no command did was pushed back
	UnstartedNudged   bool // a DONE for work on the screen with no command run at all was pushed back
	ScreenConfirmed   bool // ...and, said again, was put to the person: their answer stands
	// Hosted is whether the model is a hosted one, fixed when the goal starts.
	// The host then judges the screen itself before a result is reported, so
	// the commands need not stand in as the evidence (unperformedScreenNudge).
	Hosted bool
	BudgetWarned      bool // told that its turns are nearly used up
	LastCommand       string
	LastResult        *shellResult
	Commands          []string
	// Worked is the commands that exited 0: what was actually done, as
	// against what was tried (unperformedScreenNudge reads it).
	Worked []string
	// OutputChars is how much of a command's output the model is shown,
	// fixed when the goal starts (llm.Capabilities.CommandOutputChars) so a
	// resumed goal reads the same way it began.
	OutputChars int
	// Vision is whether the model is shown the screen after on-screen
	// commands (llm.Capabilities.Vision), fixed with OutputChars.
	Vision bool
	// Screenshot is the file Decurix's gui command leaves the screen in
	// (DECURIX_GUI_SCREENSHOT), when Vision is on.
	Screenshot string
	// ScreenScale and ScreenTextChars are handed to the gui command.
	ScreenScale     int
	ScreenTextChars int
	// Turns bounds the goal (llm.Capabilities.OperatorTurns).
	Turns int
	// Request is the goal as asked; Seen is everything the operator has
	// been shown as text since (the request, command outputs, a person's
	// replies), and ShownPages whether a document page came as a picture.
	// grounding.go refuses to type a value none of them hold.
	Request        string
	Seen           string
	ShownPages     bool
	UntracedNudges int
}

// outputChars is OutputChars, or the local default for a checkpoint written
// before it existed.
func (st *operatorState) outputChars() int {
	if st.OutputChars > 0 {
		return st.OutputChars
	}
	return maxCommandOutputChars
}

func formatCommandResult(r shellResult, limit int) string {
	out := r.Output
	if len(out) > limit {
		out = out[:limit] + fmt.Sprintf("\n... [%d more characters truncated]", len(r.Output)-limit)
	}
	return fmt.Sprintf("Command execution result:\n\nexit_code: %d\n\noutput:\n%s", r.ExitCode, out)
}

func headline(r shellResult) string {
	line := strings.TrimSpace(r.Output)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if len(line) > 160 {
		line = line[:160] + "…"
	}
	if line == "" {
		return fmt.Sprintf("exit %d", r.ExitCode)
	}
	return fmt.Sprintf("exit %d · %s", r.ExitCode, line)
}

func (o *OperatorSkill) turns(st *operatorState) int {
	if o.MaxTurns == 0 && st != nil && st.Turns > 0 {
		return st.Turns
	}
	if o.MaxTurns > 0 {
		return o.MaxTurns
	}
	return defaultOperatorTurns
}

func (o *OperatorSkill) commandTimeout() time.Duration {
	if o.CommandTimeout > 0 {
		return o.CommandTimeout
	}
	if v, err := strconv.Atoi(os.Getenv("OPERATOR_COMMAND_TIMEOUT_SECONDS")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultCommandTimeout
}

func (o *OperatorSkill) Run(rc RunContext, subtask Subtask) (Result, error) {
	st, resumed := rc.Resume.(*operatorState)
	if resumed && st != nil {
		var blocked string
		st, blocked = o.resume(rc, subtask, st)
		if blocked != "" {
			res := NeedsApproval(subtask, o.Name(), st.Pending, blocked, st)
			res.Meta["commands"] = st.Commands
			return res, nil
		}
	} else {
		st = o.start(rc, subtask)
	}
	return o.loop(rc, subtask, st)
}

func (o *OperatorSkill) start(rc RunContext, subtask Subtask) *operatorState {
	findings := ""
	if f := rc.PriorFindings(); f != "" {
		findings = "\nEarlier steps of this job already found the following - build on it rather than redoing it:\n" + f + "\n"
	}
	messages := []llm.Message{rc.SystemMessage()}
	messages = append(messages, rc.SessionHistory...)
	caps := rc.Brain.Caps()
	st := &operatorState{
		OutputChars:     caps.CommandOutputChars(),
		Vision:          caps.Vision,
		ScreenScale:     caps.ScreenshotScale(),
		ScreenTextChars: caps.ScreenTextChars(),
		Turns:           caps.OperatorTurns(),
		Hosted:          caps.Hosted,
	}
	vision := ""
	if caps.Hosted {
		vision += taskRules
	}
	if st.Vision {
		vision += visionRule
		st.Screenshot = filepath.Join(os.TempDir(), fmt.Sprintf("thebot-screen-%d.png", time.Now().UnixNano()))
	}
	st.Messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf(operatorContract, rc.Dir(), vision, findings, subtask.Step)})
	st.Request = subtask.Step
	st.noteSeen(subtask.Step + "\n" + findings)
	return st
}

// resume continues a paused transcript with the human's reply: an approved
// command runs now, anything else is handed to the model as their answer.
// A non-empty reason back means the approved command was stopped again on
// screen, and st.Pending holds it.
func (o *OperatorSkill) resume(rc RunContext, subtask Subtask, st *operatorState) (*operatorState, string) {
	reply := strings.TrimSpace(rc.HumanReply)
	if st.Pending != "" {
		pending := st.Pending
		st.Pending = ""
		if rc.IsApproved(pending) {
			rc.Logf("operator", "approved - running it now")
			if blocked := o.execute(rc, st, pending); blocked != "" {
				st.Pending = pending
				return st, blocked
			}
			return st, ""
		}
		note := fmt.Sprintf("The human did not approve running:\n$ %s", pending)
		if reply != "" {
			note += "\n\nTheir reply: " + reply
		}
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: note + "\n\nContinue the task without running that command."})
		return st, ""
	}
	if st.ScreenConfirmed {
		// The answer to "I cannot confirm this is done". Only a plain yes
		// lets the next DONE through; anything else is what is still missing,
		// and a DONE over it is put to them again.
		if screenConfirmRe.MatchString(reply) {
			st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "The human looked at the screen and confirmed the task is done. Reply DONE with a one-line report."})
			return st, ""
		}
		st.ScreenConfirmed = false
	}
	if reply != "" {
		st.noteSeen(reply)
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "The human replied: " + reply + "\n\nContinue the task."})
	}
	return st, ""
}

// screenConfirmRe is a person saying the work they were asked to look at is done.
var screenConfirmRe = regexp.MustCompile(`(?i)^\s*(done|yes|y|yep|yeah|ok|okay|confirmed|it is|it's done|looks good|correct)\s*[.!]*\s*$`)

// execute runs one command and records it in the transcript. It returns a
// non-empty reason, and records nothing, when the command was an on-screen
// action that Decurix's approval gate stopped - the caller pauses for
// approval exactly as it does for a command internal/safety stops.
func (o *OperatorSkill) execute(rc RunContext, st *operatorState, command string) string {
	rc.Logf("operator", "$ %s", command)
	started := time.Now()
	res := runShell(rc.Ctx, rc.Dir(), command, o.commandTimeout(), st.guiEnv()...)
	if reason := screenApprovalReason(res); reason != "" {
		rc.Logf("operator", "approval needed on screen - %s", reason)
		return reason
	}
	rc.Logf("operator", "%s", headline(res))
	st.CommandsRun++
	if res.ExitCode == 0 {
		st.CommandsSucceeded++
		st.Worked = append(st.Worked, command)
	}
	st.Commands = append(st.Commands, command)
	st.LastCommand = command
	st.LastResult = &res
	st.noteSeen(res.Output)
	result := formatCommandResult(res, st.outputChars())
	if res.ExitCode != 0 {
		result += missingProgramHint(res.Output, command)
	} else if st.ScreenNudged {
		result += typingFieldHint(st, command, res.Output)
	}
	msg := llm.Message{Role: "user", Content: result}
	if pages := st.documentPictures(res.Output); len(pages) > 0 {
		st.forgetOldPictures(pagesNote, maxDocumentPictures-len(pages))
		st.ShownPages = true
		msg.Images = pages
		msg.Content += pagesNote
	} else if shot := st.takeScreenshot(started); shot != nil {
		st.forgetOldPictures(screenshotNote, 0)
		msg.Images = [][]byte{shot}
		msg.Content += screenshotNote
	}
	st.Messages = append(st.Messages, msg)
	return ""
}

// The notes that mark which kind of picture a message carries. Screenshots
// and document pages are kept apart: a screenshot is out of date the moment
// the next one arrives, while a scanned invoice is read again and again for
// the whole task - live, on the Office (GPT) engine, screenshots pushed the
// invoice out of the conversation and the engine asked a person to type in
// what it had already read.
const (
	screenshotNote = "\n\n(The screenshot shows the screen now.)"
	pagesNote      = "\n\n(The document's pages are shown as pictures.)"
	// maxDocumentPictures bounds the document pages kept in the transcript.
	maxDocumentPictures = 6
)

// pageImageRe finds the pictures office read lists for a scan.
var pageImageRe = regexp.MustCompile(`(?m)^\[page image: ([^\]\n]+)\]\s*$`)

// documentPictures loads the page pictures a command's output names, for a
// brain that can see (at most maxDocumentPictures).
func (st *operatorState) documentPictures(output string) [][]byte {
	if !st.Vision {
		return nil
	}
	var out [][]byte
	for _, m := range pageImageRe.FindAllStringSubmatch(output, -1) {
		if len(out) == maxDocumentPictures {
			break
		}
		info, err := os.Stat(strings.TrimSpace(m[1]))
		if err != nil || info.Size() == 0 || info.Size() > maxScreenshotBytes {
			continue
		}
		if data, err := os.ReadFile(strings.TrimSpace(m[1])); err == nil {
			out = append(out, data)
		}
	}
	return out
}

// guiEnv is what Decurix's gui command is told about this brain: how much of
// the screen's element list to print, and - for a brain that can see -
// where to leave a screenshot and at what size.
func (st *operatorState) guiEnv() []string {
	env := []string{"DECURIX_OFFICE_IMAGES=ocr"}
	if st.Vision {
		env[0] = "DECURIX_OFFICE_IMAGES=attach"
	}
	if st.ScreenTextChars > 0 {
		env = append(env, "DECURIX_GUI_TEXT_CHARS="+strconv.Itoa(st.ScreenTextChars))
	}
	if st.Vision && st.Screenshot != "" {
		env = append(env,
			"DECURIX_GUI_SCREENSHOT="+st.Screenshot,
			"DECURIX_GUI_SCREENSHOT_SCALE="+strconv.Itoa(max(1, st.ScreenScale)),
		)
		// A whole-size picture carries a ruler (computer/pointing.py): the
		// provider shrinks what the model is shown, so a position read off
		// the picture is only right when the picture says it. A half-size
		// one would make the ruler's numbers too small to read.
		if st.ScreenScale <= 1 {
			env = append(env, "DECURIX_GUI_RULER=1")
		}
	}
	return env
}

// maxScreenshotBytes bounds one attached screenshot; a full-size PNG of a
// busy 1280x720 screen is about 1 MB.
const maxScreenshotBytes = 4 << 20

// takeScreenshot is the screenshot the command just left, or nil: none is
// asked for, the command was not a screen command, or the file is older than
// the command (left by an earlier one). The file is removed once read, so a
// later non-screen command can never pick up a stale picture.
func (st *operatorState) takeScreenshot(since time.Time) []byte {
	if !st.Vision || st.Screenshot == "" {
		return nil
	}
	info, err := os.Stat(st.Screenshot)
	if err != nil || info.ModTime().Before(since.Add(-time.Second)) || info.Size() == 0 || info.Size() > maxScreenshotBytes {
		return nil
	}
	data, err := os.ReadFile(st.Screenshot)
	_ = os.Remove(st.Screenshot)
	if err != nil {
		return nil
	}
	return data
}

// forgetOldPictures takes the pictures off earlier messages of one kind
// (note), keeping the newest `keep` of them: none for screenshots, whose old
// copies are out of date by definition, and room for the new document pages
// within maxDocumentPictures. Every picture costs as much as a page of text on
// every later turn of the goal.
func (st *operatorState) forgetOldPictures(note string, keep int) {
	for i := len(st.Messages) - 1; i >= 0; i-- {
		m := &st.Messages[i]
		if len(m.Images) == 0 || !strings.HasSuffix(m.Content, note) {
			continue
		}
		if keep >= len(m.Images) {
			keep -= len(m.Images)
			continue
		}
		if keep > 0 {
			// Some of this message's pictures still fit: it keeps its note,
			// so a later pass can still drop the rest.
			m.Images = m.Images[:keep]
			keep = 0
			continue
		}
		m.Images = nil
		m.Content = strings.TrimSuffix(m.Content, note) + "\n(Pictures that were shown here are no longer shown - run the command again to see them.)"
	}
}

// guiApprovalExit and guiApprovalMarker are how Decurix's `gui` command says
// its approval gate stopped an on-screen action and nothing happened
// (decurix_agent/computer/approval.py). internal/safety cannot see this one
// coming: "gui click e35" says nothing about what e35 is. Only the gate,
// which resolves e35 to a "Send newsletter" button, knows - so the operator
// takes the command's own word for it.
const (
	guiApprovalExit   = 3
	guiApprovalMarker = "NEEDS_APPROVAL:"
)

// screenApprovalReason finds the gate's marker on any line of the output, not
// only the first: a small model chains several gui commands into one, and
// the shell's exit code is then the last command's, not the stopped one's.
func screenApprovalReason(r shellResult) string {
	for _, raw := range strings.Split(r.Output, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, guiApprovalMarker) {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, guiApprovalMarker))
		if r.ExitCode != guiApprovalExit && !strings.HasPrefix(line, "Approval needed before I ") {
			continue
		}
		line = strings.TrimPrefix(line, "Approval needed before I ")
		if i := strings.Index(line, ". Nothing was"); i >= 0 {
			line = line[:i]
		}
		return "on screen it would " + line
	}
	return ""
}

func (o *OperatorSkill) loop(rc RunContext, subtask Subtask, st *operatorState) (Result, error) {
	invalid := 0
	repeats := 0
	turnsUsed := 0

	finish := func(output string, verified bool, meta map[string]any) (Result, error) {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["commands"] = st.Commands
		// What exited 0, in order: the way through this task that worked. A
		// host keeps it once the result has been checked, so the same kind of
		// request need not be found out again from nothing.
		meta["worked"] = st.Worked
		meta["turns"] = turnsUsed
		meta["userFacing"] = true
		return Result{Subtask: subtask, SkillName: o.Name(), Output: output, Verified: verified, Meta: meta}, nil
	}

	for turn := 0; turn < o.turns(st); turn++ {
		turnsUsed = turn + 1
		if err := rc.Ctx.Err(); err != nil {
			return finish("Stopped before finishing: "+err.Error(), false, map[string]any{"error": err.Error(), "fatal": true})
		}

		if left := o.turns(st) - turn; left == closingTurns && o.turns(st) >= 4*closingTurns && !st.BudgetWarned {
			st.BudgetWarned = true
			rc.Logf("operator", "nearly out of turns - told to finish or to ask")
			appendToLastUser(st, fmt.Sprintf(closingNote, left))
		}

		resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, st.Messages, llm.Options{TimeoutMs: 180000, Temperature: 0.2})
		if err != nil {
			// The model itself is unusable. Retrying the same goal through
			// the same model reaches the same conclusion minutes later.
			return finish("The reasoning model failed: "+err.Error(), false, map[string]any{
				"error": err.Error(), "fatal": true, "needsUserInput": true,
				"failureReasoning": "could not reason about the task (" + err.Error() + ")",
			})
		}

		action, value := parseControl(resp.Text)
		if action == "invalid" {
			if command := unmarkedCommand(resp.Text); command != "" {
				rc.Logf("operator", "sent a command without its marker - running it")
				action, value = "command", command
			}
		}
		if prompt.IsNeedsTools(value) || prompt.IsNeedsTools(resp.Text) {
			// The chat signal has no meaning here - this loop is the tools.
			action = "invalid"
		}
		reply := llm.Message{Role: "assistant", Content: resp.Text}
		if logReplies {
			rc.Logf("operator", "model replied: %s", truncateText(strings.Join(strings.Fields(resp.Text), " "), 300))
		}

		switch action {
		case "done":
			if value == "" {
				value = "Done."
			}
			if st.CommandsRun > 0 && st.CommandsSucceeded == 0 && !st.UngroundedNudged {
				// Every command so far failed, so nothing on this machine has
				// shown the answer: a DONE now is from memory, unless the
				// failure itself is the answer ("that file does not exist"),
				// which the model can say again, quoting the error.
				st.UngroundedNudged = true
				rc.Logf("operator", "answered although every command failed - asking it where the answer came from")
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: ungroundedDoneNudge})
				continue
			}
			if st.CommandsRun == 0 && !st.EarlyDoneNudged {
				if nudge := earlyDoneNudge(st, rc.SessionHistory, value); nudge != "" {
					st.EarlyDoneNudged = true
					rc.Logf("operator", "answered before running anything - pushing back once")
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: nudge})
					continue
				}
			}
			if nudge := unstartedScreenNudge(st); nudge != "" && !st.ScreenConfirmed {
				if !st.UnstartedNudged {
					st.UnstartedNudged = true
					rc.Logf("operator", "said done for work on the screen without running anything - pushing back once")
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: nudge})
					continue
				}
				rc.Logf("operator", "still says done with nothing run - asking the person")
				st.ScreenConfirmed = true
				question := unstartedScreenDoubt(value)
				st.Messages = append(st.Messages, reply)
				res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
				res.Checkpoint = st
				return res, nil
			}
			// Only where nothing else will look: with a hosted model the host's final check judges the
			// screen. Found live there: sent back to a record it had entered correctly, the operator
			// rightly changed nothing and said so - and "the task says to type, and nothing was typed"
			// refused that twice and paged a person about a finished record.
			if nudge := unperformedScreenNudge(st); nudge != "" && !st.ScreenConfirmed && !st.Hosted {
				if !st.ScreenNudged {
					st.ScreenNudged = true
					rc.Logf("operator", "said done, but no command did the typing or clicking the task asks for - pushing back once")
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: nudge})
					continue
				}
				// Told once and still saying done over work no command did:
				// a person looks, rather than a success nothing showed going
				// out. Their answer comes back to this goal's transcript.
				rc.Logf("operator", "still says done with nothing typed or clicked - asking the person to look")
				st.ScreenConfirmed = true
				question := unperformedScreenDoubt(nudge, value)
				st.Messages = append(st.Messages, reply)
				res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
				res.Checkpoint = st
				return res, nil
			}
			if question := doneAsksThePerson(value); question != "" {
				// A DONE that only asks the person for what is missing is a
				// question, whatever marker it came under. Found live
				// (2026-10-04): told to ask for a sign-off only the person
				// knows, the model was pushed off its HELP once and then
				// "finished" with "I need your sign-off text ..." - reported
				// as the task's result, with nothing paused for the answer.
				rc.Logf("operator", "said done, but only asks the person for something - asking them")
				st.Messages = append(st.Messages, reply)
				res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
				res.Checkpoint = st
				return res, nil
			}
			return finish(value, true, nil)

		case "help":
			if st.CommandsRun == 0 && !st.Nudged {
				st.Nudged = true
				rc.Logf("operator", "had a query before trying anything - pushing back once")
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: prematureQueryNudge})
				continue
			}
			if st.LastResult != nil && st.LastResult.ExitCode == 0 && !st.SuccessNudged && !humanOnlyRe.MatchString(value) {
				st.SuccessNudged = true
				rc.Logf("operator", "had a query right after a command succeeded - showing it the output again")
				out := strings.TrimSpace(st.LastResult.Output)
				if out == "" {
					out = "(no output)"
				}
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: fmt.Sprintf(successQueryNudge, truncateText(out, st.outputChars()))})
				continue
			}
			if st.LastResult != nil && st.LastResult.ExitCode != 0 && st.FailureNudges < maxFailureNudges && !humanOnlyRe.MatchString(value) {
				st.FailureNudges++
				rc.Logf("operator", "had a query right after a command failed - asking it to try another way")
				out := strings.TrimSpace(st.LastResult.Output)
				if out == "" {
					out = "(no output)"
				}
				nudge := fmt.Sprintf(failureQueryNudge, st.LastResult.ExitCode, truncateText(out, st.outputChars())) + missingProgramHint(out, st.LastCommand)
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: nudge})
				continue
			}
			st.Messages = append(st.Messages, reply)
			res, _ := finish(value, false, map[string]any{"needsUserInput": true, "question": value, "failureReasoning": value})
			res.Checkpoint = st
			return res, nil

		case "command":
			// Keep only the command in the transcript. A small model writes
			// the answer it expects alongside the command - and if that stays
			// in its history it reads its own guess back next turn and reports
			// it. Measured: `df -h` printed 1007G/121G/835G and the reply was
			// "1.8T total, 20G used, 1.6T available", word for word the guess
			// written before the command ran; and `uname -r` came back as a
			// HELP saying the real kernel version "is not what was expected",
			// because the guess beside the command said something else.
			if value != "" {
				if strings.TrimSpace(resp.Text) != "COMMAND:\n"+value {
					reply = llm.Message{Role: "assistant", Content: "COMMAND:\n" + value}
				}
			}
			if value == "" {
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: "Your COMMAND was empty. Reply with a real command, or DONE with your answer."})
				continue
			}
			if value == st.LastCommand && st.LastResult != nil {
				repeats++
				if repeats == 1 {
					rc.Logf("operator", "repeated a command it already ran - showing it the output again")
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: repeatedCommandNudge(value, *st.LastResult, st.outputChars())})
					continue
				}
				// Told once and still repeating. What the command actually
				// printed is the answer - never the model's own trailing
				// DONE, which on "what is the kernel version?" predicted
				// 5.10.124 before running anything; the real answer was 6.6.87.
				rc.Logf("operator", "still repeating the same command - ending with what it printed")
				if st.LastResult.ExitCode == 0 {
					return finish(answerFromCommand(value, *st.LastResult, st.outputChars()), true, nil)
				}
				return finish(fmt.Sprintf("I kept retrying `%s`, and it keeps failing with exit code %d:\n\n%s",
					value, st.LastResult.ExitCode, truncateText(st.LastResult.Output, 1000)), false,
					map[string]any{"needsUserInput": true, "failureReasoning": "the same command keeps failing"})
			}
			repeats = 0
			invalid = 0

			if nudge := groundingNudge(st, value); nudge != "" {
				if st.UntracedNudges < maxUntracedNudges {
					st.UntracedNudges++
					rc.Logf("operator", "would have typed values it never read - sent back to find them")
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: nudge})
					continue
				}
				// Sent back twice and still typing what it cannot trace: a
				// person knows where the values are, a guess does not.
				question := "I could not find where the values to type come from, so I have not typed them. " +
					"Which values should I use? (" + strings.TrimPrefix(nudge, "Not run: ") + ")"
				st.Messages = append(st.Messages, reply)
				res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
				res.Checkpoint = st
				return res, nil
			}
			st.Messages = append(st.Messages, reply)

			if concern := safety.AssessCommand(value); concern != nil && !rc.IsApproved(value) {
				rc.Logf("operator", "approval needed before `%s` - %s", value, concern.Reason)
				st.Pending = value
				res := NeedsApproval(subtask, o.Name(), value, concern.Reason, st)
				res.Meta["commands"] = st.Commands
				return res, nil
			}
			if blocked := o.execute(rc, st, value); blocked != "" {
				st.Pending = value
				res := NeedsApproval(subtask, o.Name(), value, blocked, st)
				res.Meta["commands"] = st.Commands
				return res, nil
			}

		default:
			invalid++
			if invalid > maxInvalidReplies {
				// An answer without a marker is still an answer. The loop
				// this replaces argued about format until its budget ran out
				// and then reported a stuck agent - for "how many files are
				// in /etc?", answered correctly ("241") on turn two.
				// But a marker word and nothing else answers nothing. Found
				// live on a native Windows worker: asked to open an app, a
				// 1.5B model replied "DONE" four times without running a
				// command, and that word was reported as the result.
				text := strings.TrimSpace(resp.Text)
				bare := bareMarkerRe.FindStringSubmatch(text)
				if text != "" && bare == nil {
					rc.Logf("operator", "never used a marker - taking its reply as the answer")
					return finish(text, true, nil)
				}
				// "DONE" and nothing else, after a command that worked: the
				// model is finished and has not said with what. What the
				// command printed is the answer, as it is when a command is
				// repeated. Found live: `$env:COMPUTERNAME` printed the
				// computer's name, the model replied "DONE" four times, and
				// the person was paged about a step that had "failed".
				// Not for work in a window no command did - that is asked.
				if bare != nil && strings.EqualFold(bare[1], "DONE") && st.LastResult != nil && st.LastResult.ExitCode == 0 &&
					strings.TrimSpace(st.LastResult.Output) != "" && unperformedScreenNudge(st) == "" {
					rc.Logf("operator", "said done without an answer - ending with what its last command printed")
					return finish(answerFromCommand(st.LastCommand, *st.LastResult, st.outputChars()), true, nil)
				}
				// Nothing usable and nothing to report: said as it is, to the
				// person, with the goal kept so their reply carries it on. (A
				// plain failure here went to the scheduler's failure
				// reasoning, which explained it with a story of its own.)
				question := fmt.Sprintf("I could not finish this: the model stopped giving usable replies after %d command(s). "+
					"Send the request again, or tell me how to go on.", st.CommandsRun)
				res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
				res.Checkpoint = st
				return res, nil
			}
			st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: invalidReplyNudge(resp.Text)})
		}
	}

	if changedTheScreen(st) {
		// Work on the screen that ran out of turns is put to the person as it stands, with the goal kept so
		// their reply carries it on. Found live: a form was filled and could not be saved for want of one value
		// the request did not give; the plain failure went to the scheduler, whose recovery plan was to enter
		// the whole document again from the start - into an application that already held most of it.
		question := fmt.Sprintf("I ran out of steps (%d commands) before this was finished, and stopped rather than "+
			"start over.", st.CommandsRun)
		appendToLastUser(st, outOfTurnsNote)
		resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, st.Messages, llm.Options{TimeoutMs: 120000, Temperature: 0.2})
		if err == nil {
			if _, said := parseControl(resp.Text); strings.TrimSpace(said) != "" {
				st.Messages = append(st.Messages, llm.Message{Role: "assistant", Content: resp.Text})
				question += "\n\n" + strings.TrimSpace(said)
			}
		}
		question += "\n\nTell me how to go on, or reply `stop`."
		res, _ := finish(question, false, map[string]any{"needsUserInput": true, "question": question, "failureReasoning": question})
		res.Checkpoint = st
		return res, nil
	}

	res, _ := finish(fmt.Sprintf("I ran out of turns (%d) before finishing, after running %d command(s).", o.turns(st), st.CommandsRun), false,
		map[string]any{"needsUserInput": true, "failureReasoning": "ran out of turns without finishing - probably repeating an approach that cannot work"})
	res.Checkpoint = st
	return res, nil
}

// closingTurns is how many turns from the end the operator is told its
// budget is nearly spent (only for a budget of at least four times that).
const closingTurns = 8

// closingNote is said once, closingTurns from the end. Found live: an
// application refused to save for want of a value the request did not give,
// and the operator spent its last twenty-five turns scrolling and clicking
// for it without ever asking.
const closingNote = "\n\n(You have %d turns left for this task. Do not begin anything new. If the application is asking " +
	"for something the request and its documents do not give - a value, a choice between candidates - reply HELP now: " +
	"say what it asks for, in its own words, and what you have entered and saved so far. Otherwise finish, save and reply DONE.)"

// outOfTurnsNote asks for the state of the work when the turns are used up.
const outOfTurnsNote = "\n\n(No turns are left and no more commands will be run. Reply HELP: with three things, briefly - " +
	"what is entered or changed so far and whether it is saved; what is still missing; and the one thing you need from " +
	"the person to finish. Report only what the screen showed you.)"

// appendToLastUser adds a note to the newest message when it is the user's
// (a command's result), else as a message of its own: two user turns in a
// row are not accepted by every provider.
func appendToLastUser(st *operatorState, note string) {
	if n := len(st.Messages); n > 0 && st.Messages[n-1].Role == "user" {
		st.Messages[n-1].Content += note
		return
	}
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: strings.TrimSpace(note)})
}

// screenChangeRe is a gui command that acts on the screen, as against one that only looks at it.
var screenChangeRe = regexp.MustCompile(`^\s*gui\s+(click|double-click|doubleclick|dblclick|right-click|type|select|key|drag|upload|do|replay|open)\b`)

// changedTheScreen reports whether a command that worked acted on the screen.
func changedTheScreen(st *operatorState) bool {
	for _, command := range st.Worked {
		if screenChangeRe.MatchString(command) {
			return true
		}
	}
	return false
}

// invalidReplyNudge quotes the model's own opening back to it. The generic
// "use one of the three markers" version, measured on the real model,
// turned a correct marker-less "241" into a HELP; quoting it produced
// "DONE: 241".
func invalidReplyNudge(reply string) string {
	opening := strings.Join(strings.Fields(reply), " ")
	if len(opening) > 60 {
		opening = opening[:60]
	}
	return fmt.Sprintf(`Your last reply started with "%s", which is not one of the three markers, so it could not be used.

Send it again, unchanged, with the right marker in front:
If it was your final answer for the human, prefix it with DONE:
If it was a shell command to run, prefix it with COMMAND:
If you need the human before you can continue, prefix it with HELP:`, opening)
}

func repeatedCommandNudge(command string, r shellResult, limit int) string {
	out := strings.TrimSpace(r.Output)
	if out == "" {
		out = "(no output)"
	}
	if r.ExitCode != 0 {
		// "If that answers the task, reply with DONE" after a traceback is
		// an invitation to answer from memory. Measured live: python3 with
		// the missing `requests` module, run twice, then DONE with a
		// Content-Type header nothing had printed ("text/html;
		// charset=utf-8"; the real one was "text/html").
		return fmt.Sprintf(`You have already run this command, and it FAILED with exit code %d:

$ %s

%s

It did not produce the answer, so you do not have it yet - do not answer from memory. Send a DIFFERENT command with COMMAND.`, r.ExitCode, command, truncateText(out, limit)) + missingProgramHint(out, command)
	}
	return fmt.Sprintf(`You have already run this command, and it finished with exit code %d:

$ %s

%s

Do not run it again. If that answers the task, reply now with DONE: and the answer. Only send a COMMAND if it is a different command you still need.`, r.ExitCode, command, truncateText(out, limit))
}

func answerFromCommand(command string, r shellResult, limit int) string {
	out := strings.TrimSpace(r.Output)
	if out == "" {
		return fmt.Sprintf("`%s` finished with exit code %d and printed nothing.", command, r.ExitCode)
	}
	return truncateText(out, limit)
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated]"
}
