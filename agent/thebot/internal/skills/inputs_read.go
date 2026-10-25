package skills

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// ============================================================
// What the work gathered and what the screens offered: facts for the reviewer
//
// Which documents the goal's own commands saved or copied, and which document names its screens showed (a chat's
// attachments, a list of files). Facts only: whether one that was offered and never taken mattered is the reviewer's
// to weigh (reviewer.go) - found live (2026-10-22), a photo invoice saved and never read, and an invoice
// a chat offered and never saved, each reported as done.
// ============================================================

var (
	// fetchFlagRe is curl's or wget's output file: -o FILE, -O FILE, -fsSLo FILE, --output FILE, --output-document=FILE.
	fetchFlagRe = regexp.MustCompile(`(?:\s-[A-Za-z]*[oO]|\s--output(?:-document)?(?:=|\s))\s*('[^']+'|"[^"]+"|[^\s;&|]+)`)
	// docFileRe is a file name with a document's or picture's extension.
	docFileRe = regexp.MustCompile(`(?i)\.(?:pdf|jpe?g|png|tiff?|webp|heic|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml)$`)
)

// unquoteWord strips one layer of shell quotes from a word.
func unquoteWord(w string) string {
	if len(w) >= 2 && (w[0] == '\'' || w[0] == '"') && w[len(w)-1] == w[0] {
		return w[1 : len(w)-1]
	}
	return w
}

