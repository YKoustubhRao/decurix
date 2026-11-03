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
// home. A path that is not there is desktop.ErrNotFound.
type Home interface {
	ReadTar(ctx context.Context, id, p string, limit int64) (io.ReadCloser, error)
	WriteTar(ctx context.Context, id, dir string, archive io.Reader) error
}

// DefaultLimit bounds what one run's bundle may hold when Puller.Limit is unset.
const DefaultLimit = 64 << 20

// homeDir is the worker's home in every desktop; bundle paths are relative to it.
const homeDir = "/home/headless"

// tarSlack is room for tar's own headers and padding on top of the bytes a read may bring.
const tarSlack = 1 << 20

// TaskIDShape is the form of a task id the puller reads a run for: it names files in the worker's home.
var TaskIDShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrBadTaskID is a task id not of TaskIDShape: nothing is read for it.
var ErrBadTaskID = errors.New("runs: not a task id")

// ErrNothingFound is a pull that found no part of the run at all: no bundle is saved (the run has none, or the
// desktop is gone), so the execution stays unpulled.
var ErrNothingFound = errors.New("runs: no part of the run was found on the desktop")

// Puller reads a run's files out of the worker's home and saves them as its bundle.
type Puller struct {
	Home  Home
	Store *Store
	Blobs Blobs
	// Limit bounds the bytes kept for one run, across all its parts (DefaultLimit when 0). Over it, the recording's
	// frames are dropped first, oldest first; nothing else is.
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

// pull is one Pull's state: what is kept, what is not, and the frames held back until the budget is known.
type pull struct {
	p        *Puller
	ref      ExecutionRef
	limit    int64
	kept     int64 // bytes of every file kept but frames
	files    []File
	missing  []string
	frames   []spooled
	spoolDir string
	events   string // the recording's events.jsonl blob, to order the frames
	found    bool
}

// spooled is a frame read off the desktop and held on local disk until the budget decides.
type spooled struct {
	rel   string // under the home
	name  string // its file name, as events.jsonl names it
	local string
	bytes int64
}

// Pull reads ref's run off its desktop as the worker, stores every file once as a blob, saves the bundle, and writes
// EXPORTED (C8) into the run's folder on the desktop. A part that is not there is listed in Missing by name; one
// that could not be read, with a short reason. Writing EXPORTED is logged when it fails; the bundle is saved anyway.
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
	st := &pull{p: &Puller{Home: p.Home, Store: p.Store, Blobs: blobs, Limit: limit}, ref: ref, limit: limit}
	defer st.cleanup()

	for _, pt := range partsOf(ref.TaskID) {
		if err := ctx.Err(); err != nil {
			return Bundle{}, err
		}
		st.readPart(ctx, pt)
	}
	if !st.found {
		return Bundle{}, ErrNothingFound
	}
	st.keepFrames()

	b := Bundle{ExecID: ref.ExecID, TaskID: ref.TaskID, Worker: ref.Worker, PulledAt: time.Now(), Files: st.files,
		Missing: st.missing}
	if err := p.Store.SaveBundle(b); err != nil {
		return Bundle{}, err
	}
	saved, _, err := p.Store.Bundle(ref.ExecID)
	if err != nil {
		return Bundle{}, err
	}
	if err := p.writeExported(ctx, ref, saved.ManifestSHA); err != nil {
		log.Printf("runs: the bundle of %s is saved, but EXPORTED could not be written on %s: %v", ref.ExecID,
			ref.Worker, err)
	}
	return saved, nil
}

