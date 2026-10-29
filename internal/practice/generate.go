package practice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/YKoustubhRao/decurix/internal/qualification"
)

// Offline runs a program in readiness's offline container (*desktop.Manager's
// RunQualification): no network, no mounts, no credentials, a read-only root.
type Offline interface {
	RunQualification(ctx context.Context, d qualification.Definition, c qualification.Case) (qualification.Result, error)
}

// generatorWrapper is the fixed program the offline container runs. It runs the
// owner-reviewed generate.py and flattens its output into /work, because the
// offline collector takes flat files only: DIR/files/<case>/<name> becomes
// <case>__<name> (case ids never hold "_").
const generatorWrapper = `import io, os, re, subprocess, sys, zipfile
seed, count = sys.argv[1], sys.argv[2]
out = "/tmp/practice-out"
os.makedirs(out, exist_ok=True)

def steady(data):
    # Office files (zip packages) carry the time they were saved: in each zip
    # entry and in docProps/core.xml. Fix both, so a seed alone decides the bytes.
    if not data.startswith(b"PK\x03\x04"):
        return data
    try:
        src = zipfile.ZipFile(io.BytesIO(data))
        buf = io.BytesIO()
        with zipfile.ZipFile(buf, "w") as dst:
            for info in src.infolist():
                body = src.read(info.filename)
                if info.filename == "docProps/core.xml":
                    body = re.sub(rb"(<dcterms:(?:created|modified)[^>]*>)[^<]*", rb"\g<1>2000-01-01T00:00:00Z", body)
                fixed = zipfile.ZipInfo(info.filename, date_time=(1980, 1, 1, 0, 0, 0))
                fixed.compress_type, fixed.external_attr = info.compress_type, info.external_attr
                dst.writestr(fixed, body)
        return buf.getvalue()
    except Exception:
        return data

env = dict(os.environ, PYTHONHASHSEED="0")
proc = subprocess.run([sys.executable, "/work/generate.py", "--seed", seed, "--count", count, "--out", out],
                      cwd="/tmp", env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=240)
if proc.returncode != 0:
    sys.stdout.write(proc.stderr.decode("utf-8", "replace")[-1500:])
    sys.exit(3)
with open(os.path.join(out, "cases.json"), "rb") as src, open("/work/cases.json", "wb") as dst:
    dst.write(src.read())
files = os.path.join(out, "files")
if os.path.isdir(files):
    for case in sorted(os.listdir(files)):
        folder = os.path.join(files, case)
        if "_" in case or os.path.islink(folder) or not os.path.isdir(folder):
            sys.stdout.write("files/ holds something that is not a case folder: " + case)
            sys.exit(4)
        for name in sorted(os.listdir(folder)):
            path = os.path.join(folder, name)
            if os.path.islink(path) or not os.path.isfile(path):
                sys.stdout.write("a case folder holds something that is not a file: " + case + "/" + name)
                sys.exit(4)
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_. -]{0,119}", case + "__" + name) or name.startswith("."):
                sys.stdout.write("file name not allowed: " + case + "/" + name + " (use only letters, digits, spaces, dots, hyphens and underscores, start with a letter or digit, and keep case id plus name under 118 characters)")
                sys.exit(4)
            with open(path, "rb") as src, open(os.path.join("/work", case + "__" + name), "wb") as dst:
                dst.write(steady(src.read()))
`

// GeneratorTimeoutSeconds bounds one generator run.
const GeneratorTimeoutSeconds = 280

// generatedCase is one entry of cases.json as the generator contract writes it.
type generatedCase struct {
	ID       string                        `json:"id"`
	Category string                        `json:"category"`
	Request  string                        `json:"request"`
	Files    []string                      `json:"files"`
	Fixture  json.RawMessage               `json:"fixture,omitempty"`
	Expected string                        `json:"expected"`
	Checks   []qualification.PracticeCheck `json:"checks"`
}

