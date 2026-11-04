package desktop

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"

	"github.com/YKoustubhRao/decurix"
)

// desktopImage pairs an image tag with the Dockerfile that produces it.
type desktopImage struct {
	Tag        string
	Dockerfile string
}

// desktopImages lists the images built by Decurix: the unified base desktop
// and the shared tools runtime image.
var desktopImages = []desktopImage{
	{Tag: desktopImageBasic, Dockerfile: "docker/desktop.basic.Dockerfile"},
	{Tag: ToolsImage, Dockerfile: "docker/tools.Dockerfile"},
}

// MissingImages returns the desktop image tags that are not present locally.
func (m *Manager) MissingImages(ctx context.Context) []string {

	var missing []string

	for _, img := range desktopImages {
		if _, _, err := m.client.ImageInspectWithRaw(ctx, img.Tag); err != nil {
			missing = append(missing, img.Tag)
		}
	}

	return missing
}

// BuildImages builds the unified desktop and shared tools images from the build
// context embedded in the binary, streaming the daemon's progress to out.
// It also tags the base image for developer and ai profiles, and populates
// the shared decurix-tools volume. which is "desktop" (the desktop image only),
// "tools" (the tools image only) or "all": a desktop works without the tools
// image (its tools volume is filled once the image exists), so the app builds
// the desktop first and the tools after its dashboard is up.
func (m *Manager) BuildImages(ctx context.Context, out io.Writer, onlyMissing bool, which string) error {
	switch which {
	case "all", "desktop", "tools":
	default:
		return fmt.Errorf("images: %q is not desktop, tools or all", which)
	}

	for _, img := range desktopImages {
		if isTools := img.Tag == ToolsImage; (which == "desktop" && isTools) || (which == "tools" && !isTools) {
			continue
		}

		hash, err := contextHash(decurix.DesktopBuildContext, img)
		if err != nil {
			return fmt.Errorf("build %s: %w", img.Tag, err)
		}

		if onlyMissing {
			if inspect, _, err := m.client.ImageInspectWithRaw(ctx, img.Tag); err == nil {
				if inspect.Config != nil && inspect.Config.Labels[contextLabel] == hash {
					fmt.Fprintf(out, "==> %s is up to date, skipping\n", img.Tag)
					continue
				}

				fmt.Fprintf(out, "==> %s was built from other sources than this decurix carries\n", img.Tag)
			}
		}

		fmt.Fprintf(out, "==> Building %s from %s\n", img.Tag, img.Dockerfile)

		if err := m.buildImage(ctx, out, img, hash); err != nil {
			return fmt.Errorf("build %s: %w", img.Tag, err)
		}

		fmt.Fprintf(out, "==> %s ready\n", img.Tag)
	}

	// Tag the base desktop image as developer and ai for backwards compatibility.
	_ = m.client.ImageTag(ctx, desktopImageBasic, desktopImageDeveloper)
	_ = m.client.ImageTag(ctx, desktopImageBasic, desktopImageAI)

	// Prune what the builds superseded - only after every build asked for succeeded (a failed one keeps all it had),
	// and after the desktop build too, the one the app's every start runs (resource plan P1 #10, review RES-CP-1).
	// The current builder stages are tagged (buildImage), so their cache is kept; only older copies go.
	if reclaimed, err := m.PruneDanglingResources(ctx); err == nil && reclaimed > 0 {
		fmt.Fprintf(out, "==> Pruned superseded build layers (reclaimed %d MB)\n", reclaimed/(1024*1024))
	}

	if which == "desktop" {
		return nil
	}

	// Ensure the shared tools volume is populated from the tools image, under the tools lock so it never overlaps a
	// desktop creation's populate (RW1-G-04).
	if err := m.populateToolsLocked(ctx); err != nil {
		fmt.Fprintf(out, "==> warning: populate tools volume: %v\n", err)
	}

	return nil
}

