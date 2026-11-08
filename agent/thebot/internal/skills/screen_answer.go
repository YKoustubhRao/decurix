package skills

import (
	"regexp"
	"strings"
	"unicode"
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

// A question for names the application lists, answered by a list on the screen. Found live (2026-10-26): "please
// confirm the exact ledger names to use for 5% sales and tax in this client's books" - names that stood on the
// application's own list. The gate rests on a fact of the listing, never on the question's wording (N-W3-3: a regex of
// "exact/correct ... names/labels/options" decided it until 2026-11-08): the question names a list a listed window
// shows - a field's options (a combobox, list, menu or tree and the entries under it) or a column of a table (a cell of
// its first row, with the rows under it) - whose entries are names (they hold letters, not only figures or dates).
// Such a HELP is sent back once with the list's lines; the next question goes through. A question naming no list on a
// listed screen, or with no screen listed, goes to the person.

// listRoles are the roles of an element whose entries follow it in a listing.
var listRoles = map[string]bool{"combobox": true, "listbox": true, "list": true, "menu": true, "menubar": true, "tree": true, "tablist": true, "radiogroup": true}

// entryRoles are the roles of an entry of a list.
var entryRoles = map[string]bool{"option": true, "listitem": true, "menuitem": true, "menuitemradio": true, "menuitemcheckbox": true, "treeitem": true, "tab": true, "radio": true}

// maxLabelWords bounds a list's label: a column's or field's name is short; a longer first-row cell is data.
const maxLabelWords = 4

// listedEntry is one element line of a listing: its role, its quoted text and the line as listed.
type listedEntry struct{ role, name, line string }

// listedEntries is the element lines of a listing, in order (a line with no element is skipped).
func listedEntries(listing string) []listedEntry {
	var out []listedEntry
	for _, line := range strings.Split(listing, "\n") {
		if m := screenElementRe.FindStringSubmatch(line); m != nil {
			out = append(out, listedEntry{strings.ToLower(m[1]), m[2], strings.TrimSpace(line)})
		}
	}
	return out
}

// stemWord is a word with a plural ending taken off (accounts, categories), so a label and a question that names it
// in the plural meet.
func stemWord(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// stemmedWords is a text's plain words, each stemmed.
func stemmedWords(text string) []string {
	words := strings.Fields(plainWords(text))
	for i, w := range words {
		words[i] = stemWord(w)
	}
	return words
}

// namesLabel reports whether the question's words hold a list's label as a run of words: a short name (at most
// maxLabelWords words, a word of three letters or more, no figure), stemmed.
func namesLabel(question []string, label string) bool {
	if strings.ContainsAny(label, "0123456789") {
		return false
	}
	want := stemmedWords(label)
	if len(want) == 0 || len(want) > maxLabelWords {
		return false
	}
	long := false
	for _, w := range want {
		long = long || len(w) >= 3
	}
	if !long {
		return false
	}
	for i := 0; i+len(want) <= len(question); i++ {
		match := true
		for j, w := range want {
			if question[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// hasLetter reports whether a text holds a letter (an entry that is a name, not only a figure or a date).
func hasLetter(s string) bool {
	return strings.IndexFunc(s, unicode.IsLetter) >= 0
}

// rowCells is a row's text split into its cells.
func rowCells(name string) []string {
	cells := strings.Split(name, " | ")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// listOnScreen is the lines of a list the newest listings show that the question names - the list's own line and its
// entries, at most maxOnScreenLines - when its entries are names; "" when the question names no such list.
func listOnScreen(question string, listings []string) string {
	words := stemmedWords(question)
	if len(words) == 0 {
		return ""
	}
	for i := len(listings) - 1; i >= 0; i-- {
		entries := listedEntries(listings[i])
		for k := 0; k < len(entries); k++ {
			e := entries[k]
			switch {
			case listRoles[e.role] && namesLabel(words, e.name):
				lines, named := []string{e.line}, false
				for _, x := range entries[k+1:] {
					if !entryRoles[x.role] || len(lines) >= maxOnScreenLines {
						break
					}
					lines = append(lines, x.line)
					named = named || hasLetter(x.name)
				}
				if named {
					return strings.Join(lines, "\n")
				}
			case e.role == "row" && (k == 0 || entries[k-1].role != "row"):
				// The first row of a table names its columns.
				for col, cell := range rowCells(e.name) {
					if !namesLabel(words, cell) {
						continue
					}
					lines, named := []string{e.line}, false
					for _, x := range entries[k+1:] {
						if x.role != "row" || len(lines) >= maxOnScreenLines {
							break
						}
						lines = append(lines, x.line)
						if cells := rowCells(x.name); col < len(cells) {
							named = named || hasLetter(cells[col])
						}
					}
					if named {
						return strings.Join(lines, "\n")
					}
				}
			}
		}
	}
	return ""
}

// listOnScreenNudge is what the model is told instead of the person.
const listOnScreenNudge = `Not sent: what you ask for is a list the screen you listed shows - a name, label or option the application holds is read from the application, never asked. These lines show it:
%s
Read its entries and take the one the documents or a rule decide. Ask again only if none of them fits or several do - then say which entries you saw - and it goes through.`

// asksToContinueRe is a HELP that asks to be allowed to go on, to run commands, or to have the task sent again.
var asksToContinueRe = regexp.MustCompile(`(?i)\b(let me (continue|carry on|proceed|go on)|resend the task|command mode|permission to (run|continue|proceed)|i can only get (them|it|that) by running|allow me to (run|continue)|need (one|a|another) (more |new )?(command|turn|command turn)|before i can (truthfully |honestly )?report|one (more|new) (command )?turn)\b`)

// continueNudge is what the model is told instead of the person.
const continueNudge = `Not sent: nothing stops you. You can run any command now - send it with COMMAND: and go on with the task; the person's last answer stands. Ask the person only for a choice or a value that nothing here has.`
