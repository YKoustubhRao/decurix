package skills

import (
	"path"
	"regexp"
	"strings"
)

// ============================================================
// What the work gathered and what the screens offered: facts for the reviewer
//
// Which documents the goal's own commands saved or copied, and which document names its screens showed (a chat's
// attachments, a list of files). Facts only: whether one that was offered and never taken mattered is the reviewer's
// to weigh (reviewer.go) - found live (GST suite T8, 2026-10-22), a photo invoice saved and never read, and an invoice
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

// maxScreenDocs bounds the names kept.
const maxScreenDocs = 120

// noteScreenDocs keeps the document names a screen command's output shows.
func (st *operatorState) noteScreenDocs(command, output string) {
	if !strings.HasPrefix(strings.TrimSpace(command), "gui ") {
		return
	}
	for _, name := range screenDocRe.FindAllString(output, -1) {
		if !contains(st.ScreenDocs, name) && len(st.ScreenDocs) < maxScreenDocs {
			st.ScreenDocs = append(st.ScreenDocs, name)
		}
	}
}
