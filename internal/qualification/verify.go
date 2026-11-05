package qualification

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"reflect"
	"strconv"
	"strings"
)

// Verify judges actual saved bytes against hidden expectations without a model.
// Any unexpected artifact or output on an escalation fails the entire case.
func Verify(c Case, result Result, runErr error) Finding {
	f := Finding{CaseID: c.ID, Category: c.Category, Expected: c.Expected, Observed: "error", Problems: []string{}, Hashes: map[string]string{}, ExitCode: result.ExitCode, DurationMS: result.DurationMS, Output: result.Output}
	if len(f.Output) > 8192 {
		f.Output = f.Output[:8192]
	}
	if runErr != nil {
		f.Problems = append(f.Problems, "The isolated runner failed or timed out.")
		return f
	}
	switch result.ExitCode {
	case 0:
		f.Observed = "complete"
	case 20:
		f.Observed = "escalate"
	case 21:
		f.Observed = "approval"
	}
	if f.Observed != c.Expected {
		f.Problems = append(f.Problems, "The outcome did not match the expected completion or stop.")
	}
	allowed := map[string]Check{}
	for _, check := range c.Checks {
		allowed[check.File] = check
	}
	total := 0
	for n, b := range result.Artifacts {
		total += len(b)
		if !FileName(n) || len(b) > MaxFileBytes {
			f.Problems = append(f.Problems, "An artifact exceeded the file policy.")
			continue
		}
		f.Hashes[n] = SHA256Hex(b)
		if _, ok := allowed[n]; !ok {
			f.Problems = append(f.Problems, fmt.Sprintf("Unexpected artifact: %s.", n))
		}
	}
	if total > MaxTotalBytes {
		f.Problems = append(f.Problems, "Artifacts exceeded the total size limit.")
	}
	for _, check := range c.Checks {
		b, ok := result.Artifacts[check.File]
		if !ok {
			f.Problems = append(f.Problems, fmt.Sprintf("Missing saved artifact: %s.", check.File))
			continue
		}
		if err := verifyCheck(check, b); err != nil {
			f.Problems = append(f.Problems, fmt.Sprintf("%s: %s", check.File, err))
		}
	}
	f.Passed = len(f.Problems) == 0
	return f
}

// VerifyFileCheck judges saved bytes against one file check (text_equals, json_equals, csv_equals, xlsx_cells) with
// readiness's own verifier, for other graders (internal/eval) to share it.
func VerifyFileCheck(check Check, b []byte) error {
	return verifyCheck(check, b)
}

// ValidateFileCheck says what is wrong with a file check's own fields, "" when it is well formed.
func ValidateFileCheck(check Check) string {
	if !fileCheckKind(check.Kind) {
		return "unsupported file check."
	}
	return validateFileCheck(PracticeCheck{Kind: check.Kind, File: check.File, Text: check.Text, JSON: check.JSON, Rows: check.Rows, Cells: check.Cells})
}

// DraftMatches says how the drafts differ from an outbox check ("" when exactly one draft matches it).
func DraftMatches(ch PracticeCheck, drafts []PracticeDraft) string {
	return draftMatches(ch, drafts)
}

// PresentWhenAbsent says what is there that an absent check (a file, one record or a recipient) says must not be;
// "" when it is absent.
func PresentWhenAbsent(ch PracticeCheck, ev PracticeEvidence) string {
	return presentWhenAbsent(ch, ev)
}

func verifyCheck(check Check, b []byte) error {
	switch check.Kind {
	case "text_equals":
		if string(b) != check.Text {
			return fmt.Errorf("saved text differs from the expected text")
		}
	case "json_equals":
		actual, err := decodeJSON(b)
		if err != nil {
			return fmt.Errorf("saved JSON is invalid or ambiguous")
		}
		expected, err := decodeJSON(check.JSON)
		if err != nil {
			return fmt.Errorf("expected JSON is invalid or ambiguous")
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("saved JSON differs from the expected result")
		}
	case "csv_equals":
		r := csv.NewReader(bytes.NewReader(b))
		r.FieldsPerRecord = -1
		rows, err := r.ReadAll()
		if err != nil || !reflect.DeepEqual(rows, check.Rows) {
			return fmt.Errorf("saved CSV rows differ from the expected rows")
		}
	case "xlsx_cells":
		return verifyWorkbook(check.Cells, b)
	default:
		return fmt.Errorf("unsupported independent check")
	}
	return nil
}

