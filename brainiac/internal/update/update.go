// Package update keeps Brainiac on the best open models this machine can
// run, with no person in the loop. One cycle:
//
//  1. scout the Hugging Face Hub for new models this machine could run;
//  2. read public evaluations of them: independent leaderboards, and the
//     models' own cards for the names their makers use;
//  3. rate each new model per role against the catalog models the same
//     leaderboard scores, and add the ones rated above the current pick to
//     the store's catalog overlay (marked Auto);
//  4. plan again; when the plan changes for the better, install it beside
//     the running one (lock.next.json) while Brainiac is idle;
//  5. check every changed role with answers code can verify (a canary) and
//     switch, keeping the old lock as lock.prev.json. A model that fails is
//     set aside for 30 days and the next best is tried.
//
// Ratings rest on independent leaderboards, never on a person's opinion or
// a maker's own card alone (a card only says what the maker calls its model). The canary does not
// judge quality - it catches a model that does not load, cannot see, or
// answers in the wrong template.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"brainiac/internal/bench"
	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/hub"
	"brainiac/internal/install"
	"brainiac/internal/scout"
	"brainiac/internal/selector"
	"brainiac/internal/store"
	"brainiac/internal/web"
)

// Files under the store root.
const (
	OverlayFile = "catalog.local.json" // the catalog additions
	StateFile   = "update-state.json"  // set-aside models and history
	ReportFile  = "update.json"        // the last cycle's report
	NextLock    = "lock.next.json"     // a plan installed, not yet checked
	PrevLock    = "lock.prev.json"     // the lock before the last switch
)

const (
	// Margin is how many quality points (0-100) a new model must be rated
	// above the current pick for a role before it is added to the catalog.
	Margin = 3.0
	// SetAside is how long a model that failed its check is not tried again.
	SetAside = 30 * 24 * time.Hour
	// attempts is how many plans one cycle checks before giving up.
	attempts = 3
)

// ErrNotNow is a check that could not run yet (memory is in use); the
// cycle tries again later without holding it against the model.
var ErrNotNow = errors.New("not now")

// CheckFailed is a role whose new model failed its check.
type CheckFailed struct {
	Role string
	Err  error
}

func (e *CheckFailed) Error() string { return e.Role + ": " + e.Err.Error() }
func (e *CheckFailed) Unwrap() error { return e.Err }

// Options configure one cycle.
type Options struct {
	Store *store.Store
	// Census measures the machine for planning.
	Census func(ctx context.Context) (census.Census, error)
	// Policy is the operator's selector policy (licences, reserve...).
	Policy selector.Policy
	// Apply installs, checks and switches. Without it the cycle only
	// reports what it would do and writes nothing but its report.
	Apply bool
	// Idle says whether heavy work (downloads, loading models) may start
	// now; nil means always.
	Idle func() bool
	// MakeRoom frees the memory the running engine holds, before a check.
	MakeRoom func()
	// Check loads lock's models and tests the roles named (the engine's
	// canary). A *CheckFailed names the role that failed; an error wrapping
	// ErrNotNow means try later.
	Check     func(ctx context.Context, lock *install.Lock, cat *catalog.Catalog, roles []string) error
	WorkerDir string
	Log       io.Writer

	// The rest are replaced by tests.
	Hub     *hub.Client
	Web     *web.Client
	Now     func() time.Time
	Scout   func(ctx context.Context, opt scout.Options) (*scout.Report, error)
	Board   func(ctx context.Context) *bench.Board
	Cards   func(ctx context.Context, repo string) ([]bench.Score, error)
	Install func(ctx context.Context, plan *selector.Plan, opt install.Options) (*install.Lock, error)
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Hub == nil {
		o.Hub = hub.New()
	}
	if o.Web == nil {
		o.Web = web.New()
	}
	w := o.Web
	if o.Scout == nil {
		o.Scout = scout.Run
	}
	if o.Board == nil {
		o.Board = func(ctx context.Context) *bench.Board { return bench.Collect(ctx, w, bench.LiveBenchDate) }
	}
	if o.Cards == nil {
		o.Cards = func(ctx context.Context, repo string) ([]bench.Score, error) { return bench.CardScores(ctx, w, repo) }
	}
	if o.Install == nil {
		o.Install = install.Run
	}
	return o
}

