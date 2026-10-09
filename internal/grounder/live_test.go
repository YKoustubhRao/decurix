package grounder

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// TestLiveGrounding scores a real grounding model on real worker screens:
// how many targets it puts inside their box, and how long each takes.
// Opt-in, like TheBot's real-Ollama test: it loads a model.
//
//	DECURIX_GROUNDER_MODEL=qwen3-vl:2b DECURIX_GROUNDER_SHOTS=<dir with erp.png, wiki.png> \
//	  OLLAMA_URL=http://localhost:11434 go test -run TestLiveGrounding -v ./internal/grounder/
func TestLiveGrounding(t *testing.T) {
	shots := os.Getenv("DECURIX_GROUNDER_SHOTS")
	c := FromEnv(os.Getenv("OLLAMA_URL"))
	if shots == "" || c == nil {
		t.Skip("set DECURIX_GROUNDER_MODEL and DECURIX_GROUNDER_SHOTS to score a real model")
	}

	var cases struct {
		Screens map[string][]struct {
			Say string `json:"say"`
			Box [4]int `json:"box"`
		} `json:"screens"`
	}
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(cases.Screens))
	for name := range cases.Screens {
		names = append(names, name)
	}
	sort.Strings(names)

	limit, _ := strconv.Atoi(os.Getenv("DECURIX_GROUNDER_CASES")) // 0: every case
	hits, total := 0, 0
	var took []time.Duration
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(shots, name))
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases.Screens[name] {
			if limit > 0 && total >= limit {
				break
			}
			start := time.Now()
			p, err := c.Locate(context.Background(), data, img.Width, img.Height, tc.Say)
			d := time.Since(start)
			took = append(took, d)
			total++
			b := tc.Box
			hit := err == nil && p.X >= b[0] && p.X < b[0]+b[2] && p.Y >= b[1] && p.Y < b[1]+b[3]
			if hit {
				hits++
			}
			t.Logf("%-5v %5.1fs %s %q -> %d,%d box %v %v answer %q", hit, d.Seconds(), name, tc.Say, p.X, p.Y, b, err, p.Answer)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	t.Logf("%s (%s, %s): %d of %d inside the box; median %.1fs, slowest %.1fs",
		c.Model, c.API, c.Coords, hits, total, took[len(took)/2].Seconds(), took[len(took)-1].Seconds())
}
