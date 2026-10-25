package bench

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"brainiac/internal/web"
)

func TestKeyMatchesNamesAcrossSources(t *testing.T) {
	for in, want := range map[string]string{
		"Qwen/Qwen3.5-4B": "qwen3.5-4b", "unsloth/Qwen3.5-4B-GGUF": "qwen3.5-4b", "Qwen3-32B (FC)": "qwen3-32b",
		"qwen3.5-35b-a3b_none": "qwen3.5-35b-a3b", "deepseek-v4.1-flash-1017_max": "deepseek-v4.1-flash-1017",
		"gemma-3-27b-it": "gemma3-27b", "Qwen3-30B-A3B-Instruct-2507": "qwen3-30b-a3b-instruct-2507",
		"Qwen3.5 (9B)": "qwen3.5-9b", "Gemma 4 (E4B)": "gemma4-e4b", "gemma-4-e4b": "gemma4-e4b", "Qwen3.5 4B Q4_K_M": "qwen3.5-4b",
		"Gemma4 E4B-it BF16": "gemma4-e4b", "Nemotron-3.5 Bolt-E4B-4GB Q4_K_M": "nemotron3.5-bolt-e4b-4gb", "Qwen3.6 (35B-A3B)": "qwen3.6-35b-a3b",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

const card = `# Nova
<table><thead><tr><th></th><th>GPT-OSS-20B</th><th>Qwen3.5-4B</th><th>Nova-4B</th></tr></thead>
<tr><td colspan="4">Knowledge</td></tr>
<tr><td>MMLU-Pro</td><td>74.8</td><td>79.1</td><td><b>83.0</b></td></tr>
<tr><td>GPQA-Diamond</td><td>71.5</td><td>76.2</td><td>80.0</td></tr>
<tr><td>Throughput (tok/s)</td><td>40</td><td>60</td><td>55</td></tr>
</table>

| Benchmark | Qwen3.5-4B | Nova-4B |
|---|---|---|
| AIME26 | 89.7 | 92.0* |
| MMMU | 77.6 | 79.0 |
| LibriSpeech WER | 2.0 | 1.6 |
`

func TestCardTablesBecomeComparisons(t *testing.T) {
	s := ParseCard(card, "card newlab/Nova-4B")
	b := &Board{}
	b.Add(s...)
	if v, ok := b.Get("nova-4b", "mmlu-pro", "card newlab/Nova-4B"); !ok || v.Value != 83 {
		t.Fatalf("mmlu-pro %+v %v (scores %v)", v, ok, s)
	}
	if _, ok := b.Get("nova-4b", "throughput (tok/s)", "card newlab/Nova-4B"); ok {
		t.Error("a non-benchmark row was read")
	}
	if v, _ := b.Get("nova-4b", "librispeech wer", "card newlab/Nova-4B"); !v.Lower {
		t.Error("WER not marked lower-is-better")
	}
	brain := b.EstimateRole([]string{"newlab/Nova-4B-GGUF"}, "brain", []Anchor{{Keys: []string{"qwen3.5-4b"}, Quality: 46, Name: "qwen3.5-4b"}})
	if !brain.OK || len(brain.Pairs) != 2 || brain.Quality < 48 || brain.Quality > 49 {
		t.Fatalf("brain %+v", brain)
	}
	math := b.EstimateRole([]string{"Nova-4B"}, "math", []Anchor{{Keys: []string{"qwen3.5-4b"}, Quality: 60, Name: "qwen3.5-4b"}})
	if math.OK {
		t.Errorf("one vendor row made an estimate: %+v", math)
	}
	asr := b.EstimateRole([]string{"Nova-4B"}, "asr", []Anchor{{Keys: []string{"qwen3.5-4b"}, Quality: 50, Name: "x"}})
	if len(asr.Pairs) != 1 || asr.Pairs[0].Estimate != 62.5 {
		t.Errorf("lower-is-better ratio: %+v", asr)
	}
}

func TestOwnColumnAndTurnedTables(t *testing.T) {
	scores := ParseCard("| Benchmark | Gemma 4 (E4B) | Mellum2 Thinking | Mellum2.1 Thinking | Qwen3.5 (9B) |\n"+
		"|---|---|---|---|---|\n| [GPQA Diamond](https://example.org/gpqa) | 58.6 | 61.0 | 66.2 | 81.7 |\n", "card c")
	if own := Own(scores, "JetBrains/Mellum2.1-12B-A2.5B-Thinking-GGUF", "mellum2.1-12b-a2.5b-thinking"); own != "mellum2.1-thinking" {
		t.Errorf("own column %q (models %v)", own, scores)
	}
	if len(scores) != 4 || scores[0].Benchmark != "gpqa diamond" {
		t.Fatalf("link not stripped: %+v", scores)
	}
	if own := Own([]Score{{Model: "nemotron3.5-bolt-e4b-4gb"}, {Model: "nemotron3.5-bolt-e4b"}}, "nvidia/NVIDIA-Nemotron-3.5-Bolt-E4B-4GB"); own != "nemotron3.5-bolt-e4b-4gb" {
		t.Errorf("maker prefix: %q", own)
	}
	if own := Own([]Score{{Model: "nova-2b"}, {Model: "nova-7b"}}, "lab/Nova-Mini"); own != "" {
		t.Errorf("two sizes of one name is not one column: %q", own)
	}
	turned := ParseCard("| Model | GPQA | MMLU-Pro |\n|---|---|---|\n| Nova-4B | 50.0 | 70.0 |\n| Qwen3.5-4B | 45.0 | 66.0 |\n", "card t")
	b := &Board{}
	b.Add(turned...)
	if v, ok := b.Get("nova-4b", "mmlu-pro", "card t"); !ok || v.Value != 70 {
		t.Fatalf("models in rows: %+v", turned)
	}
}

func TestOneIndependentComparisonIsEnough(t *testing.T) {
	b := &Board{}
	b.Add(Score{Model: "nova-4b", Benchmark: "screenspot-pro", Value: 66, Source: "screenspot-pro", Independent: true},
		Score{Model: "qwen3.5-2b", Benchmark: "screenspot-pro", Value: 54.5, Source: "screenspot-pro", Independent: true},
		// a different source is never compared with another
		Score{Model: "qwen3.5-2b", Benchmark: "screenspot-pro", Value: 10, Source: "card x"})
	e := b.EstimateRole([]string{"nova-4b"}, "ground", []Anchor{{Keys: []string{"qwen3.5-2b"}, Quality: 54, Name: "qwen3.5-2b"}})
	if !e.OK || len(e.Pairs) != 1 || e.Quality != 65.4 {
		t.Fatalf("%+v", e)
	}
}

func TestLeaderboardParsers(t *testing.T) {
	lb := parseLiveBench([]byte("model,reasoning,coding\nqwen3.8-27b,80.0,86.8\nbad,x,1\n"))
	if len(lb) != 4 || lb[2].Benchmark != "livebench average" || lb[2].Value != 83.4 { // two columns + average; bad: one column, no average
		t.Fatalf("livebench %+v", lb)
	}
	ss := parseScreenSpot([]byte(`{"Holo2-4B":{"results":{"overall":{"avg":0.572}}},"Holo2-4B (Agentic)":{"results":{"overall":{"avg":0.686}}},"A + B":{"results":{"overall":{"avg":0.9}}}}`))
	if len(ss) != 1 || ss[0].Model != "holo2-4b" || ss[0].Value != 57.2 {
		t.Fatalf("screenspot %+v", ss)
	}
	bf := parseBFCL([]byte("Rank,Overall Acc,Model\n1,52.15%,Qwen3-32B (Prompt)\n2,48.71%,Qwen3-32B (FC)\n"))
	if len(bf) != 1 || bf[0].Value != 52.15 {
		t.Fatalf("bfcl %+v", bf)
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create("gpqa_diamond.csv")
	f.Write([]byte("Model version,mean_score\nqwen3.5-35b-a3b_none,0.80\nqwen3.5-35b-a3b_high,0.84\n"))
	zw.Close()
	ep, err := parseEpoch(zb.Bytes())
	if err != nil || len(ep) != 1 || ep[0].Model != "qwen3.5-35b-a3b" || ep[0].Value != 84 {
		t.Fatalf("epoch %+v %v", ep, err)
	}
	if RoleOf("SWE-bench Verified") != "code" || RoleOf("ScreenSpot-Pro") != "ground" || RoleOf("GPQA-Diamond") != "brain" || RoleOf("Throughput") != "" {
		t.Fatal("RoleOf")
	}
}

func TestLiveCollect(t *testing.T) {
	if os.Getenv("BRAINIAC_LIVE") == "" {
		t.Skip("set BRAINIAC_LIVE=1 to read the real leaderboards")
	}
	w := web.New()
	b := Collect(context.Background(), w, LiveBenchDate)
	bySource := map[string]int{}
	for _, s := range b.Scores {
		bySource[s.Source]++
	}
	t.Logf("scores by source: %v notes %v", bySource, b.Notes)
	cs, err := CardScores(context.Background(), w, "Qwen/Qwen3.5-4B")
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]int{}
	for _, s := range cs {
		models[s.Model]++
	}
	t.Logf("Qwen3.5-4B card: %d scores, models %v", len(cs), models)
}

// TestLiveCards prints what the cards of BRAINIAC_CARDS (comma-separated
// repos) give: the model names their tables use and the benchmarks.
func TestLiveCards(t *testing.T) {
	repos := os.Getenv("BRAINIAC_CARDS")
	if repos == "" {
		t.Skip("set BRAINIAC_CARDS=org/repo,... to read real cards")
	}
	w := web.New()
	for _, repo := range strings.Split(repos, ",") {
		cs, err := CardScores(context.Background(), w, repo)
		if err != nil {
			t.Logf("%s: %v", repo, err)
			continue
		}
		models, benches := map[string]int{}, map[string]string{}
		for _, s := range cs {
			models[s.Model+" ("+s.Raw+")"]++
			benches[s.Benchmark] = RoleOf(s.Benchmark)
		}
		t.Logf("%s: %d scores\n  models %v\n  benchmarks %v", repo, len(cs), models, benches)
	}
}
