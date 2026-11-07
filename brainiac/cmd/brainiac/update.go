package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brainiac/internal/api"
	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/config"
	"brainiac/internal/engine"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/store"
	"brainiac/internal/update"
)

// runUpdate runs one update cycle by hand: without --apply it says what a
// better plan would change; with --apply it installs, checks and stages it
// (when no server runs). `update promote` switches to a staged plan.
func runUpdate(args []string) error {
	if len(args) > 0 && args[0] == "approve-template" {
		return runApproveTemplate(args[1:])
	}
	if len(args) > 0 && args[0] == "promote" {
		return runPromote(args[1:])
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	apply := fs.Bool("apply", false, "install, check and stage a better plan now; `brainiac update promote` switches to it")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	if *apply && serverAnswers(cfg.Listen) {
		return fmt.Errorf("a Brainiac is serving on %s; stop it to install and check a plan by hand, or set update_every so serve does it when idle", cfg.Listen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lg := log.New(os.Stderr, "", 0)
	res, err := update.Run(ctx, updateOptions(st, cfg, lg, *apply, nil))
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	renderUpdate(os.Stdout, res)
	return nil
}

// runPromote is a person switching to the staged plan. A running serve
// notices lock.json changed and moves to it, finishing what it is answering
// on the old models first.
func runPromote(args []string) error {
	fs := flag.NewFlagSet("update promote", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	lock, staged, err := update.Promote(st, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("Switched to the plan checked on %s: %s.\n", staged.At.Local().Format("2006-01-02 15:04"), staged.Why)
	if staged.Runtime != nil {
		fmt.Printf("llama.cpp is now pinned to %s.\n", staged.Runtime.LlamaCppTag)
	}
	if b, ok := lock.Plan.Pick(catalog.RoleBrain); ok {
		fmt.Printf("The brain is %s. A running serve checks every 30 seconds and moves to the new models once what it is answering finishes; lock.prev.json is the plan before.\n", b.Model)
	}
	return nil
}

// runApproveTemplate is a person accepting a model's changed chat template.
func runApproveTemplate(args []string) error {
	fs := flag.NewFlagSet("update approve-template", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: brainiac update approve-template <model>")
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	rec, err := update.ApproveTemplate(st, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("Accepted the chat template of %s from %s at %s. If that model is installed, `brainiac pull --latest` moves it there.\n", fs.Arg(0), rec.Repo, rec.Commit)
	return nil
}

// updateOptions wires a cycle to this store, config and (when serving) the
// running server.
func updateOptions(st *store.Store, cfg config.Config, lg *log.Logger, apply bool, srv *api.Server) update.Options {
	o := update.Options{
		Store:     st,
		Policy:    selector.Policy{ReserveGB: cfg.ReserveGB, PlannedLoadGB: cfg.PlannedLoadGB, Licenses: cfg.Licenses},
		Apply:     apply,
		WorkerDir: workerDir(),
		Log:       lg.Writer(),
		Census: func(ctx context.Context) (census.Census, error) {
			return census.Take(ctx, census.Options{StoreDir: st.Path("models"), OllamaURL: cfg.OllamaURL, Network: true})
		},
		Check: canary(st, cfg, lg),
		Idle:  func() bool { return !machineBusy() },
	}
	if srv != nil {
		o.Idle = func() bool { return srv.IdleFor() >= 10*time.Minute && !machineBusy() }
		// The canary's models and the served ones are counted by different
		// residency managers, so work waits while the canary runs: freeing
		// the served models, loading the new ones and asking them a few
		// questions. Requests already running finish first.
		check := o.Check
		o.Check = func(ctx context.Context, lock *install.Lock, cat *catalog.Catalog, roles []string) error {
			release := srv.Hold()
			defer release()
			srv.Current().Unload()
			return check(ctx, lock, cat, roles)
		}
	}
	return o
}

// canary checks a lock's models on a throwaway engine that runs everything
// here (no remote brain, no web), so the check is of the models alone.
func canary(st *store.Store, cfg config.Config, lg *log.Logger) func(context.Context, *install.Lock, *catalog.Catalog, []string) error {
	return func(ctx context.Context, lock *install.Lock, cat *catalog.Catalog, roles []string) error {
		c := cfg
		c.Mode, c.Remote, c.Web = config.ModeLocal, nil, config.WebOff
		eng, err := engine.New(engine.Options{Config: c, Store: st, Lock: lock, Logger: lg, Catalog: cat})
		if err != nil {
			return err
		}
		defer eng.Close()
		err = eng.Canary(ctx, roles)
		var nr *engine.ErrNoRoom
		var ce *engine.CanaryError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &nr):
			return fmt.Errorf("%w: %v", update.ErrNotNow, err)
		case errors.As(err, &ce):
			return &update.CheckFailed{Role: ce.Role, Err: ce.Err}
		}
		return err
	}
}

// updateLoop looks for a better plan for this machine: a cycle ten minutes
// after start, then every interval. A better plan waiting for an idle moment
// is tried again every 30 minutes. It only installs, checks and stages: it
// never changes what is served (a person promotes; followLock then moves).
func updateLoop(ctx context.Context, st *store.Store, cfg config.Config, srv *api.Server, every time.Duration, lg *log.Logger) {
	wait := 10 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		res, err := update.Run(ctx, updateOptions(st, cfg, lg, true, srv))
		if err != nil {
			lg.Printf("update: %v", err)
			wait = min(every, 6*time.Hour)
			continue
		}
		switch res.Outcome {
		case update.OutcomeDeferred:
			wait = min(every, 30*time.Minute)
		case update.OutcomeFailed:
			wait = min(every, 6*time.Hour)
		case update.OutcomeStaged:
			lg.Printf("update: %s", res.Why)
		}
	}
}

// updateEvery is how often serve's update loop runs, and whether it runs at
// all: only when update_every is a positive duration and the web is not off
// (it reads public evaluations on the web). The default is off.
func updateEvery(cfg config.Config) (time.Duration, bool) {
	every, err := time.ParseDuration(cfg.UpdateEvery)
	return every, err == nil && every > 0 && cfg.Web != config.WebOff
}

// lockFollower moves a running serve to the plan in lock.json when a person
// changes it (`brainiac update promote`, or `brainiac pull` by hand). It
// compares the file's bytes, so nothing else moves the served models.
type lockFollower struct {
	path string
	seen string // the sha256 of the lock the current engine was built from
	// build makes an engine from the new lock; swap serves it and returns
	// the engine it replaced, which is retired once its requests finish.
	build func() (*engine.Engine, error)
	swap  func(*engine.Engine) *engine.Engine
	logf  func(string, ...any)
}

// check moves to a changed lock once, and reports whether it did.
func (f *lockFollower) check() bool {
	got := lockSHA(f.path)
	if got == "" || got == f.seen {
		return false
	}
	eng, err := f.build()
	if err != nil {
		f.logf("lock.json changed, but its plan does not load (%v); still serving the old one", err)
		f.seen = got // say so once, not at every check
		return false
	}
	f.seen = got
	old := f.swap(eng)
	go old.Retire(f.logf)
	f.logf("lock.json changed: now serving %s", rolesLine(eng.Status().Roles))
	return true
}

// run checks the lock every interval until ctx ends.
func (f *lockFollower) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.check()
		}
	}
}

func rolesLine(roles map[string]string) string {
	var parts []string
	for _, r := range []string{catalog.RoleBrain, catalog.RoleVision, catalog.RoleGround, catalog.RoleOCR, catalog.RoleASR, catalog.RoleEmbed} {
		if m, ok := roles[r]; ok {
			parts = append(parts, r+" "+m)
		}
	}
	return strings.Join(parts, ", ")
}

// machineBusy reports a CPU already working hard for someone else (the
// one-minute load average above 70% of the cores), when the OS says.
func machineBusy() bool {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return false
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return false
	}
	load, err := strconv.ParseFloat(f[0], 64)
	return err == nil && load > 0.7*float64(runtime.NumCPU())
}

