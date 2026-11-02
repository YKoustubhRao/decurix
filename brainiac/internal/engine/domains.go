package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/evals"
	"brainiac/internal/router"
	"brainiac/internal/selector"
	"brainiac/internal/trace"
)

// RemoteEvalName is the name the remote brain's eval results are kept under.
const RemoteEvalName = "remote"

// RouterExamplesFile holds checked requests the router learns from, one JSON
// object {"text": ..., "domain": ...} per line, under the store root.
const RouterExamplesFile = "router-examples.jsonl"

// OutcomesFile holds clients' verdicts on real answers, under the store root.
const OutcomesFile = "outcomes.json"

// buildRouter trains the field router on the catalog's examples and on the
// checked requests the store has gathered.
func buildRouter(cat *catalog.Catalog, examplesPath string) *router.Router {
	r := router.New()
	if cat == nil {
		return r
	}
	for _, d := range cat.Domains() {
		r.Add(d.ID, d.Description)
		for _, ex := range d.Examples {
			r.Add(d.ID, ex)
		}
	}
	for _, ex := range cat.GeneralExamples {
		r.Add(router.General, ex)
	}
	if f, err := os.Open(examplesPath); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var ex struct {
				Text   string `json:"text"`
				Domain string `json:"domain"`
			}
			if json.Unmarshal(sc.Bytes(), &ex) == nil && ex.Text != "" && ex.Domain != "" {
				r.Add(ex.Domain, ex.Text)
			}
		}
	}
	return r
}

// evalCache reads <home>/evals.json again only when it changes.
type evalCache struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	res  *evals.Results
}

func (c *evalCache) get() *evals.Results {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := os.Stat(c.path)
	if err != nil {
		return nil
	}
	if c.res == nil || !st.ModTime().Equal(c.mod) {
		if r, err := evals.Load(c.path); err == nil {
			c.res, c.mod = r, st.ModTime()
		}
	}
	return c.res
}

// isDomain reports whether a name is a field the catalog knows.
func (e *Engine) isDomain(name string) bool {
	if e.cat == nil {
		return false
	}
	r, ok := e.cat.Role(name)
	return ok && r.Kind == catalog.KindDomain
}

