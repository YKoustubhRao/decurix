package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"

	"github.com/YKoustubhRao/decurix/internal/sqlitedb"
)

// Every host-app helper a control plane starts carries that control plane's instance tag on its command line
// (-DecurixInstance <tag>), and the start-up sweep, the reaper and the file-release script end only helpers with this
// instance's tag. Found during the review of 2026-11-03 (W-04): a second control plane on the same computer (a dev
// one, a test run) ended the owner's live helper and the app it had open, because every helper looked the same.

// instanceFlag is the helper's command-line parameter that carries the tag (bridge.ps1's -DecurixInstance).
const instanceFlag = "-DecurixInstance"

// hostInstanceTag is this control plane's tag: a short hash of its data directory, which is what makes it this
// instance (two control planes cannot share one database).
var hostInstanceTag = sync.OnceValue(func() string { return instanceTagFor(sqlitedb.DataDir()) })

// instanceTagFor is the tag of the control plane whose data directory is dir: twelve hex digits, so it is one shell
// word and, being of fixed length, never a prefix of another instance's tag.
func instanceTagFor(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return hex.EncodeToString(sum[:6])
}

// ownHelperMatch is how the command line of a helper started by the instance tagged tag reads, as a PowerShell -like
// pattern: the helper script, then the tag, then the rest of its parameters (hostGUIStarter puts the tag first).
func ownHelperMatch(tag string) string {
	return helperMatch + " " + instanceFlag + " " + tag + " *"
}

// taggedHelperMatch is any helper started by a control plane that tags its helpers, whichever instance it is.
const taggedHelperMatch = helperMatch + " " + instanceFlag + " *"

// likeMatch is PowerShell's -like for the patterns above: case-insensitive, with * for any run of characters and ?
// for one. The patterns carry no [ ] sets (the tag is hex), so that is all of -like they use. It lets a test check the
// very pattern the scripts are given against a command line.
func likeMatch(pattern, s string) bool {
	p, t := []rune(strings.ToLower(pattern)), []rune(strings.ToLower(s))
	star, mark := -1, 0
	i, j := 0, 0
	for j < len(t) {
		switch {
		case i < len(p) && (p[i] == '?' || p[i] == t[j]):
			i++
			j++
		case i < len(p) && p[i] == '*':
			star, mark = i, j
			i++
		case star >= 0:
			i = star + 1
			mark++
			j = mark
		default:
			return false
		}
	}
	for i < len(p) && p[i] == '*' {
		i++
	}
	return i == len(p)
}