// PruneDanglingResources prunes the untagged image layers Decurix's own builds left behind, reclaiming disk space
// without affecting any active desktop image - and never another project's dangling images on a Docker engine Decurix
// shares (L-02): only images carrying contextLabel, which every desktop and tools image Decurix builds has.
func (m *Manager) PruneDanglingResources(ctx context.Context) (uint64, error) {

	var reclaimed uint64
	// The dangling images of Decurix's own builds, and of their builder stages (a Go toolchain, a compiler suite:
	// gigabytes the final images do not carry), each by its own label. The current stages are tagged
	// (stageTag), so only superseded copies are dangling: the next build still has its cache.
	for _, f := range []filters.Args{pruneFilter(), stagePruneFilter()} {
		report, err := m.client.ImagesPrune(ctx, f)
		if err != nil {
			return reclaimed, fmt.Errorf("prune dangling images: %w", err)
		}
		reclaimed += report.SpaceReclaimed
	}
	return reclaimed, nil
}

// buildStageLabel marks the images a builder stage of Decurix's Dockerfiles leaves (LABEL in the stage).
const buildStageLabel = "decurix.build-stage"

// stagePruneFilter selects the dangling images Decurix's builder stages left behind.
func stagePruneFilter() filters.Args {
	f := filters.NewArgs()
	f.Add("dangling", "true")
	f.Add("label", buildStageLabel)
	return f
}

// buildImage builds one image from the embedded build context: first each builder stage of its Dockerfile, tagged
// (stageTag) so the stage's cache is a tagged image and never pruned as dangling, then the image itself.
func (m *Manager) buildImage(ctx context.Context, out io.Writer, img desktopImage, hash string) error {
	if _, err := fs.Stat(decurix.DesktopBuildContext, decurix.TheBotSource); err != nil {
		return errNoTheBotSource
	}
	dockerfile, err := contextDockerfile(decurix.DesktopBuildContext, img.Dockerfile)
	if err != nil {
		return err
	}
	for _, stage := range builderStages(dockerfile) {
		fmt.Fprintf(out, "==> Building stage %s of %s\n", stage, img.Tag)
		if err := m.buildContext(ctx, out, img.Dockerfile, build.ImageBuildOptions{
			Target: stage,
			Tags:   []string{stageTag(img.Tag, stage)},
			Labels: map[string]string{buildStageLabel: "1"},
		}); err != nil {
			return fmt.Errorf("stage %s: %w", stage, err)
		}
	}
	return m.buildContext(ctx, out, img.Dockerfile, build.ImageBuildOptions{
		Tags:   []string{img.Tag},
		Labels: map[string]string{contextLabel: hash},
	})
}

// buildContext streams the embedded build context to the daemon for one build (opts' target, tags and labels) and
// reports its output.
func (m *Manager) buildContext(ctx context.Context, out io.Writer, dockerfile string, opts build.ImageBuildOptions) error {

	// The daemon consumes the context as it builds, so produce the tar on a
	// pipe rather than buffering the whole agent tree in memory.
	reader, writer := io.Pipe()

	go func() {
		writer.CloseWithError(tarBuildContext(writer, decurix.DesktopBuildContext))
	}()

	defer reader.Close()

	opts.Platform, opts.Dockerfile, opts.Remove = "linux/amd64", dockerfile, true
	resp, err := m.client.ImageBuild(ctx, reader, opts)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return streamBuildOutput(out, resp.Body)
}

// contextLabel is the image label holding contextHash of what the image was
// built from. "Skip what exists" alone kept every image an earlier binary had
// built: a new app reused yesterday's desktop image, agent and all, and went on
// creating desktops with code it no longer carried. An image whose label differs
// is built again; the daemon's layer cache keeps the unchanged steps.
const contextLabel = "decurix.context"

