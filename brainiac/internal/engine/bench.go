// Measuring how fast installed models run on this machine (brainiac bench):
// a real llama-server with the settings serve would use, a short prompt, a
// long one read cold and then again with its start cached, and the short
// one again at the end to see whether the machine slowed as it warmed.
// Thread counts are compared first when more than one is worth trying.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"brainiac/internal/backend"
	"brainiac/internal/catalog"
	"brainiac/internal/install"
	"brainiac/internal/selector"
	"brainiac/internal/speed"
)

// SpeedOptions configure MeasureSpeed.
type SpeedOptions struct {
	// Models are the catalog ids to measure; empty means the brain's model.
	Models []string
	// Threads are the thread counts to compare; empty means half the
	// logical CPUs and all of them, when those differ.
	Threads []int
	// LongTokens is the long prompt's size; 0 means three quarters of the
	// model's window, at most 6000 (a desktop turn's prompt is about that).
	LongTokens int
	NoSandbox  bool
	// TempDir holds the servers' logs while measuring.
	TempDir string
	Logf    func(format string, a ...any)
}

// MeasureSpeed measures the chosen llama-server models of a lock.
func MeasureSpeed(ctx context.Context, lock *install.Lock, cat *catalog.Catalog, opt SpeedOptions) ([]speed.Result, error) {
	if lock == nil || lock.Plan == nil || lock.LlamaServer == "" {
		return nil, errors.New("nothing installed runs on llama-server")
	}
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	want := map[string]bool{}
	for _, m := range opt.Models {
		want[m] = true
	}
	brain, _ := lock.Plan.Pick(catalog.RoleBrain)
	var picks []selector.Pick
	seen := map[string]bool{}
	for _, p := range lock.Plan.Picks {
		if p.Source.Runtime != catalog.RuntimeLlamaCpp || p.Role == catalog.RoleEmbed || seen[p.Model] {
			continue
		}
		if (len(want) == 0 && p.Model == brain.Model) || want[p.Model] {
			seen[p.Model] = true
			picks = append(picks, p)
		}
	}
	if len(picks) == 0 {
		return nil, errors.New("no installed llama-server model matches what was asked to measure")
	}
	threads := opt.Threads
	if len(threads) == 0 {
		threads = []int{max(1, runtime.NumCPU()/2)}
		if n := runtime.NumCPU(); n != threads[0] {
			threads = append(threads, n)
		}
	}
	var out []speed.Result
	for _, p := range picks {
		r, err := measureOne(ctx, lock, cat, p, threads, opt, logf)
		if err != nil {
			return out, fmt.Errorf("%s: %w", p.Model, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// measureOne measures one model.
func measureOne(ctx context.Context, lock *install.Lock, cat *catalog.Catalog, p selector.Pick, threads []int, opt SpeedOptions, logf func(string, ...any)) (speed.Result, error) {
	inst := lock.Units[install.UnitKey(p.Model, p.Source)]
	window := selector.Context
	if m, ok := cat.Model(p.Model); ok {
		window = selector.ContextFor(m, p.Source)
	}
	long := opt.LongTokens
	if long <= 0 {
		long = min(window*3/4, 6000)
	}
	dir := opt.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	newProc := func(t int) *LlamaProc {
		return &LlamaProc{Bin: lock.LlamaServer, Model: inst.Files["model"], MMProj: inst.Files["mmproj"], Ctx: window, Threads: t,
			LogPath: filepath.Join(dir, "bench-"+p.Model+".log"), TemplateSHA: inst.TemplateSHA, NoSandbox: opt.NoSandbox,
			CacheRAMMiB: p.PromptCacheMB, Checkpoints: selector.CtxCheckpoints, OnGPU: p.OnGPU, Logf: logf}
	}
	r := speed.Result{Model: p.Model, Source: p.Source.Label(), File: speed.FileSig(inst.Files["model"]), Build: speed.FileSig(lock.LlamaServer),
		OnGPU: p.OnGPU, Power: speed.Power(), At: time.Now().UTC()}
	best := threads[0]
	if len(threads) > 1 {
		r.ThreadsTried = map[int]float64{}
		bestSec := 0.0
		var bestWall time.Duration
		for _, t := range threads {
			logf("%s: trying %d threads", p.Model, t)
			var short backend.Timings
			var wall time.Duration
			err := withServer(ctx, newProc(t), func(c *backend.OpenAI) error {
				// More threads than free cores make llama.cpp's spinning
				// threads starve each other: a turn can take many times
				// longer. A count three times slower than the best so far
				// is given up on, not waited out.
				tctx := ctx
				if bestWall > 0 {
					var cancel context.CancelFunc
					tctx, cancel = context.WithTimeout(ctx, max(3*bestWall, time.Minute))
					defer cancel()
				}
				t0 := time.Now()
				var err error
				short, err = benchTurn(tctx, c, benchText(int64(100+t), shortChars), countQuestion, 32)
				wall = time.Since(t0)
				return err
			})
			if err != nil && bestWall > 0 && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				logf("%s: %d threads is over three times slower than %d; not used", p.Model, t, best)
				r.ThreadsTried[t] = -1
				continue
			}
			if err != nil {
				return r, err
			}
			if bestWall == 0 || wall < bestWall {
				bestWall = wall
			}
			sec := turnSec(short, 1000, 200)
			r.ThreadsTried[t] = round1(sec)
			if bestSec == 0 || (sec > 0 && sec < bestSec) {
				best, bestSec = t, sec
			}
		}
	}
	r.Threads = best
	r.Settings = fmt.Sprintf("ctx=%d threads=%d kv=q8_0 spec=none cache=%dMiB", window, best, p.PromptCacheMB)
	logf("%s: measuring with %d threads (a %d-token prompt read cold takes a while on a CPU)", p.Model, best, long)
	err := withServer(ctx, newProc(best), func(c *backend.OpenAI) error {
		text := benchText(1, shortChars)
		short, err := benchTurn(ctx, c, text, countQuestion, 64)
		if err != nil {
			return err
		}
		r.PrefillShortTPS, r.DecodeTPS = tps(short.PromptN, short.PromptMs), tps(short.PredictedN, short.PredictedMs)
		// The long prompt is sized in this model's own tokens: figures
		// tokenize densely, so the short turn says how many characters one
		// token is here. It stays inside the window with room to answer.
		perToken := 4.0
		if n := short.CacheN + short.PromptN; n > 0 {
			perToken = float64(len(text)+len(countQuestion)) / float64(n)
		}
		system := benchText(2, int(float64(min(long, window-512))*perToken))
		cold, err := benchTurn(ctx, c, system, countQuestion, 64)
		if err != nil {
			return err
		}
		r.LongTokens = cold.CacheN + cold.PromptN
		r.PrefillLongTPS, r.DecodeLongTPS = tps(cold.PromptN, cold.PromptMs), tps(cold.PredictedN, cold.PredictedMs)
		r.ColdTurnSec = round1((cold.PromptMs + cold.PredictedMs) / 1000)
		warm, err := benchTurn(ctx, c, system, "Which entry has the largest amount? Answer with its number only.", 16)
		if err != nil {
			return err
		}
		if total := warm.CacheN + warm.PromptN; total > 0 {
			r.WarmCachedShare = round2(float64(warm.CacheN) / float64(total))
		}
		r.WarmTurnSec = round1((warm.PromptMs + warm.PredictedMs) / 1000)
		again, err := benchTurn(ctx, c, benchText(3, shortChars), countQuestion, 64)
		if err != nil {
			return err
		}
		if r.PrefillShortTPS > 0 {
			r.Sustained = round2(tps(again.PromptN, again.PromptMs) / r.PrefillShortTPS)
		}
		return nil
	})
	return r, err
}

// countQuestion makes the model write a steady stream of tokens to time.
const countQuestion = "Count from 1 to 100, separated by commas."

// withServer starts a server, warms it with one tiny turn (the first one
// pays for setting up), runs f, and stops it.
func withServer(ctx context.Context, p *LlamaProc, f func(c *backend.OpenAI) error) error {
	if err := p.Start(ctx); err != nil {
		return err
	}
	defer p.Stop()
	c := p.Client()
	if c == nil {
		return errors.New("llama-server stopped after loading")
	}
	if _, err := benchTurn(ctx, c, "You are brief.", "Say OK.", 2); err != nil {
		return err
	}
	return f(c)
}

// benchTurn runs one turn and returns llama-server's timings for it.
func benchTurn(ctx context.Context, c *backend.OpenAI, system, user string, maxTokens int) (backend.Timings, error) {
	zero := 0.0
	res, err := c.Chat(ctx, backend.ChatRequest{MaxTokens: maxTokens, Temperature: &zero,
		Messages: []backend.Message{{Role: "system", Text: system}, {Role: "user", Text: user}}})
	if err != nil {
		return backend.Timings{}, err
	}
	if res.Timings == nil {
		return backend.Timings{}, errors.New("this llama-server reports no timings")
	}
	return *res.Timings, nil
}

// shortChars is the short prompt's size: a few hundred tokens.
const shortChars = 1600

// benchText is a made-up ledger of about chars characters: varied numbers
// and words like the documents desktops read, the same for the same seed.
func benchText(seed int64, chars int) string {
	rng := rand.New(rand.NewPCG(uint64(seed), 7))
	words := []string{"invoice", "payment", "credit note", "receipt", "transfer", "refund", "advance", "adjustment"}
	parties := []string{"North Mill", "Blue River Stores", "Kite Logistics", "Amber Foods", "Delta Works", "Cedar Traders", "Lumen Press", "Orbit Supplies"}
	var b strings.Builder
	b.WriteString("Ledger extract for review.\n")
	for i := 1; b.Len() < chars; i++ {
		fmt.Fprintf(&b, "Entry %d: %s from %s, amount %d.%02d, dated day %d of month %d, reference %c%c-%d.\n",
			i, words[rng.IntN(len(words))], parties[rng.IntN(len(parties))], rng.IntN(90000)+100, rng.IntN(100),
			rng.IntN(28)+1, rng.IntN(12)+1, 'A'+rune(rng.IntN(26)), 'A'+rune(rng.IntN(26)), rng.IntN(9000)+1000)
	}
	return b.String()
}

// turnSec is a turn's seconds at the speeds one call measured.
func turnSec(t backend.Timings, newTokens, outTokens int) float64 {
	pre, dec := tps(t.PromptN, t.PromptMs), tps(t.PredictedN, t.PredictedMs)
	if pre <= 0 || dec <= 0 {
		return 0
	}
	return float64(newTokens)/pre + float64(outTokens)/dec
}

func tps(n int, ms float64) float64 {
	if n <= 0 || ms <= 0 {
		return 0
	}
	return round1(float64(n) / (ms / 1000))
}

// SortThreads is a thread list in order without repeats (for flags).
func SortThreads(in []int) []int {
	var out []int
	for _, t := range in {
		if t > 0 && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}
