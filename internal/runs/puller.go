package runs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/desktop"
)

// Home is the as-worker file access desktop.Manager already has (homefs.go): paths are absolute, under the worker's
// home. A path that is not there is desktop.ErrPathNotFound; a desktop that is not there, desktop.ErrNotFound.
type Home interface {
	ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error)
	WriteTar(ctx context.Context, id, dir string, archive io.Reader) error
}

// DefaultLimit bounds the recording's observations, program and frames kept for one run when Puller.Limit is unset.
const DefaultLimit = 64 << 20

// LabelLimit bounds one label part's read (the run folder, the ledger, notes, files, outputs, soul's trace, the
// recording's meta and events). Labels are kept whole whatever the frames take; one over this is cut, and said.
const LabelLimit = 64 << 20

// MaxFiles bounds how many files one pull keeps of the recording, and MaxLabelFiles of each label part (review M3): a
// worker can make zero-byte entries without end, and the manifest and Missing list must not grow with them. Each part
// has its own count, so a flood in one never crowds out another's labels.
const (
	MaxFiles      = 20000
	MaxLabelFiles = 2000
)

// maxMissing bounds the Missing list; past it one line says how many more there were.
const maxMissing = 200

// homeDir is the worker's home in every desktop; bundle paths are relative to it.
const homeDir = "/home/headless"

// tarSlack is room for tar's own headers and padding on top of the bytes a read may bring.
const tarSlack = 1 << 20

// TaskIDShape is the form of a task id the puller reads a run for: it names files in the worker's home.
var TaskIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrBadTaskID is a task id not of TaskIDShape: nothing is read for it.
var ErrBadTaskID = errors.New("runs: not a task id")

// ErrNothingFound is a pull that found no part of the run at all: no bundle is saved (the run has none), so the
// execution stays unpulled.
var ErrNothingFound = errors.New("runs: no part of the run was found on the desktop")

// ErrDesktopGone is a pull of a desktop that is not there: nothing is saved.
var ErrDesktopGone = errors.New("runs: the desktop is not there")

// Puller reads a run's files out of the worker's home and saves them as its bundle.
type Puller struct {
	Home  Home
	Store *Store
	Blobs Blobs
	// Limit bounds the recording's observations, program and frames kept for one run (DefaultLimit when 0). Over
	// it, frames are dropped first, then observations, oldest first. The labels (the run folder, ledger, notes,
	// files, outputs, trace, the recording's meta and events) are never dropped for it.
	Limit int64
}

// part is one thing a run leaves in the worker's home.
type part struct {
	name string // as the bundle's Missing list and the trajectory's integrity name it
	rel  string // under the home
}

// partsOf is every part of task's run (PLAN-P2 C4, C8; docs/recordings.md).
func partsOf(task string) []part {
	return []part{
		{"recording", ".decurix/recordings/task-" + task},
		{"notes", ".decurix/computer/evidence/" + task + ".jsonl"},
		{"ledger", ".decurix/computer/evidence/" + task + ".records.json"},
		{"files", ".decurix/computer/evidence/" + task + ".files"},
		{"outputs", ".decurix/computer/evidence/" + task + ".outputs"},
		{"run", ".decurix/runs/" + task},
		{"soul_trace", ".decurix/ai/traces/" + task + ".jsonl"},
	}
}

// labelParts are the parts read first and kept whole: everything that labels the run but the recording.
func labelParts(task string) []part {
	var out []part
	for _, pt := range partsOf(task) {
		if pt.name != "recording" {
			out = append(out, pt)
		}
	}
	return out
}

// Kinds of the recording's budgeted files, in the order they are kept: observations, then the program, then frames.
const (
	kindObs = iota
	kindProgram
	kindFrame
)

// pull is one Pull's state: what is kept, what is not, and the recording's files held back until the budget decides.
type pull struct {
	p        *Puller
	ref      ExecutionRef
	limit    int64
	files    []File
	missing  []string
	dropped  int // Missing lines past maxMissing
	held     []spooled
	spoolDir string
	events   string // the recording's events.jsonl blob, to order observations and frames
	found    bool
	gone     bool
	capped   map[string]int // entries not kept for the file cap, by part
	counted  map[string]int // entries kept or held, by part
}

// spooled is a budgeted recording file read off the desktop and held on local disk until the budget decides.
type spooled struct {
	rel   string // under the home
	name  string // its file name, as events.jsonl names it
	kind  int
	local string
	bytes int64
}

