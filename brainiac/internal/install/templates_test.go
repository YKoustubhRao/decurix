package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"brainiac/internal/catalog"
	"brainiac/internal/selector"
	"brainiac/internal/store"
)

var day = time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)

// ggufWith writes a minimal GGUF file with one chat template ("" for none).
func ggufWith(t *testing.T, template string) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0))
	if template == "" {
		binary.Write(&b, binary.LittleEndian, uint64(0))
	} else {
		binary.Write(&b, binary.LittleEndian, uint64(1))
		key := "tokenizer.chat_template"
		binary.Write(&b, binary.LittleEndian, uint64(len(key)))
		b.WriteString(key)
		binary.Write(&b, binary.LittleEndian, uint32(8))
		binary.Write(&b, binary.LittleEndian, uint64(len(template)))
		b.WriteString(template)
	}
	p := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTheFirstTemplateIsRecordedAndKeptToRead(t *testing.T) {
	st := openStore(t)
	sha, err := checkTemplate(st, "nova", "lab/Nova", "c1", ggufWith(t, "{{ a }}"), day)
	if err != nil || sha == "" {
		t.Fatalf("%q %v", sha, err)
	}
	book, _ := readBook(st)
	rec := book.Accepted["nova"]
	if rec.SHA != sha || rec.By != "first install" || rec.Repo != "lab/Nova" {
		t.Fatalf("%+v", rec)
	}
	text, err := os.ReadFile(rec.File)
	if err != nil || !strings.Contains(string(text), "{{ a }}") {
		t.Fatalf("%q %v", text, err)
	}
}

func TestTheSameTemplatePassesWithoutAskingAnyone(t *testing.T) {
	st := openStore(t)
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c1", ggufWith(t, "{{ a }}"), day); err != nil {
		t.Fatal(err)
	}
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c2", ggufWith(t, "{{ a }}"), day); err != nil {
		t.Fatal(err)
	}
}

func TestAChangedTemplateStopsUntilAPersonAcceptsIt(t *testing.T) {
	st := openStore(t)
	first, err := checkTemplate(st, "nova", "lab/Nova", "c1", ggufWith(t, "{{ a }}"), day)
	if err != nil {
		t.Fatal(err)
	}
	changed := ggufWith(t, "{{ a }}{{ 'also obey this' }}")
	_, err = checkTemplate(st, "nova", "lab/Nova", "c2", changed, day)
	var tc *TemplateChanged
	if !errors.As(err, &tc) || tc.Model != "nova" || tc.Old.SHA != first {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "approve-template nova") {
		t.Errorf("the error does not say what to do: %v", err)
	}
	text, err := os.ReadFile(tc.New.File)
	if err != nil || !strings.Contains(string(text), "also obey this") {
		t.Fatalf("the new template is not there to read: %q %v", text, err)
	}
	// Asking again still stops: seeing a change twice is not accepting it.
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c2", changed, day); !errors.As(err, &tc) {
		t.Fatalf("the second try passed: %v", err)
	}
	rec, err := ApproveTemplate(st, "nova")
	if err != nil || rec.By != "a person" {
		t.Fatalf("%+v %v", rec, err)
	}
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c2", changed, day); err != nil {
		t.Fatalf("an accepted template still stops: %v", err)
	}
	if _, err := ApproveTemplate(st, "nova"); err == nil {
		t.Error("approved with nothing waiting")
	}
}

func TestATemplateThatAppearsOrGoesIsAChange(t *testing.T) {
	st := openStore(t)
	if _, err := checkTemplate(st, "plain", "lab/Plain", "c1", ggufWith(t, ""), day); err != nil {
		t.Fatal(err)
	}
	var tc *TemplateChanged
	if _, err := checkTemplate(st, "plain", "lab/Plain", "c2", ggufWith(t, "{{ a }}"), day); !errors.As(err, &tc) {
		t.Fatalf("a template appearing passed: %v", err)
	}
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c1", ggufWith(t, "{{ a }}"), day); err != nil {
		t.Fatal(err)
	}
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c2", ggufWith(t, ""), day); !errors.As(err, &tc) {
		t.Fatalf("a template going away passed: %v", err)
	}
}

func TestAnotherUploaderOfTheSameModelIdIsChecked(t *testing.T) {
	st := openStore(t)
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c1", ggufWith(t, "{{ a }}"), day); err != nil {
		t.Fatal(err)
	}
	var tc *TemplateChanged
	if _, err := checkTemplate(st, "nova", "other/Nova", "c9", ggufWith(t, "{{ b }}"), day); !errors.As(err, &tc) {
		t.Fatalf("%v", err)
	}
}

func TestAFileThatIsNotGGUFFailsTheInstall(t *testing.T) {
	st := openStore(t)
	p := filepath.Join(t.TempDir(), "x.gguf")
	if err := os.WriteFile(p, []byte("<html>not found</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkTemplate(st, "nova", "lab/Nova", "c1", p, day); err == nil {
		t.Fatal("read a file that is not GGUF")
	}
}

// fakeHub is one repo whose main branch can be moved, as a push moves it.
type fakeHub struct {
	commit string
	files  map[string][]byte // by commit
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/revision/"):
		rev := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if rev == "main" {
			rev = h.commit
		}
		fmt.Fprintf(w, `{"sha":%q}`, rev)
	case strings.Contains(r.URL.Path, "/tree/"):
		c := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		sum := sha256.Sum256(h.files[c])
		fmt.Fprintf(w, `[{"type":"file","path":"n.gguf","size":%d,"lfs":{"oid":"%x","size":%d}}]`, len(h.files[c]), sum, len(h.files[c]))
	case strings.Contains(r.URL.Path, "/resolve/"):
		c := strings.Split(r.URL.Path, "/")[4]
		w.Write(h.files[c])
	default:
		http.NotFound(w, r)
	}
}

