package runs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func has(b Bundle, rel string) bool {
	for _, f := range b.Files {
		if f.Path == rel {
			return true
		}
	}
	return false
}

var labels = []string{
	".decurix/computer/evidence/t_1.records.json", ".decurix/computer/evidence/t_1.jsonl", ".decurix/runs/t_1/run.json",
	".decurix/ai/traces/t_1.jsonl", ".decurix/recordings/task-t_1/meta.json", ".decurix/recordings/task-t_1/events.jsonl",
}

// B5: the recording's observations never crowd out the run's labels.
func TestObservationsNeverStarveTheLabels(t *testing.T) {
	h := fixtureHome()
	h.files[".decurix/recordings/task-t_1/obs/0000000000000000.json"] = bytes.Repeat([]byte{'x'}, 9_830)
	p, _ := newPuller(t, h)
	p.Limit = 1000 // less than the one observation alone
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range labels {
		if !has(b, want) {
			t.Errorf("%s was dropped for the budget: %v", want, paths(b))
		}
	}
	if has(b, ".decurix/recordings/task-t_1/obs/0000000000000000.json") || !hasMissing(b, "obs/0000000000000000.json: over the bundle limit") {
		t.Fatalf("the oversized observation: files %v, missing %v", paths(b), b.Missing)
	}
}

// B5: frames past the read cap cut only the frames' own read; the recording's meta and events are kept.
func TestFramesPastTheReadCapLeaveTheEventsWhole(t *testing.T) {
	h := fixtureHome()
	for i := 0; i < 4; i++ {
		h.files[fmt.Sprintf(".decurix/recordings/task-t_1/frames/big%d.png", i)] = bytes.Repeat([]byte{byte(i)}, 600<<10)
	}
	p, _ := newPuller(t, h)
	p.Limit = 1000 // a read cap of 4 KB + 1 MB: the frames' tar is cut part way
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range labels {
		if !has(b, want) {
			t.Errorf("%s lost to the frames: %v", want, paths(b))
		}
	}
	if !hasMissing(b, "frames: stopped part way") {
		t.Fatalf("the cut is not said: %v", b.Missing)
	}
}

// Frames are dropped before observations, and observations before nothing else.
func TestFramesGoBeforeObservations(t *testing.T) {
	h := fixtureHome()
	obs := ".decurix/recordings/task-t_1/obs/0123456789abcdef.json"
	p, _ := newPuller(t, h)
	p.Limit = int64(len(h.files[obs])) + 1000 // the observation and one frame of 1000
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if !has(b, obs) || !has(b, ".decurix/recordings/task-t_1/frames/f3.png") || has(b, ".decurix/recordings/task-t_1/frames/f1.png") {
		t.Fatalf("kept %v", paths(b))
	}
}

// A desktop that is not there saves nothing (it is not a run with every part missing).
func TestAGoneDesktopSavesNothing(t *testing.T) {
	h := fixtureHome()
	h.gone = true
	p, _ := newPuller(t, h)
	if _, err := p.Pull(context.Background(), ref1); !errors.Is(err, ErrDesktopGone) {
		t.Fatalf("Pull = %v, want ErrDesktopGone", err)
	}
	if ok, _ := p.Store.Has("e_1"); ok {
		t.Fatal("a bundle was saved for a desktop that is not there")
	}
}

// M3: a flood of zero-byte entries in one part is capped, and every other part's labels are still kept.
func TestAFloodOfEntriesIsCappedAndLabelsKept(t *testing.T) {
	h := fixtureHome()
	var flood []*tar.Header
	for i := 0; i < MaxLabelFiles+500; i++ {
		flood = append(flood, &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("t_1/junk-%05d", i), Mode: 0o600})
	}
	h.extra[".decurix/runs/t_1"] = flood
	p, _ := newPuller(t, h)
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) > MaxLabelFiles+20 {
		t.Fatalf("%d files kept", len(b.Files))
	}
	for _, want := range []string{".decurix/ai/traces/t_1.jsonl", ".decurix/recordings/task-t_1/events.jsonl"} {
		if !has(b, want) {
			t.Fatalf("%s lost to the flood", want)
		}
	}
	if !hasMissing(b, "run: 5") || len(b.Missing) > maxMissing+1 {
		t.Fatalf("missing (%d lines): %v", len(b.Missing), b.Missing[:min(5, len(b.Missing))])
	}
}

// M1: a re-pull after the desktop pruned a file keeps the earlier reference, and a changed file keeps its old
// content in History.
func TestARePullMergesAndNeverDrops(t *testing.T) {
	h := fixtureHome()
	p, _ := newPuller(t, h)
	first, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	delete(h.files, ".decurix/computer/evidence/t_1.records.json")
	h.files[".decurix/runs/t_1/run.json"] = []byte(`{"schema":"decurix.run-manifest/v1","changed":true}`)
	second, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	if !has(second, ".decurix/computer/evidence/t_1.records.json") || len(second.Files) != len(first.Files) {
		t.Fatalf("the re-pull dropped a reference: %v", paths(second))
	}
	if !hasMissing(second, "ledger: not present on the desktop now; kept from an earlier pull") {
		t.Fatalf("missing = %v", second.Missing)
	}
	if len(second.History) != 1 || second.History[0].Path != ".decurix/runs/t_1/run.json" {
		t.Fatalf("history = %+v", second.History)
	}
	stored, _, _ := p.Store.Bundle("e_1")
	if len(stored.History) != 1 || !strings.Contains(stored.ManifestSHA, second.ManifestSHA) {
		t.Fatalf("stored = %+v", stored)
	}
}