// decodeJSON rejects duplicate object keys and trailing documents, rather than
// silently accepting a file that another application could interpret differently.
func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 100 {
			return nil, fmt.Errorf("JSON too deep")
		}
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				obj := map[string]any{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return nil, err
					}
					k, ok := key.(string)
					if !ok {
						return nil, fmt.Errorf("key")
					}
					if _, ok := obj[k]; ok {
						return nil, fmt.Errorf("duplicate key")
					}
					v, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					obj[k] = v
				}
				_, err = d.Token()
				return obj, err
			case '[':
				arr := []any{}
				for d.More() {
					v, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					arr = append(arr, v)
				}
				_, err = d.Token()
				return arr, err
			}
			return nil, fmt.Errorf("unexpected delimiter")
		}
		return t, nil
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return v, nil
}

type richText struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (r richText) value() string {
	v := r.Text
	for _, run := range r.Runs {
		v += run.Text
	}
	return v
}

type worksheetCell struct {
	Ref     string   `xml:"r,attr"`
	Type    string   `xml:"t,attr"`
	Value   *string  `xml:"v"`
	Formula *string  `xml:"f"`
	Inline  richText `xml:"is"`
}

// verifyWorkbook reads OOXML on the control plane, including cached formula
// values. A workbook formula without a saved value never passes a cell check.
func verifyWorkbook(checks []CellCheck, b []byte) error {
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("saved workbook is not an XLSX package")
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		total += f.UncompressedSize64
		if total > 64<<20 || len(files) > 2000 {
			return fmt.Errorf("workbook package exceeds limits")
		}
		if _, ok := files[f.Name]; ok {
			return fmt.Errorf("duplicate workbook package entry")
		}
		files[f.Name] = f
	}
	for name := range files {
		if strings.Contains(name, "externalLinks/") || strings.Contains(name, "vbaProject") || strings.Contains(name, "embeddings/") {
			return fmt.Errorf("active workbook content is outside the qualification scope")
		}
	}
	read := func(name string, target any) error {
		f := files[name]
		if f == nil {
			return fmt.Errorf("missing workbook part")
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		return xml.NewDecoder(io.LimitReader(r, 64<<20)).Decode(target)
	}
	var book struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if read("xl/workbook.xml", &book) != nil || read("xl/_rels/workbook.xml.rels", &rels) != nil {
		return fmt.Errorf("invalid workbook structure")
	}
	relations := map[string]string{}
	for _, r := range rels.Items {
		if r.Mode == "External" {
			return fmt.Errorf("external workbook relationships are outside the qualification scope")
		}
		target := path.Clean(path.Join("xl", r.Target))
		if strings.HasPrefix(r.Target, "/") {
			target = strings.TrimPrefix(r.Target, "/")
		}
		if strings.HasPrefix(target, "xl/") {
			relations[r.ID] = target
		}
	}
	shared := []string{}
	if files["xl/sharedStrings.xml"] != nil {
		var ss struct {
			Items []richText `xml:"si"`
		}
		if read("xl/sharedStrings.xml", &ss) != nil {
			return fmt.Errorf("invalid shared strings")
		}
		for _, s := range ss.Items {
			shared = append(shared, s.value())
		}
	}
	sheets := map[string]map[string]worksheetCell{}
	for _, sheet := range book.Sheets {
		var ws struct {
			Rows []struct {
				Cells []worksheetCell `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if read(relations[sheet.ID], &ws) != nil {
			return fmt.Errorf("invalid worksheet")
		}
		cells := map[string]worksheetCell{}
		for _, row := range ws.Rows {
			for _, c := range row.Cells {
				if _, ok := cells[c.Ref]; ok {
					return fmt.Errorf("duplicate worksheet cell")
				}
				cells[c.Ref] = c
			}
		}
		sheets[sheet.Name] = cells
	}
	for _, want := range checks {
		c, ok := sheets[want.Sheet][want.Cell]
		if !ok {
			return fmt.Errorf("expected worksheet cell is missing")
		}
		if want.Formula != nil && (*want.Formula != (c.Formula != nil)) {
			return fmt.Errorf("worksheet cell formula requirement failed")
		}
		if c.Formula != nil && c.Value == nil {
			return fmt.Errorf("formula has no saved result")
		}
		if c.Formula != nil && (want.FormulaText == "" || strings.TrimPrefix(want.FormulaText, "=") != *c.Formula) {
			return fmt.Errorf("saved formula differs from the independently specified formula")
		}
		if want.FormulaText != "" && c.Formula == nil {
			return fmt.Errorf("expected formula is missing")
		}
		value := ""
		if c.Value != nil {
			value = *c.Value
		}
		if c.Type == "inlineStr" {
			value = c.Inline.value()
		}
		if c.Type == "s" {
			index, err := strconv.Atoi(value)
			if err != nil || index < 0 || index >= len(shared) {
				return fmt.Errorf("invalid shared string index")
			}
			value = shared[index]
		}
		if c.Type == "e" || value != want.Value {
			return fmt.Errorf("saved worksheet cell differs from its expected value")
		}
	}
	return nil
}
