package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"thebot/internal/llm"
	"thebot/internal/prompt"
)

// ============================================================
// Evidence: what the goal was shown, kept as units with their source (docs/harness-v2.md, Phase 1)
//
// The goal's "seen" text answered one question - does any text hold this value? - and could not answer the ones the
// live failures turned on: did the values typed into one record come from one source record
// (2026-10-23: one record's number typed with the next record's amount, from a list written from memory)? Was every document
// of the work taken in? Is a value one only a picture showed? Units answer them: a document read whole (an invoice,
// a letter), a row of a sheet, a row of a screen, a line a command computed, the request, a reply of the person -
// each with where it came from. Code only finds where values stand; what a finding means is the reviewer's, the
// verifier's and the person's.
// ============================================================

// evidenceUnit is one source record the goal was shown.
type evidenceUnit struct {
	Kind   string `json:"k"` // document, ocr, row, screen, computed, request, person
	Source string `json:"s"` // the file, screen or command it came from
	Text   string `json:"t"`

	nums map[string]bool // its numbers in canonicalNumber's form (computed when first needed)
	norm string          // its text, normalised (computed when first needed)
}

const (
	maxUnits         = 5000
	maxUnitChars     = 24_000
	maxEvidenceChars = 300_000
	// maxRecordFindings and maxPictureOnly bound what is kept for the reviewer.
	maxRecordFindings = 20
	maxPictureOnly    = 40
)

var (
	// docFileTypeRe is a document read whole: one record (an invoice, a letter, a scan), not a table of records.
	docFileTypeRe = regexp.MustCompile(`(?i)\.(?:pdf|docx?|odt|rtf|txt|eml|msg|png|jpe?g|tiff?|webp|heic|bmp|gif)$`)
	officeReadRe  = regexp.MustCompile(`^\s*office\s+read\s`)
	// loopBodyRe opens a shell loop's body ("do office read ...").
	loopBodyRe = regexp.MustCompile(`^\s*do\s+`)
	// readHeaderRe is the line an office read opens a file's output with: "<name.ext>: ...".
	readHeaderRe = regexp.MustCompile(`(?m)^([^\n:/\\]{1,200}\.(?:pdf|docx?|odt|rtf|txt|eml|xlsx|xlsm|xls|ods|csv|png|jpe?g|tiff?|webp)):\s`)
	guiReadRe    = regexp.MustCompile(`^\s*gui\s+read\s`)
	digitRe      = regexp.MustCompile(`\d`)
	// inlineRefRe is an element ref inside a screen line (e123): it names the element, it is not a figure.
	inlineRefRe = regexp.MustCompile(`\be[0-9]+\b`)
	// clickVerbs end the record being typed: what a form was filled with is put somewhere by a click.
	clickVerbs = map[string]bool{"click": true, "double-click": true, "doubleclick": true, "dblclick": true}
)

// holds reports whether the unit holds value v: a number by its digits (6,000.00 is 6000), anything else as text.
func (u *evidenceUnit) holds(v string) bool {
	if n, ok := canonicalNumber(v); ok {
		if u.nums == nil {
			u.nums = numbersIn(u.Text)
		}
		return u.nums[n]
	}
	if u.norm == "" {
		u.norm = normalise(u.Text)
	}
	return strings.Contains(u.norm, normalise(v))
}

// label is the unit as a finding names it.
func (u *evidenceUnit) label() string {
	switch u.Kind {
	case "document", "ocr":
		return "the document " + u.Source
	case "row":
		return "a row of " + u.Source + ": " + truncateText(strings.Join(strings.Fields(u.Text), " "), 120)
	case "screen":
		return "the screen: " + truncateText(strings.Join(strings.Fields(u.Text), " "), 120)
	case "computed":
		return "a line printed by `" + u.Source + "`"
	}
	return u.Source
}

// addUnit keeps one unit, dropping the oldest screen and computed lines first when the store is full.
func (st *operatorState) addUnit(kind, source, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	st.Units = append(st.Units, evidenceUnit{Kind: kind, Source: source, Text: truncateText(text, maxUnitChars)})
	size := 0
	for _, u := range st.Units {
		size += len(u.Text)
	}
	for (len(st.Units) > maxUnits || size > maxEvidenceChars) && len(st.Units) > 1 {
		drop := 0
		for i, u := range st.Units {
			if u.Kind == "screen" || u.Kind == "computed" {
				drop = i
				break
			}
		}
		size -= len(st.Units[drop].Text)
		st.Units = append(st.Units[:drop], st.Units[drop+1:]...)
	}
}

// addLines keeps each line of text that holds a figure as a unit of its own (a row of a sheet, of a screen, of what a
// command printed): the records a table holds are its lines.
func (st *operatorState) addLines(kind, source, text string) {
	for _, line := range strings.Split(text, "\n") {
		if digitRe.MatchString(line) {
			st.addUnit(kind, source, line)
		}
	}
}

