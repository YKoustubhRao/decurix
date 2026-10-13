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

// atCommand names the rules about which program runs. They are matched
// against the command with its quoted arguments blanked out: in
// `grep -E 'outbox|mail'` the bar is part of a pattern, not a pipe into mail.
var atCommand = map[string]bool{"it sends email": true, "it shuts down or restarts the machine": true}

// commandRules are checked against a shell command about to run.
var commandRules = []rule{
	{regexp.MustCompile(`(?i)(^|[;&|]\s*|\bsudo\s+|['"]\s*)(sendmail|mailx?|mutt|ssmtp|msmtp|swaks)\b`),
		"it sends email"},
	{regexp.MustCompile(`(?i)\b(twilio|whatsapp|telegram-send|slack-cli|signal-cli)\b`),
		"it sends a message on someone's behalf"},
	{regexp.MustCompile(`(?i)\b(stripe|paypal|braintree|razorpay)\b[\s\S]*\b(charge|payment|payout|transfer|refund|invoice)`),
		"it moves money"},
	{regexp.MustCompile(`(?i)\bgit\s+push\b`),
		"it publishes commits to a remote repository"},
	{regexp.MustCompile(`(?i)\b(npm|yarn|pnpm|cargo|twine|gem)\s+publish\b|\bdocker\s+push\b`),
		"it publishes a package or image"},
	{regexp.MustCompile(`(?i)\bgh\s+(pr\s+(create|merge|close)|issue\s+(create|close|comment)|release\s+create|repo\s+(delete|create))\b`),
		"it acts on GitHub as the user"},
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*\s+)*-[a-z]*r[a-z]*\s+(-[a-z]*\s+)*("|')?(/|~/?|\$HOME/?|/home/?|/home/[^/\s]+/?)("|')?(\s|$)`),
		"it recursively deletes a home or root directory"},
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b|\bdd\b[^|;&]*\bof=/dev/|\bwipefs\b|\bshred\b`),
		"it can destroy a disk or file permanently"},
	{regexp.MustCompile(`(?i)(^|[;&|]\s*|\bsudo\s+|['"]\s*)(shutdown|reboot|poweroff|halt)\b|\binit\s+[06]\b`),
		"it shuts down or restarts the machine"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`),
		"it is a fork bomb"},
	{regexp.MustCompile(`(?i)\bchmod\s+(-[a-z]*\s+)*-[a-z]*R[a-z]*\s+[0-7]*7[0-7]*7\s+/(\s|$)|\bchown\s+(-[a-z]*\s+)*-[a-z]*R[a-z]*\s+\S+\s+/(\s|$)`),
		"it changes permissions on the whole filesystem"},
	{regexp.MustCompile(`(?i)\bcurl\b[^|;&]*(\s-X\s*(POST|PUT|PATCH|DELETE)\b|\s(-d|--data(-\w+)?|-F|--form|-T|--upload-file)\s)`),
		"it sends data to a web service"},
	{regexp.MustCompile(`(?i)\bwget\b[^|;&]*\s--(post-data|post-file|method=(POST|PUT|PATCH|DELETE))`),
		"it sends data to a web service"},
}

// localTargetRe recognises a request aimed at this machine itself. Sending
// data to a server the agent is running locally (testing an API it just
// built) is ordinary development, not a real-world consequence.
var localTargetRe = regexp.MustCompile(`(?i)(https?://)?(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(:\d+)?(/\S*)?`)

// AssessCommand reports whether a shell command needs approval before it
// runs, or nil if it does not.
func AssessCommand(command string) *Concern {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	unquoted := blankQuoted(command)
	for _, r := range commandRules {
		target := command
		if atCommand[r.reason] {
			target = unquoted
		}
		if !r.re.MatchString(target) {
			continue
		}
		if r.reason == "it sends data to a web service" && localTargetRe.MatchString(command) && !externalURLRe.MatchString(stripLocalURLs(command)) {
			continue
		}
		return &Concern{Reason: r.reason}
	}
	return nil
}

var externalURLRe = regexp.MustCompile(`(?i)https?://`)

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
		if runsQuotedRe.MatchString(command[:m[0]]) {
			b.WriteString(command[m[0]:m[1]])
		} else {
			b.WriteString("''")
		}
		last = m[1]
	}
	b.WriteString(command[last:])
	return b.String()
}

func stripLocalURLs(s string) string { return localTargetRe.ReplaceAllString(s, " ") }

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
)

// IsApproval reports whether a human's reply to an approval request grants
// it. A reply that says yes and no at once ("ok but don't push yet") is not
// an approval - the gate stays shut and the reply is passed on as input.
func IsApproval(reply string) bool {
	reply = strings.TrimSpace(reply)
	if reply == "" || !approvalRe.MatchString(reply) {
		return false
	}
	return !refusalRe.MatchString(reply)
}
