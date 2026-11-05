package desktop

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestWindowsAppIconProducesUsablePixels pins that the Windows shell's icon for an app comes back as a PNG with pixels.
// It used to run under the 20s bound a desktop creation gives one launcher; the script compiles C# (Add-Type) on every
// call, which takes seconds on an idle machine and more than 20 under two parallel test runs, so the test failed with
// nothing wrong. What it asserts is the pixels, not the speed: it runs within the test binary's own deadline, and
// TestWindowsAppIconGivesUpAtItsBound pins that the creation's bound still ends a slow extraction.
func TestWindowsAppIconProducesUsablePixels(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires the Windows shell")
	}
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-5*time.Second))
		defer cancel()
	}
	target := filepath.Join(os.Getenv("WINDIR"), "System32", "notepad.exe")
	data, err := extractWindowsAppIcon(ctx, HostAppItem{AppType: "windows_app", Name: "Notepad", HostCmd: `cmd.exe /c start "" "` + target + `"`})
	if err != nil {
		t.Fatalf("no icon from the Windows shell: %v", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width == 0 || config.Height == 0 {
		t.Fatalf("not a usable icon: %v (%d bytes)", err, len(data))
	}
}

// TestWindowsAppIconGivesUpAtItsBound pins that an extraction past its context's end returns no icon and says so,
// rather than holding up the launcher it was for.
func TestWindowsAppIconGivesUpAtItsBound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires the Windows shell")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(os.Getenv("WINDIR"), "System32", "notepad.exe")
	data, err := extractWindowsAppIcon(ctx, HostAppItem{AppType: "windows_app", Name: "Notepad", HostCmd: `cmd.exe /c start "" "` + target + `"`})
	if !errors.Is(err, context.Canceled) || data != nil {
		t.Fatalf("an ended extraction gave %d bytes and %v, want none and context.Canceled", len(data), err)
	}
}

func TestMissingHostIconStillPackagesDisplayableIcon(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	app := HostAppItem{Name: "Example", HostCmd: "example", Icon: filepath.Join(t.TempDir(), "missing.png")}
	if err := packDynamicApp(writer, app, testPack); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	reader := tar.NewReader(&archive)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(data)
	}
	for _, line := range strings.Split(files["Desktop/example-host.desktop"], "\n") {
		if strings.HasPrefix(line, "Icon=") {
			path := strings.TrimPrefix(strings.TrimPrefix(line, "Icon="), "/home/headless/")
			if !strings.Contains(files[path], "<svg") {
				t.Fatalf("launcher references missing icon %q", path)
			}
			return
		}
	}
	t.Fatal("launcher has no icon")
}
