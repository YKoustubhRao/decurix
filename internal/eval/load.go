package eval

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

var (
	idPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	collectionPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	hostPattern       = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252}[A-Za-z0-9])?$`)
	isoDate           = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// rolePattern is a control's role as the gate words it (button, link, push button): what GateControl reads.
	rolePattern = regexp.MustCompile(`^[a-z][a-z ]{0,39}$`)
)

// Problems is every reason a suite cannot be run, in the order found.
type Problems []string

func (p Problems) Error() string {
	if len(p) == 1 {
		return p[0]
	}
	return fmt.Sprintf("%d problems: %s", len(p), strings.Join(p, "; "))
}

// entries is a suite tar read into memory, by cleaned path.
type entries map[string][]byte

// readTar reads a suite tar: regular files and folders only, flat cleaned paths, nothing repeated, bounded in size.
func readTar(b []byte) (entries, Problems) {
	var problems Problems
	out := entries{}
	tr := tar.NewReader(bytes.NewReader(b))
	total := int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, append(problems, "The suite is not a readable tar.")
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag == tar.TypeDir {
			continue
		}
		clean := path.Clean(name)
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || clean != strings.TrimSuffix(name, "/") || strings.HasPrefix(clean, "..") {
			problems = append(problems, fmt.Sprintf("The tar entry %q is not a plain relative path.", h.Name))
			continue
		}
		if h.Typeflag != tar.TypeReg {
			problems = append(problems, fmt.Sprintf("The tar entry %s is not a regular file.", clean))
			continue
		}
		total += h.Size
		if total > MaxSuiteBytes {
			return out, append(problems, fmt.Sprintf("The suite is larger than %d MB.", MaxSuiteBytes>>20))
		}
		body, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return out, append(problems, "The suite is not a readable tar.")
		}
		if _, dup := out[clean]; dup {
			problems = append(problems, fmt.Sprintf("The tar holds %s twice.", clean))
			continue
		}
		out[clean] = body
	}
	return out, problems
}

// decodeStrict decodes one JSON document, refusing unknown fields and trailing data.
func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data after the document")
	}
	return nil
}

// Load reads and validates a suite tar (layout in docs/evals.md). It returns every problem it finds, as Problems,
// not the first; a suite with any problem is never run.
func Load(tarBytes []byte) (Suite, error) {
	files, problems := readTar(tarBytes)
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	var s Suite
	raw, ok := files["suite.json"]
	if !ok {
		add("The suite has no suite.json.")
		return s, problems
	}
	if err := decodeStrict(raw, &s); err != nil {
		add("suite.json is not a valid suite document: %v.", err)
		return s, problems
	}
	if s.Schema != SuiteSchema {
		add("suite.json must name the schema %q.", SuiteSchema)
	}
	if !idPattern.MatchString(s.Name) {
		add("The suite name %q is not a lowercase id.", s.Name)
	}
	if s.Budget.MaxTokens <= 0 || s.Budget.MaxMinutes <= 0 || s.Budget.MaxMinutes > MaxCaseMinutes {
		add("The suite's budget needs max_tokens above 0 and max_minutes between 0 and %d.", MaxCaseMinutes)
	}
	if len(s.CaseIDs) == 0 || len(s.CaseIDs) > MaxCases {
		add("The suite lists %d cases; it needs between 1 and %d.", len(s.CaseIDs), MaxCases)
	}
	s.MockSources = map[string][]byte{}
	for name, m := range s.Mocks {
		if !idPattern.MatchString(name) {
			add("The mock name %q is not a lowercase id.", name)
			continue
		}
		src, ok := files["mocks/"+name+"/mock_app.py"]
		if !ok || len(src) == 0 {
			add("The mock %s has no mocks/%s/mock_app.py in the tar.", name, name)
		} else if !utf8.Valid(src) {
			add("The mock %s's mock_app.py is not UTF-8 text.", name)
		}
		s.MockSources[name] = src
		if len(m.Hosts) == 0 {
			add("The mock %s names no hosts.", name)
		}
		for _, h := range m.Hosts {
			if !hostPattern.MatchString(h) {
				add("The mock %s's host %q is not a host name.", name, h)
			}
		}
	}
	listed := map[string]bool{}
	for _, id := range s.CaseIDs {
		if listed[id] {
			add("The suite lists the case %s twice.", id)
			continue
		}
		listed[id] = true
		if !idPattern.MatchString(id) {
			add("The case id %q is not a lowercase id.", id)
			continue
		}
		c, caseProblems := loadCase(s, id, files)
		c.ConfirmsPlans = s.ConfirmsPlans
		problems = append(problems, caseProblems...)
		s.Cases = append(s.Cases, c)
	}
	// Every entry belongs somewhere: an unknown one is a mistake in the suite, not something to skip quietly.
	for _, name := range sortedNames(files) {
		switch {
		case name == "suite.json":
		case strings.HasPrefix(name, "mocks/"):
			parts := strings.Split(name, "/")
			_, named := s.Mocks[parts[1]]
			if len(parts) != 3 || parts[2] != "mock_app.py" || !named {
				add("The tar entry %s is not a mock the suite names.", name)
			}
		case strings.HasPrefix(name, "cases/"):
			parts := strings.Split(name, "/")
			if len(parts) < 3 || !listed[parts[1]] {
				add("The tar entry %s is not in a case the suite lists.", name)
			}
		default:
			add("The tar entry %s is not part of the suite layout.", name)
		}
	}
	s.SHA = suiteSHA(files)
	if len(problems) > 0 {
		return s, problems
	}
	return s, nil
}

// loadCase reads and validates one case folder.
func loadCase(s Suite, id string, files entries) (Case, Problems) {
	var problems Problems
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("Case %s: ", id)+fmt.Sprintf(format, args...))
	}
	dir := "cases/" + id + "/"
	c := Case{ID: id, Files: map[string][]byte{}}
	raw, ok := files[dir+"case.json"]
	if !ok {
		add("there is no %scase.json.", dir)
		return c, problems
	}
	if err := decodeStrict(raw, &c); err != nil {
		add("case.json is not a valid case document: %v.", err)
		return Case{ID: id, Files: map[string][]byte{}}, problems
	}
	c.Files = map[string][]byte{}
	if c.Schema != CaseSchema {
		add("case.json must name the schema %q.", CaseSchema)
	}
	if c.ID != id {
		add("case.json says its id is %q, but its folder is %s.", c.ID, id)
		c.ID = id
	}
	if !knownCategory(c.Category) {
		add("the category %q is not one of %s.", c.Category, strings.Join(qualification.PracticeCategories, ", "))
	}
	if strings.TrimSpace(c.Kind) == "" || len(c.Kind) > 64 {
		add("needs a kind (a short label for grouping).")
	}
	if strings.TrimSpace(c.Request) == "" || len(c.Request) > MaxRequestBytes || !utf8.ValidString(c.Request) {
		add("needs a request of at most %d bytes.", MaxRequestBytes)
	}
	withheld := func(text string) string {
		for _, sec := range c.OracleSecrets {
			if sec != "" {
				text = strings.ReplaceAll(text, sec, "")
			}
		}
		return text
	}
	for _, sec := range c.OracleSecrets {
		if len(strings.TrimSpace(sec)) < 4 {
			add("an oracle secret must be at least 4 characters.")
		}
	}
	if qualification.CredentialLike(withheld(c.Request)) {
		add("the request looks like it holds a credential.")
	}
	// Input files: each named one exists, has a flat name, and holds nothing credential-shaped.
	caseBytes := 0
	named := map[string]bool{}
	for _, name := range c.Start.Files {
		if named[name] {
			add("start.files names %s twice.", name)
			continue
		}
		named[name] = true
		if !qualification.FileName(name) {
			add("the input file name %q is not a flat file name.", name)
			continue
		}
		b, ok := files[dir+"files/"+name]
		if !ok {
			add("the input file %s is not in %sfiles/.", name, dir)
			continue
		}
		if len(b) > qualification.MaxFileBytes {
			add("the input file %s is larger than %d MB.", name, qualification.MaxFileBytes>>20)
		}
		caseBytes += len(b)
		if utf8.Valid(b) && qualification.CredentialLike(withheld(string(b))) {
			add("the input file %s looks like it holds a credential.", name)
		}
		c.Files[name] = b
	}
	for name := range files {
		if rest, ok := strings.CutPrefix(name, dir+"files/"); ok && !named[rest] {
			add("the file %s is in files/ but not in start.files.", rest)
		}
	}
	// The mock and its start state.
	if c.Start.Mock != "" {
		if _, ok := s.Mocks[c.Start.Mock]; !ok {
			add("start.mock names %q, which the suite does not list.", c.Start.Mock)
		}
	}
	if c.Start.Fixture != "" {
		if c.Start.Mock == "" {
			add("a fixture needs start.mock.")
		}
		if !qualification.FileName(c.Start.Fixture) || c.Start.Fixture == "case.json" {
			add("start.fixture %q is not a flat file name.", c.Start.Fixture)
		} else if b, ok := files[dir+c.Start.Fixture]; !ok {
			add("the fixture %s is not in the case folder.", c.Start.Fixture)
		} else {
			var v any
			if len(b) > qualification.MaxFileBytes || json.Unmarshal(b, &v) != nil {
				add("the fixture is not JSON of at most %d MB.", qualification.MaxFileBytes>>20)
			}
			if qualification.CredentialLike(withheld(string(b))) {
				add("the fixture looks like it holds a credential.")
			}
			caseBytes += len(b)
			c.Fixture = b
		}
	}
	for name := range files {
		rest, ok := strings.CutPrefix(name, dir)
		if !ok || rest == "case.json" || rest == c.Start.Fixture || strings.HasPrefix(rest, "files/") {
			continue
		}
		add("the case folder holds %s, which the case does not name.", rest)
	}
	if caseBytes > qualification.MaxPracticeCaseBytes {
		add("its inputs exceed %d MB.", qualification.MaxPracticeCaseBytes>>20)
	}
	// Facts.
	if len(c.Facts) > MaxFacts {
		add("has more than %d facts.", MaxFacts)
	}
	factIDs := map[string]bool{}
	for _, f := range c.Facts {
		if !idPattern.MatchString(f.ID) || factIDs[f.ID] {
			add("the fact id %q is invalid or repeated.", f.ID)
		}
		factIDs[f.ID] = true
		if strings.TrimSpace(f.About) == "" || strings.TrimSpace(f.Answer) == "" || len(f.Answer) > 4000 || len(f.About) > 400 {
			add("the fact %s needs what it is about and its answer.", f.ID)
		}
		if f.Required == nil {
			add("the fact %s must say \"required\": true or false.", f.ID)
		}
		if len(f.RefersTo) == 0 || len(f.RefersTo) > MaxRefersTo {
			add("the fact %s needs refers_to: 1 to %d phrases a question must use to be about it.", f.ID, MaxRefersTo)
		}
		for _, p := range f.RefersTo {
			if n := len(words(p)); n == 0 || n > 6 || len(p) > 80 {
				add("the fact %s's refers_to %q must be 1 to 6 words.", f.ID, clipText(p, 80))
			}
		}
		// The ask arm's question is one any worker could ask of any case: no fact may be about it, so asking it
		// never passes a case whichever model serves the expert.
		if f.NamedBy(AskAnything) {
			add("the fact %s's refers_to matches the ask arm's question (%q), which is about nothing.", f.ID, AskAnything)
		}
		if qualification.CredentialLike(withheld(f.Answer)) {
			add("the fact %s's answer looks like it holds a credential.", f.ID)
		}
	}
	// Approvals the person gives: each a click on a named control, matched against the gate's own question.
	if len(c.Approves) > MaxApproves {
		add("approves more than %d actions.", MaxApproves)
	}
	approved := map[string]bool{}
	for i, a := range c.Approves {
		name := strings.Join(strings.Fields(a.Control), " ")
		if name == "" || utf8.RuneCountInString(name) > MaxControlName || strings.Contains(name, `"`) || !utf8.ValidString(name) {
			add("approval %d needs the control a click acts on: 1 to %d characters, no double quote.", i+1, MaxControlName)
		}
		if a.Role != "" && !rolePattern.MatchString(a.Role) {
			add("approval %d's role %q must be 1 to 40 lower-case letters and spaces (button, link).", i+1, clipText(a.Role, 80))
		}
		key := strings.ToLower(name) + "|" + strings.ToLower(a.Role)
		if approved[key] {
			add("approval %d is listed twice.", i+1)
		}
		approved[key] = true
		if qualification.CredentialLike(withheld(a.Control)) {
			add("approval %d looks like it holds a credential.", i+1)
		}
	}
	if len(c.Approves) > 0 && c.Expected.Outcome == OutcomeApproval {
		add("a case that expects the worker to stop at the approval gate approves nothing.")
	}
	// Budget: a case may only lower the suite's.
	if c.Budget.MaxTokens < 0 || c.Budget.MaxMinutes < 0 ||
		(c.Budget.MaxTokens > 0 && c.Budget.MaxTokens > s.Budget.MaxTokens) ||
		(c.Budget.MaxMinutes > 0 && c.Budget.MaxMinutes > s.Budget.MaxMinutes) {
		add("its budget may only lower the suite's.")
	}
	// The oracle.
	if len(c.Oracle) == 0 || len(c.Oracle) > MaxOracleSteps {
		add("needs an oracle (a scripted solution) of 1 to %d steps.", MaxOracleSteps)
	}
	for i, st := range c.Oracle {
		switch st.Kind {
		case StepRun, StepApproval:
			if strings.TrimSpace(st.Line) == "" || len(st.Line) > 8000 || strings.ContainsRune(st.Line, 0) {
				add("oracle step %d needs a command line.", i+1)
			}
		case StepSleep:
			if st.Seconds <= 0 || st.Seconds > 60 {
				add("oracle step %d sleeps between 0 and 60 seconds.", i+1)
			}
		}
		if qualification.CredentialLike(withheld(st.Line)) {
			add("oracle step %d looks like it holds a credential; list a demo password under oracle_secrets.", i+1)
		}
	}
	problems = append(problems, validateExpected(c, factIDs)...)
	return c, problems
}

