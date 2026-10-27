package verify

import (
	"reflect"
	"testing"
)

func TestUnsupportedFiguresFindsOnlyFiguresNoSourceHas(t *testing.T) {
	sources := []string{"Invoice 4417 Total 12,345.00 due 2026-10-31", "1. pay the vendor"}
	got := UnsupportedFigures("Invoice 4417 totals 12345, due 2026-10-31; tax is 2,222.10 and step 3", sources...)
	if !reflect.DeepEqual(got, []string{"2222.1"}) {
		t.Fatalf("got %v", got)
	}
}

func TestFiguresNormalise(t *testing.T) {
	got := Figures("1,20,000.50 and 007 and 3.00 and 9")
	want := []string{"120000.5", "7", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
