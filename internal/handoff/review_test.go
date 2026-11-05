package handoff

import "testing"

// No file of a program's own data (a dot-folder of the home) moves between workers.
func TestHomePathRefusesDotFolders(t *testing.T) {
	for _, p := range []string{"~/.claude/.credentials.json", home + "/.decurix/state.json", "~/work/.ssh/id_rsa", "~/.config/x"} {
		if _, ok := homePath(p); ok {
			t.Errorf("%s was accepted", p)
		}
	}
	for _, p := range []string{"~/work/invoice.pdf", home + "/Downloads/statement.csv"} {
		if _, ok := homePath(p); !ok {
			t.Errorf("%s was refused", p)
		}
	}
}
