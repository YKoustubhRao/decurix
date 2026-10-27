package skills

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A new folder among siblings that share a naming shape is named like them. Found live (2026-10-27): a client's month
// folders were "APR 2026" ... "OCT 2026"; the worker made "November" beside them, filed the month's bills there, and
// every check of the folder failed though every entry was right. A month is a calendar fact, not a domain's: the
// shape of a month's name among its siblings is decided in code before the folder is made.

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

// folderNameNudge is what the model is told instead of running a mkdir whose new folder names a month in another
// shape than the month folders beside it; "" to run it. Told once a path: a second send runs as written.
func (o *OperatorSkill) folderNameNudge(rc RunContext, st *operatorState, command string) string {
	for _, target := range mkdirTargets(command) {
		name, parent := path.Base(target), path.Dir(target)
		if name == "" || parent == "" || parent == "." || st.FolderNudged[target] {
			continue
		}
		mine := monthShape(name)
		if mine == "" {
			continue
		}
		res := runShell(rc.Ctx, rc.Dir(), "ls -1 "+shellQuote(parent)+" 2>/dev/null", 15*time.Second, st.guiEnv()...)
		var siblings []string
		for _, l := range strings.Split(res.Output, "\n") {
			if l = strings.TrimSpace(l); l != "" && l != name {
				siblings = append(siblings, l)
			}
		}
		shape, examples := siblingShape(siblings)
		if shape == "" || shape == mine {
			continue
		}
		if st.FolderNudged == nil {
			st.FolderNudged = map[string]bool{}
		}
		st.FolderNudged[target] = true
		rc.Logf("operator", "a new month folder named unlike the ones beside it - sent back (%s among %s)", name, strings.Join(examples, ", "))
		return fmt.Sprintf("Not run: the new folder %q sits beside month folders named like %s - a new month's folder is "+
			"named the same way (the same month form, case, separators and year as theirs). Look at the folder (ls) "+
			"and make it in their form; then copy into it. If a folder for this month already exists there in another "+
			"spelling, use that one.", name, strings.Join(quoteAll(examples), ", "))
	}
	return ""
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
