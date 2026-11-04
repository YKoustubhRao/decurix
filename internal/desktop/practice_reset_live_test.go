package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// TestPracticeResetLive runs the real practice reset on a throwaway container of a real desktop image (review EVAL-2,
// R2-B1: a reset that killed the worker's processes took the desktop's init with it, and nothing was cleared). Set
// DECURIX_PRACTICE_IMAGE to an installed desktop image (decurix-desktop:basic) to run it; it never builds or pulls,
// and removes its container at the end.
func TestPracticeResetLive(t *testing.T) {
	image := os.Getenv("DECURIX_PRACTICE_IMAGE")
	if image == "" {
		t.Skip("set DECURIX_PRACTICE_IMAGE to an installed desktop image to run the practice reset on a real container")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no Docker here: %v", err)
	}
	m := NewManager(cli)

	memory := int64(1800 << 20)
	name := fmt.Sprintf("decurix-reset-live-%d", time.Now().UnixNano())
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  image,
		Env:    []string{"VNC_PW=reset-live-only", "DESKTOP_ID=reset-live", "DECURIX_AGENT_TOKEN=reset-live-token-0123456789abcdef"},
		Labels: map[string]string{PracticeLabel: "true"},
	}, &container.HostConfig{Resources: container.Resources{Memory: memory, MemorySwap: memory}}, nil, nil, name)
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	defer func() {
		_ = cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	}()
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}

	health := `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8081/health`
	waitHealthy := func(when string) {
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			if out, _ := liveExec(ctx, cli, id, "headless", health); out == "200" {
				return
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("the agent did not answer /health %s", when)
	}
	waitHealthy("after the first start")

	// What a case leaves: a method the agent kept, a work file, soul's job log, and a grant in the gate's store.
	if out, err := liveExec(ctx, cli, id, "headless", `mkdir -p ~/.decurix/methods ~/work/x && echo m > ~/.decurix/methods/m.json && echo w > ~/work/x/out.txt && echo j >> /tmp/thebot-soul.log && echo t > /tmp/decurix-current-task && echo ok`); out != "ok" {
		t.Fatalf("could not plant a case's leftovers: %q %v", out, err)
	}
	gate := "absent"
	if out, _ := liveExec(ctx, cli, id, "root", `[ -d /var/lib/decurix-gate ] && mkdir -p /var/lib/decurix-gate/computer && echo '{}' > /var/lib/decurix-gate/computer/approval.json && echo planted`); out == "planted" {
		gate = "planted"
	}

	keyCheck := "[ -e /var/lib/decurix-gate/key ] && echo key || echo none"
	keyBefore, _ := liveExec(ctx, cli, id, "root", keyCheck)

	var empty bytes.Buffer
	_ = tar.NewWriter(&empty).Close()
	started := time.Now()
	if err := m.ResetPractice(ctx, id, empty.Bytes()); err != nil {
		t.Fatalf("the reset failed on a real desktop: %v", err)
	}
	t.Logf("reset in %s", time.Since(started).Round(time.Millisecond))
	waitHealthy("after the reset")

	for _, path := range []string{"/home/headless/.decurix/methods", "/home/headless/work/x", "/tmp/thebot-soul.log", "/tmp/decurix-current-task"} {
		if out, _ := liveExec(ctx, cli, id, "root", fmt.Sprintf("[ -e %s ] && echo there || echo gone", path)); !strings.HasSuffix(out, "gone") {
			// soul writes its log again at start; it must not hold the earlier case's line.
			if path == "/tmp/thebot-soul.log" {
				if log, _ := liveExec(ctx, cli, id, "root", "grep -cx j /tmp/thebot-soul.log || true"); log == "0" {
					continue
				}
			}
			t.Errorf("%s survived the reset", path)
		}
	}
	if gate == "planted" {
		if out, _ := liveExec(ctx, cli, id, "root", "[ -e /var/lib/decurix-gate/computer/approval.json ] && echo there || echo gone"); out != "gone" {
			t.Error("the gate's store survived the reset")
		}
		if out, _ := liveExec(ctx, cli, id, "root", keyCheck); out != keyBefore {
			t.Errorf("the gate's key changed with the reset: %s before, %s after", keyBefore, out)
		}
	}

	// The next case's worker must own its state folder again: live (LIVE-2 round 3, F-R3-2), ~/.decurix came back
	// an empty root:root 755 folder after a reset, and every case after the first failed on a PermissionError.
	workerOwns := func(when string) {
		t.Helper()
		if out, _ := liveExec(ctx, cli, id, "root", "stat -c '%U:%G %a' /home/headless/.decurix 2>&1"); !strings.HasPrefix(out, "headless:headless ") {
			t.Errorf("%s: ~/.decurix is %q, not the worker's", when, out)
		}
		if out, _ := liveExec(ctx, cli, id, "headless", "mkdir -p ~/.decurix/computer ~/.decurix/recordings && touch ~/.decurix/computer/probe && echo ok"); out != "ok" {
			t.Errorf("%s: the worker cannot write its state folder: %q", when, out)
		}
	}
	workerOwns("after a reset")

	// A case's bundle is pulled before the next reset (runs.Puller through PracticeHome): its reads, most of them of
	// paths a case may not have made, and EXPORTED written back. Then the next case's reset.
	home := PracticeHome{M: m}
	for _, p := range []string{"/home/headless/.decurix/runs/t1", "/home/headless/.decurix/computer/evidence/t1.records.json",
		"/home/headless/.decurix/ai/traces/t1.jsonl", "/home/headless/.decurix/recordings/task-t1/meta.json"} {
		if rc, err := home.ReadTar(ctx, id, p, 1<<20); err == nil {
			rc.Close()
		}
	}
	var exported bytes.Buffer
	tw := tar.NewWriter(&exported)
	for _, dir := range []string{".decurix/", ".decurix/runs/", ".decurix/runs/t1/"} {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o700, ModTime: time.Now()})
	}
	body := []byte("x1\nsha\n\n")
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ".decurix/runs/t1/EXPORTED", Mode: 0o600, Size: int64(len(body)), ModTime: time.Now()})
	_, _ = tw.Write(body)
	_ = tw.Close()
	if err := home.WriteTar(ctx, id, "/home/headless", &exported); err != nil {
		t.Errorf("EXPORTED could not be written back: %v", err)
	}
	if err := m.ResetPractice(ctx, id, empty.Bytes()); err != nil {
		t.Fatalf("the second reset failed: %v", err)
	}
	waitHealthy("after the second reset")
	workerOwns("after a bundle pull and the next reset")
}

// liveExec runs a shell line in a container as user and returns its trimmed standard output.
func liveExec(ctx context.Context, cli *client.Client, id, user, line string) (string, error) {
	exec, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{User: user, Cmd: []string{"/bin/sh", "-c", line}, AttachStdout: true, AttachStderr: true,
		Env: []string{"HOME=/home/headless"}})
	if err != nil {
		return "", err
	}
	attach, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer attach.Close()
	var out, errs bytes.Buffer
	_, err = stdcopy.StdCopy(&out, &errs, attach.Reader)
	return strings.TrimSpace(out.String()), err
}
