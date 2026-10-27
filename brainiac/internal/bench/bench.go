// Package bench reads public evaluations from the web - independent
// leaderboards and the comparison tables in model cards - and turns them into
// quality estimates Brainiac can compare with what it already runs.
//
// The rule is comparison inside one source: a new model's score is set
// against a model the catalog already rates, on the same benchmark in the
// same table or leaderboard (the same harness), and the catalog model's rating
// is scaled by the ratio. Scores from different sources are never compared
// with each other, and one vendor table alone is not enough: an estimate
// needs two such comparisons, or one from an independent leaderboard.
//
// Depends on web only.
package bench

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"brainiac/internal/web"
)

// Score is one published result.
type Score struct {
	Model       string  `json:"model"` // normalised name (Key)
	Raw         string  `json:"raw"`   // as the source writes it
	Benchmark   string  `json:"benchmark"`
	Value       float64 `json:"value"`
	Lower       bool    `json:"lower,omitempty"` // lower is better (WER, error rates)
	Source      string  `json:"source"`
	Independent bool    `json:"independent"`
}

// Board is every score collected in one run.
type Board struct {
	Scores []Score
	Notes  []string
}

// Add appends scores.
func (b *Board) Add(s ...Score) { b.Scores = append(b.Scores, s...) }

var (
	effortRe = regexp.MustCompile(`[_ -](none|minimal|low|medium|high|xhigh|max|thinking-on|thinking-off)$`)
	// sizeParenRe is a size written in brackets, "Qwen3.5 (9B)": part of the name.
	sizeParenRe = regexp.MustCompile(`(?i)\s*\(\s*((?:\d+(?:\.\d+)?|[ae]\d+(?:\.\d+)?)[bm](?:[-\s]?a\d+(?:\.\d+)?b)?)\s*\)`)
	parenRe     = regexp.MustCompile(`\s*\([^)]*\)`)
	sepRe       = regexp.MustCompile(`[\s_]+`)
	// quantTailRe is a precision or quantisation at the end of a name: the
	// same model.
	quantTailRe = regexp.MustCompile(`-(i?q\d(-k)?(-[a-z]{1,2})?|q\d-\d|bf16|fp16|f16|fp8|fp4|int4|int8|w4a16|awq|gptq|mxfp4|nvfp4)$`)
	// versionRe joins a short version number to the name, "gemma-4-e4b" to
	// "gemma4-e4b": sources write both. Sizes (4b) and dates (2507) stay apart.
	versionRe = regexp.MustCompile(`([a-z])-(\d{1,2}(?:\.\d+)?)(-|$)`)
)

// Key normalises a model name so the same model matches across sources:
// lower case, no publisher, no "(FC)" or "_xhigh", no -GGUF/-Instruct/-it.
func Key(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	n = sizeParenRe.ReplaceAllString(n, " $1")
	n = parenRe.ReplaceAllString(n, "")
	n = strings.ToLower(sepRe.ReplaceAllString(strings.TrimSpace(n), "-"))
	for {
		before := n
		n = effortRe.ReplaceAllString(n, "")
		n = quantTailRe.ReplaceAllString(n, "")
		for _, suf := range []string{"-gguf", "-instruct", "-it", "-chat", "-hf", "-preview"} {
			n = strings.TrimSuffix(n, suf)
		}
		if n == before {
			break
		}
	}
	n = versionRe.ReplaceAllString(n, "$1$2$3")
	return strings.Trim(n, "-")
}

var sizeWordRe = regexp.MustCompile(`^(\d+(\.\d+)?[bm]|[ae]\d+(\.\d+)?b|\d+gb)$`)