// readPart reads one part as a tar and keeps its files.
func (s *pull) readPart(ctx context.Context, pt part) {
	// A part's own read may bring the whole budget (frames are spooled, not counted, until keepFrames); the recording
	// may bring more, since its frames are chosen afterwards.
	readLimit := s.limit - s.kept + tarSlack
	if pt.name == "recording" {
		readLimit = 4*s.limit + tarSlack
	}
	rc, err := s.p.Home.ReadTar(ctx, s.ref.Worker, homeDir+"/"+pt.rel, readLimit)
	if errors.Is(err, desktop.ErrNotFound) {
		s.missing = append(s.missing, pt.name+": not present")
		return
	}
	if err != nil {
		log.Printf("runs: reading %s of %s on %s: %v", pt.name, s.ref.ExecID, s.ref.Worker, err)
		s.missing = append(s.missing, pt.name+": could not be read")
		return
	}
	defer rc.Close()
	s.found = true
	parent := path.Dir(pt.rel)
	root := path.Base(pt.rel)
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			log.Printf("runs: reading %s of %s on %s: %v", pt.name, s.ref.ExecID, s.ref.Worker, err)
			s.missing = append(s.missing, pt.name+": stopped part way (over the read limit, or the copy failed)")
			return
		}
		name, ok := entryName(hdr.Name, root)
		if !ok {
			s.missing = append(s.missing, clip(hdr.Name)+": refused (a path outside "+pt.name+")")
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		rel := parent + "/" + name
		if hdr.Typeflag != tar.TypeReg {
			s.missing = append(s.missing, clip(rel)+": not a regular file (skipped)")
			continue
		}
		if pt.name == "recording" && isFrame(name, root) {
			if err := s.spool(tr, rel, hdr.Size); err != nil {
				s.missing = append(s.missing, clip(rel)+": could not be read")
			}
			continue
		}
		if s.kept+hdr.Size > s.limit {
			s.missing = append(s.missing, clip(rel)+": over the bundle limit")
			continue
		}
		sha, n, err := s.p.Blobs.Put(io.LimitReader(tr, hdr.Size))
		if err != nil || n != hdr.Size {
			log.Printf("runs: storing %s of %s: %v (%d of %d bytes)", rel, s.ref.ExecID, err, n, hdr.Size)
			s.missing = append(s.missing, clip(rel)+": could not be stored")
			continue
		}
		s.kept += n
		s.files = append(s.files, File{Path: rel, SHA256: sha, Bytes: n})
		if pt.name == "recording" && name == root+"/events.jsonl" {
			s.events = sha
		}
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

// isFrame reports whether name (under the recording's root) is one of its frames.
func isFrame(name, root string) bool {
	return strings.HasPrefix(name, root+"/frames/") && !strings.Contains(strings.TrimPrefix(name, root+"/frames/"), "/")
}

// clip bounds a name the worker chose before it goes into the Missing list.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// spool holds one frame on local disk until the budget is known.
func (s *pull) spool(r io.Reader, rel string, size int64) error {
	if s.spoolDir == "" {
		dir, err := os.MkdirTemp("", "decurix-pull-")
		if err != nil {
			return err
		}
		s.spoolDir = dir
	}
	f, err := os.CreateTemp(s.spoolDir, "frame-")
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != size {
		os.Remove(f.Name())
		return fmt.Errorf("frame %s: %v (%d of %d bytes)", rel, err, n, size)
	}
	s.frames = append(s.frames, spooled{rel: rel, name: path.Base(rel), local: f.Name(), bytes: n})
	return nil
}

// keepFrames stores the frames that fit in what the budget has left, newest first by the recording's events; the
// rest are dropped, oldest first, each named in Missing.
func (s *pull) keepFrames() {
	if len(s.frames) == 0 {
		return
	}
	order := s.frameOrder()
	rank := func(f spooled) int {
		if i, ok := order[f.name]; ok {
			return i
		}
		return -1 // a frame no event names is the oldest
	}
	sort.SliceStable(s.frames, func(i, j int) bool { return rank(s.frames[i]) > rank(s.frames[j]) })
	left := s.limit - s.kept
	var dropped []string
	for _, f := range s.frames {
		if f.bytes > left {
			dropped = append(dropped, f.rel)
			continue
		}
		file, err := os.Open(f.local)
		if err != nil {
			s.missing = append(s.missing, clip(f.rel)+": could not be stored")
			continue
		}
		sha, n, err := s.p.Blobs.Put(file)
		file.Close()
		if err != nil || n != f.bytes {
			log.Printf("runs: storing %s of %s: %v", f.rel, s.ref.ExecID, err)
			s.missing = append(s.missing, clip(f.rel)+": could not be stored")
			continue
		}
		left -= n
		s.files = append(s.files, File{Path: f.rel, SHA256: sha, Bytes: n})
	}
	// Named oldest first, which is the order they were given up in.
	for i := len(dropped) - 1; i >= 0; i-- {
		s.missing = append(s.missing, clip(dropped[i])+": over the bundle limit (frames are dropped first, oldest first)")
	}
}

// frameOrder is when each frame was taken, by the recording's events: its first appearance, counting an event's
// "before" ahead of its "after" (a larger number is a newer frame).
func (s *pull) frameOrder() map[string]int {
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
		var ev struct{ Before, After string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		for k, f := range []string{ev.Before, ev.After} {
			if _, seen := order[path.Base(f)]; f != "" && !seen {
				order[path.Base(f)] = 2*i + k
			}
		}
	}
	return order
}

// cleanup removes the frames spooled on local disk.
func (s *pull) cleanup() {
	if s.spoolDir != "" {
		os.RemoveAll(s.spoolDir)
	}
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