// contextHash is a SHA-256 over the files img is built from: the tools image
// reads only its own Dockerfile; a desktop image reads the whole context except
// the tools Dockerfile. Paths are part of the hash, in WalkDir's lexical order.
func contextHash(fsys fs.FS, img desktopImage) (string, error) {
	h := sha256.New()

	if img.Tag == ToolsImage {
		data, err := fs.ReadFile(fsys, img.Dockerfile)
		if err != nil {
			return "", err
		}

		h.Write(data)

		return hex.EncodeToString(h.Sum(nil)), nil
	}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		if strings.HasPrefix(p, "docker/") && strings.HasSuffix(p, ".Dockerfile") && p != img.Dockerfile {
			return nil
		}

		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		fmt.Fprintf(h, "%s|%d|", p, len(data))
		h.Write(data)

		return nil
	})
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// errNoTheBotSource is a binary built without `go generate .`: go:embed left
// agent/thebot out (it is a Go module of its own), and nothing put it back.
var errNoTheBotSource = errors.New("this decurix was built without TheBot's sources, so it cannot build the desktop images: run `go generate .` at the repository root, then build it again")

// tarBuildContext tars every file of fsys into w under its own path, except
// decurix.TheBotSource, whose files are written under decurix.TheBotDir -
// where they are in the repository, and where the Dockerfiles COPY them from.
// contextDockerfile is a Dockerfile as the build context carries it (tarBuildContext): with the prebuilt soul in
// place of its TheBot build stage when there is one.
func contextDockerfile(fsys fs.FS, path string) (string, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return "", err
	}
	if _, statErr := fs.Stat(fsys, decurix.TheBotSoul); statErr == nil {
		return prebuiltSoul(string(data)), nil
	}
	return string(data), nil
}

// builderStages is the names of a Dockerfile's named stages but its last (the image): the ones built and tagged on
// their own (buildImage).
func builderStages(dockerfile string) []string {
	var stages []string
	var last string
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		if last != "" {
			stages = append(stages, last)
		}
		last = ""
		if len(fields) == 4 && strings.EqualFold(fields[2], "AS") {
			last = fields[3]
		}
	}
	return stages
}

// stageTag is the tag a builder stage of image is kept under: decurix-build-stage:<image>-<stage>.
func stageTag(image, stage string) string {
	name := strings.NewReplacer(":", "-", "/", "-").Replace(image) + "-" + stage
	if len(name) > 120 {
		name = name[:120]
	}
	return "decurix-build-stage:" + name
}

func tarBuildContext(w io.Writer, fsys fs.FS) error {
	tw := tar.NewWriter(w)

	_, statErr := fs.Stat(fsys, decurix.TheBotSoul)
	prebuilt := statErr == nil

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || p == "." {
			return nil
		}

		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		if p == decurix.TheBotSource {
			return unpackInto(tw, data, decurix.TheBotDir)
		}

		if prebuilt && strings.HasPrefix(p, "docker/") && strings.HasSuffix(p, ".Dockerfile") {
			data = []byte(prebuiltSoul(string(data)))
		}

		return writeContextFile(tw, p, data)
	})
	if err != nil {
		return err
	}

	return tw.Close()
}

// prebuiltSoul is a desktop Dockerfile with its TheBot build stage (the
// `FROM golang... AS <name>` stage, up to the next FROM) replaced by a stage
// that copies the soul compiled by `go generate .` (decurix.TheBotSoul): a
// first build of the desktop image then pulls no Go image (~900 MB), downloads
// no modules and compiles nothing. The stage keeps its name, so the
// Dockerfile's `COPY --from=` is untouched. A Dockerfile with no such stage
// is returned as it is.
func prebuiltSoul(dockerfile string) string {
	lines := strings.Split(dockerfile, "\n")

	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 4 || !strings.EqualFold(fields[0], "FROM") || !strings.HasPrefix(fields[1], "golang:") || !strings.EqualFold(fields[2], "AS") {
			continue
		}

		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if f := strings.Fields(lines[j]); len(f) > 0 && strings.EqualFold(f[0], "FROM") {
				end = j
				break
			}
		}

		stage := []string{"FROM scratch AS " + fields[3], "COPY " + decurix.TheBotSoul + " /soul", ""}
		return strings.Join(append(append(append([]string{}, lines[:i]...), stage...), lines[end:]...), "\n")
	}

	return dockerfile
}

