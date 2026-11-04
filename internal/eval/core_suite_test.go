package eval

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// packSuite tars a suite folder and the mocks it names in the upload layout (evals/run.py does the same).
func packSuite(t *testing.T, root, suite string) []byte {
	t.Helper()
	dir := filepath.Join(root, "evals", "suites", suite)
	files := map[string][]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc Suite
	if err := json.Unmarshal(files["suite.json"], &doc); err != nil {
		t.Fatal(err)
	}
	for m := range doc.Mocks {
		b, err := os.ReadFile(filepath.Join(root, "evals", "mocks", m, "mock_app.py"))
		if err != nil {
			t.Fatal(err)
		}
		files["mocks/"+m+"/mock_app.py"] = b
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))})
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	return buf.Bytes()
}

// TestCoreSuiteLoads loads the first dev suite from the repo (the one test that reads evals/) and calibrates it as
// far as can be done without a practice desktop: a worker that only claims "done" passes no case, and every case
// whose scripted solution needs only the mock and Python passes on the mock's own state.
func TestCoreSuiteLoads(t *testing.T) {
	root := repoRoot(t)
	s, err := Load(packSuite(t, root, "core-v1"))
	if err != nil {
		t.Fatalf("core-v1 does not load:\n%v", err)
	}
	if len(s.Cases) != 12 || s.SHA == "" {
		t.Fatalf("core-v1 has %d cases", len(s.Cases))
	}
	byOutcome := map[string]int{}
	for _, c := range s.Cases {
		byOutcome[c.Expected.Outcome]++
	}
	t.Logf("core-v1: %d cases %v, SHA %s", len(s.Cases), byOutcome, s.SHA)

	// The "nothing" arm: claims done and checked, changes nothing. No case may pass, in either mode.
	for _, c := range s.Cases {
		ev := Evidence{Observation: ObservedFinal, Checked: boolPtr(true), FinalText: "done", MockState: startState(t, c), Outputs: map[string][]byte{}}
		for _, mode := range []Mode{{}, {Expert: true}} {
			r := Grade(c, ev, mode)
			if r.Passed || !r.FalseDone {
				t.Errorf("%s: a bare \"done\" passed or was not counted false-done (expert %v): %+v", c.ID, mode.Expert, r)
			}
		}
	}

	// The "ask" arm: asks one question about nothing (the expert matches no fact to it) and changes nothing. No case
	// may accept it, in either mode (review EVAL-1, B2).
	for _, c := range s.Cases {
		ev := Evidence{Observation: ObservedQuery, FinalText: AskAnything, MockState: startState(t, c), Outputs: map[string][]byte{},
			Questions: []Asked{{Kind: ObservedQuery, Text: AskAnything}}}
		for _, mode := range []Mode{{}, {Expert: true}} {
			if r := Grade(c, ev, mode); r.Passed {
				t.Errorf("%s: a question about nothing passed (expert %v): %+v", c.ID, mode.Expert, r)
			}
		}
	}

	// Do-nothing finals that game a word or a number pass nothing either (review EVAL-1, B3).
	for _, text := range []string{"Tickets 1 2 3 4 5 6 7 8 9 10 11 12. It already exists.", "12 tickets; 7 open, 5 closed. Already done."} {
		for _, c := range s.Cases {
			ev := Evidence{Observation: ObservedFinal, Checked: boolPtr(true), FinalText: text, MockState: startState(t, c), Outputs: map[string][]byte{}}
			if r := Grade(c, ev, Mode{Expert: true}); r.Passed {
				t.Errorf("%s: %q passed", c.ID, text)
			}
		}
	}

	py := findPython(t)
	if py == "" {
		t.Log("no Python 3 here: the oracle calibration needs the mock; skipped")
		return
	}
	calibrated := 0
	for _, c := range s.Cases {
		ev, why := offlineOracle(t, py, s, c)
		if why != "" {
			t.Logf("%s: oracle not calibrated offline (%s); the L2 live run covers it", c.ID, why)
			continue
		}
		r := Grade(c, ev, Mode{Expert: true})
		if !r.Passed {
			t.Errorf("%s: the scripted solution does not pass its own graders: %+v", c.ID, r)
			continue
		}
		calibrated++
	}
	if calibrated < 9 {
		t.Fatalf("only %d cases calibrated offline; expected every case but the gate and the formula ones", calibrated)
	}
	t.Logf("oracle calibrated offline on %d of %d cases", calibrated, len(s.Cases))
}

func boolPtr(b bool) *bool { return &b }

// startState is the case's mock state before any work: its fixture's collections.
func startState(t *testing.T, c Case) map[string][]map[string]any {
	t.Helper()
	var f struct {
		Collections map[string][]map[string]any `json:"collections"`
	}
	if len(c.Fixture) > 0 {
		if err := json.Unmarshal(c.Fixture, &f); err != nil {
			t.Fatal(err)
		}
	}
	return f.Collections
}

