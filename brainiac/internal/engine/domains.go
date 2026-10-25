package engine

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/evals"
	"brainiac/internal/router"
)

// RemoteEvalName is the name the remote brain's eval results are kept under.
const RemoteEvalName = "remote"

// RouterExamplesFile holds checked requests the router learns from, one JSON
// object {"text": ..., "domain": ...} per line, under the store root.
const RouterExamplesFile = "router-examples.jsonl"

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

// routeDomain decides whether a field specialist answers instead of base.
//
//   - explicit (the client named the field) always goes to its specialist when
//     one is installed;
//   - otherwise the router must be confident (router.Threshold);
//   - with a remote brain, a specialist takes the field only once checked
//     cases show it does at least as well as the remote brain there - never
//     on its published numbers alone;
//   - with a local brain, a specialist that has been measured worse than the
//     brain in its field loses it back to the brain.
func (e *Engine) routeDomain(req Request, explicit string, base brainChoice, baseErr error) (brainChoice, error) {
	d, p := explicit, 1.0
	if d == "" {
		dec := e.rt.Classify(lastUserText(req.Messages))
		if !dec.Routes() {
			return base, baseErr
		}
		d, p = dec.Class, dec.P
	}
	spec := e.byRole[d]
	if spec == nil || (base.local != nil && spec == base.local && base.remote == nil) {
		base.domain, base.domainP = d, p
		if spec == nil {
			base.why = "no " + d + " specialist is installed here; the brain answers"
		}
		return base, baseErr
	}
	res := e.evals.get()
	if base.remote != nil && explicit == "" {
		if better, known := res.Compare(spec.model, RemoteEvalName, d); !known || !better {
			base.domain, base.domainP = d, p
			base.why = "the remote brain answers " + d + " until " + spec.model + " is measured at least as good (brainiac eval)"
			return base, baseErr
		}
	}
	if base.local != nil && base.remote == nil && explicit == "" {
		if better, known := res.Compare(spec.model, base.local.model, d); known && !better {
			base.domain, base.domainP = d, p
			base.why = spec.model + " was measured worse than " + base.local.model + " at " + d + "; the brain answers"
			return base, baseErr
		}
	}
	mode := base.mode
	if mode == "" {
		mode = config.ModeLocal
	}
	why := "routed to the " + d + " specialist"
	if explicit != "" {
		why = "the client asked for the " + d + " specialist"
	}
	return brainChoice{mode: mode, local: spec, name: spec.model, sees: spec.vision, domain: d, domainP: p, thinks: spec.thinks, why: why}, nil
}