// lastUserText is the text of the newest user message, what the router reads.
func lastUserText(msgs []InMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var b strings.Builder
		for _, p := range msgs[i].Parts {
			if p.Kind == "text" {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	return ""
}

// routeDomain decides who answers a request about a field.
//
//   - explicit (the client named the field) always goes to its specialist when
//     one is installed;
//   - otherwise the router must be confident (router.Threshold);
//   - with a remote brain, a local path takes the field only once it has earned
//     it (earnedPath), never on its published numbers alone;
//   - with a local brain, a specialist that has been measured worse than the
//     brain in its field loses it back to the brain, and so does one the brain
//     is proven as good as and answers faster than.
func (e *Engine) routeDomain(req Request, explicit string, base brainChoice, baseErr error) (brainChoice, error) {
	d, p := explicit, 1.0
	if d == "" {
		dec := e.rt.Classify(lastUserText(req.Messages))
		if !dec.Routes() {
			return base, baseErr
		}
		d, p = dec.Class, dec.P
	}
	mode := base.mode
	if mode == "" {
		mode = config.ModeLocal
	}
	answer := func(u *unit, why string) (brainChoice, error) {
		return brainChoice{mode: mode, local: u, name: u.model, sees: u.vision, domain: d, domainP: p, thinks: u.thinks, why: why}, nil
	}
	if base.remote != nil && explicit == "" {
		if u, why := e.earnedPath(d, req); u != nil {
			return answer(u, why)
		} else {
			base.domain, base.domainP, base.why = d, p, why
		}
		return base, baseErr
	}
	spec := e.byRole[d]
	if spec == nil || (base.local != nil && spec == base.local && base.remote == nil) {
		base.domain, base.domainP = d, p
		if spec == nil {
			base.why = "no " + d + " specialist is installed here; the brain answers"
		}
		return base, baseErr
	}
	if base.local != nil && base.remote == nil && explicit == "" {
		res := e.evals.get()
		if better, known := res.Compare(spec.model, base.local.model, d); known && !better {
			base.domain, base.domainP = d, p
			base.why = spec.model + " was measured worse than " + base.local.model + " at " + d + "; the brain answers"
			return base, baseErr
		}
		if res.Proven(base.local.model, spec.model, d) && res.Scores[base.local.model][d].MeanMs < res.Scores[spec.model][d].MeanMs {
			base.domain, base.domainP = d, p
			base.why = base.local.model + " is proven as good as " + spec.model + " at " + d + " and answers faster; the brain answers"
			return base, baseErr
		}
	}
	why := "routed to the " + d + " specialist"
	if explicit != "" {
		why = "the client asked for the " + d + " specialist"
	}
	return answer(spec, why)
}

// PathStatus says why one local model does or does not answer a field.
type PathStatus struct {
	Model string `json:"model"`
	// Earned: it may answer the field in place of the remote brain.
	Earned bool          `json:"earned"`
	Why    string        `json:"why"`
	Score  *evals.Score  `json:"score,omitempty"`
	Live   evals.Outcome `json:"live"`
}

// localPaths are the models that could answer a field here: its specialist
// and the local brain.
func (e *Engine) localPaths(d string) []*unit {
	var out []*unit
	if u := e.byRole[d]; u != nil {
		out = append(out, u)
	}
	if b := e.byRole[catalog.RoleBrain]; b != nil && (len(out) == 0 || out[0] != b) {
		out = append(out, b)
	}
	return out
}

// pathStatus judges one local model against the remote brain. Quality comes
// first and is a gate, not a trade: time and money only choose among the
// models that passed it.
func (e *Engine) pathStatus(u *unit, d string) PathStatus {
	st := PathStatus{Model: u.model, Live: e.outcomes.Get(u.model, d)}
	res := e.evals.get()
	var score evals.Score
	if res != nil {
		if s, ok := res.Scores[u.model][d]; ok {
			score, st.Score = s, &s
		}
	}
	switch {
	case st.Score == nil:
		st.Why = "not measured in this field (brainiac eval)"
	case !res.Proven(u.model, RemoteEvalName, d):
		st.Why = fmt.Sprintf("%d/%d correct can claim only %.0f%%, which does not show it is as good as the remote brain (brainiac eval)", score.Correct, score.N, 100*score.LowerBound())
	case st.Live.LastMiss.After(score.At):
		st.Why = "a real answer was reported wrong after it was measured; it must be measured again (brainiac eval)"
	case e.cfg.MaxLocalSeconds > 0 && float64(score.MeanMs)/1000 > e.cfg.MaxLocalSeconds:
		st.Why = fmt.Sprintf("it takes %.0f s on average, over the %.0f s allowed (max_local_seconds)", float64(score.MeanMs)/1000, e.cfg.MaxLocalSeconds)
	default:
		st.Earned = true
		st.Why = fmt.Sprintf("%d/%d correct is proven as good as the remote brain, at no hosted cost", score.Correct, score.N)
	}
	return st
}

// earnedPath picks the fastest local model that has earned a field and can
// take this request, or says why the remote brain keeps it.
func (e *Engine) earnedPath(d string, req Request) (*unit, string) {
	var best *unit
	var bestMs int64
	why := "the remote brain answers " + d + ": nothing local can answer it here"
	for i, u := range e.localPaths(d) {
		st := e.pathStatus(u, d)
		if !st.Earned {
			if i == 0 {
				why = "the remote brain answers " + d + " until a local model is measured at least as good; " + u.model + ": " + st.Why
			}
			continue
		}
		if !fitsWindow(u, req) {
			why = "the remote brain answers " + d + ": this request is too long for " + u.model
			continue
		}
		if best == nil || st.Score.MeanMs < bestMs {
			best, bestMs = u, st.Score.MeanMs
			why = u.model + " answers " + d + ": " + st.Why
		}
	}
	return best, why
}

// fitsWindow reports whether a request can fit a local model's context window,
// counting about 3 characters a token and leaving a quarter for the answer. A
// path was proven on short cases, and a cut prompt is a wrong answer.
func fitsWindow(u *unit, req Request) bool {
	chars := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			chars += len(p.Text)
		}
	}
	window := u.ctx
	if window <= 0 {
		window = selector.Context
	}
	return chars/3 <= window*3/4
}

// Paths reports, per field, what could answer here and why each does or does not.
func (e *Engine) Paths() map[string][]PathStatus {
	out := map[string][]PathStatus{}
	if e.cat == nil {
		return out
	}
	for _, r := range e.cat.Domains() {
		for _, u := range e.localPaths(r.ID) {
			out[r.ID] = append(out[r.ID], e.pathStatus(u, r.ID))
		}
	}
	return out
}

// ErrNoTrace is an outcome reported for a request no longer remembered.
var ErrNoTrace = errors.New("no recent trace with that id")

// RecordOutcome files a client's final verdict on real answers: one trace, or
// every answer since a moment (a whole task's, whose calls the client does not
// track one by one). Only answers a local model gave in a field count. It
// returns how many were filed.
func (e *Engine) RecordOutcome(id string, since time.Time, passed bool) (int, error) {
	var recs []*trace.Record
	if id != "" {
		r, ok := e.traces.Get(id)
		if !ok {
			return 0, ErrNoTrace
		}
		recs = append(recs, r)
	} else {
		for _, r := range e.traces.Recent(200) {
			if !r.At.Before(since) {
				recs = append(recs, r)
			}
		}
	}
	n := 0
	now := time.Now().UTC()
	for _, r := range recs {
		if r.Domain == "" || r.RemoteT > 0 || r.Error != "" {
			continue
		}
		if err := e.outcomes.Record(r.Brain, r.Domain, passed, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