// findPython is a Python 3 that runs here, or "".
func findPython(t *testing.T) string {
	for _, name := range []string{"python3", "python", "py"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		args := []string{"-c", "import sys; print(sys.version_info[0])"}
		if name == "py" {
			args = append([]string{"-3"}, args...)
		}
		out, err := exec.Command(p, args...).Output()
		if err == nil && strings.TrimSpace(string(out)) == "3" {
			if name == "py" {
				return p + " -3"
			}
			return p
		}
	}
	return ""
}

func pyCmd(py string, args ...string) *exec.Cmd {
	parts := strings.Fields(py)
	return exec.Command(parts[0], append(parts[1:], args...)...)
}

// shellWords splits a command line as a POSIX shell would for the quoting the cases use.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				j = len(line) - i - 1
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
			in = true
		case ch == '"':
			i++
			for ; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				cur.WriteByte(line[i])
			}
			in = true
		case ch == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			in = true
		case ch == ' ' || ch == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(ch)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// offlineOracle runs a case's scripted solution against the real mock app and Python, where every step can run
// without a practice desktop: form posts through curl, Python one-liners, and the answer. A step that needs the
// desktop (the screen, the office tool) is the live run's, and the reason is returned instead.
func offlineOracle(t *testing.T, py string, s Suite, c Case) (Evidence, string) {
	t.Helper()
	ev := Evidence{Observation: ObservedFinal, Checked: boolPtr(true), FinalText: "Done.", Outputs: map[string][]byte{}}
	work := t.TempDir()
	for n, b := range c.Files {
		if err := os.WriteFile(filepath.Join(work, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var base string
	var stateFile string
	if c.Start.Mock != "" {
		base, stateFile = startMock(t, py, s.MockSources[c.Start.Mock], c.Fixture)
	}
	for i, st := range c.Oracle {
		switch st.Kind {
		case StepAnswer:
			ev.FinalText = st.Line
			continue
		case StepSleep:
			continue
		case StepApproval:
			return ev, "an approval step needs the practice desktop's gate"
		}
		words := shellWords(st.Line)
		switch {
		case len(words) > 0 && words[0] == "curl" && strings.Contains(st.Line, "--data-urlencode"):
			form := url.Values{}
			target := ""
			for j := 0; j < len(words); j++ {
				if words[j] == "--data-urlencode" && j+1 < len(words) {
					k, v, _ := strings.Cut(words[j+1], "=")
					form.Set(k, v)
					j++
				} else if strings.HasPrefix(words[j], "http://") {
					target = words[j]
				}
			}
			u, err := url.Parse(target)
			if err != nil || base == "" {
				t.Fatalf("%s: oracle step %d posts to %q", c.ID, i+1, target)
			}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.PostForm(base+u.Path, form)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("%s: oracle step %d got %d from the mock", c.ID, i+1, resp.StatusCode)
			}
		case strings.Contains(st.Line, "curl $MOCK_ROUTE") || strings.HasPrefix(st.Line, "office read "):
			// A read the solution makes before it acts; nothing to change.
		case len(words) == 3 && words[0] == "python3" && words[1] == "-c":
			cmd := pyCmd(py, "-c", words[2])
			cmd.Dir = work
			if out, err := cmd.CombinedOutput(); err != nil {
				if strings.Contains(string(out), "ModuleNotFoundError") {
					return ev, "this Python lacks a module the step imports"
				}
				t.Fatalf("%s: oracle step %d: %v\n%s", c.ID, i+1, err, out)
			}
		default:
			return ev, fmt.Sprintf("step %d (%s) needs the practice desktop", i+1, strings.Fields(st.Line)[0])
		}
	}
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if _, input := c.Files[e.Name()]; input {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(work, e.Name()))
		ev.Outputs[e.Name()] = b
	}
	if stateFile != "" {
		b, err := os.ReadFile(stateFile)
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Collections map[string][]map[string]any `json:"collections"`
		}
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		ev.MockState = st.Collections
	}
	return ev, ""
}

// startMock runs the mock app on a free local port until the test ends; it returns its base URL and state file.
func startMock(t *testing.T, py string, source, fixture []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	app := filepath.Join(dir, "mock_app.py")
	fix := filepath.Join(dir, "fixture.json")
	state := filepath.Join(dir, "state.json")
	_ = os.WriteFile(app, source, 0o644)
	_ = os.WriteFile(fix, fixture, 0o644)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := pyCmd(py, app, "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--fixture", fix, "--state", state)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// The app is up when it answers and has failed when it exits; how long it takes to start is not under test.
	for {
		if resp, err := http.Get(base + "/"); err == nil {
			resp.Body.Close()
			return base, state
		}
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("the mock app exited before it answered: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
