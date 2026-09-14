package tasks

import (
	"regexp"
	"strings"
)

// Task types. A type is decided from the request's own words, deterministically
// and without a model call, so the same request always lands in the same bucket
// and a dashboard row means the same thing next week. The one thing that can
// change it later is the agent working the screen (see Store.AddStep).
const (
	TypeConsequential = "consequential"
	TypeScreen        = "screen"
	TypeWeb           = "web"
	TypeCode          = "code"
	TypeFiles         = "files"
	TypeSystem        = "system"
	TypeQuestion      = "question"
	TypeOther         = "other"
)

var typeLabels = map[string]string{
	TypeConsequential: "Payments & messages",
	TypeScreen:        "On-screen work",
	TypeWeb:           "Web research",
	TypeCode:          "Code",
	TypeFiles:         "Files & documents",
	TypeSystem:        "System & shell",
	TypeQuestion:      "Questions & chat",
	TypeOther:         "Other",
}

// TypeLabel is the human-readable name of a task type.
func TypeLabel(taskType string) string {
	if label, ok := typeLabels[taskType]; ok {
		return label
	}

	return taskType
}

// typeRules are tried in order; the first match wins. Consequential work comes
// first because it is the type whose interventions are expected - an approval
// before a payment is the agent doing its job - and it must not be hidden
// inside "on-screen work" when the payment happens to be a button.
var typeRules = []struct {
	Type    string
	Pattern *regexp.Regexp
}{
	{TypeConsequential, regexp.MustCompile(`\b(pay|pays|paying|payment|payments|invoice|invoices|purchase|buy|checkout|refund|wire|e-?mail|emails|whatsapp|sms|newsletter|publish|tweet)\b|\bsend (a |an |the |my )?(message|mail|reply|text|note)\b|\btransfer (money|funds|\$)`)},
	{TypeScreen, regexp.MustCompile(`\b(click|button|buttons|window|screen|browser|chromium|chrome|firefox|libreoffice|writer|calc|impress|spreadsheet|form|gui|mousepad|thunar|file manager|admin panel|dialog|menu|text editor|checkbox|dropdown)\b`)},
	{TypeWeb, regexp.MustCompile(`https?://|\bwww\.|\b(website|web page|webpage|search the web|search online|google|look up|internet|online|latest version|news|weather|download)\b`)},
	{TypeCode, regexp.MustCompile(`\b(code|script|function|program|python|javascript|typescript|golang|rust|compile|refactor|unit tests?|bug|regex)\b`)},
	{TypeFiles, regexp.MustCompile(`\b(file|files|folder|folders|directory|directories|rename|zip|unzip|archive|document|documents)\b|\.(txt|csv|md|json|pdf|docx|xlsx|odt|ods)\b|/home/|~/|/etc\b|/work\b`)},
	{TypeSystem, regexp.MustCompile(`\b(install|uninstall|kernel|disk|disks|ram|cpu|process|processes|service|services|package|packages|uptime|hostname|ip address|network|port|apt|pip|npm|uname|df|systemctl|environment variable|os version)\b`)},
}

var questionStart = regexp.MustCompile(`^(what|whats|what's|how|who|why|when|where|which|is|are|can|could|do|does|did|hello|hi|hey|thanks|thank you|ping)\b`)

// Classify returns the task type for a request.
func Classify(request string) string {
	text := strings.ToLower(strings.TrimSpace(request))

	for _, rule := range typeRules {
		if rule.Pattern.MatchString(text) {
			return rule.Type
		}
	}

	if strings.HasSuffix(text, "?") || questionStart.MatchString(text) {
		return TypeQuestion
	}

	return TypeOther
}

var statusLabels = map[string]string{
	StatusQueued:    "Queued",
	StatusRunning:   "Running",
	StatusWaiting:   "Waiting on you",
	StatusDone:      "Done",
	StatusError:     "Failed",
	StatusHelp:      "Ended waiting",
	StatusAbandoned: "Stopped",
}

// StatusLabel is the human-readable name of a task or execution status.
func StatusLabel(status string) string {
	if label, ok := statusLabels[status]; ok {
		return label
	}

	return status
}

// Reasons a human had to step in. An intervention is stored with its reason
// key; errors, stops and wrong verdicts are counted under their own.
const (
	ReasonApprovalPayment = "approval_payment"
	ReasonApprovalMessage = "approval_message"
	ReasonApprovalOther   = "approval_other"
	ReasonBackend         = "backend"
	ReasonCredentials     = "credentials"
	ReasonPermission      = "permission"
	ReasonMissingTool     = "missing_tool"
	ReasonScreen          = "screen"
	ReasonLoop            = "loop"
	ReasonClarification   = "clarification"
	ReasonOtherHelp       = "other_help"
	ReasonFailed          = "failed"
	ReasonStopped         = "stopped"
	ReasonWrong           = "wrong"
)

