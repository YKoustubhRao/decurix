// The ladder's four prompts: their system texts and how each is worded from its evidence (escalation.py, byte for
// byte).

package escalation

import (
	"strconv"
	"strings"

	"thebot/internal/pyread"
)

// The system texts (escalation.py GROUND_SYSTEM, JUDGE_SYSTEM, DETOUR_SYSTEM, POINT_SYSTEM, byte for byte): fixed, so a
// local model's prompt cache carries them from call to call.
const (
	GroundSystem = "You are the grounding step of a workflow automation that runs a recorded task again on a computer screen. " +
		"One control could not be found the way it was recorded. You are given the candidates that are on the screen " +
		"now, numbered. Choose the one that does what the recorded control did, or say none of them is. " +
		"Choosing wrongly can cost the person real work, so say 0 when you are not sure. " +
		`Reply with exactly one line of JSON and nothing else: {"choice": <number, or 0>, "confidence": <0 to 1>, "reason": "<short>"}`

	JudgeSystem = "You check the work of a workflow automation that runs a recorded task again on a computer screen. " +
		"One step ran, but the result it left when it was recorded did not appear exactly as recorded. " +
		"Say whether the screen shows the step did what it was meant to do. Say null when you cannot tell - " +
		"a wrong yes lets a broken run carry on. " +
		`Reply with exactly one line of JSON and nothing else: {"achieved": true, "false" or null, "confidence": <0 to 1>, "reason": "<short>"}`

	DetourSystem = "You help a workflow automation run a task a person recorded, on a computer screen whose content can differ " +
		"from run to run. The recording reached a step that is not on the screen now. You are given the text the " +
		"recording's screen showed at that step, the text it showed a few steps later, and the text on the screen now. " +
		"Decide whether the steps in between dealt with something that is not here this time - a pop-up, a cookie " +
		"banner, a sign-in or robot check, a notice - so the run should leave them out and carry on from the later step. " +
		"Say false when the screen now is simply somewhere else, and null when you cannot tell. " +
		`Reply with exactly one line of JSON and nothing else: {"skip": true, false or null, "confidence": <0 to 1>, "reason": "<short>"}`

	PointSystem = "You are the grounding step of a workflow automation that runs a task a person recorded, on a screen whose " +
		"content differs from run to run (another search, another customer). When recorded, the person clicked a line " +
		"of text. You are given that line with the lines around it, and the numbered lines of text on the screen now. " +
		"Choose the line that plays the same part in the task now - the same kind of thing in the same role, not the " +
		`same words - or 0 when none does. When the person explained what the step is for ("the first search result"), ` +
		"that is what the line must be. A wrong click can cost the person real work, so say 0 when you are not sure. " +
		`Reply with exactly one line of JSON and nothing else: {"choice": <number, or 0>, "confidence": <0 to 1>, "reason": "<short>"}`
)

// The bounds of what the ladder shows (escalation.py's TYPED_SHOWN, OCR_CHARS and the slices of its prompts).
const (
	TypedShown       = 3
	OCRChars         = 700
	IntentChars      = 400
	ExplanationChars = 600
	LineChars        = 120
)

// GroundEvidence is what the grounding question is shown (escalation.ground_evidence): the run's goal, the step (its
// number and its total as Python writes them, "" for no total), the recorded control's role and name, what the person
// said the step is for, the window's title, the names of the dialogs and alerts in front, and the candidates ranked
// by healing, best first.
type GroundEvidence struct {
	Goal       string      `json:"goal"`
	Step       string      `json:"step"`
	Total      string      `json:"total"`
	Summary    string      `json:"summary"`
	Role       string      `json:"role"`
	Name       string      `json:"name"`
	Intent     string      `json:"intent"`
	Window     string      `json:"window"`
	Front      []string    `json:"front"`
	Candidates []Candidate `json:"candidates"`
}

// Candidate is one control on the screen now: its role and name as Python writes them, and whether it is disabled.
type Candidate struct {
	Role     string `json:"role"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

// JudgeEvidence is what the judging question is shown (escalation.judge_evidence): the goal, the step, the checks it
// was recorded to leave that are not met, in words, and the screen as the agent's tools render it.
type JudgeEvidence struct {
	Goal    string   `json:"goal"`
	Step    string   `json:"step"`
	Summary string   `json:"summary"`
	Missing []string `json:"missing"`
	Screen  string   `json:"screen"`
}

// DetourEvidence is what the detour question is shown (escalation.detour_evidence): the goal, what the recording typed
// and this run typed instead, the step the run is at and the later one the screen looks like, and the lines of text
// on the recording's screen at each and on the screen now (OCR).
type DetourEvidence struct {
	Goal         string      `json:"goal"`
	Typed        [][2]string `json:"typed"`
	Step         string      `json:"step"`
	Summary      string      `json:"summary"`
	LaterStep    string      `json:"later_step"`
	LaterSummary string      `json:"later_summary"`
	Here         []string    `json:"here"`
	There        []string    `json:"there"`
	Now          []string    `json:"now"`
}

// PointEvidence is what the question of where a point click goes is shown (escalation.point_evidence): the goal, what
// was typed, what the person explained of the task and of the step, the lines around the recorded click (the clicked
// one marked) and the lines of text on the screen now.
type PointEvidence struct {
	Goal        string        `json:"goal"`
	Typed       [][2]string   `json:"typed"`
	Explanation string        `json:"explanation"`
	Step        string        `json:"step"`
	Summary     string        `json:"summary"`
	Intent      string        `json:"intent"`
	Context     []ContextLine `json:"context"`
	Lines       []string      `json:"lines"`
}

// ContextLine is a line of text around the recorded click, and whether it is the one clicked.
type ContextLine struct {
	Text    string `json:"text"`
	Clicked bool   `json:"clicked"`
}

// quote is elements._quote: the text on one line, cut at limit characters with an ellipsis, in double quotes (a
// double quote inside becomes a single one).
func quote(text string, limit int) string {
	t := strings.Join(pyread.Split(text), " ")
	if pyread.Len(t) > limit {
		t = pyread.Head(t, limit-1) + "…"
	}
	return `"` + strings.ReplaceAll(t, `"`, "'") + `"`
}

