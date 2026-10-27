package skills

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ============================================================
// A file a command just wrote that is not what its name says
//
// A document fetched outside the program it is offered in (a script or curl given the link of a browser page's
// attachment) can come back as the page that asks to sign in, saved under the document's name. Found live
// (2026-10-24): an invoice "downloaded" that way was a 3 KB web page named .pdf; nothing said so until the final
// check, three reports later. Decided from the file's first bytes, no model: the worker is told at once.
// ============================================================

// kindFileRe is a file named as a kind whose first bytes say what it is.
var kindFileRe = regexp.MustCompile(`(?i)\.(pdf|png|jpe?g|gif)$`)

// quotedKindFileRe is such a file named in a script's text (a heredoc's body).
var quotedKindFileRe = regexp.MustCompile(`(?i)['"]([^'"\n]{1,300}\.(?:pdf|png|jpe?g|gif))['"]`)

// kindMagic is what each kind's bytes begin with (a PDF may have a few bytes before its marker).
var kindMagic = map[string][][]byte{
	"pdf":  {[]byte("%PDF")},
	"png":  {[]byte("\x89PNG")},
	"jpg":  {[]byte("\xFF\xD8\xFF")},
	"jpeg": {[]byte("\xFF\xD8\xFF")},
	"gif":  {[]byte("GIF8")},
}

// wrongKindNote is a note for the worker about each file the command names that it wrote (changed since started) and
// whose bytes are not the kind its name says; "" when there is none.
func wrongKindNote(dir, command string, started time.Time) string {
	var notes []string
	seen := map[string]bool{}
	names := namedKindFiles(command)
	// A script that builds its paths (os.path.join(folder, name)) names no file: the files of a checkable kind it
	// changed in the folders it names, and in the working folder, are looked at too. Found live (2026-10-25): bills
	// fetched by a script outside the browser were sign-in pages under the bills' names, and nothing said so.
	names = append(names, changedKindFiles(dir, command, started)...)
	for _, name := range names {
		p := name
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		} else if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.ModTime().Before(started.Add(-time.Second)) {
			continue
		}
		if what := wrongKind(p); what != "" {
			notes = append(notes, what)
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "\nNote: " + strings.Join(notes, "; ") + ". It is not the document: a link fetched outside the program it " +
		"is offered in can bring back the page that asks to sign in. Get it where it is offered (in the browser, " +
		"from the page or message it is attached to), then open it to check it is the document."
}

// namedKindFiles are the files of a checkable kind a command names: its words, and quoted paths in a script's text.
func namedKindFiles(command string) []string {
	var out []string
	for _, seg := range shellSegments(command) {
		for _, w := range shellWords(seg.Text) {
			if w = unquoteWord(w); kindFileRe.MatchString(w) {
				out = append(out, w)
			}
		}
		for _, m := range quotedKindFileRe.FindAllStringSubmatch(seg.Stdin, 20) {
			out = append(out, m[1])
		}
	}
	return out
}

// maxScannedFiles bounds the files of one folder looked at after a command (changedKindFiles).
const maxScannedFiles = 300

// quotedDirRe is a quoted absolute or home path in a command or a script's text.
var quotedDirRe = regexp.MustCompile(`['"]((?:/|~/)[^'"\n]{1,300})['"]`)

// changedKindFiles are the files of a checkable kind changed since started in the folders a command names (its
// words and quoted paths that are folders) and in its working folder, one level deep each.
func changedKindFiles(dir, command string, started time.Time) []string {
	folders := map[string]bool{dir: true}
	add := func(w string) {
		if strings.HasPrefix(w, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				w = filepath.Join(home, w[2:])
			}
		}
		if !filepath.IsAbs(w) {
			return
		}
		if info, err := os.Stat(w); err == nil && info.IsDir() {
			folders[w] = true
		}
	}
	for _, seg := range shellSegments(command) {
		for _, w := range shellWords(seg.Text) {
			add(unquoteWord(w))
		}
		for _, m := range quotedDirRe.FindAllStringSubmatch(seg.Text+"\n"+seg.Stdin, 40) {
			add(m[1])
		}
	}
	var out []string
	for folder := range folders {
		entries, err := os.ReadDir(folder)
		if err != nil {
			continue
		}
		for i, e := range entries {
			if i >= maxScannedFiles {
				break
			}
			if e.IsDir() || !kindFileRe.MatchString(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil && !info.ModTime().Before(started.Add(-time.Second)) {
				out = append(out, filepath.Join(folder, e.Name()))
			}
		}
	}
	return out
}

// wrongKind says what the file at p holds when its first bytes are not the kind its name says, else "".
func wrongKind(p string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	magics, ok := kindMagic[ext]
	if !ok {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := f.Read(head)
	head = head[:n]
	window := head
	if ext != "pdf" && len(window) > 8 {
		window = window[:8]
	}
	for _, m := range magics {
		if (ext == "pdf" && bytes.Contains(window, m)) || bytes.HasPrefix(window, m) {
			return ""
		}
	}
	what := "something else"
	trimmed := bytes.ToLower(bytes.TrimSpace(head))
	switch {
	case n == 0:
		what = "nothing (it is empty)"
	case bytes.HasPrefix(trimmed, []byte("<")):
		what = "a web page"
	case isText(head):
		what = "text"
	}
	return fmt.Sprintf("%s is named as a %s file but holds %s", filepath.Base(p), strings.ToUpper(ext), what)
}

// isText reports whether the first bytes are printable text.
func isText(b []byte) bool {
	if len(b) > 64 {
		b = b[:64]
	}
	for _, c := range b {
		if c < 9 || (c > 13 && c < 32) || c == 127 {
			return false
		}
	}
	return true
}
