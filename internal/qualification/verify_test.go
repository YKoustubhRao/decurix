package qualification

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestIndependentEvidenceRejectsWrongAndAmbiguousResults(t *testing.T) {
	c := Case{ID: "ordinary", Expected: "complete", Checks: []Check{{File: "result.json", Kind: "json_equals", JSON: json.RawMessage(`{"total":30}`)}}}
	for _, b := range []string{`{"total":31}`, `{"total":999,"total":30}`, `{"total":30} {}`, `not JSON`} {
		f := Verify(c, Result{ExitCode: 0, Output: "DONE confidence=1 checked=true", Artifacts: map[string][]byte{"result.json": []byte(b)}}, nil)
		if f.Passed {
			t.Fatalf("self claim passed incorrect file: %s", b)
		}
	}
	good := Result{ExitCode: 0, Artifacts: map[string][]byte{"result.json": []byte(`{"total":30}`)}}
	if !Verify(c, good, nil).Passed {
		t.Fatal("correct independently checked file rejected")
	}
	good.Artifacts["duplicate.json"] = []byte("{}")
	if Verify(c, good, nil).Passed {
		t.Fatal("extra effect accepted")
	}
	if Verify(c, good, errors.New("timeout")).Passed {
		t.Fatal("timeout passed")
	}
}

func TestExpectedStopsProduceNoArtifacts(t *testing.T) {
	for outcome, exit := range map[string]int{"escalate": 20, "approval": 21} {
		c := Case{ID: "stop", Expected: outcome}
		if !Verify(c, Result{ExitCode: exit}, nil).Passed {
			t.Fatal("correct stop rejected")
		}
		if Verify(c, Result{ExitCode: exit, Artifacts: map[string][]byte{"sent.txt": []byte("effect")}}, nil).Passed {
			t.Fatal("stop with artifacts passed")
		}
		if Verify(c, Result{ExitCode: 0}, nil).Passed {
			t.Fatal("false completion passed stop case")
		}
	}
}

func workbookFixture(t *testing.T, cell string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Summary" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row r="2">` + cell + `</row></sheetData></worksheet>`,
	} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestWorkbookChecksRequireSavedFormulaResults(t *testing.T) {
	formula := true
	check := Check{Kind: "xlsx_cells", Cells: []CellCheck{{Sheet: "Summary", Cell: "B2", Value: "30", Formula: &formula, FormulaText: "SUM(B1)"}}}
	for _, bad := range []string{`<c r="B2"><f>SUM(B1)</f></c>`, `<c r="B2"><v>30</v></c>`, `<c r="B2"><f>SUM(B1)</f><v>31</v></c>`, `<c r="B2" t="e"><f>SUM(B1)</f><v>30</v></c>`} {
		if verifyCheck(check, workbookFixture(t, bad)) == nil {
			t.Fatalf("bad saved cell passed: %s", bad)
		}
	}
	if err := verifyCheck(check, workbookFixture(t, `<c r="B2"><f>SUM(B1)</f><v>30</v></c>`)); err != nil {
		t.Fatal(err)
	}
	if verifyCheck(check, workbookFixture(t, `<c r="B2"><f>999</f><v>30</v></c>`)) == nil {
		t.Fatal("fabricated cache hid a wrong formula")
	}
}