// Loose is a name's Key without its size words (9b, a3b, e4b, 4gb).
func Loose(name string) string {
	var keep []string
	for _, t := range strings.Split(Key(name), "-") {
		if !sizeWordRe.MatchString(t) {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "-")
}

// Own picks, among the models a card scores, the card's own model, given
// the names it goes by (repo, base): a column with one of those names; else
// the one column the name ends with (the repo adds the maker,
// nvidia-nemotron-... for nemotron-...); else the one column that matches
// with sizes left out (makers often drop their own size). "" when there is
// none, or more than one.
func Own(scores []Score, names ...string) string {
	models := map[string]bool{}
	for _, s := range scores {
		models[s.Model] = true
	}
	var keys, loose []string
	for _, n := range names {
		if k := Key(n); k != "" {
			keys = append(keys, k)
			loose = append(loose, Loose(n))
		}
	}
	for _, k := range keys {
		if models[k] {
			return k
		}
	}
	// unique is the one model match accepts; "" for none, ambiguous for more.
	const ambiguous = "\x00"
	unique := func(match func(m string) bool) string {
		found := ""
		for m := range models {
			if match(m) {
				if found != "" {
					return ambiguous
				}
				found = m
			}
		}
		return found
	}
	suffix := unique(func(m string) bool {
		for _, k := range keys {
			if len(m) >= 5 && strings.HasSuffix(k, "-"+m) {
				return true
			}
		}
		return false
	})
	if suffix == ambiguous {
		return ""
	}
	if suffix != "" {
		return suffix
	}
	m := unique(func(m string) bool {
		lm := Loose(m)
		for _, l := range loose {
			if len(lm) >= 4 && (l == lm || strings.HasSuffix(l, "-"+lm)) {
				return true
			}
		}
		return false
	})
	if m == ambiguous {
		return ""
	}
	return m
}

// Get is the score of model on benchmark in source, if the board has one.
func (b *Board) Get(model, benchmark, source string) (Score, bool) {
	for _, s := range b.Scores {
		if s.Model == model && s.Benchmark == benchmark && s.Source == source {
			return s, true
		}
	}
	return Score{}, false
}

// Anchor is a model the catalog rates for a role, under the names sources may use.
type Anchor struct {
	Keys    []string
	Quality float64
	Name    string
}

// Pair is one comparison behind an estimate.
type Pair struct {
	Benchmark string  `json:"benchmark"`
	Source    string  `json:"source"`
	Anchor    string  `json:"anchor"`
	Candidate float64 `json:"candidate"`
	Versus    float64 `json:"versus"`
	Estimate  float64 `json:"estimate"`
	// Independent is set when the source is a leaderboard, not a maker's card.
	Independent bool `json:"independent,omitempty"`
}

// Estimate is a candidate's estimated quality for a role, with its evidence.
type Estimate struct {
	Role    string  `json:"role"`
	Quality float64 `json:"quality"`
	Pairs   []Pair  `json:"pairs"`
	OK      bool    `json:"ok"`
	Why     string  `json:"why,omitempty"`
}

// EstimateRole scales anchor ratings by the candidate's scores against them,
// on the role's benchmarks, inside single sources.
func (b *Board) EstimateRole(candKeys []string, role string, anchors []Anchor) Estimate {
	est := Estimate{Role: role}
	cand := map[string]bool{}
	for _, k := range candKeys {
		cand[Key(k)] = true
	}
	independent := 0
	// A quantiser's repo often copies the maker's card: the same comparison
	// read twice is one comparison.
	seen := map[string]bool{}
	for _, s := range b.Scores {
		if !cand[s.Model] || RoleOf(s.Benchmark) != role || s.Value <= 0 {
			continue
		}
		for _, a := range anchors {
			if a.Quality <= 0 {
				continue
			}
			for _, k := range a.Keys {
				as, ok := b.Get(Key(k), s.Benchmark, s.Source)
				if !ok || as.Value <= 0 || cand[as.Model] {
					continue
				}
				same := fmt.Sprintf("%s|%s|%g|%g", s.Benchmark, a.Name, s.Value, as.Value)
				if seen[same] {
					break
				}
				seen[same] = true
				ratio := s.Value / as.Value
				if s.Lower {
					ratio = as.Value / s.Value
				}
				q := math.Min(100, a.Quality*ratio)
				est.Pairs = append(est.Pairs, Pair{Benchmark: s.Benchmark, Source: s.Source, Anchor: a.Name, Candidate: s.Value, Versus: as.Value,
					Estimate: round1(q), Independent: s.Independent})
				if s.Independent {
					independent++
				}
				break
			}
		}
	}
	if len(est.Pairs) == 0 {
		est.Why = "no public result sets it beside a model the catalog rates"
		return est
	}
	est.Quality = median(est.Pairs)
	est.OK = len(est.Pairs) >= 2 || independent >= 1
	if !est.OK {
		est.Why = "only one vendor comparison"
	}
	return est
}

// benchmark keywords by role; the first role whose word a benchmark name
// holds is the role it informs.
var roleWords = []struct {
	role  string
	words []string
}{
	{"ground", []string{"screenspot", "osworld", "androidworld", "webvoyager", "mind2web"}},
	{"ocr", []string{"omnidocbench", "olmocr", "ocrbench", "docvqa", "chartqa", "textvqa", "infovqa"}},
	{"vision", []string{"mmmu", "mathvista", "mmbench", "realworldqa", "mmstar", "video-mme", "videomme", "mvbench", "charxiv", "ai2d", "hallusion", "blink"}},
	{"sql", []string{"bird", "spider"}},
	{"code", []string{"swe-bench", "swebench", "livecodebench", "humaneval", "mbpp", "aider", "terminal", "scicode", "codeforces", "cursorbench", "coding"}},
	{"math", []string{"aime", "hmmt", "math", "gsm8k", "usamo", "imo", "olympiad", "otis", "frontiermath", "minif2f", "putnam"}},
	{"medicine", []string{"medqa", "medxpert", "healthbench", "pubmedqa", "medmcqa", "mmlu-medical"}},
	{"translate", []string{"flores", "wmt", "xcomet", "chrf", "comet", "translation"}},
	{"embed", []string{"mteb", "mmteb", "beir", "retrieval"}},
	{"asr", []string{"wer", "librispeech", "common voice", "open asr", "earnings22", "tedlium"}},
	{"brain", []string{"livebench", "gpqa", "mmlu", "ifeval", "bbh", "arc", "simpleqa", "bfcl", "tau", "τ", "reasoning", "data_analysis",
		"long_agentic", "research", "intelligence", "arena", "supergpqa", "hle", "humanity", "multi-if", "longbench", "mrcr", "agentic"}},
}

// Independent is the estimate from leaderboard comparisons alone, leaving
// out the makers' cards; OK when there is at least one.
func (e Estimate) Independent() Estimate {
	out := Estimate{Role: e.Role}
	for _, p := range e.Pairs {
		if p.Independent {
			out.Pairs = append(out.Pairs, p)
		}
	}
	if len(out.Pairs) == 0 {
		out.Why = "no independent leaderboard compares it"
		return out
	}
	out.Quality, out.OK = median(out.Pairs), true
	return out
}

// median is the middle of the pairs' estimates.
func median(ps []Pair) float64 {
	qs := make([]float64, len(ps))
	for i, p := range ps {
		qs[i] = p.Estimate
	}
	sort.Float64s(qs)
	if len(qs)%2 == 0 {
		return round1((qs[len(qs)/2-1] + qs[len(qs)/2]) / 2)
	}
	return round1(qs[len(qs)/2])
}

// RoleOf is the role a benchmark informs, or "".
func RoleOf(benchmark string) string {
	b := strings.ToLower(benchmark)
	for _, rw := range roleWords {
		for _, w := range rw.words {
			if strings.Contains(b, w) {
				return rw.role
			}
		}
	}
	return ""
}

// lowerBetter reports a metric where less is better.
func lowerBetter(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "wer") || strings.Contains(n, "cer") || strings.Contains(n, "error") || strings.Contains(n, "edit dist")
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// num reads "86.8", "52.15%", "0.932" (a fraction becomes a percentage when
// asked), "—" as nothing.
func num(s string, fraction bool) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	s = strings.Trim(s, "*_ ")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0, false
	}
	if fraction && v > 0 && v <= 1 {
		v *= 100
	}
	return round2(v), true
}