// noteEvidence keeps what a command showed as evidence units. output is already without the command's own values
// (withoutCommandValues): what a command prints back of what it was given is no source of it.
func (st *operatorState) noteEvidence(command, output string) {
	if strings.TrimSpace(output) == "" {
		return
	}
	segs := shellSegments(command)
	if len(segs) == 1 && (officeReadRe.MatchString(segs[0].Text) || guiReadRe.MatchString(segs[0].Text)) {
		args := positional(shellWords(segs[0].Text)[2:])
		source := "a file"
		if len(args) > 0 {
			source = path.Base(strings.ReplaceAll(unquoteWord(args[0]), `\`, "/"))
		}
		if guiReadRe.MatchString(segs[0].Text) || (len(args) > 0 && docFileTypeRe.MatchString(source)) {
			st.addUnit("document", source, output)
			return
		}
		st.addLines("row", source, output)
		return
	}
	st.noteChainedReads(segs, output)
	kind, source := "computed", truncateText(strings.Join(strings.Fields(command), " "), 100)
	if screenOnly(command) {
		// A screen's lines are the window they were read from (its listing's header names it): a page of another
		// window is a source for a check before a commit, the window being committed never (case.go).
		kind = "screen"
		if w := listingWindow(output); w != "" {
			source = screenSource(w)
		}
	}
	st.addLines(kind, source, output)
}

// noteChainedReads credits each file a chained command reads with office read ("office read A && office read B &&
// python3 ...") with its own part of the output - office read opens each file's output with "<name>:" at the start of
// a line - as a document, or as rows of a sheet. Found live (2026-10-24): a bill and two purchase bills read in one
// chained command were "never read" to the record, and the review put that to the person as a point; the person said
// "if the record says never read then read them properly now". A file whose header is not found is not credited.
func (st *operatorState) noteChainedReads(segs []shellSegment, output string) {
	type read struct {
		source string
		doc    bool
		at     int
	}
	var reads []read
	looped := false
	for _, seg := range segs {
		// (In a loop the read is the body's: "do office read "$f"".)
		text := loopBodyRe.ReplaceAllString(seg.Text, "")
		if !officeReadRe.MatchString(text) {
			continue
		}
		args := positional(shellWords(text)[2:])
		if len(args) == 0 {
			continue
		}
		if strings.ContainsAny(unquoteWord(args[0]), "$*?") {
			looped = true // which files it read is in the output: each office read opens with "<name>:"
			continue
		}
		source := path.Base(strings.ReplaceAll(unquoteWord(args[0]), `\`, "/"))
		at := -1
		if strings.HasPrefix(output, source+":") {
			at = 0
		} else if i := strings.Index(output, "\n"+source+":"); i >= 0 {
			at = i + 1
		}
		if at >= 0 {
			reads = append(reads, read{source, docFileTypeRe.MatchString(source), at})
		}
	}
	// Files read through a loop or a pattern (for f in *.docx; do office read "$f"; done): each is credited by the
	// header its read printed. Found live (2026-10-25): six bills read that way were "never read" to the record, and
	// the review sent the worker back to read them again.
	if looped {
		for _, m := range readHeaderRe.FindAllStringSubmatchIndex(output, -1) {
			name := output[m[2]:m[3]]
			known := false
			for _, r := range reads {
				known = known || r.source == name
			}
			if !known {
				reads = append(reads, read{name, docFileTypeRe.MatchString(name), m[0]})
			}
		}
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i].at < reads[j].at })
	for i, r := range reads {
		end := len(output)
		if i+1 < len(reads) {
			end = reads[i+1].at
		}
		if r.doc {
			st.addUnit("document", r.source, output[r.at:end])
		} else {
			st.addLines("row", r.source, output[r.at:end])
		}
	}
}

// screenSourcePrefix opens the source name of the lines read off a window (screenSource).
const screenSourcePrefix = "the screen of \""

// screenSource is the source name of the lines read off a window.
func screenSource(window string) string {
	return screenSourcePrefix + window + "\""
}

// distinctive reports whether a typed value is specific enough to place: three digits or more, not a date (a period
// date is typed into many records), not a rate.
func distinctive(v string) bool {
	if len(digitRe.FindAllString(v, -1)) < 3 || strings.HasSuffix(strings.TrimSpace(v), "%") {
		return false
	}
	_, isDate := canonicalDate(v)
	return !isDate
}

// noteRecordWork follows the record being typed: values a gui type or select puts in since the last click, checked
// at the click (recordFinding). It returns what to tell the worker, or "".
func (st *operatorState) noteRecordWork(command string) string {
	var notes []string
	for _, seg := range shellSegments(command) {
		words := shellWords(seg.Text)
		if len(words) < 2 || words[0] != "gui" {
			continue
		}
		switch {
		case words[1] == "type" || words[1] == "select":
			st.RecordValues = append(st.RecordValues, segmentValues(seg)...)
		case clickVerbs[words[1]]:
			if note := st.recordFinding(st.RecordValues); note != "" {
				notes = append(notes, note)
			}
			st.RecordValues = nil
		}
	}
	return strings.Join(notes, "\n")
}

// recordFinding is, for the values typed into one record, the finding when no single unit holds them all: they were
// taken from different records (or from none). "" when one unit holds them, or there are fewer than two to place.
func (st *operatorState) recordFinding(values []string) string {
	var keyed []string
	for _, v := range values {
		if distinctive(v) && !contains(keyed, v) {
			keyed = append(keyed, v)
		}
	}
	if len(keyed) < 2 {
		return ""
	}
	for i := range st.Units {
		if st.Units[i].Kind == "memory" {
			continue // what earlier tasks kept is no record of this one: it explains, it never places values together
		}
		all := true
		for _, v := range keyed {
			if !st.Units[i].holds(v) {
				all = false
				break
			}
		}
		if all {
			return ""
		}
	}
	var where []string
	for _, v := range keyed {
		at := "in nothing you were shown"
		for i := len(st.Units) - 1; i >= 0; i-- {
			if st.Units[i].Kind != "memory" && st.Units[i].holds(v) {
				at = "in " + st.Units[i].label()
				break
			}
		}
		where = append(where, fmt.Sprintf("%q %s", v, at))
	}
	finding := "values entered into one record stand together in no single document, row or computed line: " +
		strings.Join(where, "; ")
	if len(st.RecordFindings) < maxRecordFindings {
		st.RecordFindings = append(st.RecordFindings, finding)
	}
	return "\n\nCheck (a fact, not a refusal): the " + finding + ". If they belong together, nothing changes; if one " +
		"was taken from another record, correct the record now. A figure worked out from several sources is placed " +
		"once a command works it out from them and prints it."
}

// notePictureOnly keeps values typed that only a picture showed - in no text the goal was shown, OCR included: they
// are uncertain, for the commit gate's verifier and the reviewer.
func (st *operatorState) notePictureOnly(values []string) {
	for _, v := range values {
		if !contains(st.PictureOnly, v) && len(st.PictureOnly) < maxPictureOnly {
			st.PictureOnly = append(st.PictureOnly, v)
		}
	}
}

// jsonScalarRe is a string or a number in a JSON spec (office update / create).
var jsonScalarRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|(-?\d[\d.,]*)`)

// noteWritten keeps the values the work put into the world: what it typed, and the values of an office spec.
func (st *operatorState) noteWritten(command string) {
	values := typedValues(command)
	if strings.Contains(command, "office ") && (strings.Contains(command, " update ") || strings.Contains(command, " create ")) {
		for _, m := range jsonScalarRe.FindAllStringSubmatch(command, -1) {
			if v := m[1] + m[2]; distinctive(v) {
				values = append(values, v)
				// A value written into a file that no text the goal was shown holds - read off a picture, worked out
				// in the head, or not read at all - is a fact for the reviewer (advisory): found live (2026-10-23), a
				// phone photo's two lines were written into a workbook as 8,750 and 5,000 for 2,750 and 6,000, and
				// nothing looked at a value written with office update.
				if !inSeen(st.Seen, v) && !st.unitHolds(v) && !contains(st.UntracedWrites, v) && len(st.UntracedWrites) < maxPictureOnly {
					st.UntracedWrites = append(st.UntracedWrites, v)
				}
			}
		}
	}
	for _, v := range values {
		if distinctive(v) && !contains(st.Written, v) && len(st.Written) < 4000 {
			st.Written = append(st.Written, v)
		}
	}
	st.noteInventedCodes(command)
}

// noteTypedShown keeps, of a command that failed, the typed values its output says went in ("Typed "7350.50" into
// ..."): a chain whose last step failed still typed its first ones. Found live (2026-10-25): four bills whose amounts
// were typed by such chains were "read; none of its figures went into what was entered", and the review sent the
// worker back to enter them again.
func (st *operatorState) noteTypedShown(command, output string) {
	for _, v := range typedValues(command) {
		shown := v
		if len(shown) > 30 {
			shown = shown[:30]
		}
		if (strings.Contains(output, `Typed "`+shown) || strings.Contains(output, `Selected "`+shown)) &&
			distinctive(v) && !contains(st.Written, v) && len(st.Written) < 4000 {
			st.Written = append(st.Written, v)
		}
	}
}

// maxInventedCodes bounds the invented codes kept, and the points a report gets for them.
const (
	maxInventedCodes      = 20
	maxInventedCodeIssues = 3
)

// oneWordCodeRe is a cell value of one word with three letters or more: a code or a category (KGS, OTH, Rice).
var oneWordCodeRe = regexp.MustCompile(`^[\pL][\pL\d_./-]*$`)

// noteInventedCodes keeps the one-word values an office update writes into cells that nothing the goal was shown
// holds - not a document, a screen, the request or what the person said. Found live (2026-10-25): a phone photo's two
// lines, each with its own code and rate, were written as one row whose code was "MIXED" - a word no source has; the
// numbers were traced (worked out and printed), the word was not looked at.
func (st *operatorState) noteInventedCodes(command string) {
	for _, seg := range shellSegments(command) {
		words := shellWords(seg.Text)
		if len(words) < 2 || path.Base(words[0]) != "office" || words[1] != "update" || !seg.HasStdin {
			continue
		}
		var spec struct {
			Edits []struct {
				Sheet string `json:"sheet"`
				Cell  string `json:"cell"`
				Value any    `json:"value"`
			} `json:"edits"`
		}
		if json.Unmarshal([]byte(seg.Stdin), &spec) != nil {
			continue
		}
		known := normalise(st.Request + "\n" + strings.Join(st.personWords(), "\n"))
		for _, e := range spec.Edits {
			where := strings.Trim(e.Sheet+"!"+e.Cell, "!")
			// A cell written again holds its new value: what was said of the old one no longer stands.
			kept := st.InventedCodes[:0]
			for _, entry := range st.InventedCodes {
				if !strings.HasSuffix(entry, " ("+where+")") {
					kept = append(kept, entry)
				}
			}
			st.InventedCodes = kept
			v, ok := e.Value.(string)
			v = strings.TrimSpace(v)
			if !ok || len(st.InventedCodes) >= maxInventedCodes || !oneWordCodeRe.MatchString(v) ||
				len(letterRunRe.FindString(v)) < 3 || st.sourceHolds(v) || strings.Contains(known, normalise(v)) {
				continue
			}
			st.InventedCodes = append(st.InventedCodes, fmt.Sprintf("%q (%s)", v, where))
		}
	}
}

// sourceHolds reports whether a source the goal was shown - a document, its text layer, a sheet's rows, a screen -
// holds a value; never the work's own commands (a script that prints a word it made up is no source of it).
func (st *operatorState) sourceHolds(v string) bool {
	for i := range st.Units {
		if k := st.Units[i].Kind; (sourceKind(k) || k == "screen") && st.Units[i].holds(v) {
			return true
		}
	}
	return false
}

// letterRunRe is a run of letters.
var letterRunRe = regexp.MustCompile(`\pL{3,}`)

// inventedCodeIssues are points, at a report, for the invented codes still written (noteInventedCodes) that the
// report does not name: each to check against its source, or to say in the report where it comes from.
func (st *operatorState) inventedCodeIssues(report string) []reviewIssue {
	said := strings.ToLower(report)
	var out []reviewIssue
	for _, entry := range st.InventedCodes {
		value := entry
		if i := strings.Index(entry, " ("); i > 0 {
			value = strings.Trim(entry[:i], `"`)
		}
		if strings.Contains(said, strings.ToLower(value)) {
			continue // the report accounts for it
		}
		out = append(out, reviewIssue{
			What: entry + " was written into a file, but no document, screen, request or answer of this task holds " +
				"that word",
			Why:    "a cell holds what its source says - check it against the document, or say in the report where it comes from",
			Settle: "recheck",
		})
		if len(out) == maxInventedCodeIssues {
			break
		}
	}
	return out
}

// unitHolds reports whether any source of the goal (not what earlier tasks kept) holds a value.
func (st *operatorState) unitHolds(v string) bool {
	for i := range st.Units {
		if st.Units[i].Kind != "memory" && st.Units[i].holds(v) {
			return true
		}
	}
	return false
}

// coverageGaps are the documents of the work that nothing it entered or wrote reflects: read, saved or offered, and
// none of their distinctive values in what went out - each with what is known of it. Facts for the reviewer: a
// document left out for a reason (cancelled, another period) is right when the report says so.
func (st *operatorState) coverageGaps() []string {
	docs := map[string]*evidenceUnit{}
	var order []string
	for i := range st.Units {
		if u := &st.Units[i]; u.Kind == "document" || u.Kind == "ocr" {
			if docs[u.Source] == nil {
				order = append(order, u.Source)
			}
			docs[u.Source] = u
		}
	}
	var gaps []string
	for _, name := range order {
		u := docs[name]
		used := false
		for _, v := range st.Written {
			if u.holds(v) {
				used = true
				break
			}
		}
		if !used {
			gaps = append(gaps, name+" (read; none of its figures went into what was entered or written)")
		}
	}
	offered := append([]string(nil), st.ScreenDocs...)
	for _, c := range st.Commands {
		offered = append(offered, fetchedDocuments(c)...)
	}
	for _, name := range offered {
		if docs[name] == nil && !contains(order, name) && !strings.Contains(strings.Join(gaps, "\n"), name) {
			if docFileTypeRe.MatchString(name) {
				gaps = append(gaps, name+" (offered or saved; never read)")
			}
		}
	}
	return gaps
}

// maxCoverageIssues bounds the coverage points one report gets.
const maxCoverageIssues = 4

// coverageIssues are the coverage gaps a report does not account for, as recheck points: a document the work read,
// saved or was offered that nothing it entered or wrote reflects, and whose name the report never mentions - decided in
// code. Entered, or said why it is left out, it is settled. Found live (2026-10-24): a phone photo of a sale bill was
// saved with the others and never entered; the review raised other points, and the month's totals went out 8,750 short.
func (st *operatorState) coverageIssues(report string) []reviewIssue {
	said := strings.ToLower(report)
	var out []reviewIssue
	for _, gap := range st.coverageGaps() {
		name := gap
		if i := strings.Index(gap, " ("); i > 0 {
			name = gap[:i]
		}
		base := strings.ToLower(path.Base(name))
		stem := strings.TrimSuffix(base, path.Ext(base))
		if stem == "" || saysWhyLeftOut(said, base, stem) {
			continue
		}
		out = append(out, reviewIssue{
			What:   gap + ", and the report does not say why it is left out",
			Why:    "enter what it holds, or say in the report why it is not part of the work",
			Settle: "recheck",
		})
		if len(out) == maxCoverageIssues {
			break
		}
	}
	return out
}

// leftOutRe is what a sentence says when it gives a document's reason for not being in the work: it is not part of it,
// a copy, called off, someone else's.
var leftOutRe = regexp.MustCompile(`(?i)\b(not|no|never|isn'?t|aren'?t|wasn'?t|without|exclud\w*|cancel\w*|duplicate\w*|` +
	`ignor\w*|skip\w*|left out|kept out|leave out|another|other client|someone else)\b`)

// sentenceBreakRe ends a sentence: a line break, a full stop before a space, a semicolon - never the dot of a file name.
var sentenceBreakRe = regexp.MustCompile(`\n|\.\s|;`)

// saysWhyLeftOut reports whether the report (lowercased) names a document in a sentence that gives a reason for it not
// being in the work. Named alone - in a listing of the folder - it is not accounted for. Found live (2026-10-24): a
// phone photo of a bill was saved with the month's bills and never entered; the report pasted the folder's listing,
// which named it, and the point was never raised - the month's totals went out 8,750 short.
func saysWhyLeftOut(report, base, stem string) bool {
	for _, sentence := range sentenceBreakRe.Split(report, -1) {
		if (strings.Contains(sentence, base) || strings.Contains(sentence, stem)) && leftOutRe.MatchString(sentence) &&
			!couldNotReadRe.MatchString(sentence) {
			return true
		}
	}
	return false
}

// couldNotReadRe is a sentence saying the work could not read or use a document: no reason for it to be out of the
// work. Found live (2026-10-25): "its image contents were not successfully extracted ... therefore it did not change
// the figures" settled the point, and the month went out a bill short.
var couldNotReadRe = regexp.MustCompile(`(?i)\b(?:could\s*(?:not|n't)|can\s*(?:not|'t)|cannot|unable|failed|wasn't able|` +
	`was not able|did not manage|not (?:successfully )?(?:read|extracted|opened|legible)|unreadable|illegible|` +
	`no (?:verified |usable )?reading)\b`)

// ============================================================
// The commit gate's independent verifier (docs/harness-v2.md, principle 5)
//
// Before an action that cannot be taken back, the figures the screen is about to commit are worked out again from
// the sources by a separate model call that is NOT shown them: each figure on the screen is a blank, the verifier
// fills the blanks from the documents, rows and rules, and code compares. A disagreement is a recheck point with both
// values - sent back to the worker once, then carried into the approval (knowledge.go). This is the factored form of
// verification (Chain-of-Verification): a check that sees the answer tends to copy it.
// ============================================================

const (
	maxVerifyBlanks       = 60
	maxVerifySourceChars  = 60_000
	maxVerifyMismatches   = 8
	verifyTimeoutMs       = 180_000
	maxListings           = 8
	minBlanksToVerifyWith = 2
	// maxVerifyCache bounds the verifier's workings kept for a goal (verifyFigures).
	maxVerifyCache = 16
)

// screenHeaderRe opens a screen listing (agent elements.render).
var screenHeaderRe = regexp.MustCompile(`(?m)^Screen \d+x\d+\. Active window:`)

// figureRe is a figure on a screen to be worked out: digits (grouped, with decimals), not a rate (5%).
var figureRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?(?:%)?`)

// noteListing keeps the newest screen listing of each window whole (a gate's page is often the page after the
// figures), for the last maxListings windows looked at. Found live (2026-10-23): with the last three listings of any
// window, a look at a workbook and a scan before a filing pushed the portal's page out, and both checks before the
// filing found no screen of its window and checked nothing.
func (st *operatorState) noteListing(command, output string) {
	if !listsScreen(command) || strings.TrimSpace(output) == "" {
		return
	}
	// Only the listing itself: what a chained action printed before it ("What changed: page is now http://...?period=
	// 092026; 5 element(s) appeared") is no screen - found live (2026-10-25), its figures went to the person as the
	// screen's.
	if heads := screenHeaderRe.FindAllStringIndex(output, -1); len(heads) > 0 {
		output = output[heads[len(heads)-1][0]:]
	}
	window := listingWindow(output)
	if window != "" {
		if st.ListedAt == nil {
			st.ListedAt = map[string]int{}
		}
		st.ListedAt[window] = len(st.Worked)
	}
	kept := st.Listings[:0]
	for _, l := range st.Listings {
		if window == "" || !sameWindow(listingWindow(l), window) {
			kept = append(kept, l)
		}
	}
	st.Listings = append(kept, truncateText(output, 40_000))
	if len(st.Listings) > maxListings {
		st.Listings = st.Listings[len(st.Listings)-maxListings:]
	}
}

// lookBeforeCommit makes sure the checks before a commit have a screen of the window the action acts on: when none
// was taken, the screen is looked at now (a read-only gui observe - the gate stopped there, so it is that window).
func (o *OperatorSkill) lookBeforeCommit(rc RunContext, st *operatorState, action string) {
	if !st.Hosted || st.actionListing(action) != "" {
		return
	}
	res := runShell(rc.Ctx, rc.Dir(), "gui observe", 60*time.Second, st.guiEnv()...)
	if res.ExitCode != 0 || strings.TrimSpace(res.Output) == "" {
		rc.Logf("operator", "no screen of the action's window to check before it")
		return
	}
	st.noteListing("gui observe", res.Output)
	st.noteEvidence("gui observe", res.Output)
	if st.actionListing(action) == "" {
		rc.Logf("operator", "the screen now is not the action's window - nothing of it to check before it")
	}
}

// maskedFigures is a screen listing's lines that hold figures, each figure replaced by a numbered blank, and the
// figures by blank. Element refs (e123) and rates stay as they are: they name the row, not what it says.
func maskedFigures(listing string) (string, map[string]string) {
	return maskedFiguresOf(listing, nil)
}

// maskedFiguresOf is maskedFigures, a field's value taken only when typed(value) says the work typed it (nil: every
// value) - a report's screen can be a form the application opened afresh, whose values are its defaults.
func maskedFiguresOf(listing string, typed func(string) bool) (string, map[string]string) {
	figures := map[string]string{}
	var lines []string
	n := 0
	// A table's header row holds no figure, so it was dropped, and the verifier read a row's blanks with no names for
	// its columns - it set a value against a count (found live, 2026-10-25). The header goes in front of the rows it
	// heads.
	header := ""
	for _, line := range strings.Split(listing, "\n") {
		// The listing's own header (the screen's size, the window's title, the other windows and apps by name, its notes)
		// names things, it files nothing; nor does a heading or a control's name ("5 - Large Invoices" on a link is the
		// table's number - found live 2026-10-25, it shifted the verifier's reading of the counts beside it).
		if activeWindowRe.MatchString(line) || listingHeaderRe.MatchString(line) || labelLineRe.MatchString(line) {
			header = ""
			continue
		}
		if !digitRe.MatchString(inlineRefRe.ReplaceAllString(line, "")) {
			if tableRowRe.MatchString(line) {
				header = line
			} else {
				header = ""
			}
			continue
		}
		// Only what an element says is worked out: its quoted text - of a field, its value, never its label - not the
		// line's states ("[row 3]") or notes ("61 chars, 2 lines"). Found live (2026-10-25): the check at a report put a
		// form's section numbers ("(A)(3) ... other than 1 & 2") and the filing's clock time to the person as figures.
		spans := quotedRe.FindAllStringIndex(line, -1)
		if loc := valueQuotedRe.FindStringIndex(line); loc != nil {
			spans = [][]int{{loc[0] + len(" value="), loc[1]}}
			if typed != nil && !typed(strings.Trim(line[spans[0][0]:spans[0][1]], `"`)) {
				spans = nil // (a value nobody typed: the form's own)
			}
		}
		var b strings.Builder
		last, changed := 0, false
		for _, span := range spans {
			text := line[span[0]:span[1]]
			for _, loc := range figureRe.FindAllStringIndex(text, -1) {
				f := text[loc[0]:loc[1]]
				// A figure stands alone: digits inside a word or a code (A4, FORM-2, 29-North) name the row.
				if strings.HasSuffix(f, "%") || n >= maxVerifyBlanks || joinedToWord(text, loc[0], loc[1]) ||
					numbering(text, loc[0], loc[1]) {
					continue
				}
				n++
				key := fmt.Sprintf("v%d", n)
				figures[key] = f
				changed = true
				b.WriteString(line[last:span[0]+loc[0]] + "[" + key + "]")
				last = span[0] + loc[1]
			}
		}
		if changed {
			if header != "" && tableRowRe.MatchString(line) {
				lines = append(lines, header)
			}
			b.WriteString(line[last:])
			lines = append(lines, b.String())
		}
		header = ""
	}
	return strings.Join(lines, "\n"), figures
}