// Outcomes of a cycle.
const (
	OutcomeCurrent  = "current"      // nothing better fits here
	OutcomeWould    = "would-switch" // a dry run found a better plan
	OutcomeDeferred = "deferred"     // a better plan waits for an idle moment
	OutcomeSwitched = "switched"     // the better plan passed its check and runs now
	OutcomeRejected = "rejected"     // every better plan tried failed its check
	OutcomeFailed   = "failed"       // something went wrong; nothing changed
)

// Result is one cycle's report.
type Result struct {
	MadeAt         time.Time    `json:"made_at"`
	CatalogVersion string       `json:"catalog_version"`
	Outcome        string       `json:"outcome"`
	Why            string       `json:"why"`
	Adopted        []Adoption   `json:"adopted,omitempty"`
	Considered     []Considered `json:"considered,omitempty"`
	Changes        []Change     `json:"changes,omitempty"`
	SetAside       []string     `json:"set_aside,omitempty"`
	LlamaCpp       string       `json:"llamacpp,omitempty"` // a new llama.cpp pin the plan needs
	Notes          []string     `json:"notes,omitempty"`
	// Lock and Catalog are what to serve after a switch.
	Lock    *install.Lock    `json:"-"`
	Catalog *catalog.Catalog `json:"-"`
}

// Adoption is a model added to (or refreshed in) the catalog overlay.
type Adoption struct {
	Model    string             `json:"model"`
	Repo     string             `json:"repo"`
	Roles    map[string]float64 `json:"roles"`
	Why      string             `json:"why"`
	Thinks   bool               `json:"thinks,omitempty"`
	Evidence []bench.Estimate   `json:"evidence"`
}

// Considered is a new model that was read and not added, and why.
type Considered struct {
	Repo string `json:"repo"`
	Why  string `json:"why"`
}

// Change is one role whose model a new plan changes.
type Change struct {
	Role        string  `json:"role"`
	From        string  `json:"from,omitempty"`
	To          string  `json:"to,omitempty"`
	FromQuality float64 `json:"from_quality"`
	ToQuality   float64 `json:"to_quality"`
}

// State is what the updater remembers between cycles.
type State struct {
	LastRun  time.Time            `json:"last_run"`
	Rejected map[string]Rejection `json:"rejected,omitempty"` // model id, or "llama.cpp@<tag>"
	History  []Event              `json:"history,omitempty"`  // newest last
}

// Rejection is a model set aside after failing its check.
type Rejection struct {
	Until time.Time `json:"until"`
	Why   string    `json:"why"`
}

// Event is one cycle in the history.
type Event struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Why     string    `json:"why"`
	Changes []Change  `json:"changes,omitempty"`
}

// ReadState reads the updater's state; a missing or broken file is empty.
func ReadState(st *store.Store) State {
	var s State
	_ = st.ReadJSON(StateFile, &s)
	if s.Rejected == nil {
		s.Rejected = map[string]Rejection{}
	}
	return s
}

// cycle is one run's working set.
type cycle struct {
	opt     Options
	st      *store.Store
	now     time.Time
	res     *Result
	state   State
	lock    *install.Lock
	overlay catalog.Overlay
	cat     *catalog.Catalog
	logf    func(string, ...any)
	// pinModels are the adopted models that need the newer llama.cpp.
	pinModels []string
}

// Run is one cycle. It returns an error only when it could not look at all
// (nothing installed, no network); a cycle that found nothing better, or
// whose check failed, is a Result.
func Run(ctx context.Context, opt Options) (*Result, error) {
	opt = opt.withDefaults()
	c := &cycle{opt: opt, st: opt.Store, now: opt.Now(), state: ReadState(opt.Store)}
	c.res = &Result{MadeAt: c.now.UTC()}
	c.logf = func(f string, a ...any) { fmt.Fprintf(opt.Log, "update: "+f+"\n", a...) }
	var err error
	if c.lock, err = install.ReadLock(c.st); err != nil {
		return nil, err
	}
	if c.overlay, err = catalog.ReadOverlay(c.st.Path(OverlayFile)); err != nil {
		return nil, err
	}
	if c.cat, err = catalog.Merge(c.overlay); err != nil {
		return nil, fmt.Errorf("%s: %w", OverlayFile, err)
	}
	c.res.CatalogVersion = c.cat.Version
	c.logf("looking for new open models")
	rep, err := opt.Scout(ctx, scout.Options{Hub: opt.Hub, Web: opt.Web, Catalog: c.cat, BudgetGB: hostBudget(c.lock.Plan),
		Since: c.now.AddDate(0, 0, -120), MaxVerify: 30, Log: opt.Log})
	if err != nil {
		return nil, fmt.Errorf("scout: %w", err)
	}
	_ = c.st.WriteJSON("scout.json", rep)
	c.res.Notes = append(c.res.Notes, rep.Notes...)
	c.logf("reading public evaluations")
	board := opt.Board(ctx)
	c.res.Notes = append(c.res.Notes, board.Notes...)
	overlay, pin := c.adopt(ctx, rep, board)
	c.res.LlamaCpp = pin
	c.decide(ctx, overlay, pin)
	c.finish()
	return c.res, nil
}