var reasonLabels = map[string]string{
	ReasonApprovalPayment: "Approval before a payment or purchase",
	ReasonApprovalMessage: "Approval before sending a message",
	ReasonApprovalOther:   "Approval before another consequential action",
	ReasonBackend:         "The model backend failed or timed out",
	ReasonCredentials:     "Needed a login or credentials",
	ReasonPermission:      "Lacked permission",
	ReasonMissingTool:     "A tool or app was missing",
	ReasonScreen:          "Could not find it on screen",
	ReasonLoop:            "Got stuck repeating itself",
	ReasonClarification:   "Asked what was meant",
	ReasonOtherHelp:       "Asked for help (other)",
	ReasonFailed:          "Gave up with an error",
	ReasonStopped:         "Desktop stopped mid-task",
	ReasonWrong:           "Answer marked wrong",
}

// ReasonLabel is the human-readable name of a reason key.
func ReasonLabel(reason string) string {
	if label, ok := reasonLabels[reason]; ok {
		return label
	}

	return reason
}

// IsApprovalReason reports whether a reason is an approval the agent was
// supposed to ask for, as opposed to it being stuck.
func IsApprovalReason(reason string) bool {
	return strings.HasPrefix(reason, "approval_")
}

var (
	backendRe = regexp.MustCompile(`deadline exceeded|did not finish answering in time|timed out|could not be reached|connection refused|could not reach (ollama|the model)|does not have the model|model "[^"]*" not found|harness failed to run|reasoning model failed`)

	approvalIntentRe = regexp.MustCompile(`approv|needs_approval|confirm|permission to|ok to|okay to|shall i|should i go ahead|before i (send|pay|buy|submit|publish|click)`)
	paymentRe        = regexp.MustCompile(`\b(pay|pays|paying|payment|invoice|purchase|buy|checkout|refund|transfer|charge|order)\b|\$\d`)
	messageRe        = regexp.MustCompile(`\b(send|sending|sent|email|e-mail|message|messages|messaging|whatsapp|sms|reply|newsletter|publish|post)\b`)

	credentialsRe = regexp.MustCompile(`password|credential|\bsecrets?\b|\blog ?in\b|\bsign ?in\b|api key|\btoken\b|2fa|two-factor|authenticat|username|payment method|card details`)
	permissionRe  = regexp.MustCompile(`permission denied|not permitted|access denied|\bsudo\b|root access|requires root|forbidden`)
	missingToolRe = regexp.MustCompile(`command not found|not installed|isn't installed|no such (command|program)|is not available|unavailable`)
	screenRe      = regexp.MustCompile(`nothing on the screen matches|could not find (the |a |any )?(button|element|window|field|link|control)|not visible|no window`)
	loopRe        = regexp.MustCompile(`stuck in a loop|kept retrying|full budget of|step limit|maximum (number of )?steps|stopped producing usable|repeating`)
	clarifyRe     = regexp.MustCompile(`\bwhich\b|clarif|did you mean|please specify|could you specify|more details|more information|what do you want|unclear|\?\s*$`)
)

// approvalBoilerplate is the approval gate's fixed wording around every
// request (computer/approval.py's request_message). "sent" and "reply" in it
// say nothing about the action, and would file every on-screen approval under
// sending a message.
var approvalBoilerplate = regexp.MustCompile(`nothing was clicked or sent\.?|reply yes to let me do it[^.]*\.?`)

// InterventionReason decides why an agent asked a human: kind is what the
// agent reported ("approval" or "help"), text is its own words.
func InterventionReason(kind, text string) string {
	lowered := approvalBoilerplate.ReplaceAllString(strings.ToLower(text), "")

	if backendRe.MatchString(lowered) {
		return ReasonBackend
	}

	// The Ollama loop's HELP doubles as its safety brake, so an approval can
	// arrive reported as plain help; the words still say what it is.
	if kind == InterventionApproval || approvalIntentRe.MatchString(lowered) && (paymentRe.MatchString(lowered) || messageRe.MatchString(lowered)) {
		switch {
		case paymentRe.MatchString(lowered):
			return ReasonApprovalPayment
		case messageRe.MatchString(lowered):
			return ReasonApprovalMessage
		default:
			return ReasonApprovalOther
		}
	}

	// The screen first: the action layer's "nothing on the screen matches
	// button "Sign in"" names the control it looked for, and that control's
	// label must not be read as the agent wanting a login.
	switch {
	case screenRe.MatchString(lowered):
		return ReasonScreen
	case credentialsRe.MatchString(lowered):
		return ReasonCredentials
	case permissionRe.MatchString(lowered):
		return ReasonPermission
	case missingToolRe.MatchString(lowered):
		return ReasonMissingTool
	case loopRe.MatchString(lowered):
		return ReasonLoop
	case clarifyRe.MatchString(lowered):
		return ReasonClarification
	}

	return ReasonOtherHelp
}

// ErrorReason decides why a task ended in an error.
func ErrorReason(text string) string {
	if backendRe.MatchString(strings.ToLower(text)) {
		return ReasonBackend
	}

	return ReasonFailed
}
