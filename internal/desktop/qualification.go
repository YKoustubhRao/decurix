package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/YKoustubhRao/decurix/internal/qualification"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
)

// QualificationImage resolves an existing desktop's actual immutable image ID.
// It never builds, pulls, copies a home, or seeds credentials into a practice run.
func (m *Manager) QualificationImage(ctx context.Context, workerID string) (string, error) {
	found, err := m.Get(ctx, workerID)
	if err != nil {
		return "", err
	}
	inspect, err := m.client.ContainerInspect(ctx, found.ContainerID)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(inspect.Image, "sha256:") {
		return "", fmt.Errorf("qualification requires an immutable local image")
	}
	return inspect.Image, nil
}

// qualificationConfigs cannot receive desktop env, host mounts or credentials.
// Both practice and enabled local-artifact runs use this same external boundary.
func qualificationConfigs(d qualification.Definition) (*container.Config, *container.HostConfig) {
	return &container.Config{
			Image: d.Image, User: "1001:1001", Entrypoint: []string{"/usr/bin/sleep"}, Cmd: []string{"infinity"}, WorkingDir: "/work",
			Env:    []string{"HOME=/home/headless", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "DECURIX_OUTBOX_DRY=1"},
			Labels: map[string]string{"decurix-qualification": "true"},
		}, &container.HostConfig{
			NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			Tmpfs:     map[string]string{"/work": "rw,nosuid,nodev,size=64m,uid=1001,gid=1001,mode=0700", "/tmp": "rw,nosuid,nodev,size=64m,mode=1777", "/home/headless": "rw,nosuid,nodev,size=32m,uid=1001,gid=1001,mode=0700"},
			Resources: container.Resources{Memory: 1 << 30, MemorySwap: 1 << 30, NanoCPUs: 1e9, Ulimits: desktopUlimits(), PidsLimit: func() *int64 { v := int64(64); return &v }()},
		}
}

// CleanupQualification removes only containers belonging to this offline runner.
// No interrupted run is replayed automatically after a control-plane restart.
func (m *Manager) CleanupQualification(ctx context.Context) error {
	list, err := m.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "decurix-qualification=true"))})
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := m.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return err
		}
	}
	return nil
}

// RunQualification executes a frozen program in a disposable offline container.
// The case's checks and expected answers are intentionally never copied into it.
func (m *Manager) RunQualification(ctx context.Context, d qualification.Definition, c qualification.Case) (result qualification.Result, err error) {
	if qualification.ValidateCase(c) != nil || d.Runtime != qualification.RuntimeVersion {
		return result, qualification.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.TimeoutSeconds)*time.Second)
	defer cancel()
	started := time.Now()
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	config, host := qualificationConfigs(d)
	// Docker images can declare VOLUME paths. An inherited anonymous volume
	// would defeat our mount inventory, so reject such images rather than guess.
	image, _, err := m.client.ImageInspectWithRaw(ctx, d.Image)
	if err != nil {
		return result, err
	}
	if image.Config == nil || len(image.Config.Volumes) != 0 {
		return result, fmt.Errorf("qualification image declares unexpected volumes")
	}
	if image.Os != "linux" {
		return result, fmt.Errorf("qualification requires a Linux image")
	}
	// ContainerCreate merges image ENV with configured ENV. Override every
	// inherited key so even /proc/1/environ cannot reveal image configuration.
	config.Env = qualificationEnvironment(image.Config.Env)
	if image.ID != d.Image {
		return result, fmt.Errorf("qualification image changed")
	}
	created, err := m.client.ContainerCreate(ctx, config, host, nil, nil, "")
	if err != nil {
		return result, err
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if e := m.client.ContainerRemove(clean, created.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); e != nil && err == nil {
			err = e
		}
	}()
	if err = m.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return result, err
	}
	program, interpreter := "main.py", "/usr/bin/python3"
	if d.Language == "bash" {
		program, interpreter = "main.sh", "/bin/bash"
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	files := map[string][]byte{program: []byte(d.Script)}
	for n, b := range c.Inputs {
		files[n] = b
	}
	for n, b := range files {
		if e := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0600, Uid: 1001, Gid: 1001, Size: int64(len(b))}); e != nil {
			return result, e
		}
		if _, e := tw.Write(b); e != nil {
			return result, e
		}
	}
	if err = tw.Close(); err != nil {
		return result, err
	}
	if err = m.execTarIn(ctx, created.ID, "/work", &buf); err != nil {
		return result, err
	}
	// env -i discards every image ENV as well as the caller's environment.
	cmd := []string{"/usr/bin/env", "-i", "HOME=/home/headless", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONPATH=/opt", "DECURIX_OUTBOX_DRY=1", interpreter, "/work/" + program}
	cmd = append(cmd, c.Args...)
	execID, err := m.client.ContainerExecCreate(ctx, created.ID, container.ExecOptions{User: "1001:1001", WorkingDir: "/work", Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return result, err
	}
	attach, err := m.client.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return result, err
	}
	defer attach.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attach.Close()
		case <-done:
		}
	}()
	out := &limitedOutput{}
	_, copyErr := stdcopy.StdCopy(out, out, attach.Reader)
	close(done)
	result.Output = out.String()
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return result, copyErr
	}
	state, err := m.client.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return result, err
	}
	if state.Running {
		return result, fmt.Errorf("program still running")
	}
	result.ExitCode = state.ExitCode
	// execTarOut stops every subprocess before reading, preventing post-exit
	// writes and races where the agent shows one file to a checker and then changes it.
	archive, err := m.execTarOut(ctx, created.ID, "/work", qualification.MaxTotalBytes+1<<20)
	if err != nil {
		return result, err
	}
	result.Artifacts, err = qualificationArtifacts(bytes.NewReader(archive), program, c.Inputs)
	return result, err
}

func qualificationEnvironment(inherited []string) []string {
	out := []string{}
	for _, item := range inherited {
		key, _, ok := strings.Cut(item, "=")
		if ok && key != "HOME" && key != "PATH" && key != "LANG" && key != "DECURIX_OUTBOX_DRY" {
			out = append(out, key+"=")
		}
	}
	return append(out, "HOME=/home/headless", "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "DECURIX_OUTBOX_DRY=1")
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 8192 {
		remaining := 8192 - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func qualificationArtifacts(r io.Reader, program string, inputs map[string][]byte) (map[string][]byte, error) {
	tr := tar.NewReader(r)
	out := map[string][]byte{}
	seen := map[string]bool{}
	total := int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimPrefix(h.Name, "work/")
		if h.Typeflag == tar.TypeDir && strings.TrimSuffix(h.Name, "/") == "work" {
			continue
		}
		if h.Typeflag != tar.TypeReg || path.Base(name) != name || seen[name] || h.Size < 0 || h.Size > qualification.MaxFileBytes {
			return nil, fmt.Errorf("artifact violates flat regular file policy")
		}
		seen[name] = true
		total += h.Size
		if total > qualification.MaxTotalBytes+128<<10 {
			return nil, fmt.Errorf("artifact extraction exceeded limits")
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil {
			return nil, err
		}
		if name == program {
			continue
		}
		if input, ok := inputs[name]; ok {
			if !bytes.Equal(input, b) {
				return nil, fmt.Errorf("an input file was modified")
			}
			continue
		}
		if !qualification.FileName(name) {
			return nil, fmt.Errorf("invalid artifact name %q", name)
		}
		out[name] = b
	}
	for n := range inputs {
		if !seen[n] {
			return nil, fmt.Errorf("an input file was removed")
		}
	}
	if !seen[program] {
		return nil, fmt.Errorf("frozen program was removed")
	}
	return out, nil
}