// decide plans with the new overlay and, when applying, installs, checks
// and switches.
func (c *cycle) decide(ctx context.Context, overlay catalog.Overlay, pin string) {
	planned := overlay
	if pin != "" {
		planned.Runtime = &catalog.Runtime{LlamaCppRepo: c.cat.Runtime.LlamaCppRepo, LlamaCppTag: pin}
	}
	if c.opt.Apply && len(c.res.Adopted) > 0 {
		// The models are knowledge, kept even if this machine cannot switch
		// now; the llama.cpp pin moves only with a switch that passed.
		overlay.Updated = c.now.Format("2006-01-02")
		if err := catalog.SaveOverlay(c.st.Path(OverlayFile), overlay); err != nil {
			c.set(OutcomeFailed, "could not save %s: %v", OverlayFile, err)
			return
		}
	}
	cs, err := c.opt.Census(ctx)
	if err != nil {
		c.set(OutcomeFailed, "could not measure the machine: %v", err)
		return
	}
	plannedFor(&cs, c.lock.Plan)
	for try := 0; try < attempts; try++ {
		merged, err := catalog.Merge(planned)
		if err != nil {
			c.set(OutcomeFailed, "the catalog with the new models does not validate: %v", err)
			return
		}
		pol := c.opt.Policy
		pol.Previous = c.lock.Plan
		plan := selector.Select(cs, c.withoutRejected(merged), c.st, pol)
		changes := diff(merged, c.lock.Plan, plan)
		c.res.Changes = changes
		if len(changes) == 0 {
			if try == 0 {
				c.set(OutcomeCurrent, "the installed models are still the best this machine can run")
			}
			return
		}
		if why := worse(changes); why != "" {
			c.set(OutcomeCurrent, "the new plan would be worse (%s), probably because memory is short now; keeping the installed models", why)
			return
		}
		if !c.opt.Apply {
			c.set(OutcomeWould, "a better plan is available: %s", describe(changes))
			return
		}
		if !c.idle() {
			c.set(OutcomeDeferred, "a better plan is available (%s); switching when Brainiac has been idle", describe(changes))
			return
		}
		c.logf("installing beside the running models: %s", describe(changes))
		next, err := c.opt.Install(ctx, plan, install.Options{Store: c.st, Census: cs, WorkerDir: c.opt.WorkerDir, Log: c.opt.Log, LockName: NextLock})
		if err != nil {
			c.set(OutcomeFailed, "installing the better plan failed (nothing changed; tried again next cycle): %v", err)
			return
		}
		if !c.idle() {
			c.set(OutcomeDeferred, "installed %s; checking and switching when Brainiac is idle again", describe(changes))
			return
		}
		if c.opt.MakeRoom != nil {
			c.opt.MakeRoom()
		}
		roles := changedRoles(changes)
		c.logf("checking %s", strings.Join(roles, ", "))
		err = c.opt.Check(ctx, next, merged, roles)
		var failed *CheckFailed
		switch {
		case err == nil:
			if err := c.promote(next, plan); err != nil {
				c.set(OutcomeFailed, "the check passed but switching failed: %v", err)
				return
			}
			if pin != "" && plan.LlamaCpp != nil && plan.LlamaCpp.Tag == pin {
				overlay.Runtime = planned.Runtime
				overlay.Updated = c.now.Format("2006-01-02")
				_ = catalog.SaveOverlay(c.st.Path(OverlayFile), overlay)
			}
			c.res.Lock, c.res.Catalog = next, merged
			c.set(OutcomeSwitched, "switched after the check passed: %s", describe(changes))
			return
		case errors.Is(err, ErrNotNow):
			c.set(OutcomeDeferred, "installed %s; the check waits for free memory (%v)", describe(changes), err)
			return
		case errors.As(err, &failed):
			id := modelFor(plan, failed.Role)
			keys := []string{id}
			if pin != "" && !c.newModel(id) {
				// A model that ran before failing on the new build blames the
				// build, and with it the models that needed it.
				keys = append([]string{"llama.cpp@" + pin}, c.pinModels...)
				planned.Runtime = nil
				pin = ""
			}
			for _, key := range keys {
				c.state.Rejected[key] = Rejection{Until: c.now.Add(SetAside), Why: failed.Error()}
				c.res.SetAside = append(c.res.SetAside, key)
			}
			key := strings.Join(keys, ", ")
			_ = os.Remove(c.st.Path(NextLock))
			c.logf("%s failed its check (%v); set aside until %s", key, failed.Err, c.now.Add(SetAside).Format("2006-01-02"))
			c.set(OutcomeRejected, "the better plan failed its check: %v", failed)
		default:
			c.set(OutcomeFailed, "checking the better plan failed: %v", err)
			return
		}
	}
}

