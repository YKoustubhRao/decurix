package desktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every step of a Dockerfile in this repository that downloads bounds its download: a stalled connection to a
// mirror, with a tool that has no timeout of its own, held a first start of the app at one apk step for 18 minutes
// (2026-11-04). Each tool by its own options where it has them, and by `timeout` and a retry loop where it has none.
// The app's stall watchdog (desktop/electron/build-watchdog.js) is the backstop, not the bound.
func TestEveryDockerfileDownloadIsBounded(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, dir := range []string{"docker", "desktop", "agent", "bench", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "build" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.Contains(strings.ToLower(d.Name()), "dockerfile") {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) < 5 {
		t.Fatalf("found only %d Dockerfiles: %v", len(files), files)
	}
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runInstructions(string(raw)) {
			for _, problem := range unboundedDownloads(run) {
				t.Errorf("%s: %s\n\t%s", file, problem, clipRun(run))
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no RUN instruction was read")
	}
}

// The check itself refuses each unbounded shape, and lets each bounded one through.
func TestTheDownloadCheckKnowsEachTool(t *testing.T) {
	for run, bad := range map[string]bool{
		"apk add --no-cache curl": true,
		"for try in 1 2 3; do timeout 300 apk add --no-cache curl && exit 0; done; exit 1":                false,
		"apt-get update && apt-get install -y curl":                                                       true,
		"apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=60 -o Acquire::https::Timeout=60 update": false,
		"pip3 install --no-cache-dir requests==2.32.3":                                                    true,
		"pip3 install --no-cache-dir --timeout 60 --retries 5 requests==2.32.3":                           false,
		"curl -fsSL https://example.test/x.tar.gz -o /tmp/x.tar.gz":                                       true,
		"curl -fsSL --retry 3 --speed-limit 1024 --speed-time 120 https://example.test/x -o /tmp/x":       false,
		"wget -q https://example.test/x -O /tmp/x":                                                        true,
		"wget -q --timeout=60 --tries=3 https://example.test/x -O /tmp/x":                                 false,
		"npm install -g codex": true,
		"npm install -g --fetch-timeout=60000 --fetch-retries=5 codex": false,
		"go mod download": true,
		"for try in 1 2 3; do timeout 600 go mod download && exit 0; done; exit 1":                                     false,
		"curl -fsSL --retry 3 --speed-limit 1024 --speed-time 120 https://example.test/i.sh | sh":                      true,
		"timeout 1200 sh -c 'curl -fsSL --retry 3 --speed-limit 1024 --speed-time 120 https://example.test/i.sh | sh'": false,
		"chmod 755 /opt/x": false,
		"apt-get clean && rm -rf /var/lib/apt/lists/*": false,
	} {
		if got := len(unboundedDownloads(run)) > 0; got != bad {
			t.Errorf("%q: unbounded = %v, want %v (%v)", run, got, bad, unboundedDownloads(run))
		}
	}
}

// runInstructions is each RUN of a Dockerfile, its continuation lines joined and its comment lines left out.
func runInstructions(dockerfile string) []string {
	var runs []string
	var cur strings.Builder
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(dockerfile, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !in {
			if !strings.HasPrefix(strings.ToUpper(trimmed), "RUN ") {
				continue
			}
			in = true
			cur.Reset()
			trimmed = strings.TrimSpace(trimmed[4:])
		}
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\") + " ")
			continue
		}
		cur.WriteString(trimmed)
		runs = append(runs, cur.String())
		in = false
	}
	return runs
}

var (
	segmentSplit   = regexp.MustCompile(`&&|\|\||;`)
	aptCommand     = regexp.MustCompile(`apt-get((?:\s+-o\s+\S+)*)\s+(update|install|upgrade|dist-upgrade)\b`)
	pipInstall     = regexp.MustCompile(`\bpip3?\s+install\b`)
	curlCommand    = regexp.MustCompile(`(^|[\s(|'])curl\s+-`)
	wgetCommand    = regexp.MustCompile(`(^|[\s(|'])wget\s+-`)
	npmInstall     = regexp.MustCompile(`\bnpm\s+(install|i|ci)\b`)
	pipedInstaller = regexp.MustCompile(`\|\s*(sh|bash)\b`)
	timedApk       = regexp.MustCompile(`\btimeout\s+\d+\s+apk\s+add\b`)
	timedGoMod     = regexp.MustCompile(`\btimeout\s+\d+\s+go\s+mod\s+download\b`)
	timedRun       = regexp.MustCompile(`\btimeout\s+\d+\s`)
)

// unboundedDownloads is what in one RUN downloads without a bound on a stall.
func unboundedDownloads(run string) []string {
	var problems []string
	if strings.Contains(run, "apk add") && !timedApk.MatchString(run) {
		problems = append(problems, "apk add without `timeout N`")
	}
	if strings.Contains(run, "go mod download") && !timedGoMod.MatchString(run) {
		problems = append(problems, "go mod download without `timeout N`")
	}
	for _, m := range aptCommand.FindAllStringSubmatch(run, -1) {
		if !strings.Contains(m[1], "Acquire::http::Timeout") || !strings.Contains(m[1], "Acquire::https::Timeout") || !strings.Contains(m[1], "Acquire::Retries") {
			problems = append(problems, "apt-get "+m[2]+" without Acquire::http(s)::Timeout and Acquire::Retries")
		}
	}
	if pipedInstaller.MatchString(run) && strings.Contains(run, "curl") && !timedRun.MatchString(run) {
		problems = append(problems, "an installer piped into a shell without `timeout N` around it")
	}
	for _, seg := range segmentSplit.Split(run, -1) {
		if pipInstall.MatchString(seg) && (!strings.Contains(seg, "--timeout") || !strings.Contains(seg, "--retries")) {
			problems = append(problems, "pip install without --timeout and --retries")
		}
		if curlCommand.MatchString(seg) && (!strings.Contains(seg, "--retry") || !(strings.Contains(seg, "--speed-time") || strings.Contains(seg, "--max-time"))) {
			problems = append(problems, "curl without --retry and a stall bound (--speed-time or --max-time)")
		}
		if wgetCommand.MatchString(seg) && (!strings.Contains(seg, "--timeout=") || !strings.Contains(seg, "--tries=")) {
			problems = append(problems, "wget without --timeout= and --tries=")
		}
		if npmInstall.MatchString(seg) && (!strings.Contains(seg, "--fetch-timeout") || !strings.Contains(seg, "--fetch-retries")) {
			problems = append(problems, "npm install without --fetch-timeout and --fetch-retries")
		}
	}
	return problems
}

// clip is a RUN shortened for a failure message.
func clipRun(run string) string {
	if len(run) > 160 {
		return run[:160] + "…"
	}
	return run
}
