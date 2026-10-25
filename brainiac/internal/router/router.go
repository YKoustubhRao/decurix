// Package router decides which field of knowledge a request belongs to (math,
// law, code...) so it can go to that field's specialist, without a model:
// a naive Bayes classifier over words, word pairs and a few shapes of text
// (a code fence, LaTeX, an equation, a non-Latin script). It is trained on the
// catalog's examples for each field, examples of requests that belong to no
// field, and cases checked later.
//
// Why not ask a model: on a CPU host a model call to classify costs as much as
// the answer (Decurix measured 60-100 s for "hello" when a model judged the
// shape of every request), and a router that sits in front of every request
// must cost nothing. It decides in microseconds and needs no memory.
//
// A wrong route only changes which model answers; the answer is still checked
// like any other. When the classifier is unsure, the general brain answers.
//
// Depends on nothing internal.
package router

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// General is the class of requests that belong to no field.
const General = "general"

// Router is a trained classifier.
type Router struct {
	counts map[string]map[string]float64 // class -> token -> count
	totals map[string]float64            // class -> tokens seen
	vocab  map[string]bool
}

// New returns an empty router.
func New() *Router {
	return &Router{counts: map[string]map[string]float64{}, totals: map[string]float64{}, vocab: map[string]bool{}}
}

// Add learns that text belongs to class.
func (r *Router) Add(class, text string) {
	if r.counts[class] == nil {
		r.counts[class] = map[string]float64{}
	}
	for _, t := range Tokens(text) {
		r.counts[class][t]++
		r.totals[class]++
		r.vocab[t] = true
	}
}

// Classes lists what the router knows, sorted.
func (r *Router) Classes() []string {
	var out []string
	for c := range r.counts {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Decision is the router's answer.
type Decision struct {
	Class string
	// P is the confidence in Class, 0-1, from a softmax over the classes'
	// per-token likelihoods (length-normalised, so a long request is not
	// over-confident just for being long).
	P      float64
	Scores map[string]float64
}

// temperature sharpens the per-token log-likelihood gaps into probabilities.
const temperature = 7.0

// Threshold is the confidence a field needs before its specialist answers;
// below it the general brain does. Set so that, on held-out requests, nothing
// is sent to the wrong field: a miss costs a worse answer, a non-route only
// what the general brain already costs.
const Threshold = 0.35

// Routes reports whether a decision is confident enough to leave the brain.
func (d Decision) Routes() bool { return d.Class != General && d.P >= Threshold }

// Classify says which class text most likely belongs to.
func (r *Router) Classify(text string) Decision {
	toks := Tokens(text)
	if len(toks) == 0 || len(r.counts) == 0 {
		return Decision{Class: General, P: 1}
	}
	v := float64(len(r.vocab)) + 1
	logs := map[string]float64{}
	for c, counts := range r.counts {
		var s float64
		for _, t := range toks {
			s += math.Log((counts[t] + 1) / (r.totals[c] + v))
		}
		logs[c] = s / float64(len(toks))
	}
	// Softmax over classes; uniform prior, so a field with more examples is
	// not favoured for having them.
	maxL := math.Inf(-1)
	for _, l := range logs {
		maxL = math.Max(maxL, l)
	}
	var sum float64
	probs := map[string]float64{}
	for c, l := range logs {
		p := math.Exp((l - maxL) * temperature)
		probs[c] = p
		sum += p
	}
	best, bestP := General, 0.0
	for _, c := range r.Classes() {
		probs[c] /= sum
		if probs[c] > bestP {
			best, bestP = c, probs[c]
		}
	}
	return Decision{Class: best, P: bestP, Scores: probs}
}

var (
	wordRe   = regexp.MustCompile(`[\p{L}\p{M}\p{N}_]+`)
	latexRe  = regexp.MustCompile(`\\(frac|int|sum|sqrt|lim|prod|binom|cdot|times|leq|geq|alpha|beta|theta|pi)\b|\$[^$]+\$`)
	eqRe     = regexp.MustCompile(`[a-zA-Z0-9)\]]\s*(\^|\*\*)\s*\d|\b[a-z]\s*=\s*[-\d(]|\d\s*[-+*/^]\s*\d|[a-z]\s*[+\-]\s*\d+\s*=`)
	codeRe   = regexp.MustCompile("```|\\bdef \\w+\\(|\\bfunc \\w+\\(|\\bfunction\\s*\\w*\\(|=>|;\\s*$|\\{\\s*$|#include|\\bimport \\w+|\\breturn\\b.*;")
	sqlRe    = regexp.MustCompile(`(?i)\bselect\b[\s\S]+\bfrom\b|\binsert into\b|\bcreate table\b|\bgroup by\b|\border by\b|\bleft join\b|\binner join\b`)
	moneyRe  = regexp.MustCompile(`(?i)(₹|rs\.?\s?\d|\binr\b|\$\s?\d|\d+\s?%|\blakh\b|\bcrore\b)`)
	statueRe = regexp.MustCompile(`(?i)\b(section|article|clause|act|rule|order)\s+\d+[a-z]?\b`)
)

// stop are function words: they say nothing about a field, and every class
// has them, so they only add noise.
var stop = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the is are was were be been am do does did can could should would will shall may might must
		i me my we our you your he she it its they them their this that these those there here what which who whom whose
		how why when where of in on at to for from by with about into as and or but if then so not no yes
		please tell give show let get got make made any some all much many very just also than too up out over
		hi hey ok okay thanks thank`) {
		m[w] = true
	}
	return m
}()

// Tokens is what the classifier sees of a text: lower-case words, word pairs,
// and shape tokens (marked §) that any text can carry whatever its words.
func Tokens(text string) []string {
	lower := strings.ToLower(text)
	var words []string
	for _, w := range wordRe.FindAllString(lower, -1) {
		if len(w) <= 30 && !stop[w] {
			words = append(words, w)
		}
	}
	var out []string
	for i, w := range words {
		out = append(out, w)
		if i > 0 {
			out = append(out, words[i-1]+" "+w)
		}
	}
	shape := func(cond bool, tok string, weight int) {
		if cond {
			for i := 0; i < weight; i++ {
				out = append(out, tok)
			}
		}
	}
	shape(latexRe.MatchString(text), "§latex", 3)
	shape(eqRe.MatchString(text), "§equation", 2)
	shape(codeRe.MatchString(text), "§code", 3)
	shape(sqlRe.MatchString(text), "§sql", 3)
	shape(moneyRe.MatchString(text), "§money", 2)
	shape(statueRe.MatchString(text), "§provision", 2)
	if s := script(text); s != "" {
		shape(true, "§script:"+s, 3)
	}
	return out
}

// script names the main non-Latin script of a text, if any.
func script(text string) string {
	tables := []struct {
		name string
		t    *unicode.RangeTable
	}{
		{"devanagari", unicode.Devanagari}, {"bengali", unicode.Bengali}, {"tamil", unicode.Tamil},
		{"telugu", unicode.Telugu}, {"kannada", unicode.Kannada}, {"malayalam", unicode.Malayalam},
		{"gujarati", unicode.Gujarati}, {"gurmukhi", unicode.Gurmukhi}, {"arabic", unicode.Arabic},
		{"han", unicode.Han}, {"cyrillic", unicode.Cyrillic},
	}
	counts := map[string]int{}
	for _, r := range text {
		for _, s := range tables {
			if unicode.Is(s.t, r) {
				counts[s.name]++
			}
		}
	}
	best, n := "", 0
	for k, v := range counts {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	if n < 3 {
		return ""
	}
	return best
}