func (c *cycle) idle() bool { return c.opt.Idle == nil || c.opt.Idle() }

func (c *cycle) set(outcome, f string, a ...any) {
	c.res.Outcome, c.res.Why = outcome, fmt.Sprintf(f, a...)
}

// newModel reports whether a model is not in the running plan.
func (c *cycle) newModel(id string) bool {
	for _, p := range c.lock.Plan.Picks {
		if p.Model == id {
			return false
		}
	}
	return true
}

// finish saves the report and the state.
func (c *cycle) finish() {
	c.logf("%s: %s", c.res.Outcome, c.res.Why)
	_ = c.st.WriteJSON(ReportFile, c.res)
	if !c.opt.Apply {
		return
	}
	c.state.LastRun = c.now.UTC()
	for k, r := range c.state.Rejected {
		if c.now.After(r.Until) {
			delete(c.state.Rejected, k)
		}
	}
	c.state.History = append(c.state.History, Event{At: c.now.UTC(), Outcome: c.res.Outcome, Why: c.res.Why, Changes: c.res.Changes})
	if n := len(c.state.History); n > 50 {
		c.state.History = c.state.History[n-50:]
	}
	_ = c.st.WriteJSON(StateFile, c.state)
}

// adopt rates the scouted models and returns the overlay with the ones
// rated above the current picks, and the llama.cpp build they need ("" for
// the pinned one).
func (c *cycle) adopt(ctx context.Context, rep *scout.Report, board *bench.Board) (catalog.Overlay, string) {
	o := c.overlay
	o.Models = append([]catalog.Model(nil), c.overlay.Models...)
	anchors := anchorsByRole(c.cat)
	rated := map[string]bool{} // names of the catalog's own models
	for _, as := range anchors {
		for _, a := range as {
			for _, k := range a.Keys {
				rated[bench.Key(k)] = true
			}
		}
	}
	var runnable []scout.Candidate
	cards := map[string]bool{}
	own := map[string]string{} // repo -> the column its card gives itself
	for _, cand := range rep.Candidates {
		if why := c.notRunnable(cand, rep); why != "" {
			c.consider(cand.Repo, why)
			continue
		}
		runnable = append(runnable, cand)
		// Cards first, for every candidate: one maker's table often scores
		// another candidate beside the catalog's models.
		for _, repo := range []string{baseIfSame(cand), cand.Repo} {
			if repo == "" || cards[strings.ToLower(repo)] {
				continue
			}
			cards[strings.ToLower(repo)] = true
			s, err := c.opt.Cards(ctx, repo)
			if err != nil {
				continue
			}
			board.Add(s...)
			// Makers often name their own column without the size or org
			// the repo has; one clear match is theirs, never a rated model.
			if o := bench.Own(s, cand.Repo, cand.Base, baseIfSame(cand)); o != "" && !rated[o] {
				own[strings.ToLower(repo)] = o
			}
		}
	}
	current := currentQuality(c.cat, c.lock.Plan)
	pin := ""
	for _, cand := range runnable {
		id := modelID(cand)
		if m, ok := c.cat.Model(id); ok && !m.Auto {
			c.consider(cand.Repo, "already in the catalog as "+id)
			continue
		}
		if r, ok := c.state.Rejected[id]; ok && c.now.Before(r.Until) {
			c.consider(cand.Repo, fmt.Sprintf("failed its check (%s); tried again after %s", r.Why, r.Until.Format("2006-01-02")))
			continue
		}
		var keys []string
		for _, k := range []string{cand.Repo, cand.Base, baseIfSame(cand), own[strings.ToLower(baseIfSame(cand))], own[strings.ToLower(cand.Repo)]} {
			if k != "" {
				keys = append(keys, k)
			}
		}
		// Only independent leaderboards rate: a maker chooses what its card
		// shows. The card still says what the maker calls the model.
		roles, cardOnly := map[string]float64{}, map[string]float64{}
		var evidence []bench.Estimate
		for _, role := range c.rolesFor(cand) {
			all := board.EstimateRole(keys, role, anchors[role])
			e := all.Independent()
			if !e.OK {
				if all.OK {
					cardOnly[role] = all.Quality
				}
				continue
			}
			roles[role] = math.Min(100, math.Max(0, e.Quality))
			evidence = append(evidence, e)
		}
		if len(roles) == 0 {
			if len(cardOnly) > 0 {
				c.consider(cand.Repo, "only its maker's card rates it ("+ratings(cardOnly)+"); waiting for an independent leaderboard")
			} else {
				c.consider(cand.Repo, "no public result sets it beside a model the catalog rates")
			}
			continue
		}
		better := ""
		for _, r := range sortedKeys(roles) {
			if roles[r] >= current[r]+Margin {
				better = fmt.Sprintf("%s rated %.0f against %.0f for the current pick", r, roles[r], current[r])
				break
			}
		}
		if better == "" {
			c.consider(cand.Repo, "rated "+ratings(roles)+": not above the current picks")
			continue
		}
		if needsNewerLlama(cand) {
			tag := rep.LlamaCpp.NewestSafe
			if r, ok := c.state.Rejected["llama.cpp@"+tag]; ok && c.now.Before(r.Until) {
				c.consider(cand.Repo, "needs llama.cpp "+tag+", which failed its check")
				continue
			}
			pin = tag
			c.pinModels = append(c.pinModels, id)
		}
		m := modelFrom(cand, id, roles, evidence, c.now)
		replaced := false
		for i := range o.Models {
			if o.Models[i].ID == m.ID {
				o.Models[i], replaced = m, true
			}
		}
		if !replaced {
			o.Models = append(o.Models, m)
		}
		c.res.Adopted = append(c.res.Adopted, Adoption{Model: m.ID, Repo: cand.Repo, Roles: roles, Why: better, Thinks: m.Thinks, Evidence: evidence})
		c.logf("rated %s %s (%s)", m.ID, ratings(roles), better)
	}
	return o, pin
}

