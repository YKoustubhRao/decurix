package skills

import (
	"reflect"
	"strings"
	"testing"
)

func TestAMonthFolderTakesTheShapeOfTheOnesBesideIt(t *testing.T) {
	cases := map[string]string{
		"APR 2026": "<MON> 9999", "November": "<Month>", "NOV 2026": "<MON> 9999", "7. OCTOBER": "9. <MONTH>",
		"06. SEPTEMBER": "99. <MONTH>", "Nov-26": "<Mon>-99", "BANK STATEMENTS": "", "2026-27": "",
	}
	for name, want := range cases {
		if got := monthShape(name); got != want {
			t.Errorf("monthShape(%q) = %q, want %q", name, got, want)
		}
	}
	shape, ex := siblingShape([]string{"APR 2026", "AUG 2026", "BANK STATEMENTS", "JUL 2026", "JUN 2026", "MAY 2026", "OCT 2026", "SEP 2026"})
	if shape != "<MON> 9999" || !reflect.DeepEqual(ex, []string{"APR 2026", "AUG 2026", "JUL 2026"}) {
		t.Errorf("siblingShape = %q %v", shape, ex)
	}
	if shape, _ := siblingShape([]string{"APR 2026", "May", "BANK"}); shape != "" {
		t.Errorf("no shared shape among two forms, got %q", shape)
	}
	got := mkdirTargets(`mkdir -p '/home/h/PC/Clients/A Co/2026-27/November' && cp x.pdf '/home/h/PC/Clients/A Co/2026-27/November/'`)
	if !reflect.DeepEqual(got, []string{"/home/h/PC/Clients/A Co/2026-27/November"}) {
		t.Errorf("mkdirTargets = %v", got)
	}
	got = mkdirTargets(`cd /w && mkdir -p /home/h/PC/A\ Co/NOV\ 2026 "/tmp/b c"`)
	if !reflect.DeepEqual(got, []string{"/home/h/PC/A Co/NOV 2026", "/tmp/b c"}) {
		t.Errorf("mkdirTargets escaped = %v", got)
	}
}

func TestAMonthFolderTakesTheNumberingOfTheOnesBesideIt(t *testing.T) {
	fy := []string{"4. JULY", "5. AUGUST", "6. SEPTEMBER", "7. OCTOBER", "BANK"}
	if k, w, ok := siblingNumbering(fy); !ok || k != 3 || w != 1 {
		t.Fatalf("financial-year numbering = %d %d %v", k, w, ok)
	}
	if got := expectedMonthName(fy, 11); got != "8. NOVEMBER" {
		t.Fatalf("expected %q, got %q", "8. NOVEMBER", got)
	}
	why, _ := monthFolderMismatch("11. NOVEMBER", fy)
	if !strings.Contains(why, `"8. NOVEMBER"`) {
		t.Fatalf("a wrong number was not caught or not named: %q", why)
	}
	if why, _ := monthFolderMismatch("8. NOVEMBER", fy); why != "" {
		t.Fatalf("the right name was refused: %q", why)
	}
	if got := expectedMonthName([]string{"01-Jan", "02-Feb", "03-Mar"}, 4); got != "04-Apr" {
		t.Fatalf("calendar numbering, padded: %q", got)
	}
	if got := expectedMonthName([]string{"APR 2026", "MAY 2026", "JUN 2026"}, 11); got != "" {
		t.Fatalf("a year is not a numbering to guess: %q", got)
	}
	if why, _ := monthFolderMismatch("November", []string{"APR 2026", "MAY 2026", "JUN 2026"}); why == "" {
		t.Fatal("a name in another form was not caught")
	}
	if _, _, ok := siblingNumbering([]string{"1. APRIL", "9. MAY"}); ok {
		t.Fatal("siblings that disagree have no numbering")
	}
	m := typedFolderRe.FindAllStringSubmatch(`gui key ctrl+shift+n && gui type "11. NOVEMBER" && gui key Return && gui type "/home/h/PC/A/2026-27/11. NOVEMBER"`, -1)
	if len(m) != 1 || m[0][1] != "11. NOVEMBER" {
		t.Fatalf("typed folder names = %v", m)
	}
}