// serverAnswers reports a Brainiac already serving at addr.
func serverAnswers(addr string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func renderUpdate(w io.Writer, res *update.Result) {
	fmt.Fprintf(w, "Update %s against catalog %s\n", res.MadeAt.Local().Format("2006-01-02 15:04"), res.CatalogVersion)
	fmt.Fprintf(w, "%s: %s\n", res.Outcome, res.Why)
	if len(res.Adopted) > 0 {
		fmt.Fprintln(w, "\nRated above the current picks by public evaluations:")
		for _, a := range res.Adopted {
			fmt.Fprintf(w, "  %-28s %s  (%s)\n", a.Model, ratingsLine(a.Roles), a.Why)
			for _, e := range a.Evidence {
				fmt.Fprintf(w, "      %s %.1f from %d comparisons", e.Role, e.Quality, len(e.Pairs))
				if e.Why != "" {
					fmt.Fprintf(w, " (%s)", e.Why)
				}
				fmt.Fprintln(w)
			}
		}
	}
	if len(res.Changes) > 0 {
		fmt.Fprintln(w, "\nPlan changes:")
		for _, ch := range res.Changes {
			fmt.Fprintf(w, "  %-9s %s (%.0f) -> %s (%.0f)\n", ch.Role, orBrain(ch.From), ch.FromQuality, orBrain(ch.To), ch.ToQuality)
		}
	}
	if len(res.SetAside) > 0 {
		fmt.Fprintf(w, "\nSet aside for 30 days after failing the check: %s\n", strings.Join(res.SetAside, ", "))
	}
	if len(res.Considered) > 0 {
		fmt.Fprintln(w, "\nRead and not added:")
		for _, c := range res.Considered {
			fmt.Fprintf(w, "  %-50s %s\n", c.Repo, c.Why)
		}
	}
	switch res.Outcome {
	case update.OutcomeWould:
		fmt.Fprintln(w, "\n`brainiac update --apply` installs and checks it; it changes nothing served until `brainiac update promote`.")
	case update.OutcomeStaged:
		fmt.Fprintln(w, "\n`brainiac update promote` switches to it; a running serve follows.")
	}
}

func ratingsLine(roles map[string]float64) string {
	var parts []string
	for _, r := range sortedRoles(roles) {
		parts = append(parts, fmt.Sprintf("%s %.0f", r, roles[r]))
	}
	return strings.Join(parts, ", ")
}

func sortedRoles(m map[string]float64) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func orBrain(m string) string {
	if m == "" {
		return "(the brain)"
	}
	return m
}

// lockSHA is the sha256 of the lock file serve started from ("" if unreadable).
func lockSHA(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
