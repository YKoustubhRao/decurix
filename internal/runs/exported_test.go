package runs

import (
	"context"
	"testing"
)

// C8: EXPORTED's third line is LABELLED's content as the pull read it, less trailing newlines, so the agent can tell
// whether an export covers the run's latest label without comparing times a tar copy rounds to the second. The first
// two lines stay the execution id and the manifest's SHA-256.
func TestExportedCarriesTheLabelItCovers(t *testing.T) {
	cases := []struct {
		name     string
		labelled []byte // nil: no LABELLED in the run folder
		third    string
	}{
		{"a label", []byte("final 1762240931.25\n"), "final 1762240931.25"},
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
		want := "e_1\n" + b.ManifestSHA + "\n" + c.third + "\n"
		if got := string(h.written[".decurix/runs/t_1/EXPORTED"]); got != want {
			t.Errorf("%s: EXPORTED = %q, want %q", c.name, got, want)
		}
	}
}
