package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// homeDesktop is a fake daemon with one desktop, box, in the given state, whose execs answer as tar would: a read of
// ~/present gives one file, any other path is missing, and an extraction is recorded in wrote.
func homeDesktop(running, paused bool) (*fakeAPI, *[]string) {
	var wrote []string
	fake := &fakeAPI{}
	fake.inspect = func(name string) (container.InspectResponse, error) {
		if name != "desktop_box" {
			return container.InspectResponse{}, errFakeNotFound
		}
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{ID: "cid-box", Image: "sha256:desktopimage",
				State: &container.State{Running: running, Paused: paused}},
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "desktop_box_home", Destination: homeDir},
				{Type: mount.TypeVolume, Name: "decurix-tools", Destination: "/opt/tools"},
			},
			Config: &container.Config{Image: "decurix-desktop:latest"},
		}, nil
	}
	fake.ex.onExec = func(_ string, opts container.ExecOptions, stdin []byte) ([]byte, int) {
		cmd := strings.Join(opts.Cmd, " ")
		switch {
		case strings.Contains(cmd, `[ -e "$1" ]`):
			if strings.HasSuffix(cmd, homeDir+"/present") {
				return nil, 0
			}
			return nil, 1
		case strings.Contains(cmd, "tar -c"):
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			body := []byte("hello")
			_ = tw.WriteHeader(&tar.Header{Name: "present", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
			_, _ = tw.Write(body)
			_ = tw.Close()
			return buf.Bytes(), 0
		case strings.Contains(cmd, "tar -x"):
			tr := tar.NewReader(bytes.NewReader(stdin))
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				wrote = append(wrote, h.Name)
			}
			return nil, 0
		}
		return nil, 2
	}
	return fake, &wrote
}

// assertAsWorker fails when any exec did not run as the worker with an empty environment, or when Docker's root-side
// copy was used at all.
func assertAsWorker(t *testing.T, fake *fakeAPI) {
	t.Helper()
	if c := append(fake.called("CopyFromContainer"), fake.called("CopyToContainer")...); len(c) > 0 {
		t.Errorf("a desktop's home was copied by Docker as root: %v", c)
	}
	execs := fake.called("ContainerExecCreate")
	if len(execs) == 0 {
		t.Fatal("no copy ran at all")
	}
	for _, e := range execs {
		if !strings.HasSuffix(e, "user=1001:1001") || !strings.Contains(e, "[/usr/bin/env -i ") {
			t.Errorf("a home copy ran as someone other than the worker, or with an environment: %s", e)
		}
		// A docker exec's umask is 0000: an extraction without its own made world-writable folders (2026-11-12).
		if strings.Contains(e, "tar -x") && !strings.Contains(e, "umask 022") {
			t.Errorf("an extraction into a home runs without umask 022: %s", e)
		}
	}
}

