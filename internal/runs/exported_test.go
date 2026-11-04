package runs

import (
	"context"
	"strings"
	"testing"
)

// C8: after its first two lines (the execution id and the manifest's SHA-256), EXPORTED carries LABELLED's content as
// the pull read it, less trailing newlines, so the agent can tell whether an export covers the run's latest label
// without comparing times a tar copy rounds to the second.
func TestExportedCarriesTheLabelItCovers(t *testing.T) {
	cases := []struct {
		name     string
		labelled []byte // nil: no LABELLED in the run folder
		copy     string // what follows EXPORTED's second line
	}{
		{"the agent's two-line label", []byte("e_1\n3"), "e_1\n3"},
		{"a one-line label", []byte("final 1762240931.25\n"), "final 1762240931.25"},
		{"trailing newlines of either kind", []byte("reply 1762241000.5\r\n\n"), "reply 1762241000.5"},
		{"an empty label", []byte{}, ""},
		{"no label", nil, ""},
	}
	for _, c := range cases {
		h := fixtureHome()
		if c.labelled == nil {
			delete(h.files, ".decurix/runs/t_1/LABELLED")
		} else {
			h.files[".decurix/runs/t_1/LABELLED"] = c.labelled
		}
		p, _ := newPuller(t, h)
		b, err := p.Pull(context.Background(), ref1)
		if err != nil {
			t.Fatal(err)
		}
		want := "e_1\n" + b.ManifestSHA + "\n" + c.copy + "\n"
		if got := string(h.written[".decurix/runs/t_1/EXPORTED"]); got != want {
			t.Errorf("%s: EXPORTED = %q, want %q", c.name, got, want)
		}
	}
}

// The agent's real LABELLED (H-PY-13: `<execution id>\n<label number>`, written with no final newline) makes EXPORTED
// four lines, each what it should be.
func TestExportedIsFourLinesWithTheAgentsLabel(t *testing.T) {
	h := fixtureHome()
	h.files[".decurix/runs/t_1/LABELLED"] = []byte("e_1\n2")
	p, _ := newPuller(t, h)
	b, err := p.Pull(context.Background(), ref1)
	if err != nil {
		t.Fatal(err)
	}
	got := string(h.written[".decurix/runs/t_1/EXPORTED"])
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "e_1" || lines[1] != b.ManifestSHA || lines[2] != "e_1" || lines[3] != "2" ||
		!strings.HasSuffix(got, "\n") {
		t.Fatalf("EXPORTED = %q, want e_1, the manifest, e_1, 2 (four lines)", got)
	}
}
