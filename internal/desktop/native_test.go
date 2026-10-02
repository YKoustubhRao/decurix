package desktop

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nativeForTest is a native Manager whose workers live in a temp folder. It
// has no helper: these tests are about what a worker is on disk, which needs
// no private desktop.
func nativeForTest(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("DECURIX_CONTROL_PLANE_URL", "")

	return NewNativeManager(NativeRuntime{
		Root:            t.TempDir(),
		Python:          filepath.Join(testRuntime, "python", "pythonw.exe"),
		AgentDir:        filepath.Join(testRuntime, "agent"),
		Soul:            filepath.Join(testRuntime, "soul.exe"),
		UIHelper:        filepath.Join(testRuntime, "decurix-ui.exe"),
		ControlPlaneURL: "http://127.0.0.1:8080",
	})
}

// testRuntime is where the shipped runtime is said to be in these tests. Its
// paths are joined with this system's separator: the tests run on Linux too,
// where a C:\ path is one file name.
var testRuntime = filepath.Join(os.TempDir(), "decurix-runtime")

func writeWorker(t *testing.T, m *Manager, meta nativeMeta) {
	t.Helper()
	if err := m.native.writeMeta(meta); err != nil {
		t.Fatal(err)
	}
}

func TestANativeWorkerIsListedFromItsRecord(t *testing.T) {
	m := nativeForTest(t)
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada", Profile: "basic", Harness: HarnessTheBot, Brain: BrainClaude, Model: "claude-opus-5-5", CreatedAt: "2026-10-01T10:00:00Z", Specialization: "invoices"})
	writeWorker(t, m, nativeMeta{ID: "bo", Name: "Bo", Harness: HarnessTheBot, Brain: BrainOllama, CreatedAt: "2026-10-01T09:00:00Z", Stopped: true})
	// A folder that is not a worker is not listed as one.
	if err := os.MkdirAll(filepath.Join(m.native.rt.Root, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}

	list, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "bo" || list[1].ID != "ada" {
		t.Fatalf("listed %+v, want bo then ada (oldest first)", list)
	}

	ada := list[1]
	if ada.Name != "Ada" || ada.Brain != BrainClaude || ada.Model != "claude-opus-5-5" || ada.Specialization != "invoices" {
		t.Errorf("ada = %+v", ada)
	}
	if ada.Status != "stopped" || ada.URL != "/desktop/ada/" {
		t.Errorf("a worker with no desktop open is %q at %q", ada.Status, ada.URL)
	}

	got, err := m.Get(context.Background(), "Ada")
	if err != nil || got.ID != "ada" {
		t.Errorf("Get(Ada) = %+v, %v", got, err)
	}
	if _, err := m.Get(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a worker that is not there = %v, want ErrNotFound", err)
	}
	if !m.Native() || m.UsesLocalModel(context.Background()) != true {
		t.Errorf("Native() = %v, UsesLocalModel() = %v (bo is on the Ollama brain)", m.Native(), m.UsesLocalModel(context.Background()))
	}
}

func TestANativeWorkersEnvironmentIsItsOwnFoldersAndPorts(t *testing.T) {
	m := nativeForTest(t)
	meta := nativeMeta{ID: "ada", Name: "Ada", Harness: HarnessTheBot, Brain: BrainAzure, BrainModel: "gpt-5.4", BrainVision: true}

	env := m.native.env(meta, 51001, 51002, 51003)
	home := filepath.Join(m.native.rt.Root, "ada", "home")

	for name, want := range map[string]string{
		"DESKTOP_ID":         "ada",
		"CONTROL_PLANE_URL":  "http://127.0.0.1:8080",
		"AGENT_HARNESS":      HarnessTheBot,
		"DECURIX_DIR":        filepath.Join(home, ".decurix"),
		"DECURIX_WORK_DIR":   filepath.Join(home, "work"),
		"DECURIX_AGENT_BIND": "127.0.0.1",
		"DECURIX_AGENT_PORT": "51001",
		"SOUL_HOST":          "127.0.0.1",
		"SOUL_PORT":          "51002",
		"THEBOT_URL":         "http://127.0.0.1:51002",
		"DECURIX_SOUL":       filepath.Join(testRuntime, "soul.exe"),
		"DECURIX_PYTHON":     filepath.Join(testRuntime, "python", "python.exe"),
		"DECURIX_UI_HELPER":  filepath.Join(testRuntime, "decurix-ui.exe"),
		"DECURIX_CDP_PORT":   "51003",
		// A hosted brain is reached through the control plane on loopback.
		"OPENAI_BASE_URL":      "http://127.0.0.1:8080/api/llm/azure",
		"DECURIX_BRAIN_VISION": "1",
	} {
		if got := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// What a Linux desktop is given and Windows must not be: a PATH of Linux
	// folders would hide every Windows program from the agent.
	for _, name := range []string{"PATH", "VNC_PW", "GOROOT", "HF_HOME"} {
		for _, entry := range env {
			if strings.HasPrefix(entry, name+"=") {
				t.Errorf("a native worker was given %s", entry)
			}
		}
	}
}

func TestFilesGoIntoAndComeOutOfANativeWorkersHome(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	files := []File{{Name: "invoice.pdf", Data: []byte("%PDF")}, {Name: `C:\mail\notes.txt`, Data: []byte("hello")}}
	if err := m.CopyFiles(ctx, "ada", "/home/headless/work/inbox/7", files); err != nil {
		t.Fatal(err)
	}

	inbox := filepath.Join(m.native.rt.Root, "ada", "home", "work", "inbox", "7")
	if data, err := os.ReadFile(filepath.Join(inbox, "notes.txt")); err != nil || string(data) != "hello" {
		t.Errorf("notes.txt = %q, %v (a sender's folder is not part of the name)", data, err)
	}

	for _, outside := range []string{"/home/headless", "/etc/passwd", "/home/headless/../../windows"} {
		if err := m.CopyFiles(ctx, "ada", outside, files); err == nil {
			t.Errorf("files were copied to %s, outside the worker's home", outside)
		}
	}
	if err := m.CopyFiles(ctx, "nobody", "/home/headless/work", files); !errors.Is(err, ErrNotFound) {
		t.Errorf("CopyFiles into a worker that is not there = %v, want ErrNotFound", err)
	}

	// Out again, as the tar a backup reads: named from the folder down.
	rc, err := m.CopyOut(ctx, "ada", "/home/headless/work/inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got := map[string]string{}
	tr := tar.NewReader(rc)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[header.Name] = string(data)
	}
	if got["inbox/7/invoice.pdf"] != "%PDF" || got["inbox/7/notes.txt"] != "hello" {
		t.Errorf("CopyOut gave %v", got)
	}
	if _, isDir := got["inbox/"]; !isDir {
		t.Errorf("CopyOut left out the folder itself: %v", got)
	}

	if _, err := m.CopyOut(ctx, "ada", "/home/headless/.decurix/skills"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CopyOut of a folder the worker does not have = %v, want ErrNotFound", err)
	}
}

func TestWorkerMemoryIsWrittenWhereANativeAgentReadsIt(t *testing.T) {
	m := nativeForTest(t)
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	if err := m.PersistMemoryFile(context.Background(), "ada", "../../escape.txt", "  prefers PDF invoices \n"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(m.native.rt.Root, "ada", "home", ".decurix", "memory", "escape.txt"))
	if err != nil || string(data) != "prefers PDF invoices\n" {
		t.Errorf("memory file = %q, %v (a name is only ever a name, inside the memory folder)", data, err)
	}
}

func TestStoppingAndDeletingANativeWorker(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()
	writeWorker(t, m, nativeMeta{ID: "ada", Name: "Ada"})

	// Stopped by a person: remembered, so the next start of the control
	// plane leaves it stopped.
	if err := m.Stop(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if meta, _ := m.native.readMeta("ada"); !meta.Stopped {
		t.Error("a worker a person stopped was not recorded as stopped")
	}

	if err := m.SendRequest(ctx, "ada", "hello", ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a request to a stopped worker = %v, want ErrNotRunning (the caller starts it and tries again)", err)
	}
	if _, _, err := m.ProxyTarget(ctx, "ada"); !errors.Is(err, ErrNoVNC) {
		t.Errorf("ProxyTarget = %v, want ErrNoVNC", err)
	}

	if err := m.Delete(ctx, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.native.rt.Root, "ada")); !os.IsNotExist(err) {
		t.Errorf("the worker's folder is still there after Delete: %v", err)
	}
	if err := m.Delete(ctx, "ada"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it again = %v, want ErrNotFound", err)
	}
}

func TestANativeManagerNeverReachesForDocker(t *testing.T) {
	m := nativeForTest(t)
	ctx := context.Background()

	// Each of these would dereference the nil Docker client.
	if missing := m.MissingImages(ctx); missing != nil {
		t.Errorf("MissingImages = %v", missing)
	}
	if err := m.EnsureImages(ctx); err != nil {
		t.Errorf("EnsureImages: %v", err)
	}
	if err := m.EnsureNetwork(ctx); err != nil {
		t.Errorf("EnsureNetwork: %v", err)
	}
	if err := m.EnsureToolsVolume(ctx); err != nil {
		t.Errorf("EnsureToolsVolume: %v", err)
	}
	if ips := m.GatewayIPs(ctx); ips != nil {
		t.Errorf("GatewayIPs = %v", ips)
	}
	if stale := m.StaleControlPlane(ctx); stale != nil {
		t.Errorf("StaleControlPlane = %v", stale)
	}
	if err := m.MigrateHostApps(ctx, "ada", []string{"chrome"}); err != nil {
		t.Errorf("MigrateHostApps: %v", err)
	}
	if _, _, err := m.StartClaudeLogin(ctx); err == nil {
		t.Error("the Claude login, which needs a container, started")
	}
}
