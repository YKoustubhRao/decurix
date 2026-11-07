// purge_render_test.go checks the tasks page's "Purge a leaked secret" control: shown on the primary listener with a
// password field for the value, and absent where the route is not served.

package webui

import (
	"strings"
	"testing"
)

// The control is on the tasks page only when Purge is set, and its value field is a password field.
func TestTheTasksPageOffersThePurgeOnlyWhereItIsServed(t *testing.T) {
	renderer := NewRenderer()
	for _, purge := range []bool{true, false} {
		var b strings.Builder
		if err := renderer.RenderTasks(&b, TasksPageData{Window: WindowOption{Value: "7", Phrase: "this week"}, Purge: purge}); err != nil {
			t.Fatal(err)
		}
		page := b.String()
		shown := strings.Contains(page, "Purge a leaked secret")
		if shown != purge {
			t.Errorf("Purge %v: control shown %v", purge, shown)
		}
		if purge && !strings.Contains(page, `id="purge-value" name="value" type="password"`) {
			t.Error("the value field is not a password field")
		}
	}
}