// quotedRe is an element's quoted text in a screen listing; valueQuotedRe a field's quoted value (agent
// elements.element_line); labelLineRe the line of an element whose text only names: a heading, or a control pressed
// to go somewhere or do something.
var (
	quotedRe      = regexp.MustCompile(`"[^"\n]*"`)
	valueQuotedRe = regexp.MustCompile(` value="[^"\n]*"`)
	labelLineRe   = regexp.MustCompile(`^\s*(?:e\d+\s+)?(?:heading|link|button|tab|menuitem)\b`)
	// tableRowRe is a table row's line: three cells or more, joined by " | " (agent elements: a row's name).
	tableRowRe = regexp.MustCompile(`"[^"\n]*\|[^"\n]*\|[^"\n]*"`)
)

// numbering reports whether s[i:j] is an item's number in parentheses ("(3)", "(A)(12)"), not an amount: one or two
// digits, nothing else between the brackets.
func numbering(s string, i, j int) bool {
	return j-i <= 2 && i > 0 && j < len(s) && s[i-1] == '(' && s[j] == ')'
}

// joinedToWord reports whether s[i:j] touches a letter, or a - _ / leading to one or to more digits, or a : between
// digits: part of a word, a code, or a compound value (a date "10/23/2026", a range, a time "02:40") - one value, never
// figures one by one. Found live (2026-10-23): a page's date picker "10/23/2026" had its first part checked as a figure.
func joinedToWord(s string, i, j int) bool {
	isWordy := func(r rune) bool { return unicode.IsLetter(r) || r == '_' }
	if i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		if isWordy(r) || r == '-' || r == '/' {
			return true
		}
		if r == ':' && i-size > 0 {
			if r0, _ := utf8.DecodeLastRuneInString(s[:i-size]); unicode.IsDigit(r0) {
				return true
			}
		}
	}
	if j < len(s) {
		r, size := utf8.DecodeRuneInString(s[j:])
		if isWordy(r) {
			return true
		}
		if (r == '-' || r == '/' || r == ':') && j+size < len(s) {
			if r2, _ := utf8.DecodeRuneInString(s[j+size:]); isWordy(r2) || unicode.IsDigit(r2) {
				return true
			}
		}
	}
	return false
}

