package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/config"
	"brainiac/internal/hub"
	"brainiac/internal/install"
	"brainiac/internal/scout"
	"brainiac/internal/selector"
	"brainiac/internal/store"
	"brainiac/internal/update"
)

// overlayFile is the catalog additions (the updater's and the operator's)
// under the store root.
const overlayFile = update.OverlayFile

// loadCatalog is the shipped catalog with the overlay merged in, when the
// overlay may be used: on an appliance only a signed one is
// (catalog.ReadTrustedOverlay). Why an overlay was not used goes to stderr.
func loadCatalog(st *store.Store) (*catalog.Catalog, error) {
	c, note, err := catalog.WithOverlay(st.Path(overlayFile), config.Appliance())
	if note != "" {
		fmt.Fprintln(os.Stderr, "brainiac: "+note)
	}
	return c, err
}

// errApplianceCatalog refuses a local catalog change on an appliance.
var errApplianceCatalog = errors.New("on an appliance the catalog changes only by a signed release (catalog.local.json with its .sig; `brainiac catalog verify` checks one)")

// runScout looks for open models newer than the catalog, or handles
// `scout adopt` and `scout pin`.
func runScout(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "adopt":
			return runAdopt(args[1:])
		case "pin":
			return runPin(args[1:])
		}
	}
	fs := flag.NewFlagSet("scout", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	days := fs.Int("days", 120, "only models created in the last this many days")
	max := fs.Int("max", 30, "candidates to check in detail")
	links := fs.Bool("links", true, "look up benchmark pages for the best candidates")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(os.Stderr, "Scouting the Hugging Face Hub for new open models...")
	rep, err := scoutOnce(ctx, st, scout.Options{Since: time.Now().AddDate(0, 0, -*days), MaxVerify: *max, Links: *links, Log: os.Stderr})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(rep)
	}
	renderScout(os.Stdout, rep)
	return nil
}

// scoutOnce runs the scout with this store's catalog and budget, and saves the report.
func scoutOnce(ctx context.Context, st *store.Store, opt scout.Options) (*scout.Report, error) {
	cat, err := loadCatalog(st)
	if err != nil {
		return nil, err
	}
	opt.Catalog = cat
	var plan selector.Plan
	if st.ReadJSON("plan.json", &plan) == nil {
		for _, b := range plan.Budgets {
			if b.Pool == "host" {
				opt.BudgetGB = b.BudgetGB
			}
		}
	}
	rep, err := scout.Run(ctx, opt)
	if err != nil {
		return nil, err
	}
	return rep, st.WriteJSON("scout.json", rep)
}