// Pull reads ref's run off its desktop as the worker, stores every file once as a blob, saves the bundle (merged with
// an earlier pull of the same execution, never dropping what that one kept), and writes EXPORTED (C8) into the run's
// folder on the desktop. The labels are read first and kept whole; the recording's observations and frames share
// Limit. A part not there is listed in Missing by name; one that could not be read, with a short reason. Writing
// EXPORTED is logged when it fails; the bundle is saved anyway.
func (p *Puller) Pull(ctx context.Context, ref ExecutionRef) (Bundle, error) {
	if !TaskIDShape.MatchString(ref.TaskID) {
		return Bundle{}, ErrBadTaskID
	}
	if ref.ExecID == "" || ref.Worker == "" {
		return Bundle{}, errors.New("runs: a pull needs its execution and worker")
	}
	if p.Home == nil || p.Store == nil {
		return Bundle{}, errors.New("runs: the puller has no desktop access or store")
	}
	blobs := p.Blobs
	if blobs == nil {
		blobs = p.Store.blobs
	}
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	st := &pull{p: &Puller{Home: p.Home, Store: p.Store, Blobs: blobs, Limit: limit}, ref: ref, limit: limit,
		capped: map[string]int{}, counted: map[string]int{}}
	defer st.cleanup()

	// 1. The labels, each on its own read: nothing the recording holds can crowd them out.
	for _, pt := range labelParts(ref.TaskID) {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
		st.readLabel(ctx, pt.name, pt.rel)
		if st.gone {
			return Bundle{}, ErrDesktopGone
		}
	}
	// 2. The recording's own labels, by path.
	rec := ".decurix/recordings/task-" + ref.TaskID
	metaThere := st.readLabel(ctx, "recording", rec+"/meta.json")
	eventsThere := st.readLabel(ctx, "recording", rec+"/events.jsonl")
	if st.gone {
		return Bundle{}, ErrDesktopGone
	}
	if !metaThere && !eventsThere {
		st.miss("recording: not present")
	} else {
		// 3. What the budget decides: observations, the program, frames, each its own bounded read.
		st.readHeld(ctx, rec+"/obs", kindObs, true)
		st.readHeld(ctx, rec+"/program.json", kindProgram, false)
		st.readHeld(ctx, rec+"/frames", kindFrame, true)
		if st.gone {
			return Bundle{}, ErrDesktopGone
		}
		st.keepHeld()
	}
	if !st.found {
		return Bundle{}, ErrNothingFound
	}
	for _, name := range sortedKeys(st.capped) {
		st.missing = append(st.missing, fmt.Sprintf("%s: %d entries not kept (over the cap of files a pull keeps of it)",
			name, st.capped[name]))
	}
	if st.dropped > 0 {
		st.missing = append(st.missing, fmt.Sprintf("... and %d more not listed", st.dropped))
	}

	b := Bundle{ExecID: ref.ExecID, TaskID: ref.TaskID, Worker: ref.Worker, PulledAt: time.Now(), Files: st.files,
		Missing: st.missing}
	// EXPORTED is written under the store's lock, from the bundle as stored: the last pull saved is the last written.
	saved, err := p.Store.SaveBundleThen(b, func(saved Bundle) error {
		return p.writeExported(ctx, ref, saved.ManifestSHA)
	})
	if err != nil && saved.ExecID == "" {
		return Bundle{}, err
	}
	if err != nil {
		log.Printf("runs: the bundle of %s is saved, but EXPORTED could not be written on %s: %v", ref.ExecID,
			ref.Worker, err)
	}
	return saved, nil
}

// miss adds one line to Missing, or counts it past maxMissing.
func (s *pull) miss(line string) {
	if len(s.missing) >= maxMissing {
		s.dropped++
		return
	}
	s.missing = append(s.missing, line)
}

// room reports whether one more file of part name may be kept under its cap, counting it against name when not.
func (s *pull) room(name string) bool {
	limit := MaxLabelFiles
	if name == "recording" {
		limit = MaxFiles
	}
	if s.counted[name] < limit {
		s.counted[name]++
		return true
	}
	s.capped[name]++
	return false
}