// validateExpected checks the graders' kinds and fields, and the expected outcome against them.
func validateExpected(c Case, factIDs map[string]bool) Problems {
	var problems Problems
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("Case %s: ", c.ID)+fmt.Sprintf(format, args...))
	}
	e := c.Expected
	switch e.Outcome {
	case OutcomeComplete, OutcomeAsked, OutcomeApproval:
	default:
		add("expected.outcome %q is not complete, asked or approval.", e.Outcome)
	}
	for _, a := range e.Accept {
		if a != OutcomeAsked || e.Outcome != OutcomeComplete {
			add("expected.accept may only add \"asked\" to a complete case.")
		}
	}
	if len(e.Graders) == 0 || len(e.Graders) > MaxGraders {
		add("needs between 1 and %d graders.", MaxGraders)
	}
	inputs := map[string]bool{}
	for _, n := range c.Start.Files {
		inputs[n] = true
	}
	ids := map[string]bool{}
	positive := map[string]int{} // outcome -> graders that can fail a claim alone (not absent, not worker-written)
	for i, g := range e.Graders {
		label := g.ID
		if label == "" {
			label = fmt.Sprintf("%s #%d", g.Kind, i+1)
		} else if !idPattern.MatchString(g.ID) || ids[g.ID] {
			add("the grader id %q is invalid or repeated.", g.ID)
		}
		ids[g.ID] = true
		k, ok := graderKinds[g.Kind]
		if !ok {
			add("grader %s: unknown kind %q.", label, g.Kind)
			continue
		}
		switch g.When {
		case "", OutcomeComplete, OutcomeAsked, OutcomeApproval:
		default:
			add("grader %s: when %q is not complete, asked or approval.", label, g.When)
		}
		if msg := validateGrader(g, inputs, factIDs, c.Start.Mock != ""); msg != "" {
			add("grader %s: %s", label, msg)
		}
		if g.Kind != "absent" && k.trust != TrustWorkerWritten {
			positive[g.Scope()]++
		}
	}
	// Every outcome the case accepts needs a grader that can fail it, or any ending of that kind passes: a question
	// accepted as an answer must be the right question (an asked grader names the facts), review EVAL-1 B2.
	for _, outcome := range append([]string{e.Outcome}, e.Accept...) {
		switch outcome {
		case OutcomeComplete:
			if positive[OutcomeComplete] == 0 {
				add("a complete case needs an independent or output grader besides absent, so a claim alone cannot pass.")
			}
		case OutcomeAsked:
			if positive[OutcomeAsked] == 0 {
				add("a case that accepts a question needs an asked grader naming the facts it must be about.")
			}
		case OutcomeApproval:
			if positive[OutcomeApproval] == 0 {
				add("an approval case needs a stopped_at_approval grader.")
			}
		}
	}
	if c.HasRequiredFact() && e.Outcome == OutcomeComplete && positive[OutcomeAsked] == 0 {
		add("a case with a required fact needs an asked grader, for the run without the expert.")
	}
	return problems
}