func renderScout(w io.Writer, rep *scout.Report) {
	fmt.Fprintf(w, "Scouted %s against catalog %s\n", rep.MadeAt.Local().Format("2006-01-02 15:04"), rep.CatalogVersion)
	l := rep.LlamaCpp
	switch {
	case l.NewestSafe == "":
		fmt.Fprintf(w, "llama.cpp: pinned %s; newer builds could not be listed\n", l.Pinned)
	case l.NewestSafe == l.Pinned:
		fmt.Fprintf(w, "llama.cpp: pinned %s is the newest build at least two weeks old (%d architectures)\n", l.Pinned, l.PinnedArchs)
	default:
		fmt.Fprintf(w, "llama.cpp: pinned %s; %s (%s) is at least two weeks old", l.Pinned, l.NewestSafe, l.NewestDate)
		if len(l.NewArchs) > 0 {
			fmt.Fprintf(w, " and adds %s", strings.Join(l.NewArchs, ", "))
		}
		fmt.Fprintf(w, " - `brainiac scout pin %s` moves to it\n", l.NewestSafe)
	}
	var usable, other []scout.Candidate
	for _, c := range rep.Candidates {
		if c.Usable {
			usable = append(usable, c)
		} else {
			other = append(other, c)
		}
	}
	fmt.Fprintln(w, "\nNew models Brainiac could run (a GGUF file, a licence that allows use, an architecture llama.cpp knows):")
	if len(usable) == 0 {
		fmt.Fprintln(w, "  none")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(usable) > 0 {
		fmt.Fprintln(tw, "  ROLE\tREPO\tCREATED\tPARAMS\tLICENCE\tSIZE\tFITS HERE\tCLAIMS")
	}
	for _, c := range usable {
		fits := "?"
		if c.FitsHere != nil {
			fits = map[bool]string{true: "yes", false: "no"}[*c.FitsHere]
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.1fB\t%s (%s)\t%.1f GB\t%s\t%s\n", c.Role, c.Repo, c.Created.Format("2006-01-02"), c.ParamsB,
			c.License, c.LicenseClass, c.SizeGB+c.MMProjGB, fits, claims(c.Claims, 3))
	}
	tw.Flush()
	for _, c := range usable {
		if !scout.TrustedUploader(c.Repo) {
			fmt.Fprintf(w, "  %s: its uploader is not on the trusted list, so only a person can add it\n", c.Repo)
		}
	}
	if len(other) > 0 {
		fmt.Fprintln(w, "\nNew, but not usable yet:")
		for _, c := range other {
			fmt.Fprintf(w, "  %-9s %-50s %s\n", c.Role, c.Repo, strings.Join(c.Problems, "; "))
		}
	}
	for _, c := range usable {
		if len(c.Links) > 0 {
			fmt.Fprintf(w, "\nRead about %s: %s", c.Repo, strings.Join(c.Links, "  "))
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "\nNote: %s", n)
	}
	fmt.Fprintln(w, "\n\n`brainiac serve` rates these from public evaluations and switches to the better ones on its own")
	fmt.Fprintln(w, "(`brainiac update` shows what it would do). To add one by hand with your own rating:")
	fmt.Fprintln(w, "  brainiac scout adopt <repo> --roles brain=40,vision=60   (quality 0-100 on the catalog's scale)")
}

func claims(cs []hub.Claim, n int) string {
	var out []string
	for i, c := range cs {
		if i == n {
			break
		}
		out = append(out, fmt.Sprintf("%s %g", c.Dataset, c.Value))
	}
	return strings.Join(out, ", ")
}

var quantRe = regexp.MustCompile(`(?i)(IQ\d_[A-Z]+|Q\d_K_[MSL]|Q\d_K|Q\d_\d|Q\d|MXFP4|BF16|F16)`)

// runAdopt adds a scouted model to the operator's catalog overlay.
func runAdopt(args []string) error {
	fs := flag.NewFlagSet("scout adopt", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	roles := fs.String("roles", "", "role=quality pairs, e.g. brain=40,vision=60 or code=72 (quality 0-100 on the catalog's scale)")
	id := fs.String("id", "", "the catalog id to give it (default: from the repo name)")
	file := fs.String("file", "", "the GGUF file to use (default: the scout's pick)")
	mmproj := fs.String("mmproj", "", `the vision projector file ("none" for none; default: the scout's pick)`)
	thinks := fs.Bool("thinks", false, "it is a reasoning model (gets room to think)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *roles == "" {
		return errors.New("usage: brainiac scout adopt <repo> --roles code=72 [--file x.gguf] [--mmproj y.gguf|none] [--thinks]")
	}
	repo := fs.Arg(0)
	if config.Appliance() {
		return errApplianceCatalog
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	ctx := context.Background()
	var rep scout.Report
	var cand *scout.Candidate
	if st.ReadJSON("scout.json", &rep) == nil {
		for i := range rep.Candidates {
			if strings.EqualFold(rep.Candidates[i].Repo, repo) {
				cand = &rep.Candidates[i]
			}
		}
	}
	h := hub.New()
	pinner := install.NewPinner()
	pinner.HF = h
	if cand == nil {
		d, err := h.Details(ctx, repo)
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		cand = &scout.Candidate{Repo: repo, ParamsB: d.ParamsB, Arch: d.Architecture, License: d.License,
			LicenseClass: scout.LicenseClass(d.License), Claims: d.Claims}
	}
	// The files are read at the commit "main" points at now, and the entry
	// is pinned to that commit and to the files' sha256 below: what a person
	// adopts is what they looked at, not whatever is pushed later.
	commit, err := h.Revision(ctx, repo, "main")
	if err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}
	files, err := h.Files(ctx, repo, commit)
	if err != nil {
		return fmt.Errorf("%s: %w", repo, err)
	}
	pick := func(name string) (string, int64) {
		for _, f := range files {
			if f.Path == name {
				return f.Path, f.Size
			}
		}
		return "", 0
	}
	model, modelSize := pick(cand.File)
	if *file != "" {
		model, modelSize = pick(*file)
	}
	if model == "" {
		return fmt.Errorf("%s: no GGUF file %q in the repo (pass --file)", repo, firstNonEmpty(*file, cand.File))
	}
	srcFiles := []catalog.File{{Path: model, Role: "model"}}
	total := modelSize
	if *mmproj != "none" {
		mm, mmSize := pick(cand.MMProj)
		if *mmproj != "" {
			mm, mmSize = pick(*mmproj)
		}
		if mm != "" {
			srcFiles = append(srcFiles, catalog.File{Path: mm, Role: "mmproj"})
			total += mmSize
		}
	}
	roleMap := map[string]float64{}
	for _, pair := range strings.Split(*roles, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		q, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil || q < 0 || q > 100 {
			return fmt.Errorf("--roles: %q is not role=quality (0-100)", pair)
		}
		roleMap[k] = q
	}
	mid := *id
	if mid == "" {
		mid = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(repo[strings.LastIndex(repo, "/")+1:], "-GGUF"), "-gguf"))
	}
	m := catalog.Model{
		ID: mid, Title: repo, Family: strings.SplitN(repo, "/", 2)[0], Roles: roleMap,
		Basis: fmt.Sprintf("Adopted from the scout on %s by the operator. The quality numbers are the operator's prior, not measured; brainiac eval decides routing. Card claims: %s",
			time.Now().Format("2006-01-02"), firstNonEmpty(claims(cand.Claims, 6), "none")),
		ParamsB: cand.ParamsB, KVPerTokenMB: 0.012 * cand.ParamsB, // unknown: a cautious guess
		License: cand.License, LicenseClass: firstNonEmpty(cand.LicenseClass, catalog.LicenseRestricted), Thinks: *thinks,
		Sources: []catalog.Source{{Kind: catalog.SourceHF, Runtime: catalog.RuntimeLlamaCpp, Repo: repo, Revision: commit, Quant: quantRe.FindString(model),
			Files: srcFiles, SizeGB: float64(total) / 1e9, WeightsGB: float64(modelSize) / 1e9}},
	}
	if err := pinner.Source(ctx, &m.Sources[0]); err != nil {
		return fmt.Errorf("%s: cannot pin it to a commit and sha256, so it could not be installed: %w", repo, err)
	}
	if *thinks {
		m.ThinkTokens, m.Ctx = 6000, 16384
	}
	o, err := catalog.ReadOverlay(st.Path(overlayFile))
	if err != nil {
		return err
	}
	replaced := false
	for i := range o.Models {
		if o.Models[i].ID == m.ID {
			o.Models[i], replaced = m, true
		}
	}
	if !replaced {
		o.Models = append(o.Models, m)
	}
	o.Updated = time.Now().Format("2006-01-02")
	if err := catalog.SaveOverlay(st.Path(overlayFile), o); err != nil {
		return err
	}
	if _, err := loadCatalog(st); err != nil {
		return fmt.Errorf("the overlay no longer validates, fix %s: %w", st.Path(overlayFile), err)
	}
	fmt.Printf("Adopted %s as %q (%s), pinned to commit %s. `brainiac plan` may now choose it; `brainiac eval` decides whether it takes work.\n", repo, m.ID, *roles, commit[:12])
	return nil
}