func (c *cycle) consider(repo, why string) {
	c.res.Considered = append(c.res.Considered, Considered{Repo: repo, Why: why})
}

// chatRoles are the roles a scouted GGUF model can take: speech, OCR and
// embedding models run on other engines and need a person to add.
func (c *cycle) chatRole(role string) bool {
	switch role {
	case catalog.RoleBrain, catalog.RoleVision, catalog.RoleGround:
		return true
	}
	r, ok := c.cat.Role(role)
	return ok && r.Kind == catalog.KindDomain
}

var splitRe = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)

// notRunnable says why a scouted model cannot be added on its own, or "".
func (c *cycle) notRunnable(cand scout.Candidate, rep *scout.Report) string {
	switch {
	case !cand.Usable && !(needsNewerLlama(cand) && rep.LlamaCpp.NewestSafe != ""):
		return "not runnable: " + strings.Join(cand.Problems, "; ")
	case !c.chatRole(cand.Role):
		return "a " + cand.Role + " model: the updater adds chat models (brain, vision, fields) only"
	case cand.FitsHere != nil && !*cand.FitsHere:
		return fmt.Sprintf("needs %.1f GB, more than this machine's budget", cand.MemGB)
	case splitRe.MatchString(cand.File):
		return "its GGUF is split into parts; the updater adds single-file models only"
	case !c.licenceAllowed(cand.LicenseClass):
		return "licence " + cand.License + " (" + cand.LicenseClass + ") is not allowed here"
	}
	return ""
}

