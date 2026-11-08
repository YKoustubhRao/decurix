package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brainiac/internal/bench"
	"brainiac/internal/catalog"
	"brainiac/internal/census"
	"brainiac/internal/install"
	"brainiac/internal/scout"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

// laptop is an 8 GB laptop as WSL sees it (as in the selector's tests).
func laptop() census.Census {
	return census.Census{
		Host:    census.Host{OS: "linux", Arch: "amd64", WSL: true},
		CPU:     census.CPU{Model: "Intel i5-10210U", PhysicalCores: 3, LogicalCores: 6, Flags: []string{"avx", "avx2", "fma", "f16c"}},
		Pools:   []census.Pool{{Name: "host", Kind: "ram", TotalGB: 4.92, AvailableGB: 3.4}},
		Disk:    census.Disk{FreeGB: 75},
		Bench:   &census.Bench{MemCopyGBps: 17},
		Network: census.Network{HuggingFace: true, GitHub: true, PyPI: true},
		Python:  &census.Python{Path: "python3", Version: "3.12.3", Venv: true},
	}
}

var policy = selector.Policy{HaveProgram: func(string) bool { return false }}

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// nova is a new 2B model with a vision projector, in its maker's own GGUF
// repo (a first party on the trusted list; the catalog has no model of it).
func nova() scout.Candidate {
	fits := true
	return scout.Candidate{Repo: "mistralai/Nova-2B-GGUF", BaseRepo: "mistralai/Nova-2B", Base: "nova-2b", Role: catalog.RoleBrain,
		Created: now.AddDate(0, 0, -20), ParamsB: 2, Arch: "nova", ArchOK: true, License: "apache-2.0", LicenseClass: catalog.LicensePermissive,
		File: "Nova-2B-Q4_K_M.gguf", SizeGB: 1.3, MMProj: "mmproj-Nova-2B-f16.gguf", MMProjGB: 0.6, MemGB: 2.2, FitsHere: &fits, Usable: true}
}

// novaCard is the maker's card: Nova beside the catalog's qwen3.5-2b.
const novaCard = `# Nova-2B

| Benchmark | Qwen3.5-2B | Nova-2B |
|---|---|---|
| GPQA-Diamond | 40.0 | 50.0 |
| MMLU-Pro | 55.0 | 68.0 |
| MMMU | 50.0 | 60.0 |
| MathVista | 60.0 | 70.0 |
| ScreenSpot-Pro | 30.0 | 36.0 |
| OSWorld-G | 40.0 | 48.0 |
`

// novaBoard is what independent leaderboards say about Nova beside the
// catalog's qwen3.5-2b; better=false puts them level.
func novaBoard(better bool) *bench.Board {
	row := func(source, benchmark string, qwen, nova float64) []bench.Score {
		if !better {
			nova = qwen
		}
		return []bench.Score{
			{Model: "qwen3.5-2b", Benchmark: benchmark, Value: qwen, Source: source, Independent: true},
			{Model: "nova-2b", Benchmark: benchmark, Value: nova, Source: source, Independent: true},
		}
	}
	b := &bench.Board{}
	b.Add(row("livebench", "livebench reasoning", 40, 50)...)
	b.Add(row("livebench", "livebench language", 55, 68)...)
	b.Add(row("openvlm", "mmmu", 50, 60)...)
	b.Add(row("openvlm", "mathvista", 60, 70)...)
	b.Add(row("screenspot-pro", "screenspot-pro", 30, 36)...)
	return b
}

type fixture struct {
	st      *store.Store
	opt     Options
	checked [][]string
	install int
	pinned  int
}

