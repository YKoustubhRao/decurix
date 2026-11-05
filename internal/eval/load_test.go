package eval

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

// tarFrom writes files into a tar in the given order (sorted when order is nil).
func tarFrom(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	if order == nil {
		for n := range files {
			order = append(order, n)
		}
		sort.Strings(order)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range order {
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sampleSuite is a small valid suite: one web case against a mock, one office case.
func sampleSuite(t *testing.T) map[string][]byte {
	t.Helper()
	suite := map[string]any{
		"schema": SuiteSchema, "name": "sample", "description": "test suite",
		"mocks":  map[string]any{"app": map[string]any{"hosts": []string{"app.example.test"}}},
		"budget": map[string]any{"max_tokens": 1000, "max_minutes": 10},
		"cases":  []string{"enter-bill", "sum-sheet"},
	}
	web := map[string]any{
		"schema": CaseSchema, "id": "enter-bill", "category": "varied", "kind": "doc_to_record",
		"request": "Enter the bill in bill.txt into the app.",
		"start":   map[string]any{"files": []string{"bill.txt"}, "mock": "app", "fixture": "fixture.json"},
		"facts":   []any{map[string]any{"id": "ledger", "about": "which ledger", "refers_to": []string{"ledger"}, "answer": "Use Office.", "required": true}},
		"expected": map[string]any{"outcome": "complete", "graders": []any{
			map[string]any{"kind": "mock_records", "collection": "bills", "exact_count": true,
				"records": []any{map[string]any{"number": map[string]string{"id": "B-1"}, "date": map[string]string{"date": "2026-04-03"}}}},
			map[string]any{"kind": "asked", "facts": []string{"ledger"}},
			map[string]any{"kind": "absent", "collection": "bills", "when": "asked"},
			map[string]any{"kind": "ledger_readback", "records": 1},
		}},
		"oracle": []any{"gui open http://app.example.test/bills/new", map[string]any{"run": "true", "exit": 0}},
		"budget": map[string]any{"max_minutes": 5},
	}
	office := map[string]any{
		"schema": CaseSchema, "id": "sum-sheet", "category": "ordinary", "kind": "office",
		"request": "Add a total to book.xlsx and save it as total.xlsx.",
		"start":   map[string]any{"files": []string{"book.xlsx"}},
		"facts":   []any{},
		"expected": map[string]any{"outcome": "complete", "graders": []any{
			map[string]any{"kind": "file_cells", "file": "total.xlsx", "cells": []any{map[string]any{"sheet": "S", "cell": "B6", "value": "10", "formula_text": "=SUM(B2:B5)"}}},
		}},
		"oracle": []any{"office update book.xlsx '{}'"},
	}
	return map[string][]byte{
		"suite.json":                      mustJSON(t, suite),
		"cases/enter-bill/case.json":      mustJSON(t, web),
		"cases/enter-bill/files/bill.txt": []byte("Bill B-1 dated 03/04/2026\n"),
		"cases/enter-bill/fixture.json":   []byte(`{"collections": {"bills": []}}`),
		"cases/sum-sheet/case.json":       mustJSON(t, office),
		"cases/sum-sheet/files/book.xlsx": []byte("PK\x03\x04 not really a workbook"),
		"mocks/app/mock_app.py":           []byte("print('mock')\n"),
	}
}

// edit changes one JSON document of a suite.
func edit(t *testing.T, files map[string][]byte, name string, fn func(m map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(files[name], &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	files[name] = mustJSON(t, m)
}

func graders(m map[string]any) []any {
	return m["expected"].(map[string]any)["graders"].([]any)
}

func TestLoadSampleSuite(t *testing.T) {
	s, err := Load(tarFrom(t, sampleSuite(t), nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "sample" || len(s.Cases) != 2 || s.SHA == "" {
		t.Fatalf("suite: %+v", s)
	}
	c, ok := s.Case("enter-bill")
	if !ok || string(c.Files["bill.txt"]) == "" || len(c.Fixture) == 0 || !c.HasRequiredFact() {
		t.Fatalf("case: %+v", c)
	}
	if b := s.CaseBudget(c); b.MaxMinutes != 5 || b.MaxTokens != 1000 {
		t.Fatalf("budget: %+v", b)
	}
	if h := s.Hosts(); len(h) != 1 || h[0] != "app.example.test" {
		t.Fatalf("hosts: %v", h)
	}
	recs, err := c.Expected.Graders[0].RecordList()
	if err != nil || recs[0]["number"].Kind != ValueID || recs[0]["date"].S != "2026-04-03" {
		t.Fatalf("records: %v %v", recs, err)
	}
}

func TestLoadReportsEveryMistake(t *testing.T) {
	cases := []struct {
		name string
		mut  func(t *testing.T, f map[string][]byte)
		want []string
	}{
		{"suite schema", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "suite.json", func(m map[string]any) { m["schema"] = "other" })
		}, []string{"must name the schema"}},
		{"case schema and id", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["schema"] = "x"; m["id"] = "other" })
		}, []string{"must name the schema", "but its folder is sum-sheet"}},
		{"bad case id", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "suite.json", func(m map[string]any) { m["cases"] = []string{"Enter_Bill", "sum-sheet"} })
		}, []string{"not a lowercase id", "not in a case the suite lists"}},
		{"category", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["category"] = "odd" })
		}, []string{"the category \"odd\""}},
		{"missing file", func(t *testing.T, f map[string][]byte) {
			delete(f, "cases/enter-bill/files/bill.txt")
		}, []string{"bill.txt is not in"}},
		{"nested file name", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) {
				m["start"] = map[string]any{"files": []string{"sub/book.xlsx"}}
			})
		}, []string{"not a flat file name", "is in files/ but not in start.files"}},
		{"unlisted file", func(t *testing.T, f map[string][]byte) {
			f["cases/sum-sheet/files/extra.txt"] = []byte("x")
		}, []string{"extra.txt is in files/ but not in start.files"}},
		{"oversize file", func(t *testing.T, f map[string][]byte) {
			f["cases/sum-sheet/files/book.xlsx"] = bytes.Repeat([]byte{0xff}, 9<<20)
		}, []string{"larger than 8 MB"}},
		{"credential in request", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["request"] = "Use api_key: abcdef123456 to sign in." })
		}, []string{"request looks like it holds a credential"}},
		{"credential in oracle", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["oracle"] = []any{"gui type x password=hunter2hunter2"} })
		}, []string{"oracle step 1 looks like it holds a credential"}},
		{"unknown grader kind", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) {
				m["expected"].(map[string]any)["graders"] = append(graders(m), map[string]any{"kind": "vibes"})
			})
		}, []string{"unknown kind \"vibes\""}},
		{"bad grader fields", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/enter-bill/case.json", func(m map[string]any) {
				g := graders(m)
				g[0].(map[string]any)["records"] = []any{map[string]any{"date": map[string]string{"date": "03/04/2026"}}}
				m["expected"].(map[string]any)["graders"] = append(g, map[string]any{"kind": "stopped_at_approval"},
					map[string]any{"kind": "asked", "facts": []string{"nope"}}, map[string]any{"kind": "file_cells", "file": "bill.txt", "cells": []any{}})
			})
		}, []string{"is not a YYYY-MM-DD date", "needs action_contains", "does not have", "is an input"}},
		{"complete without a positive grader", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) {
				m["expected"] = map[string]any{"outcome": "complete", "graders": []any{map[string]any{"kind": "absent", "file": "x.txt"}, map[string]any{"kind": "ledger_readback", "records": 1}}}
			})
		}, []string{"so a claim alone cannot pass"}},
		{"asked without asked", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["expected"].(map[string]any)["outcome"] = "asked" })
		}, []string{"needs an asked grader naming the facts"}},
		{"approval without stop", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["expected"].(map[string]any)["outcome"] = "approval" })
		}, []string{"needs a stopped_at_approval grader"}},
		{"required fact without asked", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/enter-bill/case.json", func(m map[string]any) {
				g := graders(m)
				m["expected"].(map[string]any)["graders"] = []any{g[0], g[2]}
			})
		}, []string{"a required fact needs an asked grader"}},
		{"facts", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/enter-bill/case.json", func(m map[string]any) {
				m["facts"] = []any{map[string]any{"id": "ledger", "about": "a", "refers_to": []string{"a"}, "answer": "b", "required": true},
					map[string]any{"id": "ledger", "about": "a", "answer": "b"}}
			})
		}, []string{"invalid or repeated", "must say \"required\""}},
		{"refers_to", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/enter-bill/case.json", func(m map[string]any) {
				m["facts"] = []any{map[string]any{"id": "ledger", "about": "a", "answer": "b", "required": true},
					map[string]any{"id": "other", "about": "a", "refers_to": []string{"anything else", "seven words is one word too many"}, "answer": "b", "required": false}}
			})
		}, []string{"the fact ledger needs refers_to", "must be 1 to 6 words", "the fact other's refers_to matches the ask arm's question"}},
		{"budget raised", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["budget"] = map[string]any{"max_minutes": 20} })
		}, []string{"may only lower the suite's"}},
		{"no oracle", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["oracle"] = []any{} })
		}, []string{"needs an oracle"}},
		{"unknown field", func(t *testing.T, f map[string][]byte) {
			edit(t, f, "cases/sum-sheet/case.json", func(m map[string]any) { m["expect"] = "done" })
		}, []string{"not a valid case document"}},
		{"missing mock", func(t *testing.T, f map[string][]byte) {
			delete(f, "mocks/app/mock_app.py")
		}, []string{"has no mocks/app/mock_app.py"}},
		{"stray entry", func(t *testing.T, f map[string][]byte) {
			f["notes.txt"] = []byte("x")
			f["mocks/other/mock_app.py"] = []byte("x")
		}, []string{"notes.txt is not part of the suite layout", "is not a mock the suite names"}},
		{"bad fixture", func(t *testing.T, f map[string][]byte) {
			f["cases/enter-bill/fixture.json"] = []byte("{not json")
		}, []string{"the fixture is not JSON"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := sampleSuite(t)
			tc.mut(t, files)
			_, err := Load(tarFrom(t, files, nil))
			var p Problems
			if !errors.As(err, &p) {
				t.Fatalf("want problems, got %v", err)
			}
			all := strings.Join(p, "\n")
			for _, w := range tc.want {
				if !strings.Contains(all, w) {
					t.Errorf("missing %q in:\n%s", w, all)
				}
			}
		})
	}
}

