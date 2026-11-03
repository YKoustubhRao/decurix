package runs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/blobstore"
	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// fakeHome serves a fixture home: files by their path under /home/headless, plus raw extra tar entries per part.
type fakeHome struct {
	mu        sync.Mutex
	files     map[string][]byte
	extra     map[string][]*tar.Header // part path -> entries appended to its tar as they are
	reads     []string
	written   map[string][]byte
	failWrite bool
	block     map[string]bool // part paths whose read waits for the context
	gone      bool            // the desktop is not there
}

func newFakeHome() *fakeHome {
	return &fakeHome{files: map[string][]byte{}, extra: map[string][]*tar.Header{}, written: map[string][]byte{},
		block: map[string]bool{}}
}

func (h *fakeHome) ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error) {
	h.mu.Lock()
	h.reads = append(h.reads, p)
	block := h.block[p]
	h.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	rel := strings.TrimPrefix(p, "/home/headless/")
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for name := range h.files {
		if name == rel || strings.HasPrefix(name, rel+"/") {
			names = append(names, name)
		}
	}
	if h.gone {
		return nil, desktop.ErrNotFound
	}
	if len(names) == 0 && len(h.extra[rel]) == 0 {
		return nil, desktop.ErrPathNotFound
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	base := path.Dir(rel)
	for _, name := range names {
		entry := strings.TrimPrefix(name, base+"/")
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: entry, Mode: 0o600, Size: int64(len(h.files[name]))}); err != nil {
			return nil, err
		}
		tw.Write(h.files[name])
	}
	for _, hdr := range h.extra[rel] {
		tw.WriteHeader(hdr)
	}
	tw.Close()
	if limit > 0 && int64(buf.Len()) > limit {
		return io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()[:limit]), errReader{})), nil
	}
	return io.NopCloser(&buf), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("over the read limit") }

func (h *fakeHome) WriteTar(ctx context.Context, id, dir string, archive io.Reader) error {
	if h.failWrite {
		return errors.New("the desktop is stopped")
	}
	tr := tar.NewReader(archive)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			h.mu.Lock()
			h.written[path.Join(strings.TrimPrefix(dir, "/home/headless"), hdr.Name)] = b
			h.mu.Unlock()
		}
	}
}

func (h *fakeHome) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reads)
}

