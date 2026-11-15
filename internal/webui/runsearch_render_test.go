// runsearch_render_test.go checks the tasks page's "Search past runs" box: shown only where its route is served, and
// its script sets a hit's text as text, never as markup.

package webui

import (
	"strings"
	"testing"
)

func TestTheTasksPageOffersRunSearchOnlyWhereItIsServed(t *testing.T) {
	renderer := NewRenderer()
	for _, search := range []bool{true, false} {
		var b strings.Builder
		if err := renderer.RenderTasks(&b, TasksPageData{Window: WindowOption{Value: "7", Phrase: "this week"}, Search: search}); err != nil {
			t.Fatal(err)
		}
		page := b.String()
		if shown := strings.Contains(page, `id="runsearch-form"`); shown != search {
			t.Errorf("Search %v: box shown %v", search, shown)
		}
		if search && strings.Contains(page[strings.Index(page, "runsearch-form\");"):], "innerHTML") {
			t.Error("the search script writes markup")
		}
	}
}
