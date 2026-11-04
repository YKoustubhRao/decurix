package desktop

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"

	"github.com/YKoustubhRao/decurix"
)

// builder is a daemon that records each image build's target and tags, and each prune, and fails builds when fail.
type builder struct {
	*fakeAPI
	builds []string
	prunes int
	fail   bool
}

func (b *builder) ImageBuild(_ context.Context, ctx io.Reader, opts build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	_, _ = io.Copy(io.Discard, ctx)
	b.builds = append(b.builds, opts.Target+" -> "+strings.Join(opts.Tags, ","))
	if b.fail {
		return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"error":"boom","errorDetail":{"message":"boom"}}`))}, nil
	}
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(`{"stream":"ok\n"}`))}, nil
}

func (b *builder) ImagesPrune(context.Context, filters.Args) (image.PruneReport, error) {
	b.prunes++
	return image.PruneReport{}, nil
}

func (b *builder) ImageTag(context.Context, string, string) error { return nil }

// A Dockerfile's builder stages are built and tagged before the image, so the stage's cache is a tagged image the
// prune never takes; the prune runs after a build that succeeded, never after one that failed (review RES-CP-1).
func TestBuilderStagesAreTaggedAndPrunedOnlyAfterSuccess(t *testing.T) {
	for file, want := range map[string][]string{"docker/desktop.basic.Dockerfile": {"thebot-builder"}, "docker/tools.Dockerfile": {"builder"}} {
		df, err := contextDockerfile(decurix.DesktopBuildContext, file)
		if err != nil {
			t.Fatal(err)
		}
		if got := builderStages(df); !slices.Equal(got, want) {
			t.Errorf("%s: stages %v, want %v", file, got, want)
		}
	}
	if got := stageTag("decurix-desktop:basic", "thebot-builder"); got != "decurix-build-stage:decurix-desktop-basic-thebot-builder" {
		t.Errorf("stage tag %q", got)
	}

	b := &builder{fakeAPI: &fakeAPI{}}
	m := newManagerWith(b)
	if err := m.BuildImages(context.Background(), io.Discard, false, "desktop"); err != nil {
		t.Fatal(err)
	}
	want := []string{"thebot-builder -> " + stageTag(desktopImageBasic, "thebot-builder"), " -> " + desktopImageBasic}
	if !slices.Equal(b.builds, want) {
		t.Fatalf("builds %v, want %v", b.builds, want)
	}
	if b.prunes == 0 {
		t.Fatal("no prune after a build that succeeded")
	}

	failing := &builder{fakeAPI: &fakeAPI{}, fail: true}
	if err := newManagerWith(failing).BuildImages(context.Background(), io.Discard, false, "desktop"); err == nil {
		t.Fatal("a failed build was not an error")
	}
	if failing.prunes != 0 {
		t.Fatalf("%d prunes after a failed build, want none", failing.prunes)
	}
}