// runGenerator runs the generator once and returns the flat files it produced.
func runGenerator(ctx context.Context, off Offline, image, generator string, seed int64, count int) (map[string][]byte, error) {
	d := qualification.Definition{Image: image, Runtime: qualification.RuntimeVersion, Language: "python", Script: generatorWrapper, TimeoutSeconds: GeneratorTimeoutSeconds}
	c := qualification.Case{ID: "generate", Expected: "escalate", Args: []string{strconv.FormatInt(seed, 10), strconv.Itoa(count)}, Inputs: map[string][]byte{"generate.py": []byte(generator)}}
	res, err := off.RunQualification(ctx, d, c)
	if err != nil {
		log.Printf("practice: generator run (seed %d): %v", seed, err)
		return nil, fmt.Errorf("the generator could not run in the offline container (seed %d)", seed)
	}
	if res.ExitCode != 0 {
		out := strings.TrimSpace(res.Output)
		if len(out) > 600 {
			out = out[len(out)-600:]
		}
		return nil, fmt.Errorf("the generator failed (seed %d, exit %d): %s", seed, res.ExitCode, out)
	}
	return res.Artifacts, nil
}

// sameFiles reports whether two flat file sets are byte-identical.
func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for n, x := range a {
		if y, ok := b[n]; !ok || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

// differing names up to max files that are missing from one set or differ in bytes.
func differing(a, b map[string][]byte, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string][]byte{a, b} {
		for n := range m {
			if seen[n] {
				continue
			}
			seen[n] = true
			x, okA := a[n]
			y, okB := b[n]
			if !okA || !okB || !bytes.Equal(x, y) {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	if len(out) > max {
		out = append(out[:max], "and more")
	}
	return out
}

// inputsOf is every generated input file, without cases.json.
func inputsOf(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for n, b := range files {
		if n != "cases.json" {
			out[n] = b
		}
	}
	return out
}

// GenerateCases runs the generator three times in the offline container: seed S
// twice, which must be byte-identical, and S+1, whose inputs must differ. It
// returns the cases of seed S and every reason they cannot be approved; the
// store adds ValidatePracticeSuite's own reasons when the draft is saved.
func GenerateCases(ctx context.Context, off Offline, image, generator string, seed int64, count int) ([]qualification.PracticeCase, []string) {
	first, err := runGenerator(ctx, off, image, generator, seed, count)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var problems []string
	again, err := runGenerator(ctx, off, image, generator, seed, count)
	if err != nil {
		problems = append(problems, err.Error())
	} else if !sameFiles(first, again) {
		problems = append(problems, "The generator is not deterministic: the same seed gave different files ("+strings.Join(differing(first, again, 5), ", ")+").")
	}
	other, err := runGenerator(ctx, off, image, generator, seed+1, count)
	if err != nil {
		problems = append(problems, err.Error())
	} else if sameFiles(inputsOf(first), inputsOf(other)) && bytes.Equal(first["cases.json"], other["cases.json"]) {
		problems = append(problems, "Another seed gave the same cases: the generator does not vary its inputs.")
	}
	cases, more := parseCases(first, count)
	return cases, append(problems, more...)
}

// parseCases turns the generator's flat output into practice cases, listing
// files a case names but nobody wrote, and files nobody names.
func parseCases(files map[string][]byte, count int) ([]qualification.PracticeCase, []string) {
	raw, ok := files["cases.json"]
	if !ok {
		return nil, []string{"The generator wrote no cases.json."}
	}
	var listed []generatedCase
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&listed); err != nil {
		return nil, []string{"cases.json does not follow the contract: " + clip(err.Error(), 200)}
	}
	var problems []string
	if count > 0 && len(listed) != count {
		problems = append(problems, fmt.Sprintf("The generator was asked for %d cases and wrote %d.", count, len(listed)))
	}
	used := map[string]bool{}
	cases := make([]qualification.PracticeCase, 0, len(listed))
	for _, g := range listed {
		c := qualification.PracticeCase{ID: g.ID, Category: g.Category, Request: g.Request, Fixture: g.Fixture, Expected: g.Expected, Checks: g.Checks, Files: map[string][]byte{}}
		for _, name := range g.Files {
			key := g.ID + "__" + name
			body, ok := files[key]
			if !ok {
				problems = append(problems, fmt.Sprintf("Case %s names an input %s the generator did not write.", g.ID, name))
				continue
			}
			used[key] = true
			c.Files[name] = body
		}
		cases = append(cases, c)
	}
	var stray []string
	for n := range files {
		if n != "cases.json" && !used[n] {
			stray = append(stray, n)
		}
	}
	sort.Strings(stray)
	for _, n := range stray {
		problems = append(problems, fmt.Sprintf("The generator wrote %s, which no case names.", n))
	}
	return cases, problems
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