// verifierSystem is the independent verifier's brief.
const verifierSystem = `You check, independently, the figures a computer worker is about to file, pay or send. You are given the source documents and rows the work was shown, the person's rules and what they taught, and the screen about to be committed - with every figure on it replaced by a blank like [v3].

For each blank, work out from the sources and rules what it should be. Fill a blank only when the sources and rules settle it; leave it null when they do not (a reference number, a balance the sources do not show, a figure from a system you cannot see). Do not guess from the layout. Leave null too a blank that names rather than says how much: a section, item or row number, a date or time, an acknowledgement number.

Reply with JSON only: {"v1": "12,345.00", "v2": null, ...}`

// activeWindowRe is the window a screen listing is of (its first line: `Screen 1280x720. Active window: "..."`).
var activeWindowRe = regexp.MustCompile(`Active window:\s+"([^"\n]{1,200})"`)

// listingWindow is the active window a screen listing names, or "".
func listingWindow(listing string) string {
	if m := activeWindowRe.FindStringSubmatch(listing); m != nil {
		return m[1]
	}
	return ""
}

// sameWindow reports whether two window titles name one window - either may be cut short ("…") where it was quoted.
func sameWindow(a, b string) bool {
	a = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(a), "…"))
	b = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(b), "…"))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// actionWindow is the window a gate's reason says the action acts on ("click button X (e9) in "<window>": ..."), or "".
