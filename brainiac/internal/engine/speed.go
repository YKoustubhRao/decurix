// What local models' recent calls measured: how much of each prompt came
// from llama-server's prompt cache, and how fast the model read prompts and
// wrote answers. On a CPU a prompt read from its start costs minutes, so a
// cache that stops hitting is the first thing to know about; the speeds
// also let routing estimate a request's time from evidence, not a guess.
package engine

import (
	"log"
	"sort"
	"sync"
	"time"

	"brainiac/internal/backend"
)

// speedWindow is how many recent calls per model are kept.
const speedWindow = 20

// Long prompts are where the cache matters: the hit rate is judged on calls
// with at least cacheJudgedTokens of prompt, once there are cacheJudgedCalls
// of them, and a rate under cacheWarnRate is a warning.
const (
	cacheJudgedTokens = 1000
	cacheJudgedCalls  = 6
	cacheWarnRate     = 0.3
	cacheWarnEvery    = 30 * time.Minute
)

// SpeedStat is one model's recent calls, summed.
type SpeedStat struct {
	Calls int `json:"calls"`
	// CacheHitRate is the share of long prompts' tokens that came from the
	// prompt cache (-1 when no long prompt was seen yet).
	CacheHitRate float64 `json:"cache_hit_rate"`
	// PrefillTPS and DecodeTPS are prompt tokens read and answer tokens
	// written per second (0 when not measured).
	PrefillTPS float64 `json:"prefill_tps,omitempty"`
	DecodeTPS  float64 `json:"decode_tps,omitempty"`
	// DraftAcceptRate is the share of speculative drafts kept, when used.
	DraftAcceptRate float64 `json:"draft_accept_rate,omitempty"`
	Warning         string  `json:"warning,omitempty"`
}

type speedCall struct {
	t  backend.Timings
	at time.Time
}

// liveSpeed keeps the last calls per model.
type liveSpeed struct {
	mu     sync.Mutex
	calls  map[string][]speedCall
	warned map[string]time.Time
}

func newLiveSpeed() *liveSpeed {
	return &liveSpeed{calls: map[string][]speedCall{}, warned: map[string]time.Time{}}
}

// record files one call's timings and logs, at most every cacheWarnEvery,
// when long prompts have stopped coming from the cache.
func (l *liveSpeed) record(model string, t backend.Timings, lg *log.Logger) {
	if l == nil {
		return
	}
	l.mu.Lock()
	cs := append(l.calls[model], speedCall{t: t, at: time.Now()})
	if len(cs) > speedWindow {
		cs = cs[len(cs)-speedWindow:]
	}
	l.calls[model] = cs
	st := summarize(cs)
	warn := st.Warning != "" && time.Since(l.warned[model]) > cacheWarnEvery
	if warn {
		l.warned[model] = time.Now()
	}
	l.mu.Unlock()
	if warn && lg != nil {
		lg.Printf("%s: %s", model, st.Warning)
	}
}

// get is one model's summary; ok is false when it has no calls yet.
func (l *liveSpeed) get(model string) (SpeedStat, bool) {
	if l == nil {
		return SpeedStat{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cs := l.calls[model]
	if len(cs) == 0 {
		return SpeedStat{}, false
	}
	return summarize(cs), true
}

// snapshot is every model's summary.
func (l *liveSpeed) snapshot() map[string]SpeedStat {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		return nil
	}
	names := make([]string, 0, len(l.calls))
	for m := range l.calls {
		names = append(names, m)
	}
	sort.Strings(names)
	out := map[string]SpeedStat{}
	for _, m := range names {
		out[m] = summarize(l.calls[m])
	}
	return out
}

// summarize adds up calls. Speeds are totals over totals, so one tiny call
// does not swing them.
func summarize(cs []speedCall) SpeedStat {
	st := SpeedStat{Calls: len(cs), CacheHitRate: -1}
	var longCalls, longCached, longTotal int
	var pn, gn, drafted, kept int
	var pms, gms float64
	for _, c := range cs {
		t := c.t
		if total := t.CacheN + t.PromptN; total >= cacheJudgedTokens {
			longCalls++
			longCached += t.CacheN
			longTotal += total
		}
		if t.PromptN > 0 && t.PromptMs > 0 {
			pn += t.PromptN
			pms += t.PromptMs
		}
		if t.PredictedN > 0 && t.PredictedMs > 0 {
			gn += t.PredictedN
			gms += t.PredictedMs
		}
		drafted += t.DraftN
		kept += t.DraftAccepted
	}
	if longTotal > 0 {
		st.CacheHitRate = round2(float64(longCached) / float64(longTotal))
	}
	if pms > 0 {
		st.PrefillTPS = round1(float64(pn) / (pms / 1000))
	}
	if gms > 0 {
		st.DecodeTPS = round1(float64(gn) / (gms / 1000))
	}
	if drafted > 0 {
		st.DraftAcceptRate = round2(float64(kept) / float64(drafted))
	}
	if longCalls >= cacheJudgedCalls && st.CacheHitRate < cacheWarnRate {
		st.Warning = "long prompts are mostly read from their start, not the prompt cache: is the prompt's start changing between calls, " +
			"or are more conversations taking turns than the cache holds (prompt_cache_prefixes)?"
	}
	return st
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
