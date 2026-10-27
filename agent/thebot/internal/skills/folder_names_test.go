package skills

import (
	"reflect"
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