func actionWindow(action string) string {
	if m := windowTitleRe.FindStringSubmatch(action); m != nil {
		return m[1]
	}
	return ""
}

// actionListing is the newest screen listing of the window the action acts on (any window when the reason names
// none), cut to that window's own part (windowPart), or "" when no listing of it was taken.
func (st *operatorState) actionListing(action string) string {
	window := actionWindow(action)
	for i := len(st.Listings) - 1; i >= 0; i-- {
		if window == "" || sameWindow(listingWindow(st.Listings[i]), window) {
			return windowPart(st.Listings[i], window)
		}
	}
	return ""
}

// listingHeaderRe is a line of a screen listing's header that is not the screen itself: the open windows, the apps on
// the desktop, a note (agent elements.render).
var listingHeaderRe = regexp.MustCompile(`^(?:Open windows:|Apps on this desktop|Note: )`)

// groupHeaderRe is the line a screen listing opens each window's elements with ("== <window> ==").
var groupHeaderRe = regexp.MustCompile(`^== (.+) ==\s*$`)

// windowPart is a screen listing cut to one window: the lines before the first window's elements (the screen, the
// open windows, notes) and the group of elements whose header names that window - never the other windows a listing
// also shows. Found live (2026-10-24): the check before a filing worked out the figures of a file manager's status bar
// behind the browser ("files: ... KiB ... Free space: ... GiB") and four meaningless points went to the person. The
// whole listing when no group is that window's.
func windowPart(listing, window string) string {
	if window == "" {
		return listing
	}
	var kept []string
	grouped, in, found := false, false, false
	for _, line := range strings.Split(listing, "\n") {
		if m := groupHeaderRe.FindStringSubmatch(line); m != nil {
			grouped = true
			in = sameWindow(m[1], window) || strings.Contains(m[1], window)
			found = found || in
		}
		if !grouped || in {
			kept = append(kept, line)
		}
	}
	if !found {
		return listing
	}
	return strings.Join(kept, "\n")
}