func setup(t *testing.T, card string) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	plan := selector.Select(laptop(), shipped, st, policy)
	if b, _ := plan.Pick(catalog.RoleBrain); b.Model != "qwen3.5-2b" {
		t.Fatalf("the laptop starts on %s, want qwen3.5-2b", b.Model)
	}
	if err := st.WriteJSON("lock.json", &install.Lock{Schema: install.LockSchema, Plan: plan, Units: map[string]install.Unit{}}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st}
	f.opt = Options{
		Store:  st,
		Census: func(context.Context) (census.Census, error) { return laptop(), nil },
		Policy: policy,
		Now:    func() time.Time { return now },
		Scout: func(context.Context, scout.Options) (*scout.Report, error) {
			return &scout.Report{MadeAt: now, LlamaCpp: scout.LlamaReport{Pinned: shipped.Runtime.LlamaCppTag}, Candidates: []scout.Candidate{nova()}}, nil
		},
		Board: func(context.Context) *bench.Board { return novaBoard(true) },
		Cards: func(_ context.Context, repo string) ([]bench.Score, error) {
			if repo == "mistralai/Nova-2B" {
				return bench.ParseCard(card, "card "+repo), nil
			}
			return nil, errors.New("no card")
		},
		Install: func(_ context.Context, plan *selector.Plan, o install.Options) (*install.Lock, error) {
			f.install++
			l := &install.Lock{Schema: install.LockSchema, Plan: plan, Units: map[string]install.Unit{}}
			return l, o.Store.WriteJSON(o.LockName, l)
		},
		Check: func(_ context.Context, l *install.Lock, _ *catalog.Catalog, roles []string) error {
			f.checked = append(f.checked, roles)
			return nil
		},
		// The Hub's and GitHub's metadata, as install.Pinner would read it.
		Pin: func(_ context.Context, o *catalog.Overlay) error {
			f.pinned++
			for i := range o.Models {
				for j := range o.Models[i].Sources {
					s := &o.Models[i].Sources[j]
					s.Revision = strings.Repeat("c", 40)
					for k := range s.Files {
						s.Files[k].SHA256 = strings.Repeat("d", 64)
					}
				}
			}
			if o.Runtime != nil {
				o.Runtime.LlamaCppSHA256 = map[string]string{"llama.zip": strings.Repeat("e", 64)}
			}
			return nil
		},
	}
	return f
}

func brainOf(t *testing.T, st *store.Store, file string) string {
	t.Helper()
	var l install.Lock
	if err := st.ReadJSON(file, &l); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	b, _ := l.Plan.Pick(catalog.RoleBrain)
	return b.Model
}

func TestDryRunFindsTheBetterModelAndChangesNothing(t *testing.T) {
	f := setup(t, novaCard)
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeWould || len(res.Adopted) != 1 || res.Adopted[0].Model != "nova-2b" {
		t.Fatalf("%s: %s; adopted %+v; considered %+v", res.Outcome, res.Why, res.Adopted, res.Considered)
	}
	a := res.Adopted[0]
	// brain: median of 34 x 50/40 and 34 x 68/55, from the leaderboard alone
	if q := a.Roles[catalog.RoleBrain]; q != 42.3 {
		t.Errorf("brain rated %.1f, want 42.3", q)
	}
	for _, e := range a.Evidence {
		for _, p := range e.Pairs {
			if !p.Independent {
				t.Errorf("a card comparison rated it: %+v", p)
			}
		}
	}
	if _, ok := a.Roles[catalog.RoleGround]; !ok {
		t.Errorf("ground not rated: %+v", a.Roles)
	}
	if f.install != 0 || len(f.checked) != 0 {
		t.Error("a dry run installed or checked")
	}
	if _, err := os.Stat(f.st.Path(OverlayFile)); err == nil {
		t.Error("a dry run wrote the catalog overlay")
	}
	if brainOf(t, f.st, "lock.json") != "qwen3.5-2b" {
		t.Error("a dry run changed the lock")
	}
	if !f.st.Exists(ReportFile) {
		t.Error("no report")
	}
}

