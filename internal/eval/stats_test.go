package eval

import (
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func TestWilsonKnownValues(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.0015 }
	if lo, hi := Wilson(0, 5, Z95); lo != 0 || !near(hi, 0.434) {
		t.Errorf("0/5: %.4f %.4f", lo, hi)
	}
	if lo, hi := Wilson(5, 5, Z95); !near(lo, 0.566) || hi != 1 {
		t.Errorf("5/5: %.4f %.4f", lo, hi)
	}
	if lo, hi := Wilson(3, 10, Z95); !near(lo, 0.108) || !near(hi, 0.603) {
		t.Errorf("3/10: %.4f %.4f", lo, hi)
	}
	if lo, hi := Wilson(0, 0, Z95); lo != 0 || hi != 1 {
		t.Errorf("0/0 knows nothing: %.4f %.4f", lo, hi)
	}
}

func fixtureRun(repeats int) (Run, []Row) {
	at := time.Date(2026, 11, 4, 9, 30, 0, 0, time.UTC)
	run := Run{ID: "e1", SuiteName: "sample", SuiteSHA: "abc123", Repeats: repeats, Status: RunDone, CreatedAt: at,
		Cases: []string{"one", "two"},
		Arms: []Arm{{Name: "default", Kind: ArmModel, Provider: "azure", Model: "default", Deployment: "big-1", Image: "sha256:0123456789abcdef0123", ExpertDeployment: "big-1"},
			{Name: "fast+expert", Kind: ArmModel, Provider: "azure", Model: "fast", Expert: true, Deployment: "small-1", AgentTreeRev: "eb15981", AgentTreeSHA: "feedfacecafe1234", Image: "sha256:0123456789abcdef0123", ExpertDeployment: "big-1"}}}
	var rows []Row
	add := func(arm, c string, rep int, passed, fd, duw bool, qr, qo int, in, cached, out int64, min float64, deps []string, problems ...string) {
		rows = append(rows, Row{RunID: "e1", Arm: arm, Repeat: rep, CaseID: c, Observed: OutcomeComplete, Passed: passed, FalseDone: fd, DoneUncheckedWrong: duw,
			QRequired: qr, QOther: qo, TokensIn: in, TokensCached: cached, TokensOut: out, Minutes: min, Deployments: deps, Problems: problems})
	}
	for rep := 1; rep <= repeats; rep++ {
		add("default", "one", rep, true, false, false, 0, 0, 200000, 150000, 4000, 3.0+float64(rep), []string{"big-1"})
		add("default", "two", rep, rep%2 == 1, rep%2 == 0, false, 1, 0, 300000, 100000, 6000, 5, []string{"big-1"})
		add("fast+expert", "one", rep, true, false, false, 0, 1, 90000, 60000, 2000, 2, []string{"small-1"})
		if rep == 2 {
			add("fast+expert", "two", rep, false, false, true, 1, 1, 120000, 40000, 3000, 4, []string{"small-1", "small-2"}, modelChanged)
		} else {
			add("fast+expert", "two", rep, true, false, false, 1, 0, 100000, 50000, 2500, 4, []string{"small-1"})
		}
	}
	return run, rows
}

func TestSummariseOverFixtureRows(t *testing.T) {
	run, rows := fixtureRun(5)
	tab := Summarise(run, rows)
	if len(tab.Rows) != 2 || tab.NotResult {
		t.Fatalf("table: %+v", tab)
	}
	two := tab.Rows[1].Arms["default"]
	if two.N != 5 || two.Pass != 3 || two.FalseDone != 2 || two.QRequired != 1 || two.TokensMedian != 306000 {
		t.Fatalf("default/two: %+v", two)
	}
	if lo, _ := Wilson(3, 5, Z95); two.PassLo != lo {
		t.Fatalf("pass lower bound: %v", two.PassLo)
	}
	one := tab.Rows[0].Arms["default"]
	if one.MinutesMedian != 6 || one.TokensP90 != 204000 || math.Abs(one.CachedShare-0.75) > 1e-9 {
		t.Fatalf("default/one: %+v", one)
	}
	fast := tab.Totals["fast+expert"]
	if fast.N != 10 || fast.Pass != 9 || fast.Invalid != 1 || fast.DoneUncheckedWrong != 1 || len(fast.Deployments) != 2 {
		t.Fatalf("fast total: %+v", fast)
	}
	if math.Abs(fast.QOther-0.6) > 1e-9 {
		t.Fatalf("fast other questions per run: %v", fast.QOther)
	}
	// A case nobody ran shows as empty, not as a zero pass rate.
	run.Cases = append(run.Cases, "three")
	if c := Summarise(run, rows).Rows[2].Arms["default"]; c.N != 0 || c.PassLo != 0 || c.FalseDoneHi != 1 {
		t.Fatalf("an empty cell: %+v", c)
	}
}

func TestTableMarkdownGolden(t *testing.T) {
	for _, tc := range []struct {
		repeats int
		file    string
	}{{5, "table-5.golden.md"}, {1, "table-1.golden.md"}} {
		run, rows := fixtureRun(tc.repeats)
		got := Summarise(run, rows).Markdown()
		path := filepath.Join("testdata", tc.file)
		if *updateGolden {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to write it)", err)
		}
		if got != strings.ReplaceAll(string(want), "\r\n", "\n") { // a Windows checkout may have rewritten its line ends
			t.Errorf("%s differs:\n%s", tc.file, got)
		}
	}
}