// commitSources is what an independent check before a commit is given of the goal: the request, what the person said,
// their rules and what they taught, and the documents and sheet rows the work read (newest first, within
// maxVerifySourceChars) - never the worker's own account of them.
func (st *operatorState) commitSources() string {
	var src strings.Builder
	fmt.Fprintf(&src, "THE REQUEST:\n%s\n", truncateText(st.Request, maxReviewChars))
	if said := st.personWords(); len(said) > 0 {
		src.WriteString("\nWHAT THE PERSON SAID:\n- " + tailText(strings.Join(said, "\n- "), maxReviewMemoryChars) + "\n")
	}
	if len(st.Messages) > 0 {
		if prefix := st.Messages[:st.prefixEnd()]; len(prefix) > 0 && prefix[0].Role == "system" {
			memory := strings.TrimSpace(strings.TrimPrefix(prefix[0].Content, prompt.Persona))
			src.WriteString("\nTHE PERSON'S RULES AND LESSONS:\n" + tailText(memory, maxReviewMemoryChars) + "\n")
		}
	}
	if len(st.Taught) > 0 {
		src.WriteString("\nWHAT THE PERSON TAUGHT:\n" + strings.Join(st.Taught, "\n\n") + "\n")
	}
	src.WriteString("\nTHE SOURCES (documents read, and rows of sheets read):\n")
	used := 0
	for i := len(st.Units) - 1; i >= 0 && used < maxVerifySourceChars; i-- {
		u := st.Units[i]
		if !sourceKind(u.Kind) {
			continue
		}
		piece := fmt.Sprintf("--- %s\n%s\n", u.Source, u.Text)
		used += len(piece)
		src.WriteString(piece)
	}
	return src.String()
}

// sourceKind reports whether evidence of a kind is a source a check before a commit reads: a document, its text layer,
// a sheet row - not a screen (the thing being checked) or a command's working.
func sourceKind(kind string) bool {
	return kind == "document" || kind == "ocr" || kind == "row"
}

// verifyCommit is the verifier's disagreements with the screen about to be committed, as recheck points; none when
// there is nothing to verify or the check cannot be made (a check that cannot be made decides nothing). The screen is
// the window the action acts on (named in action, the gate's reason): found live (2026-10-23), the last listing was of
// an image viewer the worker had opened a scan in, and the person was asked about its zoom and pixel figures before a
// filing on another window.
func (o *OperatorSkill) verifyCommit(rc RunContext, st *operatorState, action string) []reviewIssue {
	if rc.Provider == nil || !st.Hosted {
		return nil
	}
	window := actionWindow(action)
	var masked string
	var figures map[string]string
	for i := len(st.Listings) - 1; i >= 0; i-- {
		if window != "" && !sameWindow(listingWindow(st.Listings[i]), window) {
			continue
		}
		// Only that window's own figures: a listing shows every open window, each in its own group (windowPart).
		own := window
		if own == "" {
			own = listingWindow(st.Listings[i])
		}
		if m, f := maskedFigures(windowPart(st.Listings[i], own)); len(f) >= minBlanksToVerifyWith {
			masked, figures = m, f
			break
		}
	}
	if figures == nil {
		return nil
	}
	heading := "THE SCREEN ABOUT TO BE COMMITTED"
	if action == "" {
		heading = "THE SCREEN THE WORK LEAVES, AS IT REPORTS IT DONE"
	}
	return o.verifyFigures(rc, st, masked, figures, heading, action)
}

// verifyFigures is the verifier's disagreements with one screen's figures (masked, the screen with each figure a
// numbered blank; figures, the figures by blank), worked out from the sources by a call that does not see them; heading
// says what screen it is. action is what the gate stopped, "" at a report.
func (o *OperatorSkill) verifyFigures(rc RunContext, st *operatorState, masked string, figures map[string]string, heading, action string) []reviewIssue {
	var src strings.Builder
	src.WriteString(st.commitSources())
	src.WriteString("\n" + heading + " (figures blanked):\n" + masked + "\n")

	facts := llm.Message{Role: "user", Content: src.String()}
	facts.Images, facts.ImageLabels = st.documentPicturesKept(3)
	// The same blanked screen against the same sources was worked out already (a later report round, an approval asked
	// again): that working stands - the verifier never sees the figures, so it is set against the screen's figures now.
	sum := sha256.Sum256([]byte(src.String() + "\x00" + strings.Join(facts.ImageLabels, "\x00")))
	cacheKey := hex.EncodeToString(sum[:12])
	derived, seen := st.VerifyCache[cacheKey]
	if seen {
		rc.Logf("operator", "the same screen and sources were worked out already - that working stands")
	} else {
		resp, err := rc.Provider.Chat(rc.Ctx, rc.Model, []llm.Message{{Role: "system", Content: verifierSystem}, facts},
			llm.Options{TimeoutMs: verifyTimeoutMs, Temperature: reviewTemperature})
		if err != nil {
			rc.Logf("operator", "could not verify the figures before the action: %v", err)
			return nil
		}
		text := resp.Text
		if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
			text = text[i : j+1]
		}
		var answered map[string]*string
		if json.Unmarshal([]byte(text), &answered) != nil {
			rc.Logf("operator", "the verifier's answer could not be read")
			return nil
		}
		derived = map[string]string{}
		for k, v := range answered {
			if v != nil && strings.TrimSpace(*v) != "" {
				derived[k] = *v
			}
		}
		if st.VerifyCache == nil || len(st.VerifyCache) >= maxVerifyCache {
			st.VerifyCache = map[string]map[string]string{}
		}
		st.VerifyCache[cacheKey] = derived
	}
	lines := strings.Split(masked, "\n")
	before := "the action"
	if action == "" {
		before = "reporting"
	}
	var out []reviewIssue
	for i := 1; i <= len(figures) && len(out) < maxVerifyMismatches; i++ {
		key := fmt.Sprintf("v%d", i)
		want, shown := strings.TrimSpace(derived[key]), figures[key]
		if want == "" || sameValue(shown, want) {
			continue
		}
		// A figure answered with words ("Background") is no working of it: nothing to set against the screen.
		if _, isNumber := canonicalNumber(strings.TrimSuffix(strings.TrimSpace(shown), "%")); isNumber {
			if _, wantNumber := canonicalNumber(strings.TrimSuffix(want, "%")); !wantNumber {
				continue
			}
		}
		where := ""
		for _, l := range lines {
			if strings.Contains(l, "["+key+"]") {
				where = truncateText(strings.Join(strings.Fields(l), " "), 200)
				break
			}
		}
		out = append(out, reviewIssue{
			What:   fmt.Sprintf("The screen shows %s where an independent working from the sources gives %s", shown, want),
			Why:    "at " + where + " - check which is right against the documents before " + before,
			Settle: "recheck",
		})
	}
	if len(out) > 0 {
		rc.Logf("operator", "the independent verifier disagrees with %d figure(s) on the screen", len(out))
	}
	return out
}

