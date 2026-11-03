// Command brainiac measures this machine, chooses the best open models it can
// run for each kind of work, installs them, and serves them behind one
// OpenAI-compatible endpoint.
//
//	brainiac census              what this machine has
//	brainiac plan                what it would choose, and why
//	brainiac pull [--yes]        install the plan
//	brainiac serve               run the endpoint from what is installed
//	brainiac up [--yes]          census + plan + pull + serve: what build.sh runs
//	brainiac ask "..." [--image f] [--audio f]   ask a running Brainiac
//	brainiac status              what a running Brainiac holds
//	brainiac update [--apply]    a better plan from public evaluations: shown, or installed, checked and staged
//	brainiac update promote      switch to the staged plan (a person's decision)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "census":
		err = runCensus(args)
	case "plan":
		err = runPlan(args)
	case "pull":
		err = runPull(args)
	case "serve":
		err = runServe(args)
	case "up":
		err = runUp(args)
	case "ask":
		err = runAsk(args)
	case "status":
		err = runStatus(args)
	case "paths":
		err = runPaths(args)
	case "eval":
		err = runEval(args)
	case "scout":
		err = runScout(args)
	case "update":
		err = runUpdate(args)
	case "version":
		fmt.Println("brainiac", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brainiac:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `brainiac - many open models behind one endpoint, chosen for this machine

  census [--json] [--no-bench] [--no-network]   measure this machine
  plan   [--json] [--verbose] [plan flags]       choose models, and say why
  pull   [--yes] [--latest] [plan flags]         install the plan; --latest moves installed models to the newest commit
  serve                                          serve what is installed
  up     [--yes] [plan flags]                    census, plan, pull, serve
  ask    "question" [--image file] [--audio file] [--model m] [--url u]
  status [--url u]
  paths  [--url u]                                 per field: what could answer here, and why each does or does not
  eval   --cases file.jsonl [--models a,b] [--learn]  measure models field by field on checked cases
  update [--apply] [--json]                       find better open models from public evaluations; --apply
                                                 installs, checks and stages them, changing nothing served
  update promote                                  switch to the staged plan; a running serve follows
  update approve-template <model>                 accept a model's changed chat template, after reading it
  scout  [--days 120]                              find open models newer than the catalog, checked
  scout adopt <repo> --roles code=72               add a scouted model to the local catalog
  scout pin b11xxx                                 move to a newer llama.cpp build

plan flags: --offline  --licenses permissive,restricted  --reserve GB  --planned-load GB  --domains math,law|none
home: BRAINIAC_HOME (default ~/.brainiac)
`)
}

// common holds what most commands share.
type common struct {
	home        string
	offline     bool
	licenses    string
	reserve     float64
	plannedLoad float64
	domains     string
	json        bool
	verbose     bool
	yes         bool
	latest      bool
	noBench     bool
	noNetwork   bool
}

func flags(name string, args []string, plan bool) (*common, *flag.FlagSet, error) {
	c := &common{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.home, "home", "", "Brainiac's home directory (default BRAINIAC_HOME or ~/.brainiac)")
	fs.BoolVar(&c.json, "json", false, "print JSON")
	fs.BoolVar(&c.verbose, "verbose", false, "list every model not chosen, and why")
	fs.BoolVar(&c.yes, "yes", false, "install without asking")
	fs.BoolVar(&c.noBench, "no-bench", false, "skip the memory-bandwidth test")
	fs.BoolVar(&c.noNetwork, "no-network", false, "skip the network checks")
	if plan {
		fs.BoolVar(&c.latest, "latest", false, "move installed models to their repo's newest commit")
		fs.BoolVar(&c.offline, "offline", false, "choose only what is installed")
		fs.StringVar(&c.licenses, "licenses", "", "licence classes to allow (permissive,restricted,noncommercial)")
		fs.Float64Var(&c.reserve, "reserve", 0, "GB to keep free in every memory pool")
		fs.Float64Var(&c.plannedLoad, "planned-load", 0, "GB other work will need later (desktops not yet started)")
		fs.StringVar(&c.domains, "domains", "", "fields to find specialists for (math,code,law...), or none; default every field")
	}
	return c, fs, fs.Parse(args)
}

func openStore(c *common) (*store.Store, error) {
	root := c.home
	if root == "" {
		root = store.DefaultRoot()
	}
	return store.Open(root)
}

func takeCensus(ctx context.Context, c *common, st *store.Store, cfg config.Config) (census.Census, error) {
	fmt.Fprintln(os.Stderr, "Measuring this machine...")
	cs, err := census.Take(ctx, census.Options{StoreDir: st.Path("models"), OllamaURL: cfg.OllamaURL, Bench: !c.noBench, Network: !c.noNetwork})
	if err != nil {
		return cs, err
	}
	_ = st.WriteJSON("census.json", cs)
	return cs, nil
}

