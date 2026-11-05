package skills

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A new folder among siblings that share a naming shape is named like them. Found live (2026-10-27): the month folders
// beside it were "APR 2026" ... "OCT 2026"; the worker made "November" beside them, filed the month's bills there, and
// every check of the folder failed though every entry was right. How a new folder's name differs from its siblings'
// is a fact handed to the reviewer, with the name their naming gives it - never a block or a verdict in code (review
// T-18): whether the folder should follow them is the reviewer's to judge, and the person's.

// mkdirRe is a mkdir command and its arguments up to the end of that simple command.
var mkdirRe = regexp.MustCompile(`(?:^|&&|;|\|\||\n)\s*mkdir\s+([^;&|\n]+)`)

// shellArgRe is one argument of a shell command: single-quoted, double-quoted, or bare with backslash escapes.
var shellArgRe = regexp.MustCompile(`'([^']*)'|"((?:[^"\\]|\\.)*)"|((?:[^\s'"\\]|\\.)+)`)

// monthWords are the calendar months by their full and three-letter names (Sept too).
var monthWords = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6, "july": 7, "august": 8,
	"september": 9, "october": 10, "november": 11, "december": 12,
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "sept": 9, "oct": 10,
	"nov": 11, "dec": 12,
}

// wordRunRe and digitRunRe split a name into letters, digits and the rest.
var nameTokenRe = regexp.MustCompile(`[A-Za-z]+|\d+|[^A-Za-z\d]+`)

// monthShape is a name's shape when it names a month: each letter run that is a month by its case and length
// (MON, Mon, mon, MONTH, Month, month), any other letter run as itself, each digit run as its count of digits, the
// rest as written. "" when no part of it is a month.
func monthShape(name string) string {
	var b strings.Builder
	month := false
	for _, tok := range nameTokenRe.FindAllString(name, -1) {
		switch {
		case tok[0] >= '0' && tok[0] <= '9':
			b.WriteString(strings.Repeat("9", len(tok)))
		case (tok[0] >= 'A' && tok[0] <= 'Z') || (tok[0] >= 'a' && tok[0] <= 'z'):
			if _, ok := monthWords[strings.ToLower(tok)]; ok {
				month = true
				form := "<month>"
				if len(tok) <= 4 {
					form = "<mon>"
				}
				switch {
				case tok == strings.ToUpper(tok):
					form = strings.ToUpper(form)
				case tok[0] >= 'A' && tok[0] <= 'Z':
					form = strings.ToUpper(form[:2]) + form[2:]
				}
				b.WriteString(form)
			} else {
				b.WriteString(tok)
			}
		default:
			b.WriteString(tok)
		}
	}
	if !month {
		return ""
	}
	return b.String()
}

// siblingShape is the shape most of a folder's month-named entries share, with up to three of them as examples - ""
// when fewer than two share one, or the shared one is not most of them.
func siblingShape(siblings []string) (string, []string) {
	count := map[string][]string{}
	total := 0
	for _, s := range siblings {
		if sh := monthShape(s); sh != "" {
			count[sh] = append(count[sh], s)
			total++
		}
	}
	best := ""
	for sh, names := range count {
		if len(names) > len(count[best]) || (len(names) == len(count[best]) && sh < best) {
			best = sh
		}
	}
	names := count[best]
	if len(names) < 2 || len(names)*2 <= total {
		return "", nil
	}
	sort.Strings(names)
	if len(names) > 3 {
		names = names[:3]
	}
	return best, names
}

// leadNumberRe is a name's leading number ("7" of "7. OCTOBER").
var leadNumberRe = regexp.MustCompile(`^\d+`)

// folderMonth is the month a name names by its first month word, 0 when none.
func folderMonth(name string) int {
	for _, tok := range nameTokenRe.FindAllString(name, -1) {
		if m, ok := monthWords[strings.ToLower(tok)]; ok {
			return m
		}
	}
	return 0
}

// siblingNumbering reads how month folders number their months: when every month-named sibling that leads with a
// number has number = month - k (mod 12) for one k, it is k and the digits' width; ok false when fewer than two do, or
// they disagree. A financial year that starts in April numbers October 7 (k = 3); a calendar one numbers it 10 (k = 0).
// Found live (2026-10-28): beside "6. SEPTEMBER" and "7. OCTOBER" the worker made "11. NOVEMBER".
func siblingNumbering(siblings []string) (k, width int, ok bool) {
	n := 0
	for _, s := range siblings {
		num, month := leadNumberRe.FindString(s), folderMonth(s)
		if num == "" || month == 0 {
			continue
		}
		v := 0
		fmt.Sscan(num, &v)
		off := ((month-v)%12 + 12) % 12
		if n > 0 && (off != k || len(num) != width) {
			return 0, 0, false
		}
		k, width = off, len(num)
		n++
	}
	return k, width, n >= 2
}

// numberFor is the number a month takes under a numbering with offset k.
func numberFor(month, k int) int { return ((month-k-1)%12+12)%12 + 1 }