// Applying installs, checks and stages a better plan; what is served does
// not change until a person promotes it.
func TestApplyStagesAndOnlyAPersonPromotes(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStaged {
		t.Fatalf("%s: %s (changes %+v)", res.Outcome, res.Why, res.Changes)
	}
	if len(f.checked) != 1 || !strings.Contains(strings.Join(f.checked[0], ","), catalog.RoleBrain) {
		t.Fatalf("checked %v", f.checked)
	}
	if got := brainOf(t, f.st, "lock.json"); got != "qwen3.5-2b" {
		t.Fatalf("the updater switched lock.json to %s by itself", got)
	}
	if got := brainOf(t, f.st, NextLock); got != "nova-2b" || !f.st.Exists(StagedFile) || f.st.Exists(PrevLock) {
		t.Fatalf("staged %s, staged file %v", got, f.st.Exists(StagedFile))
	}
	o, err := catalog.ReadOverlay(f.st.Path(OverlayFile))
	if err != nil || len(o.Models) != 1 || !o.Models[0].Auto || !strings.Contains(o.Models[0].Basis, "livebench reasoning 50 vs qwen3.5-2b 40 (livebench)") {
		t.Fatalf("overlay %+v %v", o, err)
	}
	if o.Models[0].Sources[0].Unpinned() != "" {
		t.Fatalf("the adopted model is not pinned: %s", o.Models[0].Sources[0].Unpinned())
	}
	if _, _, err := catalog.WithOverlay(f.st.Path(OverlayFile), false); err != nil {
		t.Fatalf("the overlay does not validate: %v", err)
	}
	// The next cycle sees the same plan staged and does not install it again.
	res, err = Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStaged || f.install != 1 {
		t.Fatalf("second cycle: %s: %s, installs %d", res.Outcome, res.Why, f.install)
	}
	// A lock.next.json that changed after its check is not promoted.
	good, _ := os.ReadFile(f.st.Path(NextLock))
	os.WriteFile(f.st.Path(NextLock), append(append([]byte{}, good...), ' '), 0o644)
	if _, _, err := Promote(f.st, now, PromoteOptions{ByHand: true}); err == nil {
		t.Fatal("a changed lock.next.json was promoted")
	}
	os.WriteFile(f.st.Path(NextLock), good, 0o644)
	// A person promotes: lock.json is the staged plan, the old one is kept.
	lock, staged, err := Promote(f.st, now, PromoteOptions{ByHand: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := lock.Plan.Pick(catalog.RoleBrain); b.Model != "nova-2b" || len(staged.Changes) == 0 {
		t.Fatalf("promoted %s %+v", b.Model, staged)
	}
	if got := brainOf(t, f.st, "lock.json"); got != "nova-2b" {
		t.Fatalf("lock.json brain %s", got)
	}
	if got := brainOf(t, f.st, PrevLock); got != "qwen3.5-2b" {
		t.Fatalf("lock.prev.json brain %s", got)
	}
	if f.st.Exists(NextLock) || f.st.Exists(StagedFile) {
		t.Error("lock.next.json or the staged record left behind")
	}
	if s := ReadState(f.st); len(s.History) != 3 || s.History[2].Outcome != OutcomePromoted {
		t.Fatalf("state %+v", s)
	}
	if _, _, err := Promote(f.st, now, PromoteOptions{ByHand: true}); !errors.Is(err, ErrNothingStaged) {
		t.Fatalf("promoting twice: %v", err)
	}
	res, err = Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCurrent {
		t.Fatalf("after the promote: %s: %s %+v", res.Outcome, res.Why, res.Changes)
	}
}

func TestAFailedCheckSetsTheModelAsideAndKeepsTheOldOne(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	f.opt.Check = func(_ context.Context, _ *install.Lock, _ *catalog.Catalog, roles []string) error {
		f.checked = append(f.checked, roles)
		return &CheckFailed{Role: catalog.RoleBrain, Err: errors.New(`asked for "42", answered "<think>"`)}
	}
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeRejected || len(res.SetAside) != 1 || res.SetAside[0] != "nova-2b" {
		t.Fatalf("%s: %s set aside %v", res.Outcome, res.Why, res.SetAside)
	}
	if len(f.checked) != 1 {
		t.Errorf("checked %d plans; with nova set aside the old plan is current", len(f.checked))
	}
	if brainOf(t, f.st, "lock.json") != "qwen3.5-2b" || f.st.Exists(NextLock) || f.st.Exists(PrevLock) {
		t.Fatal("the running lock changed")
	}
	if r := ReadState(f.st).Rejected["nova-2b"]; !r.Until.Equal(now.Add(SetAside)) {
		t.Fatalf("rejection %+v", r)
	}
	res, err = Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeCurrent || !strings.Contains(fmt.Sprint(res.Considered), "failed its check") {
		t.Fatalf("second cycle: %s %+v", res.Outcome, res.Considered)
	}
	// A month later it is tried again.
	f.opt.Now = func() time.Time { return now.Add(SetAside + time.Hour) }
	if res, _ = Run(context.Background(), f.opt); res.Outcome != OutcomeRejected {
		t.Fatalf("after the set-aside: %s", res.Outcome)
	}
}

func TestBusyOrShortOfMemoryWaits(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	f.opt.Idle = func() bool { return false }
	res, _ := Run(context.Background(), f.opt)
	if res.Outcome != OutcomeDeferred || f.install != 0 {
		t.Fatalf("busy: %s, installs %d", res.Outcome, f.install)
	}
	f.opt.Idle = nil
	f.opt.Check = func(context.Context, *install.Lock, *catalog.Catalog, []string) error {
		return fmt.Errorf("%w: needs 2.4 GB, 1.1 free", ErrNotNow)
	}
	res, _ = Run(context.Background(), f.opt)
	if res.Outcome != OutcomeDeferred || len(ReadState(f.st).Rejected) != 0 {
		t.Fatalf("no room: %s, rejected %v", res.Outcome, ReadState(f.st).Rejected)
	}
	if brainOf(t, f.st, "lock.json") != "qwen3.5-2b" {
		t.Fatal("switched without a check")
	}
}

func TestAModelRatedNoBetterIsNotAdded(t *testing.T) {
	f := setup(t, `| Benchmark | Qwen3.5-2B | Nova-2B |
|---|---|---|
| GPQA-Diamond | 40.0 | 40.5 |
| MMLU-Pro | 55.0 | 55.5 |
| MMMU | 50.0 | 50.5 |
| MathVista | 60.0 | 60.5 |
| ScreenSpot-Pro | 30.0 | 30.0 |
| OSWorld-G | 40.0 | 40.0 |
`)
	f.opt.Apply = true
	f.opt.Board = func(context.Context) *bench.Board { return novaBoard(false) }
	res, _ := Run(context.Background(), f.opt)
	if res.Outcome != OutcomeCurrent || len(res.Adopted) != 0 || !strings.Contains(fmt.Sprint(res.Considered), "not above") {
		t.Fatalf("%s adopted %+v considered %+v", res.Outcome, res.Adopted, res.Considered)
	}
	if f.st.Exists(OverlayFile) {
		t.Error("the overlay was written")
	}
}

func TestOnlyTheMakersWordIsNotEnough(t *testing.T) {
	// The card rates Nova far above, but no leaderboard lists it yet.
	f := setup(t, novaCard)
	f.opt.Board = func(context.Context) *bench.Board { return &bench.Board{} }
	res, _ := Run(context.Background(), f.opt)
	if len(res.Adopted) != 0 || res.Outcome != OutcomeCurrent || !strings.Contains(fmt.Sprint(res.Considered), "waiting for an independent leaderboard") {
		t.Fatalf("%s adopted %+v considered %+v", res.Outcome, res.Adopted, res.Considered)
	}
}

func TestAModelUnderAnotherNameDoesNotInheritItsParentsScores(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Scout = func(context.Context, scout.Options) (*scout.Report, error) {
		c := nova()
		c.Repo, c.Base, c.BaseRepo = "mistralai/Starling-2B-GGUF", "starling-2b", "mistralai/Nova-2B" // tagged a quantisation of Nova
		return &scout.Report{Candidates: []scout.Candidate{c}}, nil
	}
	// The parent's rows are on a leaderboard.
	f.opt.Board = func(context.Context) *bench.Board { return novaBoard(true) }
	res, _ := Run(context.Background(), f.opt)
	if len(res.Adopted) != 0 || !strings.Contains(fmt.Sprint(res.Considered), "no public result") {
		t.Fatalf("adopted %+v considered %+v", res.Adopted, res.Considered)
	}
	for in, want := range map[[2]string]string{
		{"qwen3.5-4b", "Qwen/Qwen3.5-4B"}: "Qwen/Qwen3.5-4B", {"nanbeige-nanbeige4.2-3b", "Nanbeige/Nanbeige4.2-3B"}: "Nanbeige/Nanbeige4.2-3B",
		{"tiel-coder-35b-a3b", "ornith-ai/Ornith-1.5-35B-A3B"}: "",
	} {
		if got := baseIfSame(scout.Candidate{Base: in[0], BaseRepo: in[1]}); got != want {
			t.Errorf("baseIfSame(%v) = %q, want %q", in, got, want)
		}
	}
}

// The attack: a stranger's repo with a real model's name, tagged as that
// model's quantisation, with the real model's rows on a leaderboard.
func TestAStrangerUnderARealModelsNameIsNotAdopted(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	f.opt.Scout = func(context.Context, scout.Options) (*scout.Report, error) {
		c := nova()
		c.Repo = "someone/Nova-2B-GGUF"
		return &scout.Report{Candidates: []scout.Candidate{c}}, nil
	}
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 0 || f.install != 0 || !strings.Contains(fmt.Sprint(res.Considered), "not on the trusted list") {
		t.Fatalf("adopted %+v installs %d considered %+v", res.Adopted, f.install, res.Considered)
	}
	if b := brainOf(t, f.st, "lock.json"); b != "qwen3.5-2b" {
		t.Fatalf("now running %s", b)
	}
}

// A quantiser's copy, even of a model a leaderboard rates higher, is not
// adopted on its own since the uploader list holds first parties only
// (O-3.4); `brainiac scout adopt` is how a person adds it.
func TestAQuantisersCopyWaitsForAPerson(t *testing.T) {
	for _, repo := range []string{"unsloth/Nova-2B-GGUF", "ggml-org/Nova-2B-GGUF", "bartowski/Nova-2B-GGUF", "mradermacher/Nova-2B-GGUF"} {
		f := setup(t, novaCard)
		f.opt.Apply = true
		f.opt.Scout = func(context.Context, scout.Options) (*scout.Report, error) {
			c := nova()
			c.Repo = repo
			return &scout.Report{Candidates: []scout.Candidate{c}}, nil
		}
		res, err := Run(context.Background(), f.opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Adopted) != 0 || f.install != 0 || !strings.Contains(fmt.Sprint(res.Considered), "scout adopt") {
			t.Fatalf("%s: adopted %+v installs %d considered %+v", repo, res.Adopted, f.install, res.Considered)
		}
	}
}

// On an appliance the catalog changes only by a signed release: a cycle
// says what it found and adopts, pins and installs nothing (O-3.3).
func TestAnApplianceAdoptsNothing(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply, f.opt.SignedOnly = true, true
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if f.install != 0 || f.pinned != 0 || f.st.Exists(OverlayFile) || !strings.Contains(res.Why, "signed release") {
		t.Fatalf("%s: %s; installs %d pins %d overlay %v", res.Outcome, res.Why, f.install, f.pinned, f.st.Exists(OverlayFile))
	}
}

// A model the cycle cannot pin to a commit and sha256 is not installed.
func TestAModelThatCannotBePinnedIsNotInstalled(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	f.opt.Pin = func(context.Context, *catalog.Overlay) error { return errors.New("the Hub lists no sha256") }
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailed || f.install != 0 || f.st.Exists(OverlayFile) {
		t.Fatalf("%s: %s; installs %d", res.Outcome, res.Why, f.install)
	}
}

func TestAChangedTemplateHoldsTheModelForAPerson(t *testing.T) {
	f := setup(t, novaCard)
	f.opt.Apply = true
	installs := f.opt.Install
	f.opt.Install = func(ctx context.Context, plan *selector.Plan, o install.Options) (*install.Lock, error) {
		if b, _ := plan.Pick(catalog.RoleBrain); b.Model == "nova-2b" {
			return nil, &install.TemplateChanged{Model: "nova-2b"}
		}
		return installs(ctx, plan, o)
	}
	res, err := Run(context.Background(), f.opt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.SetAside, " "), "nova-2b") || !strings.Contains(strings.Join(res.Notes, " "), "chat template") {
		t.Fatalf("set aside %v notes %v", res.SetAside, res.Notes)
	}
	if b := brainOf(t, f.st, "lock.json"); b != "qwen3.5-2b" {
		t.Fatalf("a held model runs: %s", b)
	}
	if len(f.checked) != 0 {
		t.Errorf("checked %v", f.checked)
	}
	if ReadState(f.st).Rejected["nova-2b"].Why == "" {
		t.Fatal("the hold was not remembered")
	}
}

func TestNotRunnableModelsAreSkippedWithAReason(t *testing.T) {
	f := setup(t, novaCard)
	c := &cycle{opt: f.opt.withDefaults(), cat: mustCat(t), res: &Result{}}
	rep := &scout.Report{}
	big, nc, asr, split := nova(), nova(), nova(), nova()
	no := false
	big.FitsHere = &no
	nc.LicenseClass, nc.License = catalog.LicenseNonCommercial, "cc-by-nc-4.0"
	nc.Usable, nc.Problems = false, []string{"licence cc-by-nc-4.0 is non-commercial"}
	asr.Role = catalog.RoleASR
	split.File = "Nova-2B-Q4_K_M-00001-of-00002.gguf"
	for _, cand := range []scout.Candidate{big, nc, asr, split} {
		if why := c.notRunnable(cand, rep); why == "" {
			t.Errorf("%+v was runnable", cand)
		}
	}
	if why := c.notRunnable(nova(), rep); why != "" {
		t.Errorf("nova: %s", why)
	}
	restricted := nova()
	restricted.LicenseClass = catalog.LicenseRestricted
	if c.notRunnable(restricted, rep) == "" {
		t.Error("a restricted licence passed a permissive-only policy")
	}
}

func TestWorseAndPlannedFor(t *testing.T) {
	if worse([]Change{{Role: "brain", From: "a", To: "b", FromQuality: 34, ToQuality: 41}}) != "" {
		t.Error("a better brain was called worse")
	}
	if worse([]Change{{Role: "vision", From: "a", To: "", FromQuality: 60}, {Role: "brain", From: "a", To: "b", FromQuality: 34, ToQuality: 60}}) == "" {
		t.Error("losing vision was not worse")
	}
	if worse([]Change{{Role: "math", From: "vibe", To: "", FromQuality: 70, ToQuality: 72}}) != "" {
		t.Error("dropping a specialist the brain now beats was called worse")
	}
	if worse([]Change{{Role: "brain", From: "a", To: "b", FromQuality: 40, ToQuality: 30}}) == "" {
		t.Error("a weaker brain was not worse")
	}
	cs := laptop()
	cs.Pools[0].AvailableGB = 1.2 // our own models and a test run hold the rest
	plannedFor(&cs, &selector.Plan{Budgets: []selector.Budget{{Pool: "host", AvailableGB: 3.4}}})
	if cs.Pools[0].AvailableGB != 3.4 {
		t.Errorf("available %.1f", cs.Pools[0].AvailableGB)
	}
}

func TestPruneDeletesOnlyUnusedDownloadsInTheStore(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	old := filepath.Join(st.Path("models", "hf"), "a__old", "old.gguf")
	kept := filepath.Join(st.Path("models", "hf"), "b__kept", "kept.gguf")
	outside := filepath.Join(t.TempDir(), "mine.gguf")
	for _, p := range []string{old, kept, outside} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	hf := catalog.Source{Kind: catalog.SourceHF}
	unit := func(p string) install.Unit { return install.Unit{Source: hf, Files: map[string]string{"model": p}} }
	c := &cycle{st: st, logf: func(string, ...any) {}}
	c.prune(&install.Lock{Units: map[string]install.Unit{"o": unit(old), "k": unit(kept), "x": unit(outside)}},
		&install.Lock{Units: map[string]install.Unit{"k": unit(kept)}})
	if _, err := os.Stat(old); err == nil {
		t.Error("the unused download was kept")
	}
	for _, p := range []string{kept, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was deleted", p)
		}
	}
}

