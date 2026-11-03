package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeAPI stands in for the Docker daemon in tests that need to see every call the Manager makes. Each method a
// test does not script answers as an empty daemon would; a method it does not implement at all panics through the
// nil embedded interface, which shows at once what a change started calling.
type fakeAPI struct {
	client.APIClient

	mu    sync.Mutex
	calls []string

	inspect      func(name string) (container.InspectResponse, error)
	create       func(name string, cfg *container.Config, host *container.HostConfig) (container.CreateResponse, error)
	volumeCreate func(opts volume.CreateOptions) (volume.Volume, error)
	volumeGet    func(name string) (volume.Volume, error)
	copyFrom     func(name, path string) (io.ReadCloser, container.PathStat, error)
	copyTo       func(name, path string, content io.Reader) error
	execCreate   func(name string, opts container.ExecOptions) (container.ExecCreateResponse, error)
}

func (f *fakeAPI) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// called lists the calls whose text starts with prefix.
func (f *fakeAPI) called(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

var errFakeNotFound = errdefs.NotFound(errors.New("no such object"))

func (f *fakeAPI) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	f.record("ContainerInspect %s", name)
	if f.inspect != nil {
		return f.inspect(name)
	}
	return container.InspectResponse{}, errFakeNotFound
}

func (f *fakeAPI) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig,
	_ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.record("ContainerCreate %s", name)
	if f.create != nil {
		return f.create(name, cfg, host)
	}
	return container.CreateResponse{ID: "id-" + name}, nil
}

func (f *fakeAPI) ContainerRemove(_ context.Context, name string, _ container.RemoveOptions) error {
	f.record("ContainerRemove %s", name)
	return nil
}

func (f *fakeAPI) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	f.record("VolumeCreate %s", opts.Name)
	if f.volumeCreate != nil {
		return f.volumeCreate(opts)
	}
	return volume.Volume{Name: opts.Name, Labels: opts.Labels}, nil
}

func (f *fakeAPI) VolumeInspect(_ context.Context, name string) (volume.Volume, error) {
	f.record("VolumeInspect %s", name)
	if f.volumeGet != nil {
		return f.volumeGet(name)
	}
	return volume.Volume{}, errFakeNotFound
}

func (f *fakeAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.record("VolumeRemove %s", name)
	return nil
}

func (f *fakeAPI) CopyFromContainer(_ context.Context, name, path string) (io.ReadCloser, container.PathStat, error) {
	f.record("CopyFromContainer %s %s", name, path)
	if f.copyFrom != nil {
		return f.copyFrom(name, path)
	}
	return nil, container.PathStat{}, errFakeNotFound
}

func (f *fakeAPI) CopyToContainer(_ context.Context, name, path string, content io.Reader, _ container.CopyToContainerOptions) error {
	f.record("CopyToContainer %s %s", name, path)
	if f.copyTo != nil {
		return f.copyTo(name, path, content)
	}
	return nil
}