// expectedMonthName is the name a folder for month takes among siblings: the form of one of them with this month in
// it, renumbered by their numbering - "" when that cannot be said from them alone (no shared form, or digits that are
// not their numbering, such as a year).
func expectedMonthName(siblings []string, month int) string {
	shape, examples := siblingShape(siblings)
	if shape == "" || len(examples) == 0 || month < 1 || month > 12 {
		return ""
	}
	k, width, numbered := siblingNumbering(siblings)
	var b strings.Builder
	for i, tok := range nameTokenRe.FindAllString(examples[0], -1) {
		switch {
		case tok[0] >= '0' && tok[0] <= '9':
			if i != 0 || !numbered {
				return ""
			}
			fmt.Fprintf(&b, "%0*d", width, numberFor(month, k))
		case monthWords[strings.ToLower(tok)] != 0:
			b.WriteString(monthWordLike(tok, month))
		default:
			b.WriteString(tok)
		}
	}
	return b.String()
}

// fullMonthNames are the months' full names; a short form is the first three letters.
var fullMonthNames = []string{"", "january", "february", "march", "april", "may", "june", "july", "august", "september",
	"october", "november", "december"}

// monthWordLike is month written as tok writes its own: short or full, and in its case.
func monthWordLike(tok string, month int) string {
	w := fullMonthNames[month]
	if len(tok) <= 4 {
		w = w[:3]
	}
	switch {
	case tok == strings.ToUpper(tok):
		return strings.ToUpper(w)
	case tok[0] >= 'A' && tok[0] <= 'Z':
		return strings.ToUpper(w[:1]) + w[1:]
	}
	return w
}

// mkdirTargets are the paths a command makes with mkdir.
func mkdirTargets(command string) []string {
	var out []string
	for _, m := range mkdirRe.FindAllStringSubmatch(command, -1) {
		for _, a := range shellArgRe.FindAllStringSubmatch(m[1], -1) {
			arg := a[1] + a[2] + a[3]
			if a[3] != "" {
				arg = strings.ReplaceAll(a[3], `\`, "")
			}
			if arg == "" || strings.HasPrefix(arg, "-") {
				continue
			}
			out = append(out, strings.TrimRight(arg, "/"))
		}
	}
	return out
}

// monthFolderMismatch says how a month folder's name differs from the month folders beside it - their form, or
// their numbering - with the name it would take among them when that can be said; "" when it is named like them.
func monthFolderMismatch(name string, siblings []string) (string, []string) {
	mine := monthShape(name)
	shape, examples := siblingShape(siblings)
	if mine == "" || shape == "" {
		return "", nil
	}
	wrongNumber := false
	if k, _, numbered := siblingNumbering(siblings); numbered && shape == mine {
		v := 0
		fmt.Sscan(leadNumberRe.FindString(name), &v)
		wrongNumber = v != numberFor(folderMonth(name), k)
	}
	if shape == mine && !wrongNumber {
		return "", nil
	}
	why := "its name does not share their form (month form, case, separators, numbering)"
	if want := expectedMonthName(siblings, folderMonth(name)); want != "" && want != name {
		why += "; by their naming it would be " + fmt.Sprintf("%q", want)
	}
	return why, examples
}

// typedFolderRe is a name typed alone into a field (gui type "NAME" or gui type REF "NAME") - a new folder's name in a
// file manager's dialog among others; typed paths are not names.
var typedFolderRe = regexp.MustCompile(`gui type (?:e\d+ )?"([^"/\n]{2,60})"`)

// maxFolderChecks bounds the folders checked at a report.
const maxFolderChecks = 4

// folderNameFacts are the folders this task named - with mkdir, or by typing a name into a dialog - whose name differs
// from the shape the folders beside them on disk share, read when the work is reviewed: facts for the reviewer, with
// no verdict. Read at the review, so a folder made by any means is seen (found live, 2026-10-28: made in a file
// manager's own dialog).
func (o *OperatorSkill) folderNameFacts(rc RunContext, st *operatorState) []string {
	type folder struct{ parent, name string }
	var cands []folder
	seen := map[folder]bool{}
	add := func(parent, name string) {
		f := folder{parent, name}
		if parent == "" || parent == "." || monthShape(name) == "" || seen[f] || len(cands) == maxFolderChecks {
			return
		}
		seen[f] = true
		cands = append(cands, f)
	}
	all := strings.Join(st.Commands, "\n")
	for _, c := range st.Commands {
		for _, target := range mkdirTargets(c) {
			add(path.Dir(target), path.Base(target))
		}
		for _, m := range typedFolderRe.FindAllStringSubmatch(c, -1) {
			name := strings.TrimSpace(m[1])
			// The folder it was made in: a path the task used that ends in the name.
			re := regexp.MustCompile(`(/[^"'\n]*?)/` + regexp.QuoteMeta(name) + `(?:/|["'\s]|$)`)
			if p := re.FindStringSubmatch(all); p != nil {
				add(p[1], name)
			}
		}
	}
	var out []string
	for _, f := range cands {
		res := runShell(rc.Ctx, rc.Dir(), "ls -1 "+shellQuote(f.parent)+" 2>/dev/null", 15*time.Second, st.guiEnv()...)
		var siblings []string
		here := false
		for _, l := range strings.Split(res.Output, "\n") {
			switch l = strings.TrimSpace(l); {
			case l == f.name:
				here = true
			case l != "":
				siblings = append(siblings, l)
			}
		}
		if !here {
			continue // not made, or renamed since
		}
		why, examples := monthFolderMismatch(f.name, siblings)
		if why == "" {
			continue
		}
		out = append(out, fmt.Sprintf("the folder %q in %s, beside folders named like %s: %s", f.name, f.parent,
			strings.Join(quoteAll(examples), ", "), why))
	}
	return out
}

func quoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = fmt.Sprintf("%q", x)
	}
	return out
}

// shellQuote quotes a path for sh.
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
