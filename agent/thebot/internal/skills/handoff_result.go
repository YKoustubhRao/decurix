package skills

import (
	"regexp"
	"strings"
)

// A part of the task handed to another worker (the `handoff` command, agent/decurix_agent/tools/handoff.py) comes
// back as one printed block whose first line says whether the receiving worker's own final check confirmed it:
// "=== HANDOFF <id>: DONE (verified) ===" and nothing else is done. A report that rests on a handed-back result the
// receiver did not verify claims what nobody saw - decided in code, like the record ledger (record_ledger.go).

// handoffHeadRe is the first line of a printed handoff result: its ID and its verdict.
var handoffHeadRe = regexp.MustCompile(`(?m)^=== HANDOFF (\S+): (.+?) ===\s*$`)

// handoffFieldRe is one "name: value" line of a printed handoff result.
var handoffFieldRe = regexp.MustCompile(`(?m)^(status|verified|worker|reason): (.*)$`)

// maxHandoffIssues bounds the points raised.
const maxHandoffIssues = 4

// handedBack is one handoff result as the tool printed it.
type handedBack struct {
	ID       string
	Verdict  string
	Status   string
	Worker   string
	Reason   string
	Verified bool
}

// handedBackResults returns the latest printed result of each handoff in text, in the order first seen. A result is
// verified only when its first line says "DONE (verified)" and its verified line says yes: a block cut short after
// its first line (an output shortened for the model) is judged by that first line alone.
func handedBackResults(text string) []handedBack {
	heads := handoffHeadRe.FindAllStringSubmatchIndex(text, -1)
	byID := map[string]int{}
	var out []handedBack
	for n, m := range heads {
		end := len(text)
		if n+1 < len(heads) {
			end = heads[n+1][0]
		}
		body := text[m[1]:end]
		if cut := strings.Index(body, "\n=== END HANDOFF"); cut >= 0 {
			body = body[:cut]
		}
		r := handedBack{ID: text[m[2]:m[3]], Verdict: strings.TrimSpace(text[m[4]:m[5]])}
		verifiedLine := ""
		for _, f := range handoffFieldRe.FindAllStringSubmatch(body, -1) {
			switch f[1] {
			case "status":
				r.Status = strings.TrimSpace(f[2])
			case "verified":
				verifiedLine = strings.TrimSpace(f[2])
			case "worker":
				r.Worker = strings.TrimSpace(f[2])
			case "reason":
				r.Reason = strings.TrimSpace(f[2])
			}
		}
		r.Verified = r.Verdict == "DONE (verified)" && (verifiedLine == "" || verifiedLine == "yes")
		if i, ok := byID[r.ID]; ok {
			out[i] = r
			continue
		}
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	return out
}

// handoffResultIssues are Work points for each part handed to another worker whose latest result the receiver did
// not verify and the report does not name (by its handoff ID): the report does not account for it. Whether a report
// that names it says rightly what became of it is the reviewer's to judge, from handoffFacts - never decided here by
// the report's wording (review T-18: a list of "not done/could not/failed" words let a report settle the point).
func handoffResultIssues(seen, report string) []reviewIssue {
	var out []reviewIssue
	for _, r := range handedBackResults(seen) {
		if r.Verified || strings.Contains(report, r.ID) {
			continue
		}
		who := "another worker"
		if r.Worker != "" && !strings.HasPrefix(r.Worker, "(") {
			who = "worker " + r.Worker
		}
		out = append(out, reviewIssue{
			What: "the part handed to " + who + " (handoff " + r.ID + ") came back not verified",
			Why:  handoffWhy(r),
			Settle: "check that part yourself and do what is missing, run `handoff status " + r.ID + "` if it was still " +
				"running, or say in the report that handoff " + r.ID + " is not done and why",
			Work: true,
		})
		if len(out) == maxHandoffIssues {
			break
		}
	}
	return out
}

// handoffWhy is why an unverified handoff result is not done: its verdict and the receiver's reason.
func handoffWhy(r handedBack) string {
	why := "only a result the receiving worker's own final check confirmed is done; this one reads " + r.Verdict
	if r.Reason != "" {
		why += " (" + truncateText(r.Reason, 200) + ")"
	}
	return why
}

// unverifiedHandoffIDs are the handoffs whose latest result in seen is not verified.
func unverifiedHandoffIDs(seen string) []string {
	if !strings.Contains(seen, "=== HANDOFF ") {
		return nil
	}
	var ids []string
	for _, r := range handedBackResults(seen) {
		if !r.Verified {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// handoffFacts are the parts handed to other workers that came back not verified, as facts for the reviewer: a report
// that rests on one as done claims what nobody saw.
func handoffFacts(seen string) []string {
	var out []string
	for _, r := range handedBackResults(seen) {
		if !r.Verified {
			out = append(out, "handoff "+r.ID+" came back NOT verified - "+handoffWhy(r))
		}
		if len(out) == maxHandoffIssues {
			break
		}
	}
	return out
}

// handoffIssues are the handoff points for a report, from everything the operator was shown.
func (st *operatorState) handoffIssues(report string) []reviewIssue {
	if !strings.Contains(st.Seen, "=== HANDOFF ") {
		return nil
	}
	return handoffResultIssues(st.Seen, report)
}
