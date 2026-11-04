package desktop

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// prunesRecorded is a daemon that records each ImagesPrune's filters and reclaims 1 MB each time.
type prunesRecorded struct {
	*fakeAPI
	seen []filters.Args
}

func (p *prunesRecorded) ImagesPrune(_ context.Context, f filters.Args) (image.PruneReport, error) {
	p.seen = append(p.seen, f)
	return image.PruneReport{SpaceReclaimed: 1 << 20}, nil
}

// After a build, the dangling images of Decurix's builds and of their builder stages are pruned, each by its own
// label, and nothing unlabelled (another project's on a shared engine) is asked for (resource plan P1 #10).
func TestPruningTakesTheBuildersStagesToo(t *testing.T) {
	daemon := &prunesRecorded{fakeAPI: &fakeAPI{}}
	m := newManagerWith(daemon)
	reclaimed, err := m.PruneDanglingResources(context.Background())
	if err != nil || reclaimed != 2<<20 {
		t.Fatalf("reclaimed %d, %v", reclaimed, err)
	}
	labels := map[string]bool{}
	for _, f := range daemon.seen {
		if !f.ExactMatch("dangling", "true") {
			t.Errorf("a prune that is not of dangling images: %v", f)
		}
		vals := f.Get("label")
		if len(vals) != 1 {
			t.Fatalf("a prune with labels %v", vals)
		}
		labels[vals[0]] = true
	}
	if !labels[contextLabel] || !labels[buildStageLabel] {
		t.Fatalf("pruned by %v, want %s and %s", labels, contextLabel, buildStageLabel)
	}
}
