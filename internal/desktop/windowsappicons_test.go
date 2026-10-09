package desktop

import (
	"archive/tar"
	"bytes"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsAppIconProducesUsablePixels(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires the Windows shell")
	}
	target := filepath.Join(os.Getenv("WINDIR"), "System32", "notepad.exe")
	data := windowsAppIcon(HostAppItem{AppType: "windows_app", Name: "Notepad", HostCmd: `cmd.exe /c start "" "` + target + `"`})
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width == 0 || config.Height == 0 {
		t.Fatalf("not a usable icon: %v (%d bytes)", err, len(data))
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
