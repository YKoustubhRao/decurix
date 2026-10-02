//go:build windows

package skills

import (
	"strings"
	"testing"
)

// Found by bench/native: asked to save a Notepad file under a path the request
// gave, the operator typed that path into the Save dialog and was refused -
// "appears nowhere in the request" - twice, and gave up on the dialog. A
// backslash had been read as an escape and dropped, so C:\work\out\typed.txt
// was compared as C:workouttyped.txt. In PowerShell a backslash is a path's
// separator.
func TestGroundingReadsAWindowsPathAsItIsWritten(t *testing.T) {
	request := `Open Notepad, type hello, and save it as C:\Users\ada\workers\t1\home\work\bench-out\typed.txt.`

	for _, command := range []string{
		`gui type e994 "C:\Users\ada\workers\t1\home\work\bench-out\typed.txt"`,
		`gui type e994 "C:\Users\ada\workers\t1\home\work\bench-out"`,
		`gui type e12 'typed.txt'`,
	} {
		if n := groundingNudge(groundingState(request), command); n != "" {
			t.Errorf("%s\nwas refused: %s", command, n)
		}
	}

	// A path the request never gave is still a guess.
	if n := groundingNudge(groundingState(request), `gui type e994 "C:\Users\ada\Documents\other.txt"`); !strings.Contains(n, "appear nowhere") {
		t.Errorf("a path that is not in the request was let through: %q", n)
	}

	if got := typedValues(`gui type e1 "C:\new\table.txt"`); len(got) != 1 || got[0] != `C:\new\table.txt` {
		t.Errorf("typedValues = %q, want the path whole", got)
	}
}