// fetchedDocuments are the document files a command line saves from elsewhere: curl's / wget's output files, and the
// targets of cp and mv (a folder target takes the source's name). Base names.
func fetchedDocuments(command string) []string {
	var out []string
	add := func(name string) {
		name = path.Base(strings.ReplaceAll(unquoteWord(strings.TrimSpace(name)), `\`, "/"))
		if docFileRe.MatchString(name) && !strings.ContainsAny(name, "*?$") {
			out = append(out, name)
		}
	}
	for _, seg := range shellSegments(command) {
		words := shellWords(seg.Text)
		if len(words) == 0 {
			continue
		}
		switch path.Base(words[0]) {
		case "curl", "wget":
			for _, m := range fetchFlagRe.FindAllStringSubmatch(" "+seg.Text, -1) {
				add(m[1])
			}
		case "cp", "mv":
			var args []string
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "-") {
					args = append(args, w)
				}
			}
			if len(args) < 2 {
				continue
			}
			target := args[len(args)-1]
			if docFileRe.MatchString(target) {
				add(target)
				continue
			}
			for _, src := range args[:len(args)-1] { // into a folder: each source keeps its name
				add(src)
			}
		}
	}
	return out
}

// screenDocRe is a document's file name as a screen lists it (an attachment in a chat, a file in a list).
var screenDocRe = regexp.MustCompile(`(?i)[\w][\w.()-]*\.(?:pdf|jpe?g|png|tiff?|webp|heic|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml)\b`)

// taughtRe is a command that prints what the person taught: a taught task's description (gui skill NAME) or an app's
// guide (gui guide NAME).
var taughtRe = regexp.MustCompile(`^\s*gui\s+(?:skill|guide)\s+\S`)

const (
	maxTaught      = 4
	maxTaughtChars = 8000
)

// noteTaught keeps, whole, what a command that prints the person's teaching showed: the reviewer weighs the work
// against it, and the ledger keeps only each output's first words. Found live (2026-10-23): the taught
// task said one line per kind of entry; the review, seeing 220 characters of it, passed work saved with one line for
// all.
func (st *operatorState) noteTaught(command string, res shellResult) {
	if res.ExitCode != 0 || !taughtRe.MatchString(command) {
		return
	}
	entry := "$ " + strings.TrimSpace(command) + "\n" + truncateText(strings.TrimSpace(res.Output), maxTaughtChars)
	for i, t := range st.Taught {
		if strings.SplitN(t, "\n", 2)[0] == strings.SplitN(entry, "\n", 2)[0] {
			st.Taught[i] = entry // read again: the latest reading
			return
		}
	}
	st.Taught = append(st.Taught, entry)
	if len(st.Taught) > maxTaught {
		st.Taught = st.Taught[len(st.Taught)-maxTaught:]
	}
}

// taughtRunRe is a taught task run by name: gui do NAME.
var taughtRunRe = regexp.MustCompile(`(?m)^\s*gui\s+do\s+([A-Za-z0-9][A-Za-z0-9_.-]*)`)

// taughtRunName is the taught task a command runs (gui do NAME), or "".
func taughtRunName(command string) string {
	if m := taughtRunRe.FindStringSubmatch(command); m != nil {
		return m[1]
	}
	return ""
}

// hasTaught reports whether the teaching of the taught task name is already kept.
func (st *operatorState) hasTaught(name string) bool {
	for _, t := range st.Taught {
		if strings.HasPrefix(t, "$ gui skill "+name+"\n") || strings.HasPrefix(t, "$ gui skill '"+name+"'\n") {
			return true
		}
	}
	return false
}

// maxScreenDocs bounds the names kept.
const maxScreenDocs = 120

// noteScreenDocs keeps the documents a screen command's output offers: each shown as an element of its own (an
// attachment, a "Download X" link, a file in a list), never a name mentioned inside a longer text. Found live
// (2026-10-25): a chat list's link to another client's conversation previewed its last message - that client's
// invoice - and every report of the run was told to enter it or say why not.
func (st *operatorState) noteScreenDocs(command, output string) {
	if !strings.HasPrefix(strings.TrimSpace(command), "gui ") {
		return
	}
	keep := func(name string) {
		if !contains(st.ScreenDocs, name) && len(st.ScreenDocs) < maxScreenDocs {
			st.ScreenDocs = append(st.ScreenDocs, name)
		}
	}
	for _, m := range screenElementRe.FindAllStringSubmatch(output, -1) {
		role, text := strings.ToLower(m[1]), m[2]
		// A name with spaces ("Scan 2026-09-28 at 10.15.32 (2).jpg") is the element's whole text, or follows the word
		// that says what the element does with it ("Download ..."): the name pattern alone stops at a space and kept
		// "2).jpg" (found live, 2026-10-25: the review asked about an attachment of that name).
		if full := wholeDocName(role, text); full != "" {
			keep(full)
			continue
		}
		for _, name := range screenDocRe.FindAllString(text, -1) {
			if offersDocument(role, text, name) {
				keep(name)
			}
		}
	}
	// What the browser saved, or what was opened, is the work's whatever its form ("Downloaded: /.../X.pdf (...)"):
	// a path, spaces and all, up to its extension.
	for _, m := range savedOrOpenedRe.FindAllStringSubmatch(output, -1) {
		for _, p := range docPathRe.FindAllStringSubmatch(m[1], -1) {
			keep(path.Base(p[1]))
		}
	}
}

// savedOrOpenedRe is a gui output's line saying what was downloaded or opened.
var savedOrOpenedRe = regexp.MustCompile(`(?m)\b(?:Downloaded:|Opened)\s+([^\n]+)`)

// docExt is the extensions of a document the work may be offered.
const docExt = `(?:pdf|jpe?g|png|tiff?|webp|heic|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml)`

// docPathRe is a path to a document in a line, spaces included: from a slash to the first document extension that ends
// a word.
var docPathRe = regexp.MustCompile(`(?i)(/[^;\n]*?\.` + docExt + `)(?:\s|$|[;,)])`)

// docEndRe is text that ends with a document's extension.
var docEndRe = regexp.MustCompile(`(?i)\.` + docExt + `$`)

// wholeDocName is the document a link or button offers, spaces and all - its text after the word that says what it
// does with it ("Download Scan 12 (2).jpg") - or "" when it is not one. (A text that ends with a name may be a sentence
// - "please find attached invoice 455.pdf" - so only the control that offers the file is taken whole.)
func wholeDocName(role, text string) string {
	t := strings.TrimSpace(text)
	words := strings.Fields(t)
	if (role != "link" && role != "button") || len(words) < 3 || !docEndRe.MatchString(t) ||
		!docActionWordRe.MatchString(words[0]) {
		return "" // (two words, "Download X.pdf": the name pattern takes X.pdf as it is)
	}
	return strings.TrimSpace(strings.TrimPrefix(t, words[0]))
}

// screenElementRe is an element in a screen listing or an action's output: its role and its quoted text
// (`e12 link "Download X.pdf"`, `text "X.pdf" (e13)`).
var screenElementRe = regexp.MustCompile(`(?:^|[\s,:])([A-Za-z]+)\s+"([^"\n]*)"`)

// docActionWordRe is a word that only says what can be done with a document or what it is: Download X, X (12 kB).
var docActionWordRe = regexp.MustCompile(`(?i)^\W*(download|open|view|save|preview|attachment|attached|file|document|kb|mb|bytes?)\W*$`)

// offersDocument reports whether an element's text offers the document it names: beside the name only words saying
// what can be done with it, or its size - for a link or button, none other at all (a link that opens something else,
// a chat for one, merely mentions it); for other elements, at most one more word.
func offersDocument(role, text, name string) bool {
	others := 0
	for _, w := range strings.Fields(strings.Replace(text, name, " ", 1)) {
		if strings.IndexFunc(w, unicode.IsLetter) >= 0 && !docActionWordRe.MatchString(w) {
			others++
		}
	}
	if role == "link" || role == "button" {
		return others == 0
	}
	return others <= 1
}