func (c *cycle) licenceAllowed(class string) bool {
	allowed := c.opt.Policy.Licenses
	if len(allowed) == 0 {
		allowed = []string{catalog.LicensePermissive}
	}
	for _, a := range allowed {
		if a == class {
			return true
		}
	}
	return false
}

func needsNewerLlama(cand scout.Candidate) bool {
	return len(cand.Problems) == 1 && strings.Contains(cand.Problems[0], "newer llama.cpp")
}

// rolesFor are the roles to rate a candidate for: a field specialist only
// for its field (a math model with a good GPQA is still a math model); a
// general model for the brain, every field, and seeing when it has a
// vision projector.
func (c *cycle) rolesFor(cand scout.Candidate) []string {
	if r, ok := c.cat.Role(cand.Role); ok && r.Kind == catalog.KindDomain {
		return []string{cand.Role}
	}
	roles := []string{catalog.RoleBrain}
	if cand.MMProj != "" {
		roles = append(roles, catalog.RoleVision, catalog.RoleGround)
	}
	for _, d := range c.cat.Domains() {
		roles = append(roles, d.ID)
	}
	return roles
}

// withoutRejected drops models set aside after a failed check.
func (c *cycle) withoutRejected(cat *catalog.Catalog) *catalog.Catalog {
	out := *cat
	out.Models = nil
	for _, m := range cat.Models {
		if r, ok := c.state.Rejected[m.ID]; ok && c.now.Before(r.Until) {
			continue
		}
		out.Models = append(out.Models, m)
	}
	return &out
}

// promote makes the checked lock the running one, keeping the old as
// lock.prev.json, and deletes the downloads neither of them uses.
func (c *cycle) promote(next *install.Lock, plan *selector.Plan) error {
	var oldPrev install.Lock
	hadPrev := c.st.ReadJSON(PrevLock, &oldPrev) == nil
	if err := os.Rename(c.st.Path("lock.json"), c.st.Path(PrevLock)); err != nil {
		return err
	}
	if err := os.Rename(c.st.Path(NextLock), c.st.Path("lock.json")); err != nil {
		_ = os.Rename(c.st.Path(PrevLock), c.st.Path("lock.json"))
		return err
	}
	_ = c.st.WriteJSON("plan.json", plan)
	if hadPrev {
		c.prune(&oldPrev, next, c.lock)
	}
	return nil
}

// prune deletes the Hugging Face files of an old lock that the kept locks
// do not use. Only files Brainiac downloaded into its own store are
// touched; a shared Ollama's models never are.
func (c *cycle) prune(old *install.Lock, keep ...*install.Lock) {
	used := map[string]bool{}
	for _, l := range keep {
		for _, u := range l.Units {
			for _, p := range u.Files {
				used[filepath.Clean(p)] = true
			}
		}
	}
	root := filepath.Clean(c.st.Path("models", "hf")) + string(filepath.Separator)
	for _, u := range old.Units {
		if u.Source.Kind != catalog.SourceHF {
			continue
		}
		for _, p := range u.Files {
			p = filepath.Clean(p)
			if used[p] || !strings.HasPrefix(p, root) {
				continue
			}
			if os.Remove(p) == nil {
				c.logf("deleted %s (no longer used)", p)
				_ = os.Remove(filepath.Dir(p)) // only when empty
			}
		}
	}
}

// hostBudget is the host pool's model budget in a plan.
func hostBudget(p *selector.Plan) float64 {
	for _, b := range p.Budgets {
		if b.Pool == "host" {
			return b.BudgetGB
		}
	}
	return 0
}

// plannedFor raises each pool's available memory to what it was when the
// running plan was made: the updater looks for better models for the
// machine, not for whatever else happens to be running this minute (its own
// loaded models included). Loading checks live memory anyway. The memory
// speed measured then is reused too: measuring again on a busy machine would
// change the plan for the wrong reason.
func plannedFor(cs *census.Census, p *selector.Plan) {
	if cs.Bench == nil && p.DecodeGBps > 0 && strings.HasPrefix(p.DecodeBasis, "half the") {
		cs.Bench = &census.Bench{MemCopyGBps: p.DecodeGBps * 2}
	}
	for i := range cs.Pools {
		for _, b := range p.Budgets {
			if b.Pool == cs.Pools[i].Name && !b.Unmeasured && b.AvailableGB > cs.Pools[i].AvailableGB {
				cs.Pools[i].AvailableGB = b.AvailableGB
			}
		}
	}
}

