// brainiac bench: measure how fast the installed models run here (prompt
// reading short and long, writing, a cached and a cold turn, thread counts)
// and keep the numbers for the selector and the router.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/config"
	"brainiac/internal/engine"
	"brainiac/internal/install"
	"brainiac/internal/speed"
	"brainiac/internal/store"
)

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home directory (default BRAINIAC_HOME or ~/.brainiac)")
	models := fs.String("models", "", "catalog ids to measure, comma-separated (default: the brain's model)")
	threads := fs.String("threads", "", "thread counts to compare, comma-separated (default: half the CPUs and all of them)")
	long := fs.Int("long", 0, "the long prompt's size in tokens (default: 3/4 of the window, at most 6000)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := *home
	if root == "" {
		root = store.DefaultRoot()
	}
	st, err := store.Open(root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	lock, err := install.ReadLock(st)
	if err != nil {
		return err
	}
	cat, err := loadCatalog(st)
	if err != nil {
		return err
	}
	opt := engine.SpeedOptions{LongTokens: *long, NoSandbox: cfg.Sandbox == config.SandboxOff, TempDir: st.Path("tmp"),
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }}
	if *models != "" {
		for _, m := range strings.Split(*models, ",") {
			opt.Models = append(opt.Models, strings.TrimSpace(m))
		}
	}
	for _, t := range strings.Split(*threads, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			opt.Threads = append(opt.Threads, n)
		}
	}
	opt.Threads = engine.SortThreads(opt.Threads)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	results, err := measure(ctx, st, lock, cat, opt)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(results)
	}
	printSpeeds(os.Stdout, results)
	return nil
}

// measure runs the bench, once the machine has the memory for it, and keeps
// the results.
func measure(ctx context.Context, st *store.Store, lock *install.Lock, cat *catalog.Catalog, opt engine.SpeedOptions) ([]speed.Result, error) {
	if err := os.MkdirAll(opt.TempDir, 0o700); err != nil {
		return nil, err
	}
	// The bench loads a model of its own: never beside a served copy on a
	// machine without room for both (swapping would make every number wrong).
	for _, p := range lock.Plan.Picks {
		if len(opt.Models) == 0 && p.Role != catalog.RoleBrain {
			continue
		}
		if len(opt.Models) > 0 && !contains(opt.Models, p.Model) {
			continue
		}
		if free, err := census.LiveAvailableGB(ctx, p.Pool); err == nil && free < p.MemGB {
			return nil, fmt.Errorf("%s needs %.1f GB and the %s pool has %.1f GB free: stop `brainiac serve` (or what else holds the memory) and try again",
				p.Model, p.MemGB, p.Pool, free)
		}
	}
	results, err := engine.MeasureSpeed(ctx, lock, cat, opt)
	if len(results) > 0 {
		t, lerr := speed.Load(st.Path(speed.File))
		if lerr != nil {
			return results, lerr
		}
		for _, r := range results {
			t.Put(r)
		}
		if werr := st.WriteJSON(speed.File, t); werr != nil {
			return results, werr
		}
	}
	if err != nil {
		return results, err
	}
	if len(results) == 0 {
		return nil, errors.New("nothing was measured")
	}
	return results, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// printSpeeds shows the measurements for a person.
func printSpeeds(w io.Writer, results []speed.Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tTHREADS\tREAD (short / long)\tWRITE\tTURN (cached / cold)\tCACHED\tSUSTAINED\tPOWER")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%d\t%.0f / %.0f tok/s\t%.1f tok/s\t%.0f s / %.0f s\t%.0f%%\t%.0f%%\t%s\n", r.Model, r.Threads,
			r.PrefillShortTPS, r.PrefillLongTPS, r.DecodeTPS, r.WarmTurnSec, r.ColdTurnSec, 100*r.WarmCachedShare, 100*r.Sustained, r.Power)
	}
	tw.Flush()
	for _, r := range results {
		if len(r.ThreadsTried) > 1 {
			var ts []int
			for t := range r.ThreadsTried {
				ts = append(ts, t)
			}
			sort.Ints(ts)
			var parts []string
			for _, t := range ts {
				if r.ThreadsTried[t] < 0 {
					parts = append(parts, fmt.Sprintf("%d threads over three times slower (given up)", t))
					continue
				}
				parts = append(parts, fmt.Sprintf("%d threads %.0f s", t, r.ThreadsTried[t]))
			}
			fmt.Fprintf(w, "%s: a 1000-token turn took %s; %d chosen\n", r.Model, strings.Join(parts, ", "), r.Threads)
		}
		fmt.Fprintf(w, "%s: the long prompt was %d tokens\n", r.Model, r.LongTokens)
	}
}