// maxReportScreens bounds the screens whose figures are worked out again at a report (verifyReport).
const maxReportScreens = 3

// verifyReport is the verifier's disagreements, at a report, with the screens the work typed or chose in: the screen
// it leaves (looked at now when the work went on past its last look), when that is of such a window or of the same
// site or app as one, and the newest look at each other window it typed or chose in - newest first, at most
// maxReportScreens. Found live (2026-10-25): a return keyed over several pages of one site was checked only on its last
// page; a table typed two pages earlier as one lump across states and rates went to the person unchecked, while the
// summary page the work left showed its wrong tax beside the right one.
func (o *OperatorSkill) verifyReport(rc RunContext, st *operatorState) []reviewIssue {
	if rc.Provider == nil || !st.Hosted || len(st.TypedWindows) == 0 {
		return nil
	}
	o.lookAtReport(rc, st)
	var out []reviewIssue
	checked := 0
	for i := len(st.Listings) - 1; i >= 0 && checked < maxReportScreens; i-- {
		window := listingWindow(st.Listings[i])
		leaves := i == len(st.Listings)-1
		if !st.typedIn(window) && !(leaves && st.sameSiteAsTyped(window)) {
			continue
		}
		// A look taken before the window was typed in shows the form as it was (its defaults), not the work: working
		// its figures out set the sources against a form's "1" - found live (2026-10-25), six points about a bill
		// number the form showed before the worker typed it. Such a window is looked at again first (unseenTyping).
		if !leaves && st.typedSinceLook(window) {
			continue
		}
		masked, figures := maskedFiguresOf(windowPart(st.Listings[i], window), st.typedByWork)
		if len(figures) < minBlanksToVerifyWith {
			continue
		}
		checked++
		heading := "THE SCREEN THE WORK LEAVES, AS IT REPORTS IT DONE"
		if !leaves {
			heading = "A SCREEN THE WORK TYPED INTO, AS IT WAS LAST LOOKED AT (it may have been changed since)"
		}
		if window != "" {
			heading += fmt.Sprintf(" - the window %q", window)
		}
		out = append(out, o.verifyFigures(rc, st, masked, figures, heading, "")...)
	}
	return out
}

// lookAtReport looks at the screen now (a read-only gui observe) unless the work's last command did: a report rests on
// the screen the work leaves, and the work often clicks on (a summary, a save) after its last look.
func (o *OperatorSkill) lookAtReport(rc RunContext, st *operatorState) {
	if n := len(st.Worked); n > 0 && listsScreen(st.Worked[n-1]) {
		return
	}
	res := runShell(rc.Ctx, rc.Dir(), "gui observe", 60*time.Second, st.guiEnv()...)
	if res.ExitCode != 0 || strings.TrimSpace(res.Output) == "" {
		return
	}
	st.noteListing("gui observe", res.Output)
	st.noteEvidence("gui observe", res.Output)
}

// activeNowRe and windowOpenedRe are what a gui action prints when the active window changed or a window opened (the
// agent's computer.core.summarize).
var (
	activeNowRe    = regexp.MustCompile(`active window is now "([^"\n]{1,200})"`)
	windowOpenedRe = regexp.MustCompile(`window "([^"\n]{1,200})" opened`)
)

// noteWindow follows the active window through a command's output (a screen listing names it; an action says when it
// changed), and notes it among the windows typed or chosen in when the command typed or chose on the screen and went
// through.
func (st *operatorState) noteWindow(command string, res shellResult) {
	before := st.ActiveWindow
	if listsScreen(command) {
		if w := listingWindow(res.Output); w != "" {
			st.ActiveWindow = w
		}
	} else {
		at := -1
		for _, re := range []*regexp.Regexp{activeNowRe, windowOpenedRe} {
			for _, m := range re.FindAllStringSubmatchIndex(res.Output, -1) {
				if m[0] > at {
					at, st.ActiveWindow = m[0], res.Output[m[2]:m[3]]
				}
			}
		}
	}
	if res.ExitCode != 0 || !typedOnScreenRe.MatchString(command) {
		return
	}
	// The typing acts on the window the command began in (its refs are that screen's); the window it moved to also
	// counts when typing comes after another step of the command (gui click e2 && gui type e9 ...).
	lastTyping, firstOther := -1, -1
	for i, seg := range shellSegments(command) {
		if typedOnScreenRe.MatchString(seg.Text) {
			lastTyping = i
		} else if firstOther < 0 {
			firstOther = i
		}
	}
	if before != "" {
		st.noteTypedWindow(before)
	}
	if st.ActiveWindow != "" && (before == "" || (firstOther >= 0 && lastTyping > firstOther)) {
		st.noteTypedWindow(st.ActiveWindow)
	}
}

// noteTypedWindow puts a window last among those typed or chosen in, once, and notes when.
func (st *operatorState) noteTypedWindow(window string) {
	kept := st.TypedWindows[:0]
	for _, w := range st.TypedWindows {
		if !sameWindow(w, window) {
			kept = append(kept, w)
		}
	}
	st.TypedWindows = append(kept, window)
	if st.TypedAt == nil {
		st.TypedAt = map[string]int{}
	}
	st.TypedAt[window] = len(st.Worked)
}

// openWindowsLineRe is a screen listing's line of the open windows (agent elements.render).
var openWindowsLineRe = regexp.MustCompile(`(?m)^Open windows: (.+)$`)

// quotedTitleRe is one quoted title on that line.
var quotedTitleRe = regexp.MustCompile(`"([^"\n]+)"`)