// validateGrader checks one grader's fields for its kind; "" when it is well formed.
func validateGrader(g Grader, inputs, factIDs map[string]bool, hasMock bool) string {
	fileOK := func() string {
		if !qualification.FileName(g.File) {
			return fmt.Sprintf("file %q is not a flat file name.", g.File)
		}
		if inputs[g.File] {
			return fmt.Sprintf("file %s is an input; grade a file the work saves.", g.File)
		}
		return ""
	}
	switch g.Kind {
	case "mock_records":
		if !hasMock {
			return "mock_records needs the case to start a mock."
		}
		if !collectionPattern.MatchString(g.Collection) {
			return fmt.Sprintf("collection %q is not a collection name.", g.Collection)
		}
		recs, err := g.RecordList()
		if err != nil {
			return err.Error()
		}
		if len(recs) > 500 || (len(recs) == 0 && !g.ExactCount) {
			return "mock_records needs between 1 and 500 records (or none with exact_count)."
		}
		if g.Written != "" && g.Written != "dmy" && g.Written != "mdy" {
			return fmt.Sprintf("written %q is not dmy or mdy.", g.Written)
		}
		return validateRecords(recs)
	case "absent":
		targets := 0
		if g.File != "" {
			targets++
			if msg := fileOK(); msg != "" {
				return msg
			}
		}
		if g.Collection != "" {
			targets++
			if !hasMock || !collectionPattern.MatchString(g.Collection) {
				return "absent of records needs a mock and a collection name."
			}
			recs, err := g.RecordList()
			if err != nil {
				return err.Error()
			}
			if len(recs) > 1 {
				return "absent names one record to look for, or none for \"nothing new, changed or removed\"."
			}
			if msg := validateRecords(recs); msg != "" {
				return msg
			}
		}
		if len(g.To) > 0 {
			targets++
		}
		if g.Drafts {
			targets++
		}
		if targets != 1 {
			return "absent names exactly one of file, collection, to or drafts."
		}
	case "file_text_equals":
		return fileOK()
	case "file_text_contains":
		if len(g.Words)+len(g.Any) == 0 {
			return "file_text_contains needs words or any."
		}
		return fileOK()
	case "json_equals", "csv_equals", "file_cells":
		if msg := fileOK(); msg != "" {
			return msg
		}
		return qualification.ValidateFileCheck(g.fileCheck())
	case "outbox":
		if len(g.To) == 0 {
			return "outbox needs the expected recipients."
		}
	case "final_text_number":
		if _, ok := parseNumber(g.Number); !ok {
			return fmt.Sprintf("number %q is not a number.", g.Number)
		}
	case "final_text_contains":
		if len(g.Words)+len(g.Any) == 0 {
			return "final_text_contains needs words or any."
		}
	case "asked":
		// The right question about the right fact: asking anything at all is not the answer a case accepts.
		if len(g.Facts) == 0 {
			return "asked names the facts the question must be about (facts)."
		}
		for _, f := range g.Facts {
			if !factIDs[f] {
				return fmt.Sprintf("asked names the fact %q, which the case does not have.", f)
			}
		}
	case "stopped_at_approval":
		if strings.TrimSpace(g.ActionContains) == "" {
			return "stopped_at_approval needs action_contains."
		}
	case "ledger_readback":
		n, err := g.RecordCount()
		if err != nil || n < 1 {
			return "ledger_readback needs records: the number of records the worker declares (1 or more)."
		}
	}
	return ""
}