// ocrText is ocr.text: the lines joined, cut at limit characters at a line's end, "..." after a cut.
func ocrText(lines []string, limit int) string {
	out := strings.Join(lines, "\n")
	if pyread.Len(out) <= limit {
		return out
	}
	cut := pyread.Head(out, limit)
	if i := strings.LastIndex(cut, "\n"); i >= 0 {
		cut = cut[:i]
	}
	return cut + "\n..."
}

// task is the prompt's first line, when the run has a goal.
func task(goal string) string {
	if goal == "" {
		return ""
	}
	return "Task: " + goal + "\n"
}

// typing is Ladder._typing: what the recording typed and what this run typed instead, the first TypedShown pairs.
func typing(typed [][2]string) string {
	var b strings.Builder
	for i, pair := range typed {
		if i == TypedShown {
			break
		}
		b.WriteString("The recording typed " + quote(pair[0], 80) + "; this run typed " + quote(pair[1], 80) + ".\n")
	}
	return b.String()
}

// GroundPrompt is the grounding question's prompt (escalation.ground_prompt).
func GroundPrompt(ev GroundEvidence) string {
	var b strings.Builder
	b.WriteString(task(ev.Goal))
	b.WriteString("Step " + ev.Step)
	if ev.Total != "" {
		b.WriteString(" of " + ev.Total)
	}
	b.WriteString(": " + ev.Summary + "\n")
	b.WriteString("Recorded control: " + ev.Role + " " + quote(ev.Name, 80) + "\n")
	if ev.Intent != "" {
		b.WriteString("What the person said this step is for: " +
			pyread.Head(strings.Join(pyread.Split(ev.Intent), " "), IntentChars) + "\n")
	}
	b.WriteString("Window now: " + quote(ev.Window, 80) + "\n")
	if len(ev.Front) > 0 {
		front := make([]string, len(ev.Front))
		for i, name := range ev.Front {
			front[i] = quote(name, 60)
		}
		b.WriteString("In front: " + strings.Join(front, ", ") + "\n")
	}
	lines := make([]string, len(ev.Candidates))
	for i, c := range ev.Candidates {
		lines[i] = strconv.Itoa(i+1) + ". " + c.Role + " " + quote(c.Name, 80)
		if c.Disabled {
			lines[i] += " [disabled]"
		}
	}
	b.WriteString("Candidates:\n" + strings.Join(lines, "\n"))
	return b.String()
}

// JudgePrompt is the judging question's prompt (escalation.judge_prompt).
func JudgePrompt(ev JudgeEvidence) string {
	return task(ev.Goal) +
		"Step " + ev.Step + ": " + ev.Summary + "\n" +
		"It was recorded to leave: " + strings.Join(ev.Missing, "; ") + "\n" +
		"The screen now:\n" + ev.Screen
}

// DetourPrompt is the detour question's prompt (escalation.detour_prompt).
func DetourPrompt(ev DetourEvidence) string {
	return task(ev.Goal) + typing(ev.Typed) +
		"The run is at step " + ev.Step + ": " + ev.Summary + "\n" +
		"The recording's screen at step " + ev.Step + ":\n" + ocrText(ev.Here, OCRChars) + "\n\n" +
		"The recording's screen at step " + ev.LaterStep + " (" + ev.LaterSummary + "):\n" + ocrText(ev.There, OCRChars) + "\n\n" +
		"The screen now:\n" + ocrText(ev.Now, OCRChars)
}

// PointPrompt is the prompt of the question where a point click goes (escalation.point_prompt).
func PointPrompt(ev PointEvidence) string {
	var b strings.Builder
	b.WriteString(task(ev.Goal) + typing(ev.Typed))
	if ev.Explanation != "" {
		b.WriteString("The person explained the task: " + pyread.Head(ev.Explanation, ExplanationChars) + "\n")
	}
	b.WriteString("Step " + ev.Step + ": " + ev.Summary + "\n")
	if ev.Intent != "" {
		b.WriteString("What the person said this step is for: " + ev.Intent + "\n")
	}
	context := make([]string, len(ev.Context))
	for i, l := range ev.Context {
		mark := "  "
		if l.Clicked {
			mark = "> "
		}
		context[i] = mark + pyread.Head(l.Text, LineChars)
	}
	b.WriteString("When recorded, the person clicked the line marked >:\n" + strings.Join(context, "\n") + "\n\n")
	lines := make([]string, len(ev.Lines))
	for i, l := range ev.Lines {
		lines[i] = strconv.Itoa(i+1) + ". " + pyread.Head(l, LineChars)
	}
	b.WriteString("Lines on the screen now:\n" + strings.Join(lines, "\n"))
	return b.String()
}
