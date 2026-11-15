package llm

// runaway.go: a streamed answer that has fallen into a loop is cut, not read to the end of its budget.
//
// A small model in a degenerate loop writes one fragment over and over until it runs out of room; on the local model
// that is minutes of CPU spent on bytes nobody can use, and the loop then rides into the transcript and seeds the next
// turn. So while an answer streams, its tail is checked: a 60-character stretch repeated five times or more, the
// repeats covering at least half of a tail of 400 characters or more, and - when the tail has lines - most of its
// lines copies of a few. The last condition keeps output that is meant to repeat (thirty rows of a table that share a
// long start, a list of similar paths) from being taken for a loop: its lines differ. On a hit the stream is stopped
// and the answer returned cut off (Result.Truncated, FinishReason "repetition"), the path a reply that ran out of
// room already takes, with the loop itself left out of the text.
//
// The first check is at about 1,500 characters and the gap doubles after each (bounded by the tail checked), so the
// work stays linear in the answer. Ported from Hermes agent/repetition_guard.py (RunawayStreamWatch), with a lower
// first check: Decurix's local window is 8192 tokens, not a hosted one's.

import (
	"strings"
)

const (
	runawayWindow        = 60   // the exact repeat looked for, far longer than ordinary reuse of a phrase
	runawayMinRepeats    = 5    // how often it must recur
	runawayDominance     = 0.5  // the share of the tail the repeats must cover
	runawayMinChars      = 400  // a shorter tail is never judged
	runawayFirstCheck    = 1500 // characters streamed before the first check
	runawayTailChars     = 6000 // the most of the newest text one check reads
	runawayDistinctLines = 0.5  // a tail with lines must have at most this share of distinct ones
	runawayAnchorSamples = 32   // evenly spaced anchors the periodic scan tries
	runawayAnchorMatches = 8    // later matches tried for each anchor
)

// FinishRepetition is the FinishReason of an answer cut because it had fallen into a loop.
const FinishRepetition = "repetition"

// runawayWatch watches one answer as it streams. Its zero value is ready.
type runawayWatch struct {
	next int // the length at which the next check is due
}

// check is called with the whole answer so far; it reports whether the answer has become a runaway loop and, if so,
// how much of it comes before the loop (the part worth keeping).
func (w *runawayWatch) check(text string) (bool, int) {
	if w.next == 0 {
		w.next = runawayFirstCheck
	}
	n := len(text)
	if n < w.next {
		return false, 0
	}
	w.next = n + min(n, runawayTailChars)
	from := max(0, n-runawayTailChars)
	hit, start := runawayRepetition(text[from:])
	if !hit {
		return false, 0
	}
	return true, from + start
}

// runawayRepetition reports whether text is dominated by a repetition with the shape of a loop, and where the
// repetition starts.
func runawayRepetition(text string) (bool, int) {
	hit, start := repetitionDominated(text)
	if !hit {
		return false, 0
	}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < runawayMinRepeats {
		return true, start // no lines to judge by: a loop within one line
	}
	distinct := map[string]bool{}
	for _, l := range lines {
		distinct[l] = true
	}
	if float64(len(distinct)) > float64(len(lines))*runawayDistinctLines {
		return false, 0
	}
	return true, start
}

// repetitionDominated reports whether a run of at least runawayMinRepeats exact repeats covers at least
// runawayDominance of text, and where the repeated part first starts. Three ways to see it: one line repeated, one
// window recurring often enough, or a long periodic run whose windows each recur too rarely to count.
func repetitionDominated(text string) (bool, int) {
	n := len(text)
	if n < runawayMinChars {
		return false, 0
	}
	if hit, start := lineDominated(text); hit {
		return true, start
	}
	if hit, start := windowDominated(text); hit {
		return true, start
	}
	return periodicDominated(text)
}

// lineDominated: one line, repeated at least runawayMinRepeats times, covers the share.
func lineDominated(text string) (bool, int) {
	counts := map[string]int{}
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			counts[l]++
		}
	}
	best, bestStart := false, len(text)
	for l, c := range counts {
		if c >= runawayMinRepeats && float64(c*len(l)) >= float64(len(text))*runawayDominance {
			if i := strings.Index(text, l); i >= 0 && i < bestStart {
				best, bestStart = true, i
			}
		}
	}
	if !best {
		return false, 0
	}
	return true, lineStart(text, bestStart)
}

// windowDominated: one runawayWindow-byte window recurs often enough to cover the share.
func windowDominated(text string) (bool, int) {
	n := len(text)
	needed := max(runawayMinRepeats, int(float64(n)*runawayDominance/runawayWindow+0.999))
	type seen struct{ count, first int }
	counts := make(map[string]seen, n)
	for i := 0; i+runawayWindow <= n; i++ {
		key := text[i : i+runawayWindow]
		s, ok := counts[key]
		if !ok {
			s.first = i
		}
		s.count++
		if s.count >= needed {
			return true, lineStart(text, s.first)
		}
		counts[key] = s
	}
	return false, 0
}

// periodicDominated finds a long exact periodic run from evenly spaced anchors: a later match of an anchor gives a
// period, and comparing text[i] with text[i+period] both ways gives the whole run, measured with its true unit.
func periodicDominated(text string) (bool, int) {
	n := len(text)
	maxStart := n - runawayWindow
	if maxStart < 1 {
		return false, 0
	}
	step := max(1, (maxStart+runawayAnchorSamples-2)/(runawayAnchorSamples-1))
	starts := []int{}
	for s := 0; s <= maxStart; s += step {
		starts = append(starts, s)
	}
	if starts[len(starts)-1] != maxStart {
		starts = append(starts, maxStart)
	}
	type run struct{ lo, hi, period int }
	var rejected []run
	for _, start := range starts {
		anchor := text[start : start+runawayWindow]
		from := start + 1
	matches:
		for range runawayAnchorMatches {
			rel := strings.Index(text[from:], anchor)
			if rel < 0 {
				break
			}
			match := from + rel
			period := match - start
			from = match + 1
			for _, r := range rejected {
				if r.lo <= start && start < r.hi && period%r.period == 0 {
					continue matches
				}
			}
			left, right := expandRun(text, start, period)
			if right-left >= runawayMinRepeats*period && float64(right-left) >= float64(n)*runawayDominance {
				return true, lineStart(text, left)
			}
			rejected = append(rejected, run{left, right, period})
		}
	}
	return false, 0
}

// expandRun is the [left, right) bounds of the exact periodic run with this period through the equal window at start.
func expandRun(text string, start, period int) (int, int) {
	n := len(text)
	left := start
	for left > 0 && text[left-1] == text[left-1+period] {
		left--
	}
	right := start + runawayWindow
	for right+period < n && text[right] == text[right+period] {
		right++
	}
	return left, right + period
}

// lineStart moves i back to the start of its line, so the part kept before a loop ends at a whole line.
func lineStart(text string, i int) int {
	if j := strings.LastIndexByte(text[:i], '\n'); j >= 0 {
		return j + 1
	}
	return 0
}
