//go:build windows

package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/YKoustubhRao/decurix/internal/winhost"
)

// A real WinForms window reports Windows' own caption rectangles. Assertions
// use those coordinates and the streamed pixels, rather than hard-coded button
// widths or a second implementation of the compositor's hit testing.
const captionProbeSource = `
using System;
using System.Drawing;
using System.IO;
using System.Runtime.InteropServices;
using System.Web.Script.Serialization;
using System.Windows.Forms;
public class CaptionProbe {
    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L,T,R,B; }
    [StructLayout(LayoutKind.Sequential)] public struct INFO {
        public int cbSize; public RECT title;
        [MarshalAs(UnmanagedType.ByValArray, SizeConst=6)] public uint[] states;
        [MarshalAs(UnmanagedType.ByValArray, SizeConst=6)] public RECT[] buttons;
    }
    [DllImport("user32.dll")] static extern bool SetProcessDPIAware();
    [DllImport("user32.dll")] static extern IntPtr SendMessage(IntPtr h, uint m, IntPtr w, ref INFO i);
    [STAThread] public static void Main(string[] args) {
        SetProcessDPIAware(); Application.EnableVisualStyles();
        var form = new Form { Text="Decurix caption probe", StartPosition=FormStartPosition.Manual,
            Bounds=new Rectangle(100,100,620,420), BackColor=Color.White };
        if (args.Length > 1) form.MaximizeBox=false;
        var timer = new Timer { Interval=100 };
        string last="";
        timer.Tick += delegate {
            var info = new INFO { cbSize=Marshal.SizeOf(typeof(INFO)), states=new uint[6], buttons=new RECT[6] };
            SendMessage(form.Handle, 0x033F, IntPtr.Zero, ref info);
            string json=new JavaScriptSerializer().Serialize(new { buttons=info.buttons, state=form.WindowState.ToString() });
            if(json!=last) { File.WriteAllText(args[0],json); last=json; }
        };
        form.Shown += delegate { timer.Start(); };
        Application.Run(form); timer.Dispose();
    }
}`

type captionRect struct{ L, T, R, B int }
type captionProbe struct {
	Buttons []captionRect
	State   string
}

