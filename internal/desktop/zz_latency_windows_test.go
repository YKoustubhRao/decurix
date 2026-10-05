//go:build windows

package desktop

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// Scratch measurement, not to be committed: from a key to the picture that shows it.
func TestZZLatency(t *testing.T) {
	if os.Getenv("PROBE_LATENCY") == "" {
		t.Skip("no PROBE_LATENCY")
	}
	local := os.Getenv("LOCALAPPDATA")
	root, _ := os.MkdirTemp(local, "decurix-latency-")
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	m := NewNativeManager(NativeRuntime{Root: root, Python: PowershellExe(), AgentDir: "-NoProfile", Soul: "unused", ControlPlaneURL: "http://127.0.0.1:1", Helper: realHelper(t, true)})
	t.Cleanup(m.StopNativeWorkers)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	meta := nativeMeta{ID: "probe", Name: "Probe", Harness: HarnessTheBot, Brain: BrainClaude, CreatedAt: time.Now().Format(time.RFC3339)}
	_ = m.native.writeMeta(meta)
	session, err := winhost.OpenDesktop(ctx, m.native.rt.Helper, "w_lat", m.native.env(meta, 1, 2, 3), m.native.rt.Python, "-NoProfile -NonInteractive -Command Start-Sleep 300", m.native.rt.Root)
	if err != nil {
		t.Fatal(err)
	}
	m.native.mu.Lock()
	m.native.running["probe"] = &nativeRun{session: session, agentPort: 1, soulPort: 2, startedAt: time.Now()}
	m.native.mu.Unlock()

	for _, exe := range strings.Split(os.Getenv("PROBE_APPS"), ";") {
		if exe != "" {
			_ = m.OpenNativeProgram("probe", NativeApp{ID: "a", Name: "A", Kind: NativeAppProgram, Target: exe})
		}
	}
	time.Sleep(15 * time.Second)
	var terminal NativeApp
	for _, app := range m.native.withFilesApp("probe", nil) {
		if app.ID == NativeTerminalAppID {
			terminal = app
		}
	}
	if err := m.OpenNativeProgram("probe", terminal); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	t.Logf("windows: %+v", m.NativeWindows("probe"))

	v := session.Watch()
	defer v.Close()
	drain := func(quiet time.Duration) {
		for {
			select {
			case <-v.C:
			case <-time.After(quiet):
				return
			}
		}
	}
	measure := func(name string, act func(i int)) {
		drain(700 * time.Millisecond)
		var took []time.Duration
		for i := 0; i < 25; i++ {
			t0 := time.Now()
			act(i)
			select {
			case u := <-v.C:
				_ = u
				took = append(took, time.Since(t0))
			case <-time.After(2 * time.Second):
				took = append(took, 2*time.Second)
			}
			drain(250 * time.Millisecond)
		}
		sort.Slice(took, func(a, b int) bool { return took[a] < took[b] })
		t.Logf("%-34s median %4d ms   90th %4d ms   worst %4d ms", name, took[len(took)/2].Milliseconds(), took[len(took)*9/10].Milliseconds(), took[len(took)-1].Milliseconds())
	}
	measure("a key typed into the terminal", func(i int) { _ = session.Type(string(rune('a' + i%26))) })
}
