package web

import (
	"context"
	"math"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type netResolver struct{}

func (netResolver) lookup(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

var wordRe = regexp.MustCompile(`[\p{L}\p{M}\p{N}]+`)

func terms(s string) []string {
	var out []string
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(w)) > 1 && !stopword[w] {
			out = append(out, w)
		}
	}
	return out
}

var stopword = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the a an and or of to in on for is are was were be by with as at from that this it its what which who how why when
		where do does did can could should would will about into than then there their they them you your our we i me my`) {
		m[w] = true
	}
	return m
}()

// Passages picks the parts of a page most about the query: paragraphs are
// scored by the query words they hold (rarer words count more), and the best
// are returned in the page's own order, up to maxChars.
func Passages(text, query string, maxChars int) string {
	paras := strings.Split(text, "\n")
	q := terms(query)
	if len(q) == 0 || len(text) <= maxChars {
		return truncate(text, maxChars)
	}
	df := map[string]int{}
	pterms := make([]map[string]int, len(paras))
	for i, p := range paras {
		pterms[i] = map[string]int{}
		for _, t := range terms(p) {
			pterms[i][t]++
		}
		for t := range pterms[i] {
			df[t]++
		}
	}
	type scored struct {
		i int
		s float64
	}
	var ss []scored
	for i, p := range paras {
		if len(strings.TrimSpace(p)) < 20 {
			continue
		}
		var s float64
		for _, t := range q {
			if n := pterms[i][t]; n > 0 {
				idf := math.Log(1 + float64(len(paras))/float64(1+df[t]))
				s += idf * (1 + math.Log(float64(n)))
			}
		}
		if s > 0 {
			ss = append(ss, scored{i, s / math.Sqrt(float64(1+len(pterms[i])))})
		}
	}
	if len(ss) == 0 {
		return truncate(text, maxChars)
	}
	sort.Slice(ss, func(a, b int) bool { return ss[a].s > ss[b].s })
	keep := map[int]bool{}
	total := 0
	for _, x := range ss {
		l := len(paras[x.i]) + 1
		if total+l > maxChars && total > 0 {
			continue
		}
		keep[x.i] = true
		total += l
		if total >= maxChars {
			break
		}
	}
	var b strings.Builder
	last := -2
	for i, p := range paras {
		if !keep[i] {
			continue
		}
		if last >= 0 && i != last+1 {
			b.WriteString("…\n")
		}
		b.WriteString(truncate(p, maxChars))
		b.WriteString("\n")
		last = i
	}
	return strings.TrimSpace(b.String())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " …"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// The parts of a query that must never leave the machine: identity numbers,
// secrets and contact details. They are generic shapes, not any one product's.
var scrubRes = []*regexp.Regexp{
	regexp.MustCompile(`\b[A-Z]{5}[0-9]{4}[A-Z]\b`),                                     // PAN
	regexp.MustCompile(`\b\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`),                  // GSTIN
	regexp.MustCompile(`\b\d{4}\s?\d{4}\s?\d{4}\b`),                                     // Aadhaar-like 12 digits
	regexp.MustCompile(`\b[\w.+-]+@[\w-]+\.[\w.-]+\b`),                                  // email
	regexp.MustCompile(`(?:\+?\d{1,3}[\s-]?)?\b\d{10}\b`),                               // phone
	regexp.MustCompile(`\b(?:sk|pk|rk|ghp|gho|xox[abp]|AKIA|AIza)[-_A-Za-z0-9]{12,}\b`), // API keys
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|otp|pin|secret|token)\s*[:=]\s*\S+`), // a value given, not the word
	regexp.MustCompile(`\b\d{9,18}\b`),                                                  // account and card numbers
}

// Scrub removes identity numbers, secrets and contact details from a search
// query, and says whether it removed anything.
func Scrub(q string) (string, bool) {
	out := q
	for _, re := range scrubRes {
		out = re.ReplaceAllString(out, " ")
	}
	out = strings.Join(strings.Fields(out), " ")
	return out, out != strings.Join(strings.Fields(q), " ")
}

// QueryFrom turns a request into a search query: its own words, scrubbed,
// at most 200 characters, cut at a word.
func QueryFrom(text string) (string, bool) {
	q, scrubbed := Scrub(strings.Join(strings.Fields(text), " "))
	if len(q) > 200 {
		q = q[:200]
		if i := strings.LastIndexFunc(q, unicode.IsSpace); i > 100 {
			q = q[:i]
		}
	}
	return q, scrubbed
}

// fresh are phrases that ask about the state of the world now.
var freshRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(latest|newest|most recent|recently|currently|nowadays|right now|as of now|up to date|up-to-date|breaking)\b`),
	regexp.MustCompile(`(?i)\b(today|tonight|yesterday|tomorrow|this (week|month|year|quarter|season)|last (week|month|night)|next (week|month))\b`),
	regexp.MustCompile(`(?i)\b(news|headlines|announced|announcement|released|release date|launch(ed)?|upcoming|just came out)\b`),
	regexp.MustCompile(`(?i)\b(price|prices|rate|rates|stock|share price|exchange rate|weather|forecast|score|scores|results?|standings|election|poll)\s+(of|for|in|today|now)\b`),
	regexp.MustCompile(`(?i)\bcurrent\s+(\S+\s+){0,2}(price|rate|version|ceo|president|prime minister|chief minister|governor|status|situation|events?|affairs|leader|holder|champion|record|news)\b`),
	regexp.MustCompile(`(?i)\b(who is|who's) the (current |new )?(ceo|president|prime minister|pm|chief minister|cm|governor|chairman|captain|minister)\b`),
	regexp.MustCompile(`(?i)\b(new|latest)\s+(\S+\s+){0,2}(version|model|release|update|law|laws|rule|rules|notification|circular|amendment|policy|rate|rates)\b`),
}

var yearRe = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// NeedsFresh reports whether a request asks about things a model's training
// cannot know - the state of the world now - and why. It is a set of plain
// phrase checks plus "it names this year or last", not a judgement of the
// request's subject.
func NeedsFresh(text string, now time.Time) (bool, string) {
	for _, re := range freshRes {
		if m := re.FindString(text); m != "" {
			return true, "asks about " + strings.ToLower(m)
		}
	}
	for _, y := range yearRe.FindAllString(text, -1) {
		var n int
		for _, c := range y {
			n = n*10 + int(c-'0')
		}
		if n >= now.Year()-1 {
			return true, "names " + y
		}
	}
	return false, ""
}