func TestNativeCaptionHoverAndControls(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real private desktop and GUI fixture")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.cs")
	build := filepath.Join(dir, "build.ps1")
	exe := filepath.Join(dir, "probe.exe")
	if err := os.WriteFile(source, []byte(captionProbeSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(build, []byte(`param($Source,$Output)
$ErrorActionPreference='Stop'
Add-Type -ReferencedAssemblies System.Drawing,System.Windows.Forms,System.Web.Extensions -TypeDefinition ([IO.File]::ReadAllText($Source)) -OutputAssembly $Output -OutputType WindowsApplication
`), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(PowershellExe(), "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", build, source, exe)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile fixture: %v\n%s", err, output)
	}
	for _, compiled := range []bool{true, false} {
		name := "compiled"
		if !compiled {
			name = "hosted"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			metadata := filepath.Join(dir, name+".json")
			session, err := winhost.OpenDesktop(ctx, realHelper(t, compiled), "caption_"+name, []string{"DECURIX_SCREEN_TRACE=1"}, PowershellExe(), "-NoProfile -NonInteractive -Command Start-Sleep 120", dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(session.Stop)
			session.OnEvent(func(event string) { t.Logf("helper: %s", event) })
			started := time.Now()
			if err := session.Open(exe, `"`+metadata+`"`); err != nil {
				t.Fatal(err)
			}
			read := func(want string) captionProbe {
				t.Helper()
				until := time.Now().Add(8 * time.Second)
				for time.Now().Before(until) {
					var probe captionProbe
					data, _ := os.ReadFile(metadata)
					if json.Unmarshal(data, &probe) == nil && len(probe.Buttons) == 6 && probe.State == want && (want == "Minimized" || probe.Buttons[5].R > probe.Buttons[5].L) {
						return probe
					}
					time.Sleep(30 * time.Millisecond)
				}
				t.Fatalf("fixture did not enter %s", want)
				return captionProbe{}
			}
			probe := read("Normal")
			t.Logf("cold program launch to visible window: %s", time.Since(started))
			capture := func() image.Image {
				t.Helper()
				frameContext, frameCancel := context.WithTimeout(ctx, 5*time.Second)
				defer frameCancel()
				frame, err := session.Snapshot(frameContext)
				if err != nil {
					t.Fatal(err)
				}
				img, err := jpeg.Decode(bytes.NewReader(frame.JPEG))
				if err != nil {
					t.Fatal(err)
				}
				return img
			}
			move := func(r captionRect) {
				t.Helper()
				if err := session.Mouse("move", (r.L+r.R)/2, (r.T+r.B)/2, false, ""); err != nil {
					t.Fatal(err)
				}
			}
			click := func(r captionRect) {
				t.Helper()
				move(r)
				if err := firstError(session.Mouse("down", (r.L+r.R)/2, (r.T+r.B)/2, false, ""), session.Mouse("up", (r.L+r.R)/2, (r.T+r.B)/2, false, "")); err != nil {
					t.Fatal(err)
				}
			}
			for _, index := range []int{2, 3, 5} {
				r := probe.Buttons[index]
				_ = session.Mouse("move", 80, 80, false, "")
				before := capture()
				start := time.Now()
				move(r)
				after := capture()
				t.Logf("caption %d hover to streamed frame: %s", index, time.Since(start))
				writeDesktopProbe(t, name+"-hover-"+strconv.Itoa(index), after)
				x, y := r.L+5, (r.T+r.B)/2
				ar, ag, ab, _ := after.At(x, y).RGBA()
				br, bg, bb, _ := before.At(x, y).RGBA()
				if index == 5 {
					if ar < 160*257 || ar < ag+70*257 || ar < ab+60*257 {
						t.Errorf("close hover is not red: %d %d %d", ar/257, ag/257, ab/257)
					}
				} else if ar == br && ag == bg && ab == bb {
					t.Errorf("caption %d did not highlight", index)
				}
				_ = session.Mouse("move", 80, 80, false, "")
				cleared := capture()
				cr, cg, cb, _ := cleared.At(x, y).RGBA()
				if index == 5 && cr > cg+70*257 && cr > cb+60*257 {
					t.Error("close highlight remained after pointer left")
				}
			}
			// Releasing outside the button cancels a close, like Windows.
			r := probe.Buttons[5]
			move(r)
			_ = session.Mouse("down", (r.L+r.R)/2, (r.T+r.B)/2, false, "")
			_ = session.Mouse("move", 80, 80, false, "")
			_ = session.Mouse("up", 80, 80, false, "")
			if empty, _ := session.Empty(); empty {
				t.Fatal("releasing off close closed the window")
			}
			click(probe.Buttons[3])
			maximized := read("Maximized")
			click(maximized.Buttons[3])
			probe = read("Normal")
			click(probe.Buttons[2])
			read("Minimized")
			windows, err := session.Tasks()
			if err != nil || len(windows) == 0 || !windows[0].Minimized {
				t.Fatalf("minimize did not update taskbar: %+v, %v", windows, err)
			}
			if err := session.Window("raise", windows[0].ID); err != nil {
				t.Fatal(err)
			}
			probe = read("Normal")
			var latency []time.Duration
			for i := 0; i < 30; i++ {
				start := time.Now()
				move(probe.Buttons[3])
				latency = append(latency, time.Since(start))
			}
			sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
			t.Logf("stationary caption input median %s, p90 %s", latency[15], latency[27])
			// Reconnecting to an unchanged desktop still requires a whole frame.
			for i := 0; i < 20; i++ {
				capture()
			}
			// The shell must recover after releasing all its cached drawing surfaces.
			if compiled {
				time.Sleep(21 * time.Second)
				capture()
			}
			click(probe.Buttons[5])
			until := time.Now().Add(5 * time.Second)
			closed := false
			for time.Now().Before(until) {
				if empty, _ := session.Empty(); empty {
					closed = true
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
			if !closed {
				t.Fatal("close did not close the window")
			}
			metadata += ".disabled"
			if err := session.Open(exe, `"`+metadata+`" disabled`); err != nil {
				t.Fatal(err)
			}
			probe = read("Normal")
			if r := probe.Buttons[3]; r.R > r.L {
				click(r)
				time.Sleep(200 * time.Millisecond)
				read("Normal")
			}
		})
	}
}

func writeDesktopProbe(t *testing.T, name string, img image.Image) {
	t.Helper()
	dir := os.Getenv("DECURIX_DESKTOP_PROBE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}