// --- independent leaderboards -------------------------------------------

// Sources are the independent leaderboards read each run, by name.
var Sources = map[string]string{
	"livebench":  "https://livebench.ai/data/%s/table.csv",
	"screenspot": "https://gui-agent.github.io/grounding-leaderboard/results/screenspot_pro.json",
	"bfcl":       "https://gorilla.cs.berkeley.edu/data_overall.csv",
	"epoch":      "https://epoch.ai/data/benchmark_data.zip",
}

// LiveBenchDate is the LiveBench release read (it publishes one table per
// release date); a newer one is picked up by changing this or the overlay.
const LiveBenchDate = "20261014"

// Collect reads the independent leaderboards. A source that fails is noted
// and skipped; the run goes on with the others.
func Collect(ctx context.Context, w *web.Client, livebenchDate string) *Board {
	b := &Board{}
	if livebenchDate != "" {
		if body, err := fetchRaw(ctx, w, fmt.Sprintf(Sources["livebench"], livebenchDate)); err == nil {
			b.Add(parseLiveBench(body)...)
		} else {
			b.Notes = append(b.Notes, "LiveBench: "+err.Error())
		}
	}
	if body, err := fetchRaw(ctx, w, Sources["screenspot"]); err == nil {
		b.Add(parseScreenSpot(body)...)
	} else {
		b.Notes = append(b.Notes, "ScreenSpot-Pro: "+err.Error())
	}
	if body, err := fetchRaw(ctx, w, Sources["bfcl"]); err == nil {
		b.Add(parseBFCL(body)...)
	} else {
		b.Notes = append(b.Notes, "BFCL: "+err.Error())
	}
	if body, err := fetchRaw(ctx, w, Sources["epoch"]); err == nil {
		s, err := parseEpoch(body)
		if err != nil {
			b.Notes = append(b.Notes, "Epoch: "+err.Error())
		}
		b.Add(s...)
	} else {
		b.Notes = append(b.Notes, "Epoch: "+err.Error())
	}
	return b
}

