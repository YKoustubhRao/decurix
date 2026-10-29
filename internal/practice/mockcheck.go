package practice

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// mockWrapper is the fixed program that smoke-tests a drafted mock app in the
// offline container before the owner sees the draft: for each fixture it starts
// mock_app.py on loopback, fetches every page the demonstration visited and
// reports the first failure with the tail of the app's own error output.
const mockWrapper = `import json, os, socket, subprocess, sys, time, urllib.error, urllib.request
fixtures = json.load(open("/work/fixtures.json", encoding="utf-8"))
paths = json.load(open("/work/paths.json", encoding="utf-8"))
problems = []

def tail(log):
    log.flush()
    log.seek(0)
    lines = [l.strip() for l in log.read().decode("utf-8", "replace").splitlines() if l.strip(" -^~	")]
    return " | ".join(lines[-3:])[-300:]

for label, fixture, strict in fixtures:
    if len(problems) >= 6:
        break
    with open("/tmp/fixture.json", "w", encoding="utf-8") as f:
        json.dump(fixture, f)
    if os.path.exists("/tmp/state.json"):
        os.remove("/tmp/state.json")
    log = open("/tmp/mock.log", "w+b")
    proc = subprocess.Popen([sys.executable, "/work/mock_app.py", "--port", "18000", "--fixture", "/tmp/fixture.json",
                             "--state", "/tmp/state.json"], cwd="/tmp", stdout=log, stderr=log)
    up = False
    for _ in range(100):
        if proc.poll() is not None:
            break
        try:
            socket.create_connection(("127.0.0.1", 18000), 0.2).close()
            up = True
            break
        except OSError:
            time.sleep(0.1)
    if not up:
        problems.append("With " + label + " the mock app did not start: " + tail(log))
    else:
        for path in paths:
            why = ""
            try:
                with urllib.request.urlopen("http://127.0.0.1:18000" + path, timeout=5) as r:
                    if not r.read():
                        why = "an empty page"
            except urllib.error.HTTPError as e:
                # A case's fixture may lack the demonstration's own records; a
                # clean 4xx is then an honest answer, a 5xx never is.
                if strict or e.code >= 500:
                    why = "HTTP " + str(e.code)
            except Exception as e:
                why = type(e).__name__ + ": " + str(e)[:120]
            if why:
                problems.append("With " + label + " the mock app answered GET " + path + " with " + why + ". Its error output: " + tail(log))
                break
    proc.kill()
    proc.wait()
    log.close()
print(json.dumps(problems))
`

// maxMockPaths bounds how many recorded pages the smoke test fetches.
const maxMockPaths = 15

// mockPaths are the pages (path and query) the demonstration visited on the
// practised site, from the pinned skill's steps; "/" when none are recorded.
func mockPaths(src qualification.PracticeSource) []string {
	hosts := map[string]bool{}
	for _, h := range src.Hosts {
		hosts[strings.ToLower(h)] = true
	}
	seen := map[string]bool{}
	var out []string
	if raw, ok := src.Files[".decurix/skills/"+src.Name+".json"]; ok {
		var skill struct {
			Steps []struct {
				URL string `json:"url"`
			} `json:"steps"`
		}
		if json.Unmarshal(raw, &skill) == nil {
			for _, st := range skill.Steps {
				u, err := url.Parse(st.URL)
				if err != nil || !hosts[strings.ToLower(u.Hostname())] {
					continue
				}
				p := u.EscapedPath()
				if p == "" {
					p = "/"
				}
				if u.RawQuery != "" {
					p += "?" + u.RawQuery
				}
				if !seen[p] && len(out) < maxMockPaths {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	if len(out) == 0 {
		out = []string{"/"}
	}
	return out
}

// CheckMock starts the drafted mock app in the offline container with an empty
// fixture (what calibration uses) and with each case's fixture, fetches every
// recorded page, and returns what failed, as problems the author can fix.
func CheckMock(ctx context.Context, off Offline, image, mock string, src qualification.PracticeSource, cases []qualification.PracticeCase) []string {
	fixtures := [][3]any{{"an empty fixture (the demonstration's own replay, every recorded page must answer)", map[string]any{}, true}}
	ids := make([]string, 0, len(cases))
	byID := map[string]json.RawMessage{}
	for _, c := range cases {
		if len(c.Fixture) > 0 {
			ids = append(ids, c.ID)
			byID[c.ID] = c.Fixture
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if len(fixtures) >= 8 {
			break
		}
		fixtures = append(fixtures, [3]any{"case " + id + "'s fixture", byID[id], false})
	}
	fx, err := json.Marshal(fixtures)
	if err != nil {
		return []string{"The case fixtures could not be prepared for the mock app check."}
	}
	paths, _ := json.Marshal(mockPaths(src))
	d := qualification.Definition{Image: image, Runtime: qualification.RuntimeVersion, Language: "python", Script: mockWrapper, TimeoutSeconds: GeneratorTimeoutSeconds}
	c := qualification.Case{ID: "mockcheck", Expected: "escalate", Inputs: map[string][]byte{"mock_app.py": []byte(mock), "fixtures.json": fx, "paths.json": paths}}
	res, err := off.RunQualification(ctx, d, c)
	if err != nil {
		log.Printf("practice: mock check: %v", err)
		return []string{"the mock app could not be checked in the offline container"}
	}
	out := strings.TrimSpace(res.Output)
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		out = out[i+1:]
	}
	var problems []string
	if res.ExitCode != 0 || json.Unmarshal([]byte(out), &problems) != nil {
		return []string{fmt.Sprintf("The mock app check did not finish (exit %d): %s", res.ExitCode, clip(strings.TrimSpace(res.Output), 300))}
	}
	return problems
}
