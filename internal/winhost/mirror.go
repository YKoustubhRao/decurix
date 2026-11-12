// Mirror mode: show and drive a program the person already has open on their own desktop, instead of starting one.
package winhost

import (
	"errors"
	"regexp"
	"strings"
)

// ============================================================
// Mirror
//
// An app copied to a desktop normally runs on this computer in a private profile and on a private desktop of its own:
// nothing of the person's signed-in programs, and for a browser an empty profile, which is why a worker's Chrome was not
// signed in to anything - and why a login WhatsApp ties to one device could not come along either: a copy is a second
// device, and WhatsApp wipes it. A mirrored app is the person's own, already open, on their own desktop. Nothing is
// copied and nothing is started: the helper looks at that one window (and the popups it owns) and posts input to it,
// the way it does for any other app, so the VM shows and drives the program exactly as it is, signed in as it is, and a
// desktop costs the VM nothing more. Nothing here, in the helper, or in the reaper ever ends it.
//
// A mirror is named by a Target whose program is "mirror:<program>" (Target.Target) and whose arguments are words the
// window's title must contain, or none (Target.Args). It is signed into a launcher like any other target
// (Token), so only an app the person gave a desktop can be asked for, and only that program.
// ============================================================

// MirrorPrefix starts the Target.Target of a mirrored program: "mirror:chrome".
const MirrorPrefix = "mirror:"

// mirrorProgram is what a mirrored program's name may be: the name of an executable, no path.
var mirrorProgram = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// IsMirror reports whether a launch target names a program to mirror rather than one to start.
func IsMirror(target string) bool { return strings.HasPrefix(target, MirrorPrefix) }

// MirrorTarget is the Target.Target that mirrors program (a bare executable name such as "chrome").
func MirrorTarget(program string) string { return MirrorPrefix + program }

// mirrorLine is the helper command that mirrors a target; an error when the program's name is not one.
func mirrorLine(target, args string) (string, error) {
	program := strings.TrimPrefix(target, MirrorPrefix)
	if !mirrorProgram.MatchString(program) {
		return "", errors.New("not a program to mirror")
	}
	return "attach\t" + program + "\t" + strings.TrimSpace(args), nil
}
