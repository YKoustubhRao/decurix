package skills

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ============================================================
// What the work gathered, read; what it found, settled
//
// Found live (GST suite T8, 2026-10-22): the client sent five invoices, one of them a phone photo. The worker saved all
// five into the month's folder with curl - and read the four PDFs only. The month's sales left the photo's invoice out,
// and the report said "Done". The same report noted that the client's own challan message (13,887.30) did not match
// the workbook's cash payable (13,029.80) - and still said "Done".
//
// So, at a DONE, once each:
//   - a document the work itself fetched or copied (curl -o, wget -O, cp, mv) that no later command read - by name or
//     by a glob that covers it - is sent back: every document gathered is part of the work;
//   - a report that says its result does not match a source is sent back: find out which is right first.
// ============================================================

var (
	// fetchFlagRe is curl's or wget's output file: -o FILE, -O FILE, -fsSLo FILE, --output FILE, --output-document=FILE.
	fetchFlagRe = regexp.MustCompile(`(?:\s-[A-Za-z]*[oO]|\s--output(?:-document)?(?:=|\s))\s*('[^']+'|"[^"]+"|[^\s;&|]+)`)
	// docFileRe is a file name with a document's or picture's extension.
	docFileRe = regexp.MustCompile(`(?i)\.(?:pdf|jpe?g|png|tiff?|webp|heic|xlsx|xlsm|xls|ods|docx|doc|odt|csv|eml)$`)
	// readVerbRe is a command that reads a file's content.
	readVerbRe = regexp.MustCompile(`(?i)\b(?:office\s+read|pdftotext|pdfinfo|tesseract|gui\s+open|python3?|cat|head|less|xdg-open|soffice|libreoffice|identify|exiftool)\b`)
	// mismatchRe is a report saying its result and a source disagree.
	mismatchRe = regexp.MustCompile(`(?i)\b(?:does not|doesn'?t|do not|don'?t|did not|didn'?t)\s+(?:match|agree|tally|reconcile)\b|\bmismatch(?:es|ed)?\b|\bdisagrees?\b|\bdiffers?\s+from\b|\binconsistent\s+with\b`)
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

// readLater reports whether a command after index `after` reads the file `name`: names it, or holds a glob that
// covers it, in a command that reads content.
func readLater(commands []string, after int, name string) bool {
	for _, c := range commands[after+1:] {
		if !readVerbRe.MatchString(c) || len(fetchedDocuments(c)) > 0 && !strings.Contains(c, "office read") {
			continue
		}
		if strings.Contains(c, name) {
			return true
		}
		for _, w := range strings.Fields(c) {
			// (A glob after a quoted folder - '.../06. SEPTEMBER/'*.pdf - keeps a stray quote: quotes are not names.)
			w = path.Base(strings.ReplaceAll(strings.NewReplacer(`'`, "", `"`, "").Replace(strings.Trim(w, `;&|()`)), `\`, "/"))
			if strings.ContainsAny(w, "*?[") {
				if ok, _ := path.Match(w, name); ok {
					return true
				}
			}
		}
	}
	return false
}

// unreadFetched are the documents the goal's commands fetched or copied that no later command read.
func unreadFetched(commands []string) []string {
	var out []string
	seen := map[string]bool{}
	for i, c := range commands {
		for _, name := range fetchedDocuments(c) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if !readLater(commands, i, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// unreadFetchedNudge is said at a DONE over gathered documents never read.
func unreadFetchedNudge(names []string) string {
	return fmt.Sprintf("Not reported yet: you saved %s but no command has read %s since. Every document the work "+
		"gathered is part of it - a picture or scan as much as a PDF. Read each one (office read PATH shows a picture "+
		"too), take into the work what it holds, and then report.", quotedList(names), map[bool]string{true: "it", false: "them"}[len(names) == 1])
}

// selfApprovalRe is a question that asks the person to approve or confirm an action - which the approval gate stops and
// puts to them itself - or asks ahead for what the application has not asked for yet ("when I click FILE WITH EVC").
// Only an action on the screen, which the screen's gate always stops: a question about a shell command ("may I delete
// the old logs?") is the person's.
var selfApprovalRe = regexp.MustCompile(`(?i)(?:\b(?:approve|approval|your (?:ok|okay|go-ahead|permission)|confirm (?:that )?I should)\b` +
	`.*\b(?:click|press|button)\b|\b(?:click|press|button)\b.*\b(?:approve|approval|your (?:ok|okay|go-ahead|permission))\b` +
	`|\b(?:when|once|before|after) I (?:click|press)\b` +
	// "please approve the payment step when it is put to you on screen" (GST suite T9, 2026-10-22)
	`|\bapprove\b.*\b(?:on (?:the |this )?screen|when it is put to you|step)\b)`)

// maxSelfApprovalNudges bounds how often a goal is sent back for it.
const maxSelfApprovalNudges = 2

// selfApprovalNudge sends such a question back. Found live (GST suite T10, 2026-10-22): "please approve the GSTR-3B
// filing click", "please approve this final send click" - each followed by the gate's own approval of the same click,
// and "please complete the OTP step when I click FILE WITH EVC" before any OTP was sent: 15 questions for one task.
const selfApprovalNudge = "Do not ask for approval yourself: run the command. The approval gate stops every action that " +
	"sends, pays, files or submits, and asks the person itself - with what the action would do. Ask with HELP only for " +
	"what the application is waiting for from the person now (an OTP it says it has sent) or for what nothing you were " +
	"given says."

// mismatchNudge is said at a DONE whose report says its result and a source disagree.
const mismatchNudge = "Not reported yet: your report says something does not match. Find out which is right before " +
	"you report: go back to the source and to your own work, and recompute what disagrees. If your work is wrong, " +
	"correct it. If the source is wrong, or the difference is what the request asked you to find, say so plainly in " +
	"the report, with both figures and where each comes from."