func TestLoadRefusesUnsafeTarEntries(t *testing.T) {
	files := sampleSuite(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range []string{"suite.json", "cases/enter-bill/case.json"} {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))})
		_, _ = tw.Write(files[n])
	}
	_ = tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.WriteHeader(&tar.Header{Name: "cases/enter-bill/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	_ = tw.WriteHeader(&tar.Header{Name: "suite.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("{}"))
	_ = tw.Close()
	_, err := Load(buf.Bytes())
	var p Problems
	if !errors.As(err, &p) {
		t.Fatalf("want problems, got %v", err)
	}
	all := strings.Join(p, "\n")
	for _, w := range []string{"not a plain relative path", "not a regular file", "holds suite.json twice"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
}

func TestSuiteSHAStable(t *testing.T) {
	files := sampleSuite(t)
	a, err := Load(tarFrom(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	var reversed []string
	for n := range files {
		reversed = append(reversed, n)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(reversed)))
	// The same documents re-indented and in another tar order hash the same.
	again := map[string][]byte{}
	for n, b := range files {
		again[n] = b
		if isSuiteDocument(n) {
			var v any
			_ = json.Unmarshal(b, &v)
			again[n], _ = json.Marshal(v)
		}
	}
	b, err := Load(tarFrom(t, again, reversed))
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA != b.SHA {
		t.Fatalf("SHA changed with tar order or spacing: %s vs %s", a.SHA, b.SHA)
	}
	// A fixture or an input is read byte for byte by the mock and the worker: its spacing is part of the case.
	respaced := map[string][]byte{}
	for n, b := range files {
		respaced[n] = b
	}
	respaced["cases/enter-bill/fixture.json"] = []byte(`{ "collections": { "bills": [] } }`)
	if d, err := Load(tarFrom(t, respaced, nil)); err != nil || d.SHA == a.SHA {
		t.Fatalf("a fixture's bytes do not move the SHA: %v", err)
	}
	files["cases/enter-bill/files/bill.txt"] = []byte("Bill B-1 dated 03/04/2027\n")
	c, err := Load(tarFrom(t, files, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SHA == a.SHA {
		t.Fatal("SHA did not change with an input file")
	}
}

func TestOracleStepsAndValues(t *testing.T) {
	var steps []OracleStep
	if err := json.Unmarshal([]byte(`["gui click x", {"run": "false", "exit": 1}, {"answer": "7"}, {"approval": "gui click 'button \"Pay\"'"}, {"sleep": 2}]`), &steps); err != nil {
		t.Fatal(err)
	}
	if steps[0].Kind != StepRun || steps[1].Exit != 1 || steps[2].Kind != StepAnswer || steps[3].Exit != 3 || steps[4].Seconds != 2 {
		t.Fatalf("steps: %+v", steps)
	}
	for _, bad := range []string{`{"run": "x", "answer": "y"}`, `{"exit": 1}`, `{"other": 1}`} {
		var s OracleStep
		if json.Unmarshal([]byte(bad), &s) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var vals map[string]Value
	if err := json.Unmarshal([]byte(`{"a": "x", "b": {"number": "5,900.00"}, "c": 12, "d": true}`), &vals); err != nil {
		t.Fatal(err)
	}
	if vals["b"].Kind != ValueNumber || vals["c"].Kind != ValueNumber || vals["c"].S != "12" || vals["d"].S != "true" {
		t.Fatalf("values: %+v", vals)
	}
	var v Value
	if json.Unmarshal([]byte(`{"colour": "x"}`), &v) == nil || json.Unmarshal([]byte(`{"id": "a", "text": "b"}`), &v) == nil {
		t.Fatal("accepted a bad typed value")
	}
}

func TestWithholdOracleSecrets(t *testing.T) {
	s := Suite{Cases: []Case{{OracleSecrets: []string{"demo-pass-1"}}}}
	if got := s.Withhold(`typed demo-pass-1 into "Password"`); strings.Contains(got, "demo-pass-1") {
		t.Fatalf("not withheld: %s", got)
	}
}
