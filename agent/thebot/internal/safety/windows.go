package safety

import (
	"regexp"
	"strings"
)

// A native Windows worker is not in a container: its commands run on the
// person's own computer, as them. What a Linux desktop can afford - it is
// disposable, and nothing of the person's is in it - this cannot, so the gate
// here is wider than commandRules: it also stops what would be ordinary
// inside a throwaway machine and is not on someone's own.
//
// The same principle still holds: work inside the worker's own folder is not
// asked about. Deleting a build folder there is ordinary; deleting anything
// outside it is the person's file.

// psStart matches where a PowerShell command (or cmd.exe built-in) can begin.
const psStart = `(?i)(^|[;&|({]\s*|\b(?:then|do)\s+)`

// deleteVerb is a command that removes files or folders.
const deleteVerb = `(remove-item|rm|ri|del|erase|rd|rmdir|clear-content|clc)\b`

var (
	windowsDeleteRe = regexp.MustCompile(psStart + deleteVerb)

	// elsewhereRe is a path that is not relative to the worker's own folder:
	// a drive, a network share, the profile (~, $HOME, $env:...).
	elsewhereRe = regexp.MustCompile(`(?i)(^|[\s"'(=,])([a-z]:[\\/]|\\\\[\w.$-]+\\|~[\\/]?|\$home\b|\$env:\w+|%\w+%|hk(lm|cu|cr|u|cc):)`)

	// ownFolderRe is the worker's own folder, named in full.
	ownFolderRe = regexp.MustCompile(`(?i)[\\/]decurix[\\/]workers[\\/]`)

	// upwardsRe is a path that climbs out of the folder the command runs in.
	upwardsRe = regexp.MustCompile(`(^|[\s"'\\/])\.\.([\\/]|\s|$|"|')`)
)

// windowsRules are checked against a command a native Windows worker is
// about to run, after commandRules.
var windowsRules = []rule{
	{regexp.MustCompile(psStart + `(format-volume\b|clear-disk\b|initialize-disk\b|remove-partition\b|diskpart\b|format(\.com)?\s+[a-z]:|cipher\s+/w\b|sdelete\b)`),
		"it can erase a disk"},
	{regexp.MustCompile(psStart + `(stop-computer|restart-computer|shutdown(\.exe)?|logoff)\b`),
		"it shuts down, restarts or signs out of this computer"},
	{regexp.MustCompile(psStart + `(stop-process|spps|kill|taskkill(\.exe)?)\b`),
		"it ends running programs, which may be ones you are using"},
	{regexp.MustCompile(`(?i)\bhk(lm|cu|cr|u|cc):|\breg(\.exe)?\s+(add|delete|import|restore)\b|\bregedit\b`),
		"it changes the Windows registry"},
	{regexp.MustCompile(psStart + `(winget\s+(uninstall|remove)|uninstall-package|remove-appxpackage|msiexec\b[^|;&]*\s/x\b)`),
		"it uninstalls software from this computer"},
	{regexp.MustCompile(psStart + `(set-executionpolicy|set-mppreference|add-mppreference|netsh\s+(advfirewall|firewall)|(set|disable|remove|new)-netfirewall\w*|disable-windowsoptionalfeature|bcdedit)\b`),
		"it changes this computer's security or system settings"},
	{regexp.MustCompile(psStart + `(net(\.exe)?\s+(user|localgroup)\b|(new|remove|set|disable|enable)-localuser\b|(add|remove)-localgroupmember\b)`),
		"it changes the user accounts of this computer"},
	{regexp.MustCompile(`(?i)-verb\s+runas\b|\brunas(\.exe)?\s`),
		"it asks Windows for administrator rights"},
	{regexp.MustCompile(psStart + `send-mailmessage\b`),
		"it sends email"},
	{regexp.MustCompile(`(?i)\b(invoke-webrequest|invoke-restmethod|iwr|irm|curl(\.exe)?)\b[^|;&]*\s-(method|x)\s*["']?(post|put|patch|delete)\b|\b(invoke-webrequest|invoke-restmethod|iwr|irm)\b[^|;&]*\s-(body|infile|form)\b`),
		"it sends data to a web service"},
}

// assessWindows reports whether a PowerShell command needs approval on a
// person's own computer, or nil.
func assessWindows(command string) *Concern {
	for _, r := range windowsRules {
		if !r.re.MatchString(command) {
			continue
		}
		if r.reason == "it sends data to a web service" && localTargetRe.MatchString(command) && !externalURLRe.MatchString(stripLocalURLs(command)) {
			continue
		}
		return &Concern{Reason: r.reason}
	}

	if windowsDeleteRe.MatchString(command) && deletesElsewhere(command) {
		return &Concern{Reason: "it deletes files outside this worker's own folder"}
	}

	return nil
}

// deletesElsewhere reports whether a delete command names something that is
// not under the worker's own folder: an absolute path that is not that
// folder, or a path that climbs out of it.
func deletesElsewhere(command string) bool {
	if upwardsRe.MatchString(command) {
		return true
	}

	// Each absolute path named: all of them must be the worker's own.
	for _, at := range elsewhereRe.FindAllStringIndex(command, -1) {
		rest := command[at[0]:]
		if end := strings.IndexAny(rest[1:], "\r\n;|"); end >= 0 {
			rest = rest[:end+1]
		}
		if !ownFolderRe.MatchString(rest) {
			return true
		}
	}

	return false
}