// open reads rel off the desktop as a tar, or says why not. ok is false when there is nothing to read.
func (s *pull) open(ctx context.Context, name, rel string, limit int64, quietIfAbsent bool) (io.ReadCloser, bool) {
	rc, err := s.p.Home.ReadTar(ctx, s.ref.Worker, homeDir+"/"+rel, limit)
	switch {
	case errors.Is(err, desktop.ErrPathNotFound):
		if !quietIfAbsent {
			s.miss(name + ": not present")
		}
		return nil, false
	case errors.Is(err, desktop.ErrNotFound):
		s.gone = true
		return nil, false
	case err != nil:
		log.Printf("runs: reading %s of %s on %s: %v", rel, s.ref.ExecID, s.ref.Worker, err)
		s.miss(name + ": could not be read")
		return nil, false
	}
	s.found = true
	return rc, true
}

// readLabel reads one label part (a file or a folder) and keeps every regular file of it, under the file cap only.
// It reports whether the path was there.
func (s *pull) readLabel(ctx context.Context, name, rel string) bool {
	quiet := name == "recording" // the recording's absence is said once, for both of its labels
	rc, ok := s.open(ctx, name, rel, LabelLimit+tarSlack, quiet)
	if !ok {
		return false
	}
	defer rc.Close()
	s.eachEntry(rc, name, rel, func(tr io.Reader, rel string, size int64) {
		if !s.room(name) {
			return
		}
		sha, n, err := s.p.Blobs.Put(io.LimitReader(tr, size))
		if err != nil || n != size {
			log.Printf("runs: storing %s of %s: %v (%d of %d bytes)", rel, s.ref.ExecID, err, n, size)
			s.miss(clip(rel) + ": could not be stored")
			return
		}
		s.files = append(s.files, File{Path: rel, SHA256: sha, Bytes: n})
		if strings.HasSuffix(rel, "/events.jsonl") && name == "recording" {
			s.events = sha
		}
	})
	return true
}

// readHeld reads a budgeted recording file or folder and holds each regular file on local disk for keepHeld. Its read
// is bounded: a folder past it is cut there, the files read before the cut are still candidates, and the cut is said.
func (s *pull) readHeld(ctx context.Context, rel string, kind int, quietIfAbsent bool) {
	rc, ok := s.open(ctx, "recording", rel, 4*s.limit+tarSlack, quietIfAbsent || kind == kindProgram)
	if !ok {
		return
	}
	defer rc.Close()
	s.eachEntry(rc, "recording", rel, func(tr io.Reader, rel string, size int64) {
		if !s.room("recording") {
			return
		}
		if err := s.spool(tr, rel, kind, size); err != nil {
			s.miss(clip(rel) + ": could not be read")
		}
	})
}

// eachEntry walks a part's tar: names outside it are refused, links and devices skipped (each said), folders passed
// over; fn gets every regular file with its path under the home. A cut stream is said, and ends the walk.
func (s *pull) eachEntry(rc io.Reader, name, rel string, fn func(tr io.Reader, rel string, size int64)) {
	parent, root := path.Dir(rel), path.Base(rel)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("runs: reading %s of %s on %s: %v", rel, s.ref.ExecID, s.ref.Worker, err)
			s.miss(clip(rel) + ": stopped part way (over the read limit, or the copy failed)")
			return
		}
		entry, ok := entryName(hdr.Name, root)
		if !ok {
			s.miss(clip(hdr.Name) + ": refused (a path outside " + name + ")")
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		full := parent + "/" + entry
		if hdr.Typeflag != tar.TypeReg {
			s.miss(clip(full) + ": not a regular file (skipped)")
			continue
		}
		fn(tr, full, hdr.Size)
	}
}

// entryName is a tar entry's name cleaned, when it stays inside the part (root itself, or under it).
func entryName(name, root string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", false
	}
	for _, seg := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if seg == ".." {
			return "", false
		}
	}
	clean := path.Clean(name)
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return "", false
	}
	return clean, true
}

// clip bounds a name the worker chose before it goes into the Missing list.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// spool holds one budgeted file on local disk until the budget is known.
func (s *pull) spool(r io.Reader, rel string, kind int, size int64) error {
	if s.spoolDir == "" {
		dir, err := os.MkdirTemp("", "decurix-pull-")
		if err != nil {
			return err
		}
		s.spoolDir = dir
	}
	f, err := os.CreateTemp(s.spoolDir, "held-")
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != size {
		os.Remove(f.Name())
		return fmt.Errorf("%s: %v (%d of %d bytes)", rel, err, n, size)
	}
	s.held = append(s.held, spooled{rel: rel, name: path.Base(rel), kind: kind, local: f.Name(), bytes: n})
	return nil
}