// writeContextFile adds one file to the build context.
func writeContextFile(tw *tar.Writer, name string, data []byte) error {
	// embed.FS carries no mode or mtime, so synthesize both. The
	// Dockerfiles chmod +x whatever needs it, so 0644 is enough.
	header := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(data)),
		ModTime: time.Unix(0, 0),
	}

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err := tw.Write(data)

	return err
}

// unpackInto adds the regular files of a .tar.gz to the build context under
// dir. A name that would leave dir is refused.
func unpackInto(tw *tar.Writer, archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("%s: %w", decurix.TheBotSource, err)
	}

	tr := tar.NewReader(zr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("%s: %w", decurix.TheBotSource, err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		if !fs.ValidPath(header.Name) {
			return fmt.Errorf("%s: unsafe path %q", decurix.TheBotSource, header.Name)
		}

		data, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("%s: %w", decurix.TheBotSource, err)
		}

		if err := writeContextFile(tw, path.Join(dir, header.Name), data); err != nil {
			return err
		}
	}
}

// streamBuildOutput decodes the daemon's JSON build stream, echoing progress
// to out and surfacing any build error as a Go error. A base image the build
// pulls is shown too: each layer's own events (a download or pull complete),
// and, at most every pullProgressEvery while bytes keep coming, how much has
// come. Without them a large pull printed nothing for many minutes, which the
// app's stall watchdog (desktop/electron/build-watchdog.js) must tell from a
// download that stopped.
func streamBuildOutput(out io.Writer, body io.Reader) error {
	decoder := json.NewDecoder(body)
	layers := map[string][2]int64{} // layer id -> bytes come, bytes in all
	var shown int64 = -1
	lastShown := buildClock()

	for {
		var msg struct {
			Stream         string `json:"stream"`
			Status         string `json:"status"`
			ID             string `json:"id"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}

		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}

		if msg.Error != "" {
			if msg.ErrorDetail.Message != "" {
				return fmt.Errorf("%s", msg.ErrorDetail.Message)
			}
			return fmt.Errorf("%s", msg.Error)
		}

		if msg.Stream != "" {
			fmt.Fprint(out, msg.Stream)
		}

		if msg.Status == "" {
			continue
		}
		if msg.ProgressDetail.Total > 0 && msg.ID != "" {
			layers[msg.ID] = [2]int64{msg.ProgressDetail.Current, msg.ProgressDetail.Total}
			var come, all int64
			for _, l := range layers {
				come, all = come+l[0], all+l[1]
			}
			if now := buildClock(); come != shown && now.Sub(lastShown) >= pullProgressEvery {
				fmt.Fprintf(out, "Pulling the base image: %d of %d MB\n", come>>20, all>>20)
				shown, lastShown = come, now
			}
			continue
		}
		// A layer's or the pull's own event, without a progress bar: one line each.
		if msg.ID != "" {
			fmt.Fprintf(out, "%s: %s\n", msg.ID, msg.Status)
		} else {
			fmt.Fprintln(out, msg.Status)
		}
	}
}

// pullProgressEvery is how often, at most, a pull's bytes are reported while they keep coming.
var pullProgressEvery = 30 * time.Second

// buildClock is the clock streamBuildOutput reports a pull's progress by (a test stands one in).
var buildClock = time.Now

// pruneFilter selects what PruneDanglingResources removes: dangling images Decurix built.
func pruneFilter() filters.Args {
	f := filters.NewArgs()
	f.Add("dangling", "true")
	f.Add("label", contextLabel)
	return f
}