func runCensus(args []string) error {
	c, _, err := flags("census", args, false)
	if err != nil {
		return err
	}
	st, err := openStore(c)
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	cs, err := takeCensus(context.Background(), c, st, cfg)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(cs)
	}
	fmt.Print(cs.Summary())
	return nil
}

// makePlan measures, chooses and saves the plan.
func makePlan(ctx context.Context, c *common, st *store.Store, cfg config.Config) (*selector.Plan, census.Census, error) {
	cs, err := takeCensus(ctx, c, st, cfg)
	if err != nil {
		return nil, cs, err
	}
	cat, err := loadCatalog(st)
	if err != nil {
		return nil, cs, err
	}
	pol := selector.Policy{Offline: c.offline, ReserveGB: firstNonZero(c.reserve, cfg.ReserveGB), PlannedLoadGB: firstNonZero(c.plannedLoad, cfg.PlannedLoadGB)}
	if c.licenses != "" {
		pol.Licenses = strings.Split(c.licenses, ",")
	} else if len(cfg.Licenses) > 0 {
		pol.Licenses = cfg.Licenses
	}
	if c.domains != "" {
		pol.Domains = strings.Split(c.domains, ",")
	}
	var prev selector.Plan
	if st.ReadJSON("plan.json", &prev) == nil {
		pol.Previous = &prev
	}
	plan := selector.Select(cs, cat, st, pol)
	if err := st.WriteJSON("plan.json", plan); err != nil {
		return nil, cs, err
	}
	return plan, cs, nil
}

func firstNonZero(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}

func runPlan(args []string) error {
	c, _, err := flags("plan", args, true)
	if err != nil {
		return err
	}
	st, err := openStore(c)
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	plan, _, err := makePlan(context.Background(), c, st, cfg)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(plan)
	}
	plan.Render(os.Stdout, c.verbose)
	return nil
}

func runPull(args []string) error {
	c, _, err := flags("pull", args, true)
	if err != nil {
		return err
	}
	st, err := openStore(c)
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = pull(ctx, c, st, cfg)
	return err
}

// pull plans, shows the plan, asks, and installs.
func pull(ctx context.Context, c *common, st *store.Store, cfg config.Config) (*install.Lock, error) {
	plan, cs, err := makePlan(ctx, c, st, cfg)
	if err != nil {
		return nil, err
	}
	fmt.Println()
	plan.Render(os.Stdout, c.verbose)
	fmt.Println()
	if len(plan.Picks) == 0 {
		return nil, errors.New("nothing fits this machine; see the notes above")
	}
	if !c.yes {
		ok, err := confirm(fmt.Sprintf("Install this (%.2f GB to download)?", plan.DownloadGB))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("not installed")
		}
	}
	lock, err := install.Run(ctx, plan, install.Options{Store: st, Census: cs, WorkerDir: workerDir(), Log: os.Stdout, Latest: c.latest})
	if err != nil {
		return nil, err
	}
	fmt.Printf("\nInstalled. Lock file: %s\n", st.Path("lock.json"))
	return lock, nil
}

func confirm(question string) (bool, error) {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("not a terminal: pass --yes to install without asking")
	}
	fmt.Printf("%s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

// workerDir finds brainiac_worker.py: BRAINIAC_WORKER_DIR, or workers/python
// beside or above the executable, or under the working directory.
func workerDir() string {
	if d := os.Getenv("BRAINIAC_WORKER_DIR"); d != "" {
		return d
	}
	var tries []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		tries = append(tries, filepath.Join(dir, "workers", "python"), filepath.Join(dir, "..", "workers", "python"))
	}
	if wd, err := os.Getwd(); err == nil {
		tries = append(tries, filepath.Join(wd, "workers", "python"), filepath.Join(wd, "brainiac", "workers", "python"))
	}
	for _, d := range tries {
		if _, err := os.Stat(filepath.Join(d, "brainiac_worker.py")); err == nil {
			abs, _ := filepath.Abs(d)
			return abs
		}
	}
	return filepath.Join("workers", "python")
}

func runServe(args []string) error {
	c, _, err := flags("serve", args, false)
	if err != nil {
		return err
	}
	st, err := openStore(c)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, st, cfg, lock)
}