// keepHeld stores the budgeted files that fit in Limit: observations first, then the program, then frames, each
// kind newest first by the recording's events. The rest are dropped, frames before observations, oldest first, each
// named in Missing.
func (s *pull) keepHeld() {
	if len(s.held) == 0 {
		return
	}
	order := s.eventOrder()
	rank := func(f spooled) int {
		if i, ok := order[f.name]; ok {
			return i
		}
		return -1 // a file no event names is the oldest
	}
	sort.SliceStable(s.held, func(i, j int) bool {
		if s.held[i].kind != s.held[j].kind {
			return s.held[i].kind < s.held[j].kind
		}
		return rank(s.held[i]) > rank(s.held[j])
	})
	left := s.limit
	var dropped []spooled
	for _, f := range s.held {
		if f.bytes > left {
			dropped = append(dropped, f)
			continue
		}
		file, err := os.Open(f.local)
		if err != nil {
			s.miss(clip(f.rel) + ": could not be stored")
			continue
		}
		sha, n, err := s.p.Blobs.Put(file)
		file.Close()
		if err != nil || n != f.bytes {
			log.Printf("runs: storing %s of %s: %v", f.rel, s.ref.ExecID, err)
			s.miss(clip(f.rel) + ": could not be stored")
			continue
		}
		left -= n
		s.files = append(s.files, File{Path: f.rel, SHA256: sha, Bytes: n})
	}
	// Named in the order they were given up: frames first, each kind oldest first.
	sort.SliceStable(dropped, func(i, j int) bool {
		if dropped[i].kind != dropped[j].kind {
			return dropped[i].kind > dropped[j].kind
		}
		return rank(dropped[i]) < rank(dropped[j])
	})
	for _, f := range dropped {
		s.miss(clip(f.rel) + ": over the bundle limit (frames are dropped first, then observations, oldest first)")
	}
}

// eventOrder is when each frame and observation was first used, by the recording's events: an event's "obs" and
// "before" ahead of its "after" (a larger number is newer). Observations are named by their stem, frames by file name.
func (s *pull) eventOrder() map[string]int {
	order := map[string]int{}
	if s.events == "" {
		return order
	}
	rc, err := s.p.Blobs.Open(s.events)
	if err != nil {
		return order
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for i := 0; sc.Scan(); i++ {
		var ev struct{ Before, After, Obs string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		obs := ""
		if ev.Obs != "" {
			obs = path.Base(ev.Obs) + ".json"
		}
		for k, f := range []string{obs, ev.Before, ev.After} {
			if f == "" {
				continue
			}
			if _, seen := order[path.Base(f)]; !seen {
				order[path.Base(f)] = 3*i + k
			}
		}
	}
	return order
}

// cleanup removes the files spooled on local disk.
func (s *pull) cleanup() {
	if s.spoolDir != "" {
		os.RemoveAll(s.spoolDir)
	}
}

func sortedKeys(m map[string]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeExported writes ~/.decurix/runs/<task>/EXPORTED (C8): the execution id and the manifest's SHA-256, one line
// each, as the worker. The folders on the way are made when missing; existing ones keep their owner and mode.
func (p *Puller) writeExported(ctx context.Context, ref ExecutionRef, manifest string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, dir := range []string{".decurix/", ".decurix/runs/", ".decurix/runs/" + ref.TaskID + "/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o700, ModTime: now}); err != nil {
			return err
		}
	}
	body := []byte(ref.ExecID + "\n" + manifest + "\n")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ".decurix/runs/" + ref.TaskID + "/EXPORTED",
		Mode: 0o600, Size: int64(len(body)), ModTime: now}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return p.Home.WriteTar(ctx, ref.Worker, homeDir, &buf)
}

// FlushDesktop pulls each of refs in turn until timeout passes, and returns the execution ids it did not pull (the
// time ran out, or the pull failed). A ref with no worker is worker's.
func (p *Puller) FlushDesktop(ctx context.Context, worker string, refs []ExecutionRef, timeout time.Duration) (notPulled []string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, ref := range refs {
		if ref.Worker == "" {
			ref.Worker = worker
		}
		if ctx.Err() != nil {
			notPulled = append(notPulled, ref.ExecID)
			continue
		}
		if _, err := p.Pull(ctx, ref); err != nil {
			log.Printf("runs: flushing %s before %s goes: %v", ref.ExecID, worker, err)
			notPulled = append(notPulled, ref.ExecID)
		}
	}
	return notPulled
}