// fetchRaw reads a page's text through the safe browser.
func fetchRaw(ctx context.Context, w *web.Client, url string) ([]byte, error) {
	p, err := w.FetchRaw(ctx, url)
	return p, err
}

func parseLiveBench(body []byte) []Score {
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil || len(rows) < 2 {
		return nil
	}
	head := rows[0]
	var out []Score
	for _, r := range rows[1:] {
		if len(r) != len(head) {
			continue
		}
		var sum float64
		n := 0
		for i := 1; i < len(r); i++ {
			v, ok := num(r[i], false)
			if !ok {
				continue
			}
			out = append(out, Score{Model: Key(r[0]), Raw: r[0], Benchmark: "livebench " + head[i], Value: v, Source: "livebench", Independent: true})
			sum += v
			n++
		}
		if n == len(r)-1 && n > 0 { // an average of some columns would mislead
			out = append(out, Score{Model: Key(r[0]), Raw: r[0], Benchmark: "livebench average", Value: round2(sum / float64(n)), Source: "livebench", Independent: true})
		}
	}
	return out
}

func parseScreenSpot(body []byte) []Score {
	var d map[string]struct {
		Results struct {
			Overall struct {
				Avg float64 `json:"avg"`
			} `json:"overall"`
		} `json:"results"`
	}
	if json.Unmarshal(body, &d) != nil {
		return nil
	}
	var out []Score
	for name, v := range d {
		l := strings.ToLower(name)
		// Ensembles, zoom-in pipelines and agentic set-ups are systems, not one
		// model in one pass: comparing them with a model would be unfair.
		if strings.ContainsAny(name, "(+") || strings.Contains(l, "router") || strings.Contains(l, "cascade") || strings.Contains(l, "zoom") {
			continue
		}
		if v.Results.Overall.Avg > 0 {
			out = append(out, Score{Model: Key(name), Raw: name, Benchmark: "screenspot-pro", Value: round2(v.Results.Overall.Avg * 100), Source: "screenspot-pro", Independent: true})
		}
	}
	return out
}

func parseBFCL(body []byte) []Score {
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil || len(rows) < 2 {
		return nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	mi, ok1 := col["Model"]
	oi, ok2 := col["Overall Acc"]
	if !ok1 || !ok2 {
		return nil
	}
	best := map[string]Score{}
	for _, r := range rows[1:] {
		if len(r) <= mi || len(r) <= oi {
			continue
		}
		v, ok := num(r[oi], false)
		if !ok {
			continue
		}
		k := Key(r[mi])
		if s, seen := best[k]; !seen || v > s.Value { // a model's FC and prompt runs: keep its best
			best[k] = Score{Model: k, Raw: r[mi], Benchmark: "bfcl overall", Value: v, Source: "bfcl", Independent: true}
		}
	}
	var out []Score
	for _, s := range best {
		out = append(out, s)
	}
	return out
}

// epochFiles are the Epoch AI benchmark files read, by benchmark name.
var epochFiles = map[string]string{
	"gpqa_diamond.csv": "gpqa diamond", "swe_bench_verified.csv": "swe-bench verified", "otis_mock_aime_2024_2025.csv": "otis aime",
	"math_level_5.csv": "math level 5", "simpleqa_verified.csv": "simpleqa verified", "terminalbench_external.csv": "terminal-bench",
	"aider_polyglot_external.csv": "aider polyglot", "video_mme_external.csv": "video-mme", "os_world_external.csv": "osworld",
	"live_bench_external.csv": "livebench global", "mmlu_external.csv": "mmlu",
}

func parseEpoch(body []byte) ([]Score, error) {
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	var out []Score
	for _, f := range z.File {
		bench, ok := epochFiles[f.Name]
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
		if err != nil || len(rows) < 2 {
			continue
		}
		vcol := -1
		for i, h := range rows[0] {
			if h == "mean_score" || h == "Global average" || strings.EqualFold(h, "score") {
				vcol = i
				break
			}
		}
		if vcol < 0 {
			continue
		}
		best := map[string]Score{}
		for _, r := range rows[1:] {
			if len(r) <= vcol {
				continue
			}
			v, ok := num(r[vcol], true)
			if !ok {
				continue
			}
			k := Key(r[0])
			if s, seen := best[k]; !seen || v > s.Value {
				best[k] = Score{Model: k, Raw: r[0], Benchmark: bench, Value: v, Source: "epoch " + bench, Independent: true}
			}
		}
		for _, s := range best {
			out = append(out, s)
		}
	}
	return out, nil
}