// anchorsByRole are the catalog's own ratings, under the names public
// tables may use. Models the updater added are never anchors, so ratings
// do not drift by resting on earlier estimates.
func anchorsByRole(cat *catalog.Catalog) map[string][]bench.Anchor {
	out := map[string][]bench.Anchor{}
	for _, m := range cat.Models {
		if m.Auto {
			continue
		}
		keys := []string{m.ID, m.Title}
		for _, s := range m.Sources {
			if s.Repo != "" && s.Kind == catalog.SourceHF {
				keys = append(keys, s.Repo)
			}
			if s.Kind == catalog.SourceOllama {
				keys = append(keys, strings.ReplaceAll(s.Tag, ":", "-"))
			}
		}
		for r, q := range m.Roles {
			out[r] = append(out[r], bench.Anchor{Keys: keys, Quality: q, Name: m.ID})
		}
	}
	for r := range out {
		sort.Slice(out[r], func(i, j int) bool { return out[r][i].Name < out[r][j].Name })
	}
	return out
}

// currentQuality is what each role gets now: the pick's quality, and for a
// field the better of its specialist and the brain's field quality.
func currentQuality(cat *catalog.Catalog, p *selector.Plan) map[string]float64 {
	cur := map[string]float64{}
	for _, k := range p.Picks {
		cur[k.Role] = k.Quality
	}
	if b, ok := p.Pick(catalog.RoleBrain); ok {
		if m, ok := cat.Model(b.Model); ok {
			for _, d := range cat.Domains() {
				cur[d.ID] = math.Max(cur[d.ID], m.DomainQuality(d.ID))
			}
		}
	}
	return cur
}

// diff lists the roles whose model, source or llama.cpp build differ.
func diff(cat *catalog.Catalog, old, new *selector.Plan) []Change {
	tag := func(p *selector.Plan) string {
		if p.LlamaCpp == nil {
			return ""
		}
		return p.LlamaCpp.Tag
	}
	roles := map[string]bool{}
	for _, k := range old.Picks {
		roles[k.Role] = true
	}
	for _, k := range new.Picks {
		roles[k.Role] = true
	}
	oldQ, newQ := currentQuality(cat, old), currentQuality(cat, new)
	var out []Change
	for _, r := range sortedKeys(roles) {
		o, okO := old.Pick(r)
		n, okN := new.Pick(r)
		if okO && okN && o.Model == n.Model && o.Source.Label() == n.Source.Label() &&
			!(n.Source.Runtime == catalog.RuntimeLlamaCpp && tag(old) != tag(new)) {
			continue
		}
		out = append(out, Change{Role: r, From: o.Model, To: n.Model, FromQuality: oldQ[r], ToQuality: newQ[r]})
	}
	return out
}

// worse says why a plan's changes are not an improvement, or "": a core
// role lost, or less quality in total over the changed roles. A field
// whose specialist is dropped because the brain now does as well is not a
// loss.
func worse(changes []Change) string {
	var from, to float64
	for _, ch := range changes {
		switch ch.Role {
		case catalog.RoleBrain, catalog.RoleVision, catalog.RoleGround, catalog.RoleOCR, catalog.RoleASR, catalog.RoleVAD, catalog.RoleEmbed:
			if ch.To == "" {
				return "no model for " + ch.Role
			}
		}
		from += ch.FromQuality
		to += ch.ToQuality
	}
	if to < from {
		return fmt.Sprintf("quality %.0f against %.0f now", to, from)
	}
	if to == from {
		return "no role gets better"
	}
	return ""
}

// changedRoles are the roles to check: every role a change gives a model
// (diff counts a role whose llama.cpp build moved as changed).
func changedRoles(changes []Change) []string {
	set := map[string]bool{}
	for _, ch := range changes {
		if ch.To != "" {
			set[ch.Role] = true
		}
	}
	return sortedKeys(set)
}

func modelFor(p *selector.Plan, role string) string {
	if k, ok := p.Pick(role); ok {
		return k.Model
	}
	return ""
}

func describe(changes []Change) string {
	var parts []string
	for _, ch := range changes {
		from, to := ch.From, ch.To
		if from == "" {
			from = "brain"
		}
		if to == "" {
			to = "brain"
		}
		parts = append(parts, fmt.Sprintf("%s %s -> %s", ch.Role, from, to))
	}
	return strings.Join(parts, ", ")
}