// validateRecords checks expected records: at least one field each, and well-formed typed values.
func validateRecords(recs []map[string]Value) string {
	for i, r := range recs {
		if len(r) == 0 {
			return fmt.Sprintf("record %d names no fields.", i+1)
		}
		for field, v := range r {
			switch v.Kind {
			case ValueNumber:
				if _, ok := parseNumber(v.S); !ok {
					return fmt.Sprintf("record %d's %s is not a number.", i+1, field)
				}
			case ValueDate:
				if _, err := time.Parse("2006-01-02", v.S); err != nil || !isoDate.MatchString(v.S) {
					return fmt.Sprintf("record %d's %s is not a YYYY-MM-DD date.", i+1, field)
				}
			}
		}
	}
	return ""
}

// fileCheck is the grader as readiness's file check.
func (g Grader) fileCheck() qualification.Check {
	kind := g.Kind
	switch kind {
	case "file_text_equals":
		kind = "text_equals"
	case "file_cells":
		kind = "xlsx_cells"
	}
	cells := make([]qualification.CellCheck, len(g.Cells))
	for i, c := range g.Cells {
		cells[i] = qualification.CellCheck(c)
	}
	return qualification.Check{File: g.File, Kind: kind, Text: g.Text, JSON: g.JSON, Rows: g.Rows, Cells: cells}
}