// TestEveryHomeCopyRunsAsTheWorker: files put into, and read out of, a running desktop's home go through tar run as
// uid 1001 with an empty environment - never Docker's root-side copy, which follows the worker's links (B-01).
func TestEveryHomeCopyRunsAsTheWorker(t *testing.T) {
	fake, wrote := homeDesktop(true, false)
	m := newManagerWith(fake)
	ctx := context.Background()

	if err := m.CopyFiles(ctx, "box", homeDir+"/inbox/up-1", []File{{Name: "a.txt", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(*wrote, ","), "inbox/up-1/a.txt") {
		t.Fatalf("the file was not extracted: %v", *wrote)
	}
	rc, err := m.CopyOut(ctx, "box", homeDir+"/present")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Contains(got, []byte("hello")) {
		t.Fatalf("read back %q, %v", got, err)
	}
	if _, err := m.CopyOut(ctx, "box", homeDir+"/absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing path must be ErrNotFound, got %v", err)
	}
	if err := m.PersistMemoryFile(ctx, "box", "lesson.md", "a lesson\nEOF\nmore"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.PracticeSnapshot(ctx, "box"); err != nil {
		t.Fatalf("practice snapshot: %v", err)
	}
	assertAsWorker(t, fake)
	if c := fake.called("ContainerPause"); len(c) > 0 {
		t.Errorf("a running desktop must not be paused for a copy: %v", c)
	}
	for _, e := range fake.called("ContainerExecCreate") {
		if strings.Contains(e, "kill -s") {
			t.Errorf("a live read must not stop the worker's processes: %s", e)
		}
	}
}

// TestAMemoryFileGoesInAsDataNotAsAScript: the memory text travels as a tar on stdin; the shell command is constant.
func TestAMemoryFileGoesInAsDataNotAsAScript(t *testing.T) {
	fake, wrote := homeDesktop(true, false)
	m := newManagerWith(fake)
	text := "line one\nDECURIX_MEMORY_1\n$(true)"
	if err := m.PersistMemoryFile(context.Background(), "box", "lesson.md", text); err != nil {
		t.Fatal(err)
	}
	for _, e := range fake.called("ContainerExecCreate") {
		if strings.Contains(e, "line one") || strings.Contains(e, "lesson.md") {
			t.Fatalf("the memory text or name reached the command line: %s", e)
		}
	}
	if len(*wrote) != 1 || (*wrote)[0] != "lesson.md" {
		t.Fatalf("wrote %v", *wrote)
	}
	if err := m.PersistMemoryFile(context.Background(), "box", "../x.md", "y"); err == nil {
		t.Fatal("a name with a folder in it must be refused")
	}
}

// TestAPausedDesktopIsResumedForACopyAndPausedAgain.
func TestAPausedDesktopIsResumedForACopyAndPausedAgain(t *testing.T) {
	fake, _ := homeDesktop(true, true)
	m := newManagerWith(fake)
	if err := m.CopyFiles(context.Background(), "box", homeDir+"/inbox", []File{{Name: "a", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if len(fake.called("ContainerUnpause")) != 1 || len(fake.called("ContainerPause")) != 1 {
		t.Fatalf("want one unpause and one pause: %v", fake.calls)
	}
	assertAsWorker(t, fake)
}

// TestAStoppedDesktopsHomeIsReachedThroughALockedDownHelper: a throwaway container from the same image with only the
// home volume, as the worker, no network, no capabilities, no new privileges, read-only root - removed afterwards.
func TestAStoppedDesktopsHomeIsReachedThroughALockedDownHelper(t *testing.T) {
	fake, wrote := homeDesktop(false, false)
	var cfg *container.Config
	var host *container.HostConfig
	fake.create = func(name string, c *container.Config, h *container.HostConfig) (container.CreateResponse, error) {
		cfg, host = c, h
		return container.CreateResponse{ID: "helper-1"}, nil
	}
	m := newManagerWith(fake)
	if err := m.CopyFiles(context.Background(), "box", homeDir+"/inbox", []File{{Name: "a", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if len(*wrote) == 0 {
		t.Fatal("nothing was written")
	}
	if cfg == nil || host == nil {
		t.Fatal("no helper container was made")
	}
	if cfg.Image != "sha256:desktopimage" || cfg.User != "1001:1001" || len(cfg.Env) != 0 {
		t.Errorf("helper config: image %q user %q env %v", cfg.Image, cfg.User, cfg.Env)
	}
	if host.NetworkMode != "none" || !host.ReadonlyRootfs || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" ||
		len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" || host.Privileged {
		t.Errorf("helper host config is not locked down: %+v", host)
	}
	if len(host.Mounts) != 1 || host.Mounts[0].Source != "desktop_box_home" || host.Mounts[0].Target != homeDir ||
		len(host.Binds) != 0 {
		t.Errorf("helper must mount the home volume and nothing else: %+v %v", host.Mounts, host.Binds)
	}
	if c := fake.called("ContainerRemove helper-1"); len(c) != 1 {
		t.Errorf("the helper must be removed after the copy: %v", fake.calls)
	}
	for _, e := range fake.called("ContainerExecCreate") {
		if !strings.HasPrefix(e, "ContainerExecCreate helper-1 ") {
			t.Errorf("a stopped desktop is reached only through its helper: %s", e)
		}
	}
	assertAsWorker(t, fake)
}

// TestAHomeReadStopsAtItsLimit: a read past its limit ends in an error, never a cut-short archive taken as whole.
func TestAHomeReadStopsAtItsLimit(t *testing.T) {
	fake, _ := homeDesktop(true, false)
	m := newManagerWith(fake)
	rc, err := m.ReadTar(context.Background(), "box", homeDir+"/present", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("a read over its limit must fail")
	}
}

// TestOnlyPathsUnderTheHomeAreCopied: the layer is for the worker's home and nothing else.
func TestOnlyPathsUnderTheHomeAreCopied(t *testing.T) {
	m := newManagerWith(&fakeAPI{})
	for _, p := range []string{"/", "/etc/passwd", homeDir, homeDir + "/../root", "/var/lib/decurix-gate/key"} {
		if _, err := m.ReadTar(context.Background(), "box", p, 0); !errors.Is(err, errHomePath) {
			t.Errorf("ReadTar(%q) = %v, want refused", p, err)
		}
	}
	if err := m.WriteTar(context.Background(), "box", "/etc", strings.NewReader("")); !errors.Is(err, errHomePath) {
		t.Errorf("WriteTar outside the home must be refused: %v", err)
	}
}

// A path not there is told from a desktop not there, and still reads as not found to every older caller.
func TestAMissingPathIsStillNotFound(t *testing.T) {
	if !errors.Is(ErrPathNotFound, ErrNotFound) || errors.Is(ErrNotFound, ErrPathNotFound) {
		t.Fatal("ErrPathNotFound must wrap ErrNotFound, and not the other way round")
	}
}
