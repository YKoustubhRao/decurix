//go:build windows

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

func TestNativeFilesTracksWorkAndNavigation(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Files on a real private desktop")
	}
	t.Run("compiled", func(t *testing.T) { nativeFilesTracksWork(t, true) })
	t.Run("PowerShell fallback", func(t *testing.T) { nativeFilesTracksWork(t, false) })
}

func nativeFilesTracksWork(t *testing.T, compiled bool) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	work := filepath.Join(home, "work")
	sub := filepath.Join(work, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "agent", "decurix_agent", "files_browser.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	// Instrument the actual view model on its UI thread. No duplicate file
	// browser implementation and no access to the person's visible desktop.
	metadata := filepath.Join(dir, "files.json")
	probeScript := strings.Replace(string(raw), "[void]$form.ShowDialog()", `
$probe = New-Object System.Windows.Forms.Timer
$probe.Interval = 100
$script:ProbeProcess = [Diagnostics.Process]::GetCurrentProcess()
$probe.Add_Tick({
    $names = @($list.Items | ForEach-Object { $_.Text })
    $script:ProbeProcess.Refresh()
    $json = @{Here=$script:Here; Path=$path.Text; Names=$names; Memory=$script:ProbeProcess.WorkingSet64} | ConvertTo-Json -Compress
    if ($json -ne $script:LastProbe) { [IO.File]::WriteAllText('`+strings.ReplaceAll(metadata, "'", "''")+`', $json); $script:LastProbe=$json }
})
$probe.Start()
[void]$form.ShowDialog()
`, 1)
	probeFile := filepath.Join(dir, "files-probe.ps1")
	if err := os.WriteFile(probeFile, []byte(probeScript), 0600); err != nil {
		t.Fatal(err)
	}
	program := PowershellExe()
	args := `-NoProfile -STA -WindowStyle Hidden -ExecutionPolicy Bypass -File "` + probeFile + `" -Root "` + home + `" -StartPath "` + work + `"`
	if compiled {
		source, err := os.ReadFile(filepath.Join("..", "..", "agent", "windows", "decurix-files.cs"))
		if err != nil {
			t.Fatal(err)
		}
		source = append(source, []byte(`
public static class FilesProbe {
    [STAThread] public static void Main(string[] args) {
        Application.EnableVisualStyles(); Application.SetCompatibleTextRenderingDefault(false);
        using(var window=new WorkerFiles(args[0],args[1])) {
            var probe=new Timer { Interval=100 }; string last="";
            var process=System.Diagnostics.Process.GetCurrentProcess();
            probe.Tick += delegate {
                var names=window.Entries.Items.Cast<ListViewItem>().Select(x => x.Text).ToArray();
                process.Refresh();
                string json=new System.Web.Script.Serialization.JavaScriptSerializer().Serialize(new {Here=window.CurrentPath, Path=window.DisplayPath, Names=names, Memory=process.WorkingSet64});
                if(json!=last) { File.WriteAllText(args[2],json); last=json; }
            };
            window.Shown += delegate { probe.Start(); };
            Application.Run(window); probe.Dispose();
        }
    }
}`)...)
		probeSource := filepath.Join(dir, "files-probe.cs")
		if err := os.WriteFile(probeSource, source, 0600); err != nil {
			t.Fatal(err)
		}
		program = filepath.Join(dir, "files-probe.exe")
		compiler := filepath.Join(os.Getenv("SystemRoot"), "Microsoft.NET", "Framework64", "v4.0.30319", "csc.exe")
		cmd := exec.Command(compiler, "/nologo", "/target:winexe", "/main:FilesProbe", "/out:"+program, "/r:System.Drawing.dll", "/r:System.Windows.Forms.dll", "/r:System.Core.dll", "/r:System.Web.Extensions.dll", probeSource)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("compile Files fixture: %v\n%s", err, output)
		}
		args = `"` + home + `" "` + work + `" "` + metadata + `"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	session, err := winhost.OpenDesktop(ctx, realHelper(t, true), "files_probe", nil, PowershellExe(), "-NoProfile -NonInteractive -Command Start-Sleep 90", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Stop)
	start := time.Now()
	if err := session.OpenDirect(program, args, work, ""); err != nil {
		t.Fatal(err)
	}
	type state struct {
		Here, Path string
		Names      []string
		Memory     int64
	}
	wait := func(accept func(state) bool) state {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		var got state
		for time.Now().Before(until) {
			raw, _ := os.ReadFile(metadata)
			if json.Unmarshal(raw, &got) == nil && accept(got) {
				return got
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("Files did not update: %+v", got)
		return got
	}
	initial := wait(func(s state) bool { return s.Here == work && s.Path == `Work:\` })
	t.Logf("Files direct launch to visible contents: %s; working set %.1f MiB", time.Since(start), float64(initial.Memory)/(1024*1024))
	contains := func(s state, name string) bool {
		for _, item := range s.Names {
			if item == name {
				return true
			}
		}
		return false
	}
	start = time.Now()
	file := filepath.Join(work, "from-terminal.txt")
	if err := os.WriteFile(file, []byte("shared worker filesystem"), 0600); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return contains(s, "from-terminal.txt") })
	t.Logf("file creation to Files refresh: %s", time.Since(start))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return !contains(s, "from-terminal.txt") })
	windows, err := session.Tasks()
	if err != nil || len(windows) == 0 {
		t.Fatalf("Files has no window: %+v, %v", windows, err)
	}
	if err := session.Window("raise", windows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := firstError(session.Key(76, "c"), session.Type(`Work:\sub`), session.Key(13, "")); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return s.Here == sub && s.Path == `Work:\sub` })
	if err := session.Key(37, "a"); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return s.Here == work })
	if os.Getenv("DECURIX_DESKTOP_PROBE_DIR") != "" {
		frame, err := session.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(frame.JPEG))
		if err != nil {
			t.Fatal(err)
		}
		name := "files-powershell"
		if compiled {
			name = "files-compiled"
		}
		writeDesktopProbe(t, name, img)
	}
	if err := session.Key(39, "a"); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return s.Here == sub })
	if err := session.Key(38, "a"); err != nil {
		t.Fatal(err)
	}
	wait(func(s state) bool { return s.Here == work })
}