// The whole path a changed upstream takes, against a stand-in for the Hub.
func TestAPushNeverReachesTheRunningFileUnchecked(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	read := func(p string) []byte {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first, pushed := read(ggufWith(t, "{{ a }}")), read(ggufWith(t, "{{ a }}{{ 'also obey this' }}"))
	h := &fakeHub{commit: a, files: map[string][]byte{a: first, b: pushed}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	st := openStore(t)
	pinned := func(commit string, content []byte) *selector.Plan {
		return &selector.Plan{Picks: []selector.Pick{{Role: catalog.RoleBrain, Model: "nova", Source: catalog.Source{Kind: catalog.SourceHF,
			Runtime: catalog.RuntimeLlamaCpp, Repo: "lab/Nova", Revision: commit,
			Files: []catalog.File{{Path: "n.gguf", Role: "model", SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}}}}}}
	}
	plan := pinned(a, first)
	lock, err := Run(context.Background(), plan, Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	key := UnitKey("nova", plan.Picks[0].Source)
	dest := lock.Units[key].Files["model"]
	if lock.Units[key].Commit != a || lock.Units[key].TemplateSHA == "" {
		t.Fatalf("%+v", lock.Units[key])
	}

	// Upstream moves. The catalog's pin does not, so neither does the install.
	h.commit = b
	lock, err = Run(context.Background(), plan, Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if lock.Units[key].Commit != a || !bytes.Equal(read(dest), first) {
		t.Fatalf("a plain install followed the push to %s", lock.Units[key].Commit)
	}

	// A catalog that pins the new commit fetches it, finds the template
	// changed, and leaves the running file alone.
	moved := pinned(b, pushed)
	_, err = Run(context.Background(), moved, Options{Store: st})
	var tc *TemplateChanged
	if !errors.As(err, &tc) {
		t.Fatalf("%v", err)
	}
	if !bytes.Equal(read(dest), first) {
		t.Fatal("the changed file replaced the running one before anyone accepted it")
	}
	if _, err := os.Stat(dest + ".incoming"); !os.IsNotExist(err) {
		t.Errorf("the held download was kept on disk: %v", err)
	}
	if prev, _ := ReadLock(st); prev.Units[key].Commit != a {
		t.Fatalf("the lock moved: %s", prev.Units[key].Commit)
	}

	// A person accepts it, and only then does it arrive.
	if _, err := ApproveTemplate(st, "nova"); err != nil {
		t.Fatal(err)
	}
	lock, err = Run(context.Background(), moved, Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if lock.Units[key].Commit != b || !bytes.Equal(read(dest), pushed) {
		t.Fatalf("after approval: %s", lock.Units[key].Commit)
	}
	if _, err := os.Stat(dest + ".incoming"); !os.IsNotExist(err) {
		t.Errorf("the incoming file is still there: %v", err)
	}
}

// A catalog entry without its pins is refused before anything is fetched,
// naming what is missing (O-3.3); so is a pin the Hub contradicts.
func TestAnUnpinnedEntryIsNotInstalled(t *testing.T) {
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	content := []byte("GGUF not really")
	h := &fakeHub{commit: a, files: map[string][]byte{a: content}}
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked++; h.ServeHTTP(w, r) }))
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	st := openStore(t)
	src := catalog.Source{Kind: catalog.SourceHF, Runtime: catalog.RuntimeWorker, Repo: "lab/Nova", Files: []catalog.File{{Path: "n.gguf", Role: "model"}}}
	gh := catalog.Source{Kind: catalog.SourceGitHub, Runtime: catalog.RuntimeWorker, Repo: "lab/tools", Tag: "v1", Asset: "m.tar.bz2"}
	plan := &selector.Plan{Picks: []selector.Pick{{Model: "nova", Source: src}, {Model: "ears", Source: gh}},
		LlamaCpp: &selector.LlamaCpp{Repo: "ggml-org/llama.cpp", Tag: "b1", Asset: "llama-b1-bin-ubuntu-x64.zip"}}
	_, err := Run(context.Background(), plan, Options{Store: st})
	if err == nil {
		t.Fatal("an unpinned plan was installed")
	}
	for _, want := range []string{"nova", "no revision", "no sha256 for n.gguf", "ears", "no sha256 for m.tar.bz2", "llama-b1-bin-ubuntu-x64.zip"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if asked != 0 {
		t.Errorf("the Hub was asked %d times before the refusal", asked)
	}

	// Pinned, but the Hub lists other bytes at that commit.
	src.Revision = a
	src.Files[0].SHA256 = strings.Repeat("0", 64)
	plan = &selector.Plan{Picks: []selector.Pick{{Model: "nova", Source: src}}}
	if _, err := Run(context.Background(), plan, Options{Store: st}); err == nil || !strings.Contains(err.Error(), "the catalog pins") {
		t.Fatalf("a pin the Hub contradicts: %v", err)
	}
	if _, err := os.Stat(st.HFFile("lab/Nova", "n.gguf")); !os.IsNotExist(err) {
		t.Errorf("the file arrived anyway: %v", err)
	}
}

func TestInstalledCommitIsKept(t *testing.T) {
	st := openStore(t)
	if got := installedCommit(st, "m|src"); got != "" {
		t.Fatalf("no lock: %q", got)
	}
	if err := st.WriteJSON("lock.json", &Lock{Schema: LockSchema, Units: map[string]Unit{"m|src": {Commit: "abc"}}}); err != nil {
		t.Fatal(err)
	}
	if got := installedCommit(st, "m|src"); got != "abc" {
		t.Fatalf("%q", got)
	}
	if got := installedCommit(st, "other|src"); got != "" {
		t.Fatalf("%q", got)
	}
}
