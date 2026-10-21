package atomizer

import (
	"regexp"
	"strings"
)

// workInAppRe is a request naming an app the work is done in: "in Excel", "use PowerPoint", "built in Word", "open
// it in Photoshop". The preposition is kept: "in"/"into"/"on" may only be naming a file's format (WorkApps).
var workInAppRe = regexp.MustCompile(`(?i)\b(in|use|using|with|on|into)\s+(?:microsoft\s+|libreoffice\s+|adobe\s+)?(excel|calc|powerpoint|impress|word|writer|photoshop|gimp|illustrator|inkscape)\b`)

// AppFileKinds is the kind of file each app WorkApps names makes, as a regexp alternation of extensions.
var AppFileKinds = map[string]string{
	"excel": `xlsx|xlsm|xls|ods|csv`, "calc": `xlsx|xlsm|xls|ods|csv`,
	"powerpoint": `pptx|ppt|odp`, "impress": `pptx|ppt|odp`,
	"word": `docx|doc|odt|pdf|rtf`, "writer": `docx|doc|odt|pdf|rtf`,
	"photoshop": `jpe?g|png|gif|webp|tiff?|bmp|xcf`, "gimp": `jpe?g|png|gif|webp|tiff?|bmp|xcf`,
	"illustrator": `svg|pdf|png`, "inkscape": `svg|pdf|png`,
}

// formatWordRe follows an app named as a format: "in Excel format", "in Word file".
var formatWordRe = regexp.MustCompile(`(?i)^\s*(?:format|file|files|workbook|spreadsheet|document|deck)\b`)

// optionBeforeRe ends the text before an app's name when the app is only offered as a choice: "if you use Word there",
// "you can use Excel", "or with Calc if you prefer". Found live (2026-10-20, round 5 P4): "(or in Documents\\...\\P4 on
// my PC if you use Word there)" was taken as "work in Word", and office update - the right tool for filling the
// letters - was refused; the worker clicked Find & Replace for twenty minutes and then asked to be allowed.
var optionBeforeRe = regexp.MustCompile(`(?i)(?:\bif\s+(?:you|we|they)\b[^.;:\n]{0,30}|\b(?:you|we)\s+(?:can|could|may|might)\s*|\boptionally\s*|\bor\s*)$`)

// optionAfterRe follows an app offered as a choice: "Word if you like", "Excel or anything else".
var optionAfterRe = regexp.MustCompile(`(?i)^\s*(?:if\s+you\s+(?:like|prefer|want|wish)|or\s+(?:any|another|whatever|something))\b`)

// fileNameAfterRe is a file name set right after the app, as an apposition: "in Excel, orders.xlsx",
// "in Excel (orders.xlsx)", "in Excel: orders.xlsx". The %s is the app's file kinds.
const fileNameAfterRe = `(?i)^\s*[,:(\x{2013}\x{2014}-]\s*["'\x{201C}]?[\w-][\w.-]*\.(?:%s)\b`

// WorkApps is the apps a request says the work is to be done in, lower case, in the order named. An app given only
// as the format of the file asked for is not one: "make his order sheet in Excel, wa_orders.xlsx in my work folder"
// asks for a workbook, and found live (2026-10-19) it was taken as "work in the app" - the operator was sent back
// from writing the file, typed the sheet into a spreadsheet cell by cell and ran out of time with nothing saved.
// "Use Excel to ...", "in Excel (the app on your desktop)", "open it in Excel" and "built in Impress:" still name
// where to work. An app offered as a choice ("if you use Word there") is not one either.
func WorkApps(prompt string) []string {
	var out []string
	for _, loc := range workInAppRe.FindAllStringSubmatchIndex(prompt, -1) {
		prep := strings.ToLower(prompt[loc[2]:loc[3]])
		app := strings.ToLower(prompt[loc[4]:loc[5]])
		after := prompt[loc[1]:]
		if prep == "in" || prep == "into" || prep == "on" {
			if formatWordRe.MatchString(after) ||
				regexp.MustCompile(strings.Replace(fileNameAfterRe, "%s", AppFileKinds[app], 1)).MatchString(after) {
				continue
			}
		}
		// An app offered as a choice ("if you use Word there") is not where the work must be done.
		if optionBeforeRe.MatchString(prompt[:loc[0]]) || optionAfterRe.MatchString(after) {
			continue
		}
		out = append(out, app)
	}
	return out
}

// stageAppRe and stageHandsInRe are the machine lines of a planned stage's prompt (long_task.stage_prompt): the app its
// own output is to be made in, and what it hands in.
var (
	stageAppRe     = regexp.MustCompile(`(?m)^Work in app: ([A-Za-z]+)\s*$`)
	stageHandsInRe = regexp.MustCompile(`(?m)^Hands in: .*$`)
)

// AppsFor is the apps a task is held to: for a stage of a planned run, only the app its prompt names for the stage's
// own output (a named app binds to its deliverable - "a deck in PowerPoint" is the .pptx's, never the workbook
// stage's), none when it names none; for any other task, WorkApps. Found live (round 5 P1, 2026-10-20): the workbook
// stage was sent back to open, check and save month_end_close_Oct2026.xlsx in PowerPoint, which cannot open it.
func AppsFor(prompt string) []string {
	if m := stageAppRe.FindStringSubmatch(prompt); m != nil {
		return []string{strings.ToLower(m[1])}
	}
	if stageHandsInRe.MatchString(prompt) {
		return nil
	}
	return WorkApps(prompt)
}