var (
	thinkRe  = regexp.MustCompile(`(?i)(thinking|reason|(^|[-_])r1($|[-_])|think)`)
	activeRe = regexp.MustCompile(`(?i)[-_]a(\d+(\.\d+)?)b([-_]|$)`)
	quantRe  = regexp.MustCompile(`(?i)(IQ\d_[A-Z]+|Q\d_K_[MSL]|Q\d_K|Q\d_\d|Q\d|MXFP4|BF16|F16)`)
)

// baseIfSame is the repo a candidate quantises when it is the same model
// under the same name (unsloth/Qwen3.5-4B-GGUF of Qwen/Qwen3.5-4B), else "".
// A repo under another name (a fine-tune, or a re-upload calling itself
// something new) is a different model: it must not inherit its parent's
// scores, whatever the Hub's tags say.
func baseIfSame(cand scout.Candidate) string {
	if cand.BaseRepo == "" {
		return ""
	}
	own, base := bench.Key(cand.Base), bench.Key(cand.BaseRepo)
	if base != "" && (own == base || strings.HasSuffix(own, "-"+base)) {
		return cand.BaseRepo
	}
	return ""
}

// modelID is the catalog id for a scouted model.
func modelID(cand scout.Candidate) string {
	if b := baseIfSame(cand); b != "" {
		return bench.Key(b)
	}
	if cand.Base != "" {
		return bench.Key(cand.Base)
	}
	return bench.Key(cand.Repo)
}

// modelFrom builds the catalog entry for a scouted model.
func modelFrom(cand scout.Candidate, id string, roles map[string]float64, ev []bench.Estimate, now time.Time) catalog.Model {
	files := []catalog.File{{Path: cand.File, Role: "model"}}
	size := cand.SizeGB
	_, sees := roles[catalog.RoleVision]
	_, points := roles[catalog.RoleGround]
	if (sees || points) && cand.MMProj != "" {
		files = append(files, catalog.File{Path: cand.MMProj, Role: "mmproj"})
		size += cand.MMProjGB
	}
	name := cand.Repo + " " + cand.BaseRepo
	thinks := thinkRe.MatchString(name)
	org := cand.Repo
	if cand.BaseRepo != "" {
		org = cand.BaseRepo
	}
	org, _, _ = strings.Cut(org, "/")
	m := catalog.Model{
		ID: id, Title: cand.Repo, Family: org, Roles: roles, Basis: basis(ev, now),
		ParamsB: cand.ParamsB, KVPerTokenMB: round3(0.012 * cand.ParamsB), // not read from the file: a cautious guess
		License: cand.License, LicenseClass: cand.LicenseClass, Thinks: thinks, Auto: true,
		Sources: []catalog.Source{{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: cand.Repo,
			Quant: strings.ToUpper(quantRe.FindString(cand.File)), Files: files, SizeGB: round3(size), WeightsGB: round3(cand.SizeGB)}},
	}
	if a := activeRe.FindStringSubmatch(name); a != nil {
		if v, err := strconv.ParseFloat(a[1], 64); err == nil && v < cand.ParamsB {
			m.ActiveB = v
		}
	}
	if thinks {
		m.ThinkTokens, m.Ctx = 6000, 16384
	}
	return m
}

// basis says where an automatic rating comes from.
func basis(ev []bench.Estimate, now time.Time) string {
	var parts []string
	for _, e := range ev {
		var src []string
		seen := map[string]bool{}
		for _, p := range e.Pairs {
			s := fmt.Sprintf("%s %g vs %s %g (%s)", p.Benchmark, p.Candidate, p.Anchor, p.Versus, p.Source)
			if !seen[s] && len(src) < 4 {
				seen[s] = true
				src = append(src, s)
			}
		}
		parts = append(parts, fmt.Sprintf("%s %.1f from %d comparisons: %s", e.Role, e.Quality, len(e.Pairs), strings.Join(src, "; ")))
	}
	return fmt.Sprintf("Added by the updater on %s from public evaluations, scaled against the catalog's ratings. %s",
		now.Format("2006-01-02"), strings.Join(parts, ". "))
}

func ratings(roles map[string]float64) string {
	var parts []string
	for _, r := range sortedKeys(roles) {
		parts = append(parts, fmt.Sprintf("%s %.0f", r, roles[r]))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
