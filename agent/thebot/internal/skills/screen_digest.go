package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ============================================================
// Old screens, shortened in what is re-sent
//
// Measured live (2026-10-20, P2 on Azure): 101 turns re-sent 2.84M prompt tokens; the GUI portal stage's later turns
// each carried about 22k tokens of earlier gui observe listings - element lists of screens long gone. Once a later
// action has changed the screen, such an output describes nothing that is there. It is replaced, in the history the
// model is re-sent, by a short digest: what it did, in a few lines, and where the full output is kept on disk (the
// model can read it again with one command).
//
// What is never shortened: anything that is not a screen command's output (file reads, office read, figures, the
// person's answers), the request and the contract, and the newest screen output. The grounding guard is unaffected:
// it traces against st.Seen, which keeps everything every command showed, whatever is re-sent.
//
// Caching: a message is digested once, when it goes stale, and never changed again, so each re-send is the previous
// one plus what is new, with one changed message at most - the screen output just superseded (whose screenshot is
// dropped in the same change).
// ============================================================

// screenVerbRe is a gui command whose output describes the screen as it was: looking at it or acting on it. Reading
// a document's text (gui read), guides, recordings and skills are not: their output is data, kept in full.
var screenVerbRe = regexp.MustCompile(`^\s*gui\s+(observe|look|click|double-click|doubleclick|dblclick|right-click|type|select|key|drag|open|scroll|front|focus|hover|wait|close|move)\b`)

// screenOnly reports whether every simple command of command is a screen command.
func screenOnly(command string) bool {
	segs := shellSegments(command)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if !screenVerbRe.MatchString(seg.Text) {
			return false
		}
	}
	return true
}

// observeRe is a gui command that lists the screen anew.
var observeRe = regexp.MustCompile(`^\s*gui\s+(observe|look)\b`)

// listsScreen reports whether a command lists the screen anew (one of its simple commands is gui observe).
func listsScreen(command string) bool {
	for _, seg := range shellSegments(command) {
		if observeRe.MatchString(seg.Text) {
			return true
		}
	}
	return false
}

// digestKeepRe is a line of a screen output worth keeping in its digest: what changed, opened or failed.
var digestKeepRe = regexp.MustCompile(`(?i)\b(opened|closed|appeared|disappeared|changed|active window|now|error|refused|not found|failed|stopped|saved)\b`)

const (
	// longScreenOutput is how long an action's output must be to be shortened when it goes stale.
	longScreenOutput  = 1500
	maxDigestLines    = 6
	maxDigestLineChar = 200
	staleNote         = "(An earlier screen, since changed: its full output is shortened here.)"
	// maxKeptOutputBytes bounds the outputs one goal keeps on disk (the desktop's /tmp is memory).
	maxKeptOutputBytes = 64 << 20
)

// digestScreen is the short form of a stale screen output: its exit line, the first line and the lines that say
// what changed, and the path of the full text.
func digestScreen(content, fullPath string) string {
	body := content
	exit := ""
	if i := strings.Index(body, "exit_code:"); i >= 0 {
		line := body[i:]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		exit = strings.TrimSpace(line)
	}
	if i := strings.Index(body, "output:\n"); i >= 0 {
		body = body[i+len("output:\n"):]
	}
	var kept []string
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "(The screenshot shows") || strings.HasPrefix(line, "(Pictures that were shown") {
			continue
		}
		if i == 0 || digestKeepRe.MatchString(line) {
			kept = append(kept, truncateText(line, maxDigestLineChar))
		}
		if len(kept) == maxDigestLines {
			break
		}
	}
	out := "Command execution result " + staleNote + "\n\n" + exit + "\n\n" + strings.Join(kept, "\n")
	if fullPath != "" {
		out += "\n(full output: " + fullPath + ")"
	}
	return out
}

// outputsDir is where the full text of shortened outputs is kept for this run.
func (st *operatorState) outputsDir() string {
	if st.OutputsDir == "" {
		st.OutputsDir = filepath.Join(os.TempDir(), fmt.Sprintf("thebot-outputs-%d", time.Now().UnixNano()))
	}
	return st.OutputsDir
}

// keepOutput writes an output's full text to a file of its own and returns its path, or "" when it could not or the
// goal's room is spent. Files are numbered by a counter of the goal, never by a message's place: a compaction moves the
// places, and a later output would overwrite an earlier one a pointer still names (found 2026-10-30).
func (st *operatorState) keepOutput(text string) string {
	if st.OutputBytes+int64(len(text)) > maxKeptOutputBytes {
		return ""
	}
	dir := st.outputsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	st.OutputSeq++
	p := filepath.Join(dir, fmt.Sprintf("output-%d.txt", st.OutputSeq))
	if os.WriteFile(p, []byte(text), 0o600) != nil {
		return ""
	}
	st.OutputBytes += int64(len(text))
	return p
}

// digestStaleScreens shortens every screen output not shortened yet (each once), keeping its full text on disk.
func (st *operatorState) digestStaleScreens() {
	for _, idx := range st.ScreenMsgs {
		if idx < 0 || idx >= len(st.Messages) {
			continue
		}
		m := &st.Messages[idx]
		m.Content = digestScreen(m.Content, st.keepOutput(m.Content))
		m.Images, m.ImageLabels = nil, nil
	}
	st.ScreenMsgs = nil
}