// runPin moves the llama.cpp build in the operator's overlay.
func runPin(args []string) error {
	fs := flag.NewFlagSet("scout pin", flag.ContinueOnError)
	home := fs.String("home", "", "Brainiac's home")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || !strings.HasPrefix(fs.Arg(0), "b") {
		return errors.New("usage: brainiac scout pin b11xxx")
	}
	if config.Appliance() {
		return errApplianceCatalog
	}
	st, err := openStore(&common{home: *home})
	if err != nil {
		return err
	}
	cat, err := catalog.Default()
	if err != nil {
		return err
	}
	o, err := catalog.ReadOverlay(st.Path(overlayFile))
	if err != nil {
		return err
	}
	rt := &catalog.Runtime{LlamaCppRepo: cat.Runtime.LlamaCppRepo, LlamaCppTag: fs.Arg(0)}
	missing, err := install.NewPinner().Runtime(context.Background(), rt)
	if err != nil {
		return fmt.Errorf("llama.cpp %s: %w", fs.Arg(0), err)
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "GitHub gives no digest for %d assets of %s (%s); they cannot be installed\n", len(missing), fs.Arg(0), strings.Join(missing, ", "))
	}
	o.Runtime = rt
	o.Updated = time.Now().Format("2006-01-02")
	if err := catalog.SaveOverlay(st.Path(overlayFile), o); err != nil {
		return err
	}
	fmt.Printf("llama.cpp pinned to %s; `brainiac pull` fetches it.\n", fs.Arg(0))
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
