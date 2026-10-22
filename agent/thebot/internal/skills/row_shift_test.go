package skills

import (
	"strings"
	"testing"
)

// GST suite T10 (2026-10-22): rows put into a workbook with openpyxl's insert_rows left the month above reading the
// rows the new month took.
func TestRowShiftNudge(t *testing.T) {
	sent := []string{
		"python3 - <<'PY'\nimport openpyxl\nwb=openpyxl.load_workbook(p)\nws=wb['SALES']\nws.insert_rows(54, amount=12)\nwb.save(p)\nPY",
		`python3 -c "from openpyxl import load_workbook; wb=load_workbook('a.xlsx'); wb.active.delete_rows(3); wb.save('a.xlsx')"`,
	}
	for _, command := range sent {
		if n := rowShiftNudge(command); !strings.Contains(n, `"insert_rows"`) {
			t.Errorf("not sent back: %q", command)
		}
	}
	for _, command := range []string{
		`office update book.xlsx '{"insert_rows": [{"sheet": "SALES", "before": 54, "count": 12}]}'`,
		"python3 -c \"import pandas as pd; df = pd.read_excel('a.xlsx'); df.insert_rows = 1\"",
		"grep -n insert_rows notes.txt",
	} {
		if n := rowShiftNudge(command); n != "" {
			t.Errorf("sent back: %q", command)
		}
	}
}
