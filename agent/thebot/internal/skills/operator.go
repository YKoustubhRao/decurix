package skills

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
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
// The loop's rules are Flowera's own Ollama harness's rules, measured there
// against the same model (flowera_agent/task_runner.py): one marker per
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
	// maxCommandOutputChars is how much of one command's output the model
	// is shown. One `apt install` or `find /` would otherwise inflate every
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
- Before a payment, sending a message as the human, publishing, or deleting something that cannot be recovered, reply HELP saying exactly what you would do.
- Commands run in %s with no terminal attached: never open editors or pagers, and pass -y to installers.
%s
Task: %s`

// successHelpNudge answers a HELP that arrives straight after a command
// succeeded, with that command's output in front of the model again.
//
// Measured on qwen2.5-coder:3b through this loop: asked "how many files are
// in /etc?", it ran `ls -l /etc | wc -l`, got exit 0 and "118", and replied
// HELP "It seems there was an error. Please try running the command again."
// Asked the kernel version, it ran `uname -r` and delivered the correct
// answer - under HELP. Both paged a human about a task that was already
// done. A HELP that neither follows a failure nor asks for anything a human
// alone can give is a mislabelled answer far more often than a real block.
const successHelpNudge = `Nothing failed: your last command finished with exit code 0 and printed:

%s

If that answers the task, reply now with DONE: followed by the answer.
If there is more to do, send the next COMMAND.
Only reply HELP again if the human really has to act - to choose between options, give you something only they have, or approve a consequential action.`

// humanOnlyRe recognises a HELP that is genuinely asking for something only
// a person can give. Such a HELP is always delivered, never pushed back on.
var humanOnlyRe = regexp.MustCompile(`(?i)\b(approv|permission|consent|confirm|authori[sz]|password|passphrase|token|api[ -]?key|credential|secret|log ?in|sign ?in|2fa|otp|which\s+(one|of)|choose|pick|prefer|decide|should\s+i|do\s+you\s+want|payment|pay\b|purchase|send(ing)?\s+(an?\s+)?(e-?mail|message|sms)|publish|push)`)

// prematureHelpNudge answers a HELP sent before any command ran. Its last
// paragraph matters: HELP is also the approval brake, and this must never
// talk the model out of asking permission.
const prematureHelpNudge = `You asked for help before running a single command, so nothing has been tried yet.

If you can already answer - including from the facts you were given - answer now with DONE.
If you could find out by running something, run it with COMMAND.
Only reply HELP again if the human really has to act before you can continue.

If you were pausing for approval before a payment, an outgoing message, or another real-world consequence, reply HELP again with the same request and it will go to the human.`

var (
	controlRe      = regexp.MustCompile(`\b(COMMAND|DONE|HELP):[*_` + "`" + `]*`)
	looseControlRe = regexp.MustCompile("(?im)^[\\s*_`#>-]*(COMMAND|DONE|HELP)\\s*:[*_`]*")
	fenceWrapRe    = regexp.MustCompile("(?s)^```[a-zA-Z]*\\n(.*?)\\n?```$")
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
	}
	return action, value
}

// operatorState is the operator's checkpoint: everything needed to carry
// on from exactly where it paused.
type operatorState struct {
	Messages      []llm.Message
	Pending       string // a command the approval gate stopped
	CommandsRun   int
	Nudged        bool
	SuccessNudged bool
	LastCommand   string
	LastResult    *shellResult
	Commands      []string
}

func formatCommandResult(r shellResult) string {
	out := r.Output
	if len(out) > maxCommandOutputChars {
		out = out[:maxCommandOutputChars] + fmt.Sprintf("\n... [%d more characters truncated]", len(r.Output)-maxCommandOutputChars)
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

func (o *OperatorSkill) turns() int {
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
		st = o.resume(rc, subtask, st)
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
	messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf(operatorContract, rc.Dir(), findings, subtask.Step)})
	return &operatorState{Messages: messages}
}

// resume continues a paused transcript with the human's reply: an approved
// command runs now, anything else is handed to the model as their answer.
func (o *OperatorSkill) resume(rc RunContext, subtask Subtask, st *operatorState) *operatorState {
	reply := strings.TrimSpace(rc.HumanReply)
	if st.Pending != "" {
		pending := st.Pending
		st.Pending = ""
		if rc.IsApproved(pending) {
			rc.Logf("operator", "approved - running it now")
			o.execute(rc, st, pending)
			return st
		}
		note := fmt.Sprintf("The human did not approve running:\n$ %s", pending)
		if reply != "" {
			note += "\n\nTheir reply: " + reply
		}
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: note + "\n\nContinue the task without running that command."})
		return st
	}
	if reply != "" {
		st.Messages = append(st.Messages, llm.Message{Role: "user", Content: "The human replied: " + reply + "\n\nContinue the task."})
	}
	return st
}