// openWindows are the windows a screen listing shows open: the active one and the line of the others.
func openWindows(listing string) []string {
	var out []string
	if w := listingWindow(listing); w != "" {
		out = append(out, w)
	}
	if m := openWindowsLineRe.FindStringSubmatch(listing); m != nil {
		for _, t := range quotedTitleRe.FindAllStringSubmatch(m[1], -1) {
			out = append(out, t[1])
		}
	}
	return out
}

// maxUnseenTyping bounds the windows a report is told to look at again (unseenTyping).
const maxUnseenTyping = 3

// unseenTyping are points for the windows the work typed in after its last look at them: the check of a page's
// figures reads its newest look, and a look from before the typing shows none of it. Found live (2026-10-25): a
// summary's rows were looked at only while empty; typed wrong (one invoice's goods lines under a service code), they
// were never seen again, and nothing set them against the bills.
func (st *operatorState) unseenTyping() []reviewIssue {
	if len(st.Listings) == 0 {
		return nil
	}
	// Only a window that can still be looked at: open now, or a page of a site open now - never a dialog that closed.
	open := openWindows(st.Listings[len(st.Listings)-1])
	reachable := func(w string) bool {
		for _, o := range open {
			if sameWindow(o, w) || sameWindow(siteOf(o), siteOf(w)) {
				return true
			}
		}
		return false
	}
	var out []reviewIssue
	for i := len(st.TypedWindows) - 1; i >= 0 && len(out) < maxUnseenTyping; i-- {
		w := st.TypedWindows[i]
		if !reachable(w) {
			continue
		}
		typed, listed := st.TypedAt[w], -1
		for lw, at := range st.ListedAt {
			if sameWindow(lw, w) && at > listed {
				listed = at
			}
		}
		if typed == 0 || listed >= typed {
			continue
		}
		out = append(out, reviewIssue{
			What: fmt.Sprintf("What was typed into %q was not looked at after it was typed", w),
			Why: "open it and look at it (gui observe) before reporting - nothing has seen what it holds now, so " +
				"nothing has checked it against the sources",
			Settle: "recheck",
		})
	}
	return out
}

// typedByWork reports whether the work typed or chose a value (any gui type or select of the goal, the same number or
// text however written).
func (st *operatorState) typedByWork(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, c := range append(append([]string(nil), st.Worked...), st.Commands...) {
		for _, v := range typedValues(c) {
			if sameValue(v, value) {
				return true
			}
		}
	}
	return false
}

// typedSinceLook reports whether the work typed or chose in the window after its last look at it.
func (st *operatorState) typedSinceLook(window string) bool {
	typed, listed := 0, -1
	for w, at := range st.TypedAt {
		if sameWindow(w, window) && at > typed {
			typed = at
		}
	}
	for w, at := range st.ListedAt {
		if sameWindow(w, window) && at > listed {
			listed = at
		}
	}
	return typed > 0 && listed < typed
}

// typedIn reports whether the work typed or chose in the window (noteWindow).
func (st *operatorState) typedIn(window string) bool {
	for _, w := range st.TypedWindows {
		if sameWindow(w, window) {
			return true
		}
	}
	return false
}

// sameSiteAsTyped reports whether the window is of the same site or app as one the work typed or chose in (siteOf).
func (st *operatorState) sameSiteAsTyped(window string) bool {
	site := siteOf(window)
	if site == "" {
		return false
	}
	for _, w := range st.TypedWindows {
		if sameWindow(siteOf(w), site) { // a title quoted in an action's output may be cut short
			return true
		}
	}
	return false
}

// appSuffixRe is a window title's trailing program name ("... - Chromium", "... — Mozilla Firefox").
var appSuffixRe = regexp.MustCompile(`\s+[-—]\s+[^-—|]+$`)

// siteOf is the part of a window title that names its site or document, past the page's own name and the program:
// "Summary | Portal - Chromium" and "Sales | Portal - Chromium" give "Portal"; "book.xlsx - LibreOffice Calc" gives
// "book.xlsx". "" when too short to tell.
func siteOf(title string) string {
	t := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), "…"))
	t = appSuffixRe.ReplaceAllString(t, "")
	for _, sep := range []string{" | ", " · ", " – "} {
		if i := strings.LastIndex(t, sep); i >= 0 {
			t = t[i+len(sep):]
		}
	}
	if t = strings.TrimSpace(t); len(t) < 3 {
		return ""
	}
	return t
}

// sameValue reports whether two written values are the same: numbers by their digits, anything else as text.
func sameValue(a, b string) bool {
	if x, ok := canonicalNumber(strings.TrimSuffix(strings.TrimSpace(a), "%")); ok {
		if y, ok := canonicalNumber(strings.TrimSuffix(strings.TrimSpace(b), "%")); ok {
			return x == y
		}
	}
	return normalise(a) == normalise(b)
}

// documentPicturesKept are the last n document pictures still in the transcript (scans the verifier reads too).
func (st *operatorState) documentPicturesKept(n int) ([][]byte, []string) {
	if !st.Vision {
		return nil, nil
	}
	var imgs [][]byte
	var labels []string
	for i := len(st.Messages) - 1; i >= 0 && len(imgs) < n; i-- {
		m := st.Messages[i]
		if !strings.Contains(m.Content, pagesNote) {
			continue
		}
		for j := len(m.Images) - 1; j >= 0 && len(imgs) < n; j-- {
			imgs = append([][]byte{m.Images[j]}, imgs...)
			labels = append([]string{m.ImageLabel(j)}, labels...)
		}
	}
	return imgs, labels
}

// ocrPages gives the pictures of document pages an office read showed a text layer: each page is read by OCR and
// kept as a unit of its document, and as seen - so the values typed from a scan are traced like any other, and one
// that only the picture shows stands out (notePictureOnly). Found live: once any page had come as a picture, nothing
// typed was traced at all.
func (o *OperatorSkill) ocrPages(rc RunContext, st *operatorState, output string) {
	headers := documentHeaderRe.FindAllStringSubmatchIndex(output, -1)
	n := 0
	for _, m := range pageImageRe.FindAllStringSubmatchIndex(output, -1) {
		if n >= maxDocumentPictures {
			break
		}
		n++
		pic := strings.TrimSpace(output[m[2]:m[3]])
		doc := path.Base(pic)
		for _, h := range headers {
			if h[0] < m[0] {
				doc = output[h[2]:h[3]]
			}
		}
		env := append(st.guiEnv(), "DECURIX_OFFICE_IMAGES=ocr")
		res := runShell(rc.Ctx, rc.Dir(), "office read '"+strings.ReplaceAll(pic, "'", `'\''`)+"'", 90*time.Second, env...)
		if res.ExitCode == 0 && strings.TrimSpace(res.Output) != "" {
			st.addUnit("ocr", path.Base(doc), res.Output)
			st.noteSeen(res.Output)
		}
	}
}
