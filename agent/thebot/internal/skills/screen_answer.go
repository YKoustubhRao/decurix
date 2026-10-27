package skills

import (
	"regexp"
	"strings"
)

// A question for the person that the screen answers. Found live (2026-10-26, and 46% of ~1,150 questions over four
// days were of the harness's or the worker's own making): a worker paged the person for names that stood on the page
// it had just listed. A question quoting something the newest listing of a window shows is sent back once with the
// lines that show it; the next question goes through.

// quotedRe is a run of words in quotes inside a question.
var quotedPhraseRe = regexp.MustCompile(`["“”']([^"“”']{3,80})["“”']`)

// onScreenNudge is what the model is told instead of the person.
const onScreenNudge = `Not sent: what you ask about is on the screen you listed. These lines show it:
%s
Read them and go on. If after reading them the question still stands - it needs a choice only the person can make, or a value nothing here has - ask again and it goes through.`

// maxOnScreenLines bounds the lines quoted back.
const maxOnScreenLines = 6

// answeredOnScreen is the lines of the newest listings that hold what the question quotes (a phrase in quotes, or a
// code of letters and digits), joined; "" when nothing it quotes stands on a listed screen.
func answeredOnScreen(question string, listings []string) string {
	if len(listings) == 0 {
		return ""
	}
	var wanted []string
	for _, m := range quotedPhraseRe.FindAllStringSubmatch(question, -1) {
		if w := strings.TrimSpace(m[1]); strings.Contains(w, " ") || isCodeWord(w) {
			wanted = append(wanted, w)
		}
	}
	for _, w := range alnumWordRe.FindAllString(question, -1) {
		if isCodeWord(w) {
			wanted = append(wanted, w)
		}
	}
	if len(wanted) == 0 {
		return ""
	}
	var found []string
	seen := map[string]bool{}
	for i := len(listings) - 1; i >= 0 && len(found) < maxOnScreenLines; i-- {
		for _, line := range strings.Split(listings[i], "\n") {
			low := plainWords(line)
			for _, w := range wanted {
				if q := plainWords(w); q != "" && strings.Contains(" "+low+" ", " "+q+" ") && !seen[line] {
					seen[line] = true
					found = append(found, strings.TrimSpace(line))
					break
				}
			}
			if len(found) >= maxOnScreenLines {
				break
			}
		}
	}
	// One line that only echoes the question's own words (a search box holding the value typed) answers nothing.
	if len(found) == 0 {
		return ""
	}
	return strings.Join(found, "\n")
}

// alnumWordRe is a word of letters and digits, ten to twenty characters.
var alnumWordRe = regexp.MustCompile(`\b[A-Za-z0-9]{10,20}\b`)

// isCodeWord is whether a word is a code: letters and digits together, ten to twenty characters (a registration or
// account number).
func isCodeWord(w string) bool {
	if !alnumWordRe.MatchString(w) || len(w) < 10 || len(w) > 20 {
		return false
	}
	return strings.ContainsAny(w, "0123456789") && strings.ContainsAny(strings.ToLower(w), "abcdefghijklmnopqrstuvwxyz")
}

// asksAppNamesRe is a question for the exact names, labels or options of something the application itself lists (its
// accounts, ledgers, categories, codes, settings). Found live (2026-10-26): "please confirm the exact ledger names to
// use for 5% sales and tax in this client's books" - names that stood on the chart of accounts, one click away.
var asksAppNamesRe = regexp.MustCompile(`(?i)\b(exact|correct|precise|right|proper)\b[^.?]{0,60}\b(names?|spellings?|labels?|options?|codes?)\b|\bconfirm (the |which |what )?[^.?]{0,40}\b(names?|labels?|options?|ledgers?|accounts?|categor(y|ies))\b`)

// appNamesNudge is what the model is told instead of the person.
const appNamesNudge = `Not sent: a name, label or option the application already holds is read from the application, never asked. Open the page that lists them (its master list, chart, settings or the field's own drop-down), read the entries, and take the one the documents or a rule decide. Ask only if none of its entries fits or several do - then say which entries you saw.`

// asksToContinueRe is a HELP that asks to be allowed to go on, to run commands, or to have the task sent again.
var asksToContinueRe = regexp.MustCompile(`(?i)\b(let me (continue|carry on|proceed|go on)|resend the task|command mode|permission to (run|continue|proceed)|i can only get (them|it|that) by running|allow me to (run|continue)|need (one|a|another) (more |new )?(command|turn|command turn)|before i can (truthfully |honestly )?report|one (more|new) (command )?turn)\b`)

// continueNudge is what the model is told instead of the person.
const continueNudge = `Not sent: nothing stops you. You can run any command now - send it with COMMAND: and go on with the task; the person's last answer stands. Ask the person only for a choice or a value that nothing here has.`