// fixtureHome is a home with a run of task t_1: a recording with 3 frames (f1 oldest), notes, a ledger, the run
// folder and soul's trace.
func fixtureHome() *fakeHome {
	h := newFakeHome()
	rec := ".decurix/recordings/task-t_1/"
	h.files[rec+"meta.json"] = []byte(`{"schema":"decurix.recording/v1","task_id":"t_1"}`)
	h.files[rec+"events.jsonl"] = []byte(`{"i":0,"before":"frames/f1.png","after":"frames/f2.png"}` + "\n" +
		`{"i":1,"before":"frames/f2.png","after":"frames/f3.png"}` + "\n")
	h.files[rec+"frames/f1.png"] = bytes.Repeat([]byte{1}, 1000)
	h.files[rec+"frames/f2.png"] = bytes.Repeat([]byte{2}, 1000)
	h.files[rec+"frames/f3.png"] = bytes.Repeat([]byte{3}, 1000)
	h.files[rec+"obs/0123456789abcdef.json"] = []byte(`{"ax":"tree"}`)
	h.files[".decurix/computer/evidence/t_1.jsonl"] = []byte(`{"note":"read"}` + "\n")
	h.files[".decurix/computer/evidence/t_1.records.json"] = []byte(`{"records":[]}`)
	h.files[".decurix/runs/t_1/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1"}`)
	h.files[".decurix/runs/t_1/LABELLED"] = []byte{}
	h.files[".decurix/ai/traces/t_1.jsonl"] = []byte(`{"kind":"call"}` + "\n")
	return h
}

var ref1 = ExecutionRef{ExecID: "e_1", TaskID: "t_1", Worker: "eve"}

func newPuller(t *testing.T, h *fakeHome) (*Puller, *memBlobs) {
	t.Helper()
	s, blobs := newTestStore(t)
	return &Puller{Home: h, Store: s, Blobs: blobs}, blobs
}

func paths(b Bundle) []string {
	var out []string
	for _, f := range b.Files {
		out = append(out, f.Path)
	}
	return out
}

func hasMissing(b Bundle, want string) bool {
	for _, m := range b.Missing {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}

func TestPullStoresEveryFileOnceAndMarksTheRunExported(t *testing.T) {
	h := fixtureHome()
	p, blobs := newPuller(t, h)
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 11 {
		t.Fatalf("files = %v", paths(b))
	}
	for _, f := range b.Files {
		want, ok := h.files[f.Path]
		if !ok || int64(len(want)) != f.Bytes || !blobs.Has(f.SHA256) {
			t.Errorf("file %+v not stored as on the desktop", f)
		}
	}
	// Two outputs files of the evidence are not there in the fixture: said, by name.
	if !hasMissing(b, "files: not present") || !hasMissing(b, "outputs: not present") || len(b.Missing) != 2 {
		t.Fatalf("missing = %v", b.Missing)
	}
	stored := blobs.count()
	if got := string(h.written[".decurix/runs/t_1/EXPORTED"]); got != "e_1\n"+b.ManifestSHA+"\n" {
		t.Fatalf("EXPORTED = %q", got)
	}
	if saved, ok, _ := p.Store.Bundle("e_1"); !ok || saved.ManifestSHA != b.ManifestSHA {
		t.Fatalf("the bundle was not saved: %+v", saved)
	}

	// A second pull of the same run stores nothing new.
	again, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if blobs.count() != stored || again.ManifestSHA != b.ManifestSHA {
		t.Fatalf("a second pull stored %d new blobs (manifest %s vs %s)", blobs.count()-stored, again.ManifestSHA, b.ManifestSHA)
	}
}

func TestAMissingTraceIsListed(t *testing.T) {
	h := fixtureHome()
	delete(h.files, ".decurix/ai/traces/t_1.jsonl")
	p, _ := newPuller(t, h)
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMissing(b, "soul_trace: not present") {
		t.Fatalf("missing = %v", b.Missing)
	}
}

func TestOverTheLimitFramesGoFirstOldestFirst(t *testing.T) {
	h := fixtureHome()
	p, _ := newPuller(t, h)
	nonFrames := int64(0)
	for name, b := range h.files {
		if !strings.Contains(name, "/frames/") {
			nonFrames += int64(len(b))
		}
	}
	p.Limit = nonFrames + 1500 // room for one frame of three
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(paths(b), ",")
	if !strings.Contains(got, "frames/f3.png") || strings.Contains(got, "frames/f1.png") || strings.Contains(got, "frames/f2.png") {
		t.Fatalf("kept %s; want the newest frame only", got)
	}
	for _, keep := range []string{"meta.json", "events.jsonl", "t_1.records.json", "run.json", "traces/t_1.jsonl", "obs/"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s was dropped for the limit: %s", keep, got)
		}
	}
	if b.TotalBytes > p.Limit {
		t.Fatalf("total %d over the limit %d", b.TotalBytes, p.Limit)
	}
	// Named in Missing, oldest first.
	i1, i2 := -1, -1
	for i, m := range b.Missing {
		if strings.Contains(m, "f1.png: over the bundle limit") {
			i1 = i
		}
		if strings.Contains(m, "f2.png: over the bundle limit") {
			i2 = i
		}
	}
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("missing = %v", b.Missing)
	}
}

func TestATarEscapeAndALinkAreRefusedAndListed(t *testing.T) {
	h := fixtureHome()
	h.extra[".decurix/runs/t_1"] = []*tar.Header{
		{Typeflag: tar.TypeReg, Name: "t_1/../../../etc/passwd", Size: 0, Mode: 0o600},
		{Typeflag: tar.TypeSymlink, Name: "t_1/link", Linkname: "/etc/shadow", Mode: 0o777},
		{Typeflag: tar.TypeReg, Name: "other/x", Size: 0, Mode: 0o600},
	}
	p, _ := newPuller(t, h)
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Files {
		if strings.Contains(f.Path, "..") || strings.HasSuffix(f.Path, "/link") || strings.Contains(f.Path, "other/") {
			t.Fatalf("stored %s", f.Path)
		}
	}
	if !hasMissing(b, "t_1/../../../etc/passwd: refused") || !hasMissing(b, ".decurix/runs/t_1/link: not a regular file") ||
		!hasMissing(b, "other/x: refused") {
		t.Fatalf("missing = %v", b.Missing)
	}
}

func TestAnInvalidTaskIDReadsNothing(t *testing.T) {
	h := fixtureHome()
	p, _ := newPuller(t, h)
	for _, bad := range []string{"", "../t_1", "t 1", "t_1/x", strings.Repeat("a", 65)} {
		if _, err := p.Pull(context.Background(), ExecutionRef{ExecID: "e", TaskID: bad, Worker: "eve"}); !errors.Is(err, ErrBadTaskID) {
			t.Fatalf("Pull(%q) = %v, want ErrBadTaskID", bad, err)
		}
	}
	if h.readCount() != 0 {
		t.Fatalf("%d reads for invalid task ids", h.readCount())
	}
}

func TestExportedFailingStillSavesTheBundle(t *testing.T) {
	h := fixtureHome()
	h.failWrite = true
	p, _ := newPuller(t, h)
	if _, err := p.Pull(context.Background(), ref1); err != nil {
		t.Fatalf("Pull = %v, want the bundle saved", err)
	}
	if ok, _ := p.Store.Has("e_1"); !ok {
		t.Fatal("no bundle saved")
	}
}

func TestNothingFoundSavesNoBundle(t *testing.T) {
	p, _ := newPuller(t, newFakeHome())
	if _, err := p.Pull(context.Background(), ref1); !errors.Is(err, ErrNothingFound) {
		t.Fatalf("Pull of an empty home = %v, want ErrNothingFound", err)
	}
	if ok, _ := p.Store.Has("e_1"); ok {
		t.Fatal("a bundle was saved for a run with nothing on the desktop")
	}
}

func TestFlushStopsAtItsTimeoutAndSaysWhatWasNotPulled(t *testing.T) {
	h := fixtureHome()
	// The second run's run-folder read hangs until the flush's time is up.
	h.files[".decurix/runs/t_2/run.json"] = []byte(`{}`)
	h.block["/home/headless/.decurix/runs/t_2"] = true
	h.files[".decurix/runs/t_3/run.json"] = []byte(`{}`)
	p, _ := newPuller(t, h)
	refs := []ExecutionRef{{ExecID: "e_1", TaskID: "t_1"}, {ExecID: "e_2", TaskID: "t_2"}, {ExecID: "e_3", TaskID: "t_3"}}
	start := time.Now()
	notPulled := p.FlushDesktop(context.Background(), "eve", refs, 300*time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("the flush did not stop at its timeout")
	}
	if strings.Join(notPulled, ",") != "e_2,e_3" {
		t.Fatalf("not pulled = %v, want e_2,e_3", notPulled)
	}
	if ok, _ := p.Store.Has("e_1"); !ok {
		t.Fatal("e_1 was not pulled")
	}
}

// The control plane's own types are what the run record is wired with.
var (
	_ Home  = (*desktop.Manager)(nil)
	_ Blobs = (*blobstore.Store)(nil)
)