func mustCat(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestLiveCycleOnAMidSizePC runs a real cycle (Hub, leaderboards, cards) as
// if on a 32 GB PC, without installing anything.
func TestLiveCycleOnAMidSizePC(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") == "" {
		t.Skip("set BRAINIAC_LIVE=1 to read the real Hub and leaderboards")
	}
	pc := census.Census{
		Host:    census.Host{OS: "linux", Arch: "amd64"},
		CPU:     census.CPU{Model: "Ryzen 7", PhysicalCores: 8, LogicalCores: 16, Flags: []string{"avx", "avx2", "fma", "f16c", "avx512f"}},
		Pools:   []census.Pool{{Name: "host", Kind: "ram", TotalGB: 32, AvailableGB: 28}},
		Disk:    census.Disk{FreeGB: 400},
		Bench:   &census.Bench{MemCopyGBps: 40},
		Network: census.Network{HuggingFace: true, GitHub: true, PyPI: true},
		Python:  &census.Python{Path: "python3", Version: "3.12.3", Venv: true},
	}
	st, _ := store.Open(t.TempDir())
	shipped := mustCat(t)
	plan := selector.Select(pc, shipped, st, policy)
	st.WriteJSON("lock.json", &install.Lock{Schema: install.LockSchema, Plan: plan, Units: map[string]install.Unit{}})
	for _, p := range plan.Picks {
		t.Logf("now: %-9s %s (%.0f)", p.Role, p.Model, p.Quality)
	}
	res, err := Run(context.Background(), Options{Store: st, Policy: policy, Log: os.Stderr,
		Census: func(context.Context) (census.Census, error) { return pc, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", res.Outcome, res.Why)
	for _, a := range res.Adopted {
		t.Logf("adopted %s %v: %s", a.Model, a.Roles, a.Why)
		for _, e := range a.Evidence {
			for _, p := range e.Pairs {
				t.Logf("    %s %s: %g vs %s %g -> %.1f (%s)", e.Role, p.Benchmark, p.Candidate, p.Anchor, p.Versus, p.Estimate, p.Source)
			}
		}
	}
	for _, c := range res.Considered {
		if !strings.Contains(c.Why, "not runnable") {
			t.Logf("considered %s: %s", c.Repo, c.Why)
		}
	}
	for _, ch := range res.Changes {
		t.Logf("change %s: %s (%.0f) -> %s (%.0f)", ch.Role, ch.From, ch.FromQuality, ch.To, ch.ToQuality)
	}
}