func serve(ctx context.Context, st *store.Store, cfg config.Config, lock *install.Lock) error {
	lg := log.New(os.Stderr, "brainiac: ", log.LstdFlags)
	cat, err := loadCatalog(st)
	if err != nil {
		return err
	}
	eng, err := engine.New(engine.Options{Config: cfg, Store: st, Lock: lock, Logger: lg, Catalog: cat})
	if err != nil {
		return err
	}
	as := &api.Server{Eng: eng, APIKey: cfg.APIKey(), Log: lg}
	defer func() { as.Current().Close() }()
	// The updater reads public evaluations on the web and only stages what it
	// finds; it is off unless update_every is set, and off with the web.
	if every, ok := updateEvery(cfg); ok {
		go updateLoop(ctx, st, cfg, as, every, lg)
	}
	// What is served changes when a person changes lock.json.
	follow := &lockFollower{path: st.Path("lock.json"), seen: lockSHA(st.Path("lock.json")), swap: as.SetEngine, logf: lg.Printf,
		build: func() (*engine.Engine, error) {
			l, err := install.ReadLock(st)
			if err != nil {
				return nil, err
			}
			c, err := loadCatalog(st)
			if err != nil {
				return nil, err
			}
			return engine.New(engine.Options{Config: cfg, Store: st, Lock: l, Logger: lg, Catalog: c})
		}}
	go follow.run(ctx, 30*time.Second)
	srv := &http.Server{Addr: cfg.Listen, Handler: as.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	st2 := eng.Status()
	fmt.Printf("Brainiac is serving on http://%s (mode %s)\n", cfg.Listen, st2.Mode)
	for _, r := range []string{catalog.RoleBrain, catalog.RoleVision, catalog.RoleGround, catalog.RoleOCR, catalog.RoleASR, catalog.RoleVAD, catalog.RoleEmbed} {
		if m, ok := st2.Roles[r]; ok {
			fmt.Printf("  %-7s %s\n", r, m)
		}
	}
	if st2.Remote != "" {
		fmt.Printf("  remote  %s\n", st2.Remote)
	}
	fmt.Println("OpenAI-compatible: POST /v1/chat/completions, /v1/audio/transcriptions, /v1/embeddings; GET /v1/models")
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		fmt.Println("\nStopping: unloading models...")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func runUp(args []string) error {
	c, _, err := flags("up", args, true)
	if err != nil {
		return err
	}
	st, err := openStore(c)
	if err != nil {
		return err
	}
	cfg, err := config.Load(st.Root)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lock, err := pull(ctx, c, st, cfg)
	if err != nil {
		return err
	}
	return serve(ctx, st, cfg, lock)
}

func runAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8484", "Brainiac's address")
	model := fs.String("model", "brainiac", "model to ask")
	key := fs.String("key-env", "", "environment variable holding the API key")
	var images, audios multi
	fs.Var(&images, "image", "a picture to include (repeatable)")
	fs.Var(&audios, "audio", "a recording to include (repeatable)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		return errors.New(`ask needs a question: brainiac ask "what does this say?" --image page.png`)
	}
	parts := []map[string]any{{"type": "text", "text": prompt}}
	for _, f := range images {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		mt := http.DetectContentType(data)
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)}})
	}
	for _, f := range audios {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		parts = append(parts, map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(data), "format": strings.TrimPrefix(filepath.Ext(f), ".")}})
	}
	body, _ := json.Marshal(map[string]any{"model": *model, "stream": true, "messages": []any{map[string]any{"role": "user", "content": parts}}})
	req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(*url, "/")+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if *key != "" {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(*key))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	var ext any
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": "):
			fmt.Fprintf(os.Stderr, "[%s]\n", strings.TrimPrefix(line, ": "))
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				continue
			}
			var ch struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Brainiac any `json:"brainiac"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &ch) != nil {
				continue
			}
			if ch.Error != nil {
				return errors.New(ch.Error.Message)
			}
			for _, c := range ch.Choices {
				fmt.Print(c.Delta.Content)
			}
			if ch.Brainiac != nil {
				ext = ch.Brainiac
			}
		}
	}
	fmt.Println()
	if ext != nil {
		b, _ := json.MarshalIndent(ext, "", "  ")
		fmt.Fprintf(os.Stderr, "\n%s\n", b)
	}
	return sc.Err()
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8484", "Brainiac's address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := http.Get(strings.TrimRight(*url, "/") + "/v1/brainiac/status")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

// runPaths prints, per field, which local models have earned it and why.
func runPaths(args []string) error {
	fs := flag.NewFlagSet("paths", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8484", "Brainiac's address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := http.Get(strings.TrimRight(*url, "/") + "/v1/brainiac/paths")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var paths map[string][]engine.PathStatus
	if err := json.NewDecoder(resp.Body).Decode(&paths); err != nil {
		return err
	}
	fields := make([]string, 0, len(paths))
	for f := range paths {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		fmt.Println(f)
		for _, p := range paths[f] {
			mark := "  remote keeps it"
			if p.Earned {
				mark = "  earned"
			}
			fmt.Printf("%s  %s: %s\n", mark, p.Model, p.Why)
			if p.Live.Missed+p.Live.Passed > 0 {
				fmt.Printf("    real answers reported: %d passed, %d missed\n", p.Live.Passed, p.Live.Missed)
			}
		}
	}
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// reorder puts flags before the question, so `ask "q" --image f` works.
func reorder(args []string) []string {
	var flagsOut, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") || (strings.HasPrefix(a, "-") && len(a) > 1) {
			flagsOut = append(flagsOut, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flagsOut = append(flagsOut, args[i+1])
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flagsOut, rest...)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