func knownCategory(c string) bool {
	for _, k := range qualification.PracticeCategories {
		if k == c {
			return true
		}
	}
	return false
}

// parseNumber reads a number with separators and a currency sign removed, exactly (every digit).
func parseNumber(s string) (*big.Rat, bool) {
	t := strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range t {
		switch {
		case r == ',' || r == '_' || r == ' ' || r == '\'' || r == ' ' || r == ' ':
		case isCurrency(r):
		default:
			b.WriteRune(r)
		}
	}
	n := b.String()
	if n == "" || strings.ContainsAny(n, "eE/") {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(n)
	return r, ok
}

func isCurrency(r rune) bool { return unicode.Is(unicode.Sc, r) }

// sortedNames lists a tar's entries in order.
func sortedNames(files entries) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func sortStrings(s []string) { sort.Strings(s) }

// canonicalJSON re-encodes a JSON document with sorted keys and no spacing; the bytes themselves when they are not
// JSON (Load reports that separately).
func canonicalJSON(b []byte) []byte {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// isSuiteDocument reports whether a tar path is suite.json or a case's case.json.
func isSuiteDocument(name string) bool {
	if name == "suite.json" {
		return true
	}
	parts := strings.Split(name, "/")
	return len(parts) == 3 && parts[0] == "cases" && parts[2] == "case.json"
}

// suiteSHA is the case-set hash: SHA-256 over the canonical JSON of the suite's own documents (suite.json and each
// case.json, whose spacing means nothing) and the SHA-256 of every other file's exact bytes - inputs and fixtures are
// read byte for byte by the worker and the mock, so a changed byte there is a changed case - by path in sorted order.
// The same files give the same hash whatever order the tar was written in.
func suiteSHA(files entries) string {
	h := sha256.New()
	for _, name := range sortedNames(files) {
		b := files[name]
		var part string
		if isSuiteDocument(name) {
			sum := sha256.Sum256(canonicalJSON(b))
			part = "json:" + hex.EncodeToString(sum[:])
		} else {
			sum := sha256.Sum256(b)
			part = "file:" + hex.EncodeToString(sum[:])
		}
		fmt.Fprintf(h, "%s\x00%s\n", name, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}