func (o *OperatorSkill) execute(rc RunContext, st *operatorState, command string) {
	rc.Logf("operator", "$ %s", command)
	res := runShell(rc.Ctx, rc.Dir(), command, o.commandTimeout())
	rc.Logf("operator", "%s", headline(res))
	st.CommandsRun++
	st.Commands = append(st.Commands, command)
	st.LastCommand = command
	st.LastResult = &res
	st.Messages = append(st.Messages, llm.Message{Role: "user", Content: formatCommandResult(res)})
}

func (o *OperatorSkill) loop(rc RunContext, subtask Subtask, st *operatorState) (Result, error) {
	invalid := 0
	repeats := 0

	finish := func(output string, verified bool, meta map[string]any) (Result, error) {
		if meta == nil {
			meta = map[string]any{}
		}
		meta["commands"] = st.Commands
		meta["userFacing"] = true
		return Result{Subtask: subtask, SkillName: o.Name(), Output: output, Verified: verified, Meta: meta}, nil
	}

	for turn := 0; turn < o.turns(); turn++ {
		if err := rc.Ctx.Err(); err != nil {
			return finish("Stopped before finishing: "+err.Error(), false, map[string]any{"error": err.Error(), "fatal": true})
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
			return finish(value, true, nil)

		case "help":
			if st.CommandsRun == 0 && !st.Nudged {
				st.Nudged = true
				rc.Logf("operator", "asked for help before trying anything - pushing back once")
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: prematureHelpNudge})
				continue
			}
			if st.LastResult != nil && st.LastResult.ExitCode == 0 && !st.SuccessNudged && !humanOnlyRe.MatchString(value) {
				st.SuccessNudged = true
				rc.Logf("operator", "asked for help right after a command succeeded - showing it the output again")
				out := strings.TrimSpace(st.LastResult.Output)
				if out == "" {
					out = "(no output)"
				}
				st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: fmt.Sprintf(successHelpNudge, truncateText(out, maxCommandOutputChars))})
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
					st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: repeatedCommandNudge(value, *st.LastResult)})
					continue
				}
				// Told once and still repeating. What the command actually
				// printed is the answer - never the model's own trailing
				// DONE, which on "what is the kernel version?" predicted
				// 5.10.124 before running anything; the real answer was 6.6.87.
				rc.Logf("operator", "still repeating the same command - ending with what it printed")
				if st.LastResult.ExitCode == 0 {
					return finish(answerFromCommand(value, *st.LastResult), true, nil)
				}
				return finish(fmt.Sprintf("I kept retrying `%s`, and it keeps failing with exit code %d:\n\n%s",
					value, st.LastResult.ExitCode, truncateText(st.LastResult.Output, 1000)), false,
					map[string]any{"needsUserInput": true, "failureReasoning": "the same command keeps failing"})
			}
			repeats = 0
			invalid = 0
			st.Messages = append(st.Messages, reply)

			if concern := safety.AssessCommand(value); concern != nil && !rc.IsApproved(value) {
				rc.Logf("operator", "approval needed before `%s` - %s", value, concern.Reason)
				st.Pending = value
				res := NeedsApproval(subtask, o.Name(), value, concern.Reason, st)
				res.Meta["commands"] = st.Commands
				return res, nil
			}
			o.execute(rc, st, value)

		default:
			invalid++
			if invalid > maxInvalidReplies {
				// An answer without a marker is still an answer. The loop
				// this replaces argued about format until its budget ran out
				// and then reported a stuck agent - for "how many files are
				// in /etc?", answered correctly ("241") on turn two.
				if text := strings.TrimSpace(resp.Text); text != "" {
					rc.Logf("operator", "never used a marker - taking its reply as the answer")
					return finish(text, true, nil)
				}
				return finish("The model stopped producing usable replies.", false, map[string]any{"needsUserInput": true})
			}
			st.Messages = append(st.Messages, reply, llm.Message{Role: "user", Content: invalidReplyNudge(resp.Text)})
		}
	}

	res, _ := finish(fmt.Sprintf("I ran out of turns (%d) before finishing, after running %d command(s).", o.turns(), st.CommandsRun), false,
		map[string]any{"needsUserInput": true, "failureReasoning": "ran out of turns without finishing - probably repeating an approach that cannot work"})
	res.Checkpoint = st
	return res, nil
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

func repeatedCommandNudge(command string, r shellResult) string {
	out := strings.TrimSpace(r.Output)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf(`You have already run this command, and it finished with exit code %d:

$ %s

%s

Do not run it again. If that answers the task, reply now with DONE: and the answer. Only send a COMMAND if it is a different command you still need.`, r.ExitCode, command, truncateText(out, maxCommandOutputChars))
}

func answerFromCommand(command string, r shellResult) string {
	out := strings.TrimSpace(r.Output)
	if out == "" {
		return fmt.Sprintf("`%s` finished with exit code %d and printed nothing.", command, r.ExitCode)
	}
	return truncateText(out, maxCommandOutputChars)
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated]"
}
