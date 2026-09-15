package desktop

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/docker/docker/api/types/build"

	"github.com/YKoustubhRao/flowera"
)

// desktopImage pairs an image tag with the Dockerfile that produces it.
type desktopImage struct {
	Tag        string
	Dockerfile string
}

// desktopImages lists every image a desktop can be created from, in the order
// they are built (cheapest first, so a failure surfaces quickly).
var desktopImages = []desktopImage{
	{Tag: desktopImageBasic, Dockerfile: "docker/desktop.basic.Dockerfile"},
	{Tag: desktopImageDeveloper, Dockerfile: "docker/desktop.developer.Dockerfile"},
	{Tag: desktopImageAI, Dockerfile: "docker/desktop.ai.Dockerfile"},
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

// BuildImages builds every desktop image from the build context embedded in
// the binary, streaming the daemon's progress to out. Images that already
// exist are rebuilt, which is cheap because Docker's layer cache still
// applies; pass onlyMissing to skip them entirely.
func (m *Manager) BuildImages(ctx context.Context, out io.Writer, onlyMissing bool) error {
	for _, img := range desktopImages {
		if onlyMissing {
			if _, _, err := m.client.ImageInspectWithRaw(ctx, img.Tag); err == nil {
				fmt.Fprintf(out, "==> %s already exists, skipping\n", img.Tag)
				continue
			}
		}

		fmt.Fprintf(out, "==> Building %s from %s\n", img.Tag, img.Dockerfile)

		if err := m.buildImage(ctx, out, img); err != nil {
			return fmt.Errorf("build %s: %w", img.Tag, err)
		}

		fmt.Fprintf(out, "==> %s ready\n", img.Tag)
	}

	return nil
}

// buildImage streams the embedded build context to the daemon and reports the
// resulting build output.
func (m *Manager) buildImage(ctx context.Context, out io.Writer, img desktopImage) error {
	// The daemon consumes the context as it builds, so produce the tar on a
	// pipe rather than buffering the whole agent tree in memory.
	reader, writer := io.Pipe()

	go func() {
		writer.CloseWithError(writeBuildContext(writer))
	}()

	defer reader.Close()

	resp, err := m.client.ImageBuild(ctx, reader, build.ImageBuildOptions{
		Platform:   "linux/amd64",
		Tags:       []string{img.Tag},
		Dockerfile: img.Dockerfile,
		Remove:     true,
	})
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return streamBuildOutput(out, resp.Body)
}

// writeBuildContext tars the embedded build context into w, preserving the
// paths the Dockerfiles expect.
func writeBuildContext(w io.Writer) error {
	tw := tar.NewWriter(w)

	err := fs.WalkDir(flowera.DesktopBuildContext, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || path == "." {
			return nil
		}

		data, err := flowera.DesktopBuildContext.ReadFile(path)
		if err != nil {
			return err
		}

		// embed.FS carries no mode or mtime, so synthesize both. The
		// Dockerfiles chmod +x whatever needs it, so 0644 is enough.
		header := &tar.Header{
			Name:    path,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Unix(0, 0),
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		_, err = tw.Write(data)

		return err
	})
	if err != nil {
		return err
	}

	return tw.Close()
}

// streamBuildOutput decodes the daemon's JSON build stream, echoing progress
// to out and surfacing any build error as a Go error.
func streamBuildOutput(out io.Writer, body io.Reader) error {
	decoder := json.NewDecoder(body)

	for {
		var msg struct {
			Stream      string `json:"stream"`
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
			return fmt.Errorf("%s", firstNonEmpty(msg.ErrorDetail.Message, msg.Error))
		}

		if msg.Stream != "" {
			fmt.Fprint(out, msg.Stream)
		}
	}
}

// firstNonEmpty returns the first non-empty string of the two.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
