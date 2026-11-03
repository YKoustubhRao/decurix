package webui

import (
	"strings"
	"testing"

	"github.com/YKoustubhRao/decurix/internal/dag"
)

// The DAG page on the phone/tunnel listener offers no Cancel: that route is this computer's only (RW1-F-04).
func TestTheGatedDagPageOffersNoCancel(t *testing.T) {
	r := NewRenderer()
	for _, gated := range []bool{false, true} {
		var b strings.Builder
		if err := r.RenderDag(&b, DagPageData{DAG: dag.DAG{ID: "d1", Name: "d"}, Gated: gated}); err != nil {
			t.Fatal(err)
		}
		want := "var canCancel = true;"
		if gated {
			want = "var canCancel = false;"
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("gated=%v: page lacks %q", gated, want)
		}
	}
}
