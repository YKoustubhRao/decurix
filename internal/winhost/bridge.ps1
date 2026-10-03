# Decurix host GUI bridge (Windows). Runs ONE application on a private Win32 desktop object, so it has
# no window on the person's own desktop, and lets Decurix see and drive it over stdin/stdout.
# There is deliberately no network listener: only the process that started this script can reach the app.
#
# stdin, one command per line (fields separated by TAB where a line has more than one). Each command is answered by
# exactly one line on stdout, in order:
#   desktop <name>           name the private desktop this helper will create (before launch)
#   launch <target> <args>   start the app; "OK pid=<n>" or "ERR <reason>"
#   check <target> <args>    what launch would decide, without starting anything: "OK<TAB>exe<TAB>args<TAB>dir<TAB>own<TAB>base"
#                            (the program it resolved to, for launchr) or "ERR <reason>"
#   launchr <exe> <args> <dir> <own 0|1> <base>   launch what a check resolved, with nothing left to look up
#   open <target> <args>     start one more app on the desktop a launch made (a worker's screen), resolved as launch does
#   count                    "COUNT <n>": how many windows a person would see on the desktop (0 is an empty screen)
#   shortcuts <label><TAB>id<US>name<US>icon<TAB>...   the icons on a worker's wallpaper (US is \x1f); label is its corner text
#   tasks                    "TASKS handle<US>a|m|-<US>title<TAB>...": the windows a taskbar has a button for, front first
#   win raise|min|close <handle>   what a taskbar button does to its window
#   start                    open the Start menu drawn on a worker's taskbar, or put it away
#   over <x> <y>             "OVER window" or "OVER desktop": what the picture shows at that point
#   screen                   the picture is the whole private desktop, shown even when nothing has a window on it (a
#                            worker's screen, winhost.OpenDesktop) rather than one app's windows
#   viewers <n>              how many people are watching; the picture is only captured while n > 0
#   keyframe                 the next picture sent is a whole one
#   move|down|up <x> <y> [right] [+mods]   mouse, in frame pixels; mods is any of c s a (Ctrl, Shift, Alt) held during it
#   click <x> <y>            down + up
#   wheel <x> <y> <delta>    scroll (positive is up)
#   copy <vk>                press Ctrl+<vk> (C or X) and answer "CLIP <base64 utf-8 text>" if the clipboard changed
#   type <text>              characters, to whatever was clicked last
#   key <vk> [mods]          one virtual key (Enter 13, Backspace 8, ...), with modifiers held (c s a)
#   status | focus | windows what is going on, for saying why nothing showed up
#   quit                     end the app and exit (also happens when stdin closes)
#
# Between answers the helper also says, on its own:
#   FRAME <W> <H> <x> <y> <w> <h> <whole 0|1> <n>   then n raw JPEG bytes: the picture is W x H and this JPEG is its
#                            rectangle x,y,w,h (the whole picture when <whole> is 1). Sent only when something changed.
#   EVENT closed             the app has no window left
#   EVENT open <id>          a shortcut on the wallpaper was double-clicked
param([Parameter(Mandatory = $true)][string]$DesktopName)

$NativeSource = @'
using System;
using System.Collections.Generic;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Diagnostics;

public class Native {
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    public struct STARTUPINFO {
        public int cb; public string lpReserved, lpDesktop, lpTitle;
        public int dwX, dwY, dwXSize, dwYSize, dwXCountChars, dwYCountChars, dwFillAttribute, dwFlags;
        public short wShowWindow, cbReserved2;
        public IntPtr lpReserved2, hStdInput, hStdOutput, hStdError;
    }
    [StructLayout(LayoutKind.Sequential)]
    public struct PROCESS_INFORMATION { public IntPtr hProcess, hThread; public int dwProcessId, dwThreadId; }
    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
    [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
    [StructLayout(LayoutKind.Sequential)] struct BASIC { public long PerProcUser, PerJobUser; public uint Flags; public UIntPtr MinWs, MaxWs; public uint Active; public UIntPtr Aff; public uint Prio, Sched; }
    [StructLayout(LayoutKind.Sequential)] struct IOC { public ulong a, b, c, d, e, f; }
    [StructLayout(LayoutKind.Sequential)] struct EXT { public BASIC Basic; public IOC Io; public UIntPtr ProcMem, JobMem, PeakProc, PeakJob; }

    public delegate bool EnumProc(IntPtr h, IntPtr l);

    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern IntPtr CreateDesktop(string n, IntPtr d, IntPtr dm, int f, uint a, IntPtr sa);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern bool CreateProcess(string app, string cmd, IntPtr pa, IntPtr ta, bool inh, int fl, IntPtr env, string dir, ref STARTUPINFO si, out PROCESS_INFORMATION pi);
    [DllImport("user32.dll")] static extern bool EnumDesktopWindows(IntPtr d, EnumProc p, IntPtr l);
    [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll")] static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] static extern bool PrintWindow(IntPtr h, IntPtr dc, uint f);
    [DllImport("user32.dll")] static extern IntPtr ChildWindowFromPointEx(IntPtr hParent, POINT pt, uint flags);
    // Input is posted, never sent: SendMessage waits for the app to handle the message, and a click that opens a modal dialog
    // (a MessageBox, File > Open) does not return until that dialog is closed - which would freeze this whole session.
    [DllImport("user32.dll")] static extern bool PostMessage(IntPtr h, uint msg, IntPtr wp, IntPtr lp);
    [DllImport("user32.dll")] static extern bool ScreenToClient(IntPtr h, ref POINT p);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
    [DllImport("dwmapi.dll")] static extern int DwmGetWindowAttribute(IntPtr h, int attr, out RECT r, int size);
    [DllImport("dwmapi.dll")] static extern int DwmGetWindowAttribute(IntPtr h, int attr, out int v, int size);
    [StructLayout(LayoutKind.Sequential)]
    struct GUITHREADINFO { public int cbSize, flags; public IntPtr hwndActive, hwndFocus, hwndCapture, hwndMenuOwner, hwndMoveSize, hwndCaret; public RECT rcCaret; }
    [DllImport("user32.dll")] static extern bool GetGUIThreadInfo(uint tid, ref GUITHREADINFO info);
    [DllImport("user32.dll")] static extern bool SetThreadDesktop(IntPtr h);
    [DllImport("user32.dll")] static extern bool AttachThreadInput(uint from, uint to, bool attach);
    [DllImport("user32.dll")] static extern bool IsWindow(IntPtr h);
    [DllImport("user32.dll")] static extern bool GetKeyboardState(byte[] state);
    [DllImport("user32.dll")] static extern bool SetKeyboardState(byte[] state);
    [DllImport("kernel32.dll")] static extern uint GetCurrentThreadId();
    [DllImport("user32.dll", SetLastError = true)] static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, IntPtr lp, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll")] static extern uint GetClipboardSequenceNumber();
    [DllImport("user32.dll", SetLastError = true)] static extern bool OpenClipboard(IntPtr owner);
    [DllImport("user32.dll")] static extern bool CloseClipboard();
    [DllImport("user32.dll")] static extern bool EmptyClipboard();
    [DllImport("user32.dll")] static extern int CountClipboardFormats();
    [DllImport("user32.dll")] static extern bool IsClipboardFormatAvailable(uint format);
    [DllImport("user32.dll")] static extern IntPtr GetClipboardData(uint format);
    [DllImport("user32.dll")] static extern IntPtr SetClipboardData(uint format, IntPtr mem);
    [DllImport("kernel32.dll")] static extern IntPtr GlobalLock(IntPtr h);
    [DllImport("kernel32.dll")] static extern bool GlobalUnlock(IntPtr h);
    [DllImport("kernel32.dll")] static extern IntPtr GlobalAlloc(uint flags, UIntPtr bytes);
    [DllImport("user32.dll")] static extern bool SetProcessDPIAware();
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out int pid);
    [DllImport("user32.dll")] static extern IntPtr GetWindowDpiAwarenessContext(IntPtr h);
    [DllImport("user32.dll")] static extern int GetAwarenessFromDpiAwarenessContext(IntPtr c);
    [DllImport("user32.dll")] static extern uint GetDpiForSystem();
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern IntPtr CreateJobObject(IntPtr a, string n);
    [DllImport("kernel32.dll")] static extern bool SetInformationJobObject(IntPtr j, int cls, IntPtr info, int len);
    [DllImport("kernel32.dll")] static extern bool AssignProcessToJobObject(IntPtr j, IntPtr p);
    [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(uint acc, bool inh, int pid);
    [DllImport("kernel32.dll")] static extern bool TerminateProcess(IntPtr p, uint code);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
    [DllImport("msvcrt.dll")] static extern int memcmp(IntPtr a, IntPtr b, UIntPtr n);
    [DllImport("kernel32.dll", EntryPoint = "RtlMoveMemory")] static extern void CopyMemory(IntPtr dst, IntPtr src, UIntPtr n);

    const uint WM_MOUSEMOVE = 0x0200, WM_LBUTTONDOWN = 0x0201, WM_LBUTTONUP = 0x0202,
               WM_RBUTTONDOWN = 0x0204, WM_RBUTTONUP = 0x0205, WM_MOUSEWHEEL = 0x020A,
               WM_CHAR = 0x0102, WM_KEYDOWN = 0x0100, WM_KEYUP = 0x0101;

    [DllImport("kernel32.dll")] static extern bool GetExitCodeProcess(IntPtr p, out uint code);

    static IntPtr desktop, job, mainWindow, lastTarget, captured, launched;
    // Where the last picture sat on the private desktop: its top-left corner (physical pixels) and its scale, so a click
    // in the picture that was shown is mapped back to the place it was drawn from even if windows have moved since.
    // Written by the capture thread, read by whichever thread handles an input, so always under geo.
    static readonly object geo = new object();
    static int frameL, frameT;
    static double frameScale = 1.0;
    static bool haveFrame;
    static ImageCodecInfo jpeg;
    static EncoderParameters jpegQuality;

    public static void Init() {
        SetProcessDPIAware();
        job = CreateJobObject(IntPtr.Zero, null);
        EXT e = new EXT(); e.Basic.Flags = 0x2000; // kill everything in the job when this process ends
        int len = Marshal.SizeOf(e); IntPtr buf = Marshal.AllocHGlobal(len);
        Marshal.StructureToPtr(e, buf, false);
        SetInformationJobObject(job, 9, buf, len);
        Marshal.FreeHGlobal(buf);
        foreach (var c in ImageCodecInfo.GetImageEncoders()) if (c.MimeType == "image/jpeg") jpeg = c;
        jpegQuality = new EncoderParameters(1);
        jpegQuality.Param[0] = new EncoderParameter(System.Drawing.Imaging.Encoder.Quality, 80L);
        stdout = Console.OpenStandardOutput();
        capThread = new Thread(CaptureLoop) { IsBackground = true, Name = "capture" };
        capThread.Start();
    }

    // ---- Output. Everything this helper says goes through here, under one lock: answers to commands come from the main
    // thread and pictures from the capture thread, and a picture is a header line followed by raw bytes that nothing may
    // be written into the middle of.
    static System.IO.Stream stdout;
    static readonly object outLock = new object();

    public static void Reply(string line) {
        byte[] b = Encoding.UTF8.GetBytes(line + "\n");
        lock (outLock) { stdout.Write(b, 0, b.Length); stdout.Flush(); }
    }

    // The environment the app starts with: ours, minus what only makes sense for the process that launched us. An Electron
    // app (VS Code, Cursor, Slack, Discord...) started with ELECTRON_RUN_AS_NODE=1 runs as plain Node, rejects its own
    // command line and exits with code 9 - which is exactly what a helper started from inside VS Code's extension host,
    // or any other Electron program, would hand it.
    static readonly string[] StrippedPrefixes = { "ELECTRON_", "VSCODE_", "NODE_OPTIONS", "ATOM_SHELL_" };

    static IntPtr CleanEnvironment() {
        var vars = new List<string>();
        foreach (System.Collections.DictionaryEntry e in Environment.GetEnvironmentVariables()) {
            string name = (string)e.Key;
            bool strip = false;
            foreach (string p in StrippedPrefixes) if (name.StartsWith(p, StringComparison.OrdinalIgnoreCase)) strip = true;
            if (!strip) vars.Add(name + "=" + e.Value);
        }
        vars.Sort(StringComparer.OrdinalIgnoreCase); // CreateProcess wants the block sorted
        return Marshal.StringToHGlobalUni(string.Join("\0", vars) + "\0\0");
    }

    public static string Launch(string desktopName, string exe, string args, string workDir) {
        desktop = CreateDesktop(desktopName, IntPtr.Zero, IntPtr.Zero, 0, 0x10000000, IntPtr.Zero);
        if (desktop == IntPtr.Zero) return "ERR CreateDesktop failed (" + Marshal.GetLastWin32Error() + ")";
        IntPtr process;
        string said = Spawn(desktopName, exe, args, workDir, out process);
        if (process != IntPtr.Zero) launched = process;
        return said;
    }

    // One more program on the desktop Launch already made: a person opening an app on a worker's screen. It is in the
    // same job, so it ends with the desktop, but it is not what the session was started for - Status still speaks of that.
    public static string Open(string desktopName, string exe, string args, string workDir) {
        if (desktop == IntPtr.Zero) return "ERR this desktop has not been started";
        IntPtr process;
        string said = Spawn(desktopName, exe, args, workDir, out process);
        if (process != IntPtr.Zero) CloseHandle(process);
        Kick();
        return said;
    }

    // How many windows a person would see on the desktop: 0 is an empty screen.
    public static int WindowCount() { return AppWindows().Count; }

    static string Spawn(string desktopName, string exe, string args, string workDir, out IntPtr process) {
        process = IntPtr.Zero;
        STARTUPINFO si = new STARTUPINFO();
        si.cb = Marshal.SizeOf(si);
        si.lpDesktop = desktopName;
        PROCESS_INFORMATION pi;
        string cmd = "\"" + exe + "\"" + (string.IsNullOrEmpty(args) ? "" : " " + args);
        IntPtr env = CleanEnvironment();
        bool started = CreateProcess(exe, cmd, IntPtr.Zero, IntPtr.Zero, false, 0x400 /* CREATE_UNICODE_ENVIRONMENT */, env, string.IsNullOrEmpty(workDir) ? null : workDir, ref si, out pi);
        int err = Marshal.GetLastWin32Error();
        Marshal.FreeHGlobal(env);
        if (!started) return "ERR CreateProcess failed (" + err + ")";
        AssignProcessToJobObject(job, pi.hProcess);
        process = pi.hProcess;
        return "OK pid=" + pi.dwProcessId;
    }

    static bool Owner(IntPtr h, IntPtr l) {
        int pid; GetWindowThreadProcessId(h, out pid);
        var list = (List<int>)GCHandle.FromIntPtr(l).Target;
        if (!list.Contains(pid)) list.Add(pid);
        return true;
    }

    static void Enum(EnumProc p, object state) {
        var gc = GCHandle.Alloc(state);
        EnumDesktopWindows(desktop, p, GCHandle.ToIntPtr(gc));
        gc.Free();
    }

    // Ends the app. Its windows are asked to close first, the way a person would, and given a moment: an app that is simply
    // terminated leaves a "did not shut down correctly" flag behind (Chrome then greets the next start with a Restore
    // prompt that swallows the keyboard). Whatever is still there after that is terminated, including anything the app
    // handed off to.
    public static void Shutdown() {
        stopCapture = true; wake.Set();
        if (capThread != null && capThread.IsAlive) capThread.Join(1000);
        if (desktop != IntPtr.Zero) {
            foreach (var w in AppWindows()) PostMessage(w.H, 0x0010 /* WM_CLOSE */, IntPtr.Zero, IntPtr.Zero);
            for (int i = 0; i < 25 && AppWindows().Count > 0; i++) System.Threading.Thread.Sleep(100);
        }
        // The process we started first: it may not have opened a window yet, or may have handed off and exited.
        if (launched != IntPtr.Zero) TerminateProcess(launched, 0);
        var pids = new List<int>();
        if (desktop != IntPtr.Zero) Enum(Owner, pids);
        foreach (int pid in pids) {
            IntPtr p = OpenProcess(0x0001, false, pid);
            if (p != IntPtr.Zero) { TerminateProcess(p, 0); CloseHandle(p); }
        }
        if (job != IntPtr.Zero) CloseHandle(job);
    }

    static bool Describe(IntPtr h, IntPtr l) {
        RECT r; GetWindowRect(h, out r);
        int pid; GetWindowThreadProcessId(h, out pid);
        var cls = new StringBuilder(64); GetClassName(h, cls, 64);
        ((StringBuilder)GCHandle.FromIntPtr(l).Target).Append(pid + ":" + cls + ":" + (IsWindowVisible(h) ? "V" : "h") + ":" + (r.R - r.L) + "x" + (r.B - r.T) + " ");
        return true;
    }

    // "running", or the code the launched process exited with (0xC0000142 means it could not start on this desktop).
    public static string Status() {
        uint code;
        if (launched == IntPtr.Zero || !GetExitCodeProcess(launched, out code)) return "unknown";
        return code == 259 ? "running" : "exited 0x" + code.ToString("X");
    }

    // What is on the private desktop: pid:class:visible:size, for saying why nothing showed up.
    public static string Windows() {
        var sb = new StringBuilder();
        if (desktop != IntPtr.Zero) Enum(Describe, sb);
        return sb.Length == 0 ? "none" : sb.ToString().Trim();
    }

    // A DPI-unaware app draws and reads mouse positions at 96 dpi on a scaled display, so what it renders is
    // smaller than its window rectangle. 1.0 for an aware app.
    static double Scale(IntPtr h) {
        try {
            if (GetAwarenessFromDpiAwarenessContext(GetWindowDpiAwarenessContext(h)) == 0) return 96.0 / GetDpiForSystem();
        } catch { }
        return 1.0;
    }

    // Windows Windows itself puts in every process (the UAC input indicator, IME and text-services helpers); never the app.
    static readonly string[] SystemWindowClasses = { "UAC_InputIndicatorOverlayWnd", "UAC Input Indicator", "Touch Tooltip Window", "MSCTFIME UI", "IME", "Default IME", "CicLoaderWndClass", "GDI+ Hook Window Class" };

    class Win { public IntPtr H; public RECT R, V; public double Sc; }

    static bool Collect(IntPtr h, IntPtr l) {
        if (!IsWindowVisible(h) || IsIconic(h)) return true; // a minimised window is parked off the screen
        var cls = new StringBuilder(80); GetClassName(h, cls, 80);
        if (Array.IndexOf(SystemWindowClasses, cls.ToString()) >= 0) return true;
        int cloaked; // a window the shell has hidden but not destroyed (a hosted app that is not on screen)
        if (DwmGetWindowAttribute(h, 14 /* DWMWA_CLOAKED */, out cloaked, 4) == 0 && cloaked != 0) return true;
        RECT r; GetWindowRect(h, out r);
        if (r.R - r.L < 8 || r.B - r.T < 8) return true;
        // What the window actually shows: Windows adds an invisible resize border that captures as black.
        RECT v; if (DwmGetWindowAttribute(h, 9 /* DWMWA_EXTENDED_FRAME_BOUNDS */, out v, Marshal.SizeOf(typeof(RECT))) != 0 || v.R <= v.L || v.B <= v.T) v = r;
        ((List<Win>)GCHandle.FromIntPtr(l).Target).Add(new Win { H = h, R = r, V = v, Sc = Scale(h) });
        return true;
    }

    // The app's windows, topmost first: its main window plus any menu, drop-down, tooltip or dialog it has opened,
    // each of which is a separate top-level window.
    static List<Win> AppWindows() {
        var list = new List<Win>();
        if (desktop != IntPtr.Zero) Enum(Collect, list);
        return list;
    }

    // A worker's screen: the picture is the whole private desktop, at the size of the computer's own screen, whether or
    // not anything has a window on it - a worker with nothing open still has a screen to show. An app's picture, by
    // contrast, is cut to the app's own windows and ends when it has none.
    [DllImport("user32.dll")] static extern int GetSystemMetrics(int index);
    static bool fixedScreen; static int screenW, screenH;

    public static void FixedScreen() {
        screenW = GetSystemMetrics(0); screenH = GetSystemMetrics(1);
        fixedScreen = screenW > 0 && screenH > 0;
    }

    // Finds the app's main window: the biggest of its windows.
    static bool LocateWindow() {
        long best = 0;
        mainWindow = IntPtr.Zero;
        foreach (var w in AppWindows()) {
            long a = (long)(w.V.R - w.V.L) * (w.V.B - w.V.T);
            if (a > best) { best = a; mainWindow = w.H; }
        }
        return mainWindow != IntPtr.Zero;
    }

    // One picture of everything the app has on screen: every window, painted bottom to top at its own position, over
    // the smallest rectangle that holds them all. A menu or dialog is therefore visible, and clickable, like the rest.
    // The caller disposes it; null when the app has no window.
    static Bitmap Compose() {
        var ws = AppWindows();
        if (ws.Count == 0 && !fixedScreen) return null;
        LocateWindow();
        int l = int.MaxValue, t = int.MaxValue, r = int.MinValue, b = int.MinValue;
        foreach (var w in ws) { l = Math.Min(l, w.V.L); t = Math.Min(t, w.V.T); r = Math.Max(r, w.V.R); b = Math.Max(b, w.V.B); }
        double sc = Scale(mainWindow); // the main window's scale is the picture's, so a DPI-unaware app stays crisp
        if (fixedScreen) { l = 0; t = 0; r = screenW; b = screenH; sc = 1.0; }
        int pw = (int)Math.Round((r - l) * sc), ph = (int)Math.Round((b - t) * sc);
        if (pw <= 0 || ph <= 0 || pw > 8192 || ph > 8192) return null;
        var bmp = new Bitmap(pw, ph, PixelFormat.Format24bppRgb);
        try {
            using (var g = Graphics.FromImage(bmp)) {
                if (fixedScreen) lock (deskLock) g.DrawImageUnscaled(Backdrop(pw, ph), 0, 0);
                else g.Clear(Color.FromArgb(0x1b, 0x1b, 0x1b));
                g.InterpolationMode = InterpolationMode.HighQualityBilinear;
                // A popup's drop shadow and rounded corners capture as black; over the window beneath them they should show it.
                var seeThrough = new ImageAttributes();
                seeThrough.SetColorKey(Color.FromArgb(0, 0, 0), Color.FromArgb(4, 4, 4));
                for (int i = ws.Count - 1; i >= 0; i--) {
                    var w = ws[i];
                    int bw = (int)Math.Round((w.R.R - w.R.L) * w.Sc), bh = (int)Math.Round((w.R.B - w.R.T) * w.Sc);
                    if (bw <= 0 || bh <= 0) continue;
                    using (var wb = new Bitmap(bw, bh, PixelFormat.Format24bppRgb)) {
                        using (var wg = Graphics.FromImage(wb)) {
                            IntPtr dc = wg.GetHdc();
                            PrintWindow(w.H, dc, 2 /* PW_RENDERFULLCONTENT */);
                            wg.ReleaseHdc(dc);
                        }
                        int sx = Math.Max(0, (int)Math.Round((w.V.L - w.R.L) * w.Sc)), sy = Math.Max(0, (int)Math.Round((w.V.T - w.R.T) * w.Sc));
                        int sw = Math.Min(bw - sx, (int)Math.Round((w.V.R - w.V.L) * w.Sc)), sh = Math.Min(bh - sy, (int)Math.Round((w.V.B - w.V.T) * w.Sc));
                        if (sw <= 0 || sh <= 0) continue;
                        var dest = new Rectangle((int)Math.Round((w.V.L - l) * sc), (int)Math.Round((w.V.T - t) * sc),
                                                 (int)Math.Round((w.V.R - w.V.L) * sc), (int)Math.Round((w.V.B - w.V.T) * sc));
                        if (i == ws.Count - 1) g.DrawImage(wb, dest, sx, sy, sw, sh, GraphicsUnit.Pixel);
                        else g.DrawImage(wb, dest, sx, sy, sw, sh, GraphicsUnit.Pixel, seeThrough);
                    }
                }
            }
            if (fixedScreen) using (var g = Graphics.FromImage(bmp)) lock (deskLock) DrawShell(g);
            lock (geo) { frameL = l; frameT = t; frameScale = sc; haveFrame = true; }
            return bmp;
        } catch { bmp.Dispose(); throw; }
    }

    // A click needs to know where the picture sat, so an input that arrives before the first capture takes one itself.
    static void EnsureGeometry() {
        bool have; lock (geo) have = haveFrame;
        if (have) return;
        using (var b = Compose()) { }
    }

    // ---- Capturing. A thread of its own looks at the app and says what changed, so nothing waits on it and a picture is
    // never asked for: it is sent when, and only when, it differs from the last one sent - and then only the part that differs.

    static volatile int viewers;
    static volatile bool wantKey, stopCapture;
    static long boostUntil, lastChange; // Stopwatch ticks
    static readonly AutoResetEvent wake = new AutoResetEvent(false);
    static Thread capThread;
    static IntPtr prev; static int prevBytes, prevW, prevH;
    static bool everHadWindow, closedSent; static long noWindowSince;

    public static void SetViewers(int n) {
        int old = viewers; viewers = n;
        if (n > 0 && old == 0) wantKey = true; // somebody who has nothing on screen yet needs the whole picture
        wake.Set();
    }

    public static void KeyFrame() { wantKey = true; wake.Set(); }

    // Something was just sent to the app: look at the screen again at once, and often for a moment, so its reaction shows quickly.
    public static void Kick() {
        Interlocked.Exchange(ref boostUntil, Stopwatch.GetTimestamp() + Stopwatch.Frequency * 7 / 10);
        wake.Set();
    }

    static void CaptureLoop() {
        int delay = 50;
        while (!stopCapture) {
            bool woken = wake.WaitOne(delay);
            if (stopCapture) break;
            if (desktop == IntPtr.Zero || viewers <= 0) { delay = 200; continue; }
            if (woken) Thread.Sleep(12); // whatever woke us was only just posted to the app; let it react before looking
            bool sent;
            try { sent = CaptureOnce(); } catch (Exception) { delay = 250; continue; }
            long now = Stopwatch.GetTimestamp();
            if (sent) { Interlocked.Exchange(ref lastChange, now); delay = 5; }
            else if (now < Interlocked.Read(ref boostUntil)) delay = 15;
            else {
                double idle = (now - Interlocked.Read(ref lastChange)) / (double)Stopwatch.Frequency;
                delay = idle < 0.6 ? 40 : (idle < 4 ? 100 : 250);
            }
        }
    }

    // Looks at the app once and sends what changed. False when nothing did.
    static bool CaptureOnce() {
        Bitmap bmp = Compose();
        if (bmp == null) {
            long now = Stopwatch.GetTimestamp();
            if (everHadWindow && !closedSent) {
                if (noWindowSince == 0) noWindowSince = now;
                else if (now - noWindowSince > 2 * Stopwatch.Frequency) { closedSent = true; Reply("EVENT closed"); }
            }
            return false;
        }
        noWindowSince = 0; everHadWindow = true; closedSent = false;
        using (bmp) {
            int w = bmp.Width, h = bmp.Height;
            bool full = wantKey || prev == IntPtr.Zero || prevW != w || prevH != h;
            Rectangle r = new Rectangle(0, 0, w, h);
            var bd = bmp.LockBits(new Rectangle(0, 0, w, h), ImageLockMode.ReadOnly, PixelFormat.Format24bppRgb);
            try {
                if (!full) {
                    if (!Diff(bd, w, h, out r)) return false;
                    // A change over most of the picture is cheaper and cleaner to send whole than as one big patch.
                    if ((long)r.Width * r.Height * 100 > (long)w * h * 55) { full = true; r = new Rectangle(0, 0, w, h); }
                }
                int need = bd.Stride * h;
                if (prev == IntPtr.Zero || prevBytes != need) {
                    if (prev != IntPtr.Zero) Marshal.FreeHGlobal(prev);
                    prev = Marshal.AllocHGlobal(need); prevBytes = need;
                }
                CopyMemory(prev, bd.Scan0, (UIntPtr)need);
                prevW = w; prevH = h;
            } finally { bmp.UnlockBits(bd); }
            wantKey = false;

            byte[] data;
            using (var ms = new System.IO.MemoryStream()) {
                if (full) bmp.Save(ms, jpeg, jpegQuality);
                else using (var part = bmp.Clone(r, PixelFormat.Format24bppRgb)) part.Save(ms, jpeg, jpegQuality);
                data = ms.ToArray();
            }
            byte[] head = Encoding.ASCII.GetBytes("FRAME " + w + " " + h + " " + r.X + " " + r.Y + " " + r.Width + " " + r.Height + " " + (full ? 1 : 0) + " " + data.Length + "\n");
            lock (outLock) { stdout.Write(head, 0, head.Length); stdout.Write(data, 0, data.Length); stdout.Flush(); }
            return true;
        }
    }

    // The rectangle of the picture that differs from the last one sent, grown to whole 16-pixel blocks so a JPEG patch
    // has no seam where it meets what is already on the viewer's screen. False if nothing differs.
    static bool Diff(BitmapData bd, int w, int h, out Rectangle dirty) {
        int stride = bd.Stride, row = w * 3;
        dirty = Rectangle.Empty;
        int top = -1;
        for (int y = 0; y < h; y++)
            if (memcmp(IntPtr.Add(bd.Scan0, y * stride), IntPtr.Add(prev, y * stride), (UIntPtr)row) != 0) { top = y; break; }
        if (top < 0) return false;
        int bottom = top;
        for (int y = h - 1; y > top; y--)
            if (memcmp(IntPtr.Add(bd.Scan0, y * stride), IntPtr.Add(prev, y * stride), (UIntPtr)row) != 0) { bottom = y; break; }
        int chunks = (w + 15) / 16, minC = chunks, maxC = -1;
        for (int y = top; y <= bottom; y++) {
            IntPtr a = IntPtr.Add(bd.Scan0, y * stride), b = IntPtr.Add(prev, y * stride);
            for (int c = 0; c < minC; c++) {
                int off = c * 48, n = Math.Min(48, row - off);
                if (memcmp(IntPtr.Add(a, off), IntPtr.Add(b, off), (UIntPtr)n) != 0) { minC = c; break; }
            }
            for (int c = chunks - 1; c > maxC; c--) {
                int off = c * 48, n = Math.Min(48, row - off);
                if (memcmp(IntPtr.Add(a, off), IntPtr.Add(b, off), (UIntPtr)n) != 0) { maxC = c; break; }
            }
        }
        if (maxC < 0) return false;
        int x0 = minC * 16, x1 = Math.Min(w, (maxC + 1) * 16);
        int y0 = top & ~15, y1 = Math.Min(h, (bottom | 15) + 1);
        dirty = new Rectangle(x0, y0, x1 - x0, y1 - y0);
        return true;
    }

    // ---- A worker's desktop. A private desktop has no shell: no wallpaper, no icons, no taskbar, and nothing that
    // raises a clicked window or moves one by its title bar - that is Explorer's and the window manager's work, done
    // for real input, and input here is posted messages. So in screen mode this helper is the missing half: it paints
    // a wallpaper and the worker's shortcuts behind the windows, and treats a press on a title bar, a border or a
    // caption button the way the window manager would. The taskbar is the viewer page's (drawn here it would cover the
    // foot of a maximised window); it asks for the windows with "tasks" and acts on one with "win".

    [DllImport("user32.dll")] static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
    [DllImport("user32.dll")] static extern bool ShowWindowAsync(IntPtr h, int cmd);
    [DllImport("user32.dll")] static extern bool IsIconic(IntPtr h);
    [DllImport("user32.dll")] static extern bool IsZoomed(IntPtr h);
    [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
    [DllImport("user32.dll")] static extern int GetWindowLong(IntPtr h, int index);
    [DllImport("user32.dll")] static extern uint GetClassLong(IntPtr h, int index);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] static extern bool SetForegroundWindow(IntPtr h);
    [DllImport("user32.dll")] static extern IntPtr SetActiveWindow(IntPtr h);
    [DllImport("user32.dll")] static extern uint GetDoubleClickTime();
    [DllImport("gdi32.dll")] static extern int GetObject(IntPtr h, int size, out BITMAP bm);
    [DllImport("gdi32.dll")] static extern bool DeleteObject(IntPtr h);
    [StructLayout(LayoutKind.Sequential)] struct BITMAP { public int bmType, bmWidth, bmHeight, bmWidthBytes; public short bmPlanes, bmBitsPixel; public IntPtr bmBits; }
    [StructLayout(LayoutKind.Sequential)] struct BITMAPINFOHEADER { public int biSize, biWidth, biHeight; public short biPlanes, biBitCount; public int biCompression, biSizeImage, biXPelsPerMeter, biYPelsPerMeter, biClrUsed, biClrImportant; }
    [StructLayout(LayoutKind.Sequential)] struct DIBSECTION { public BITMAP dsBm; public BITMAPINFOHEADER dsBmih; public uint mask0, mask1, mask2; public IntPtr dshSection; public uint dsOffset; }
    [DllImport("gdi32.dll", EntryPoint = "GetObject")] static extern int GetDib(IntPtr h, int size, out DIBSECTION dib);
    [StructLayout(LayoutKind.Sequential)] struct SIZE { public int cx, cy; }
    [ComImport, Guid("bcc18b79-ba16-442f-80c4-8a59c30c463b"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    interface IShellItemImageFactory { [PreserveSig] int GetImage(SIZE size, int flags, out IntPtr hbm); }
    [DllImport("shell32.dll", CharSet = CharSet.Unicode)]
    static extern int SHCreateItemFromParsingName(string path, IntPtr bindContext, [MarshalAs(UnmanagedType.LPStruct)] Guid iid, out IShellItemImageFactory item);

    class Shortcut { public string Id, Name; public Bitmap Img; public Rectangle Box; }
    static readonly object deskLock = new object();
    static List<Shortcut> shortcuts = new List<Shortcut>();
    static Bitmap backdrop;
    static string deskLabel = "";
    static int picked = -1; static long pickedAt;

    static double Dpi() { try { return GetDpiForSystem() / 96.0; } catch { return 1.0; } }

    // Where a program named without a folder is: beside Windows, or where it registered itself (App Paths).
    static string FindProgram(string exe) {
        string sys = System.IO.Path.Combine(Environment.SystemDirectory, exe);
        if (System.IO.File.Exists(sys)) return sys;
        foreach (var hive in new[] { Microsoft.Win32.Registry.CurrentUser, Microsoft.Win32.Registry.LocalMachine }) {
            try {
                using (var key = hive.OpenSubKey(@"Software\Microsoft\Windows\CurrentVersion\App Paths\" + exe)) {
                    string p = key == null ? null : key.GetValue(null) as string;
                    if (p != null) { p = Environment.ExpandEnvironmentVariables(p).Trim('"'); if (System.IO.File.Exists(p)) return p; }
                }
            } catch { }
        }
        return null;
    }

    // The icon Windows itself shows for a program, a shortcut or a Start-menu entry (shell:AppsFolder\...), at the size
    // asked for. Null when it has none to give; the shortcut is then drawn as its initial.
    static Bitmap ShellIcon(string source, int px) {
        if (string.IsNullOrEmpty(source)) return null;
        try {
            string path = Environment.ExpandEnvironmentVariables(source);
            if (path.IndexOf('\\') < 0 && path.IndexOf(':') < 0) path = FindProgram(path);
            if (path == null) return null;
            IShellItemImageFactory item;
            if (SHCreateItemFromParsingName(path, IntPtr.Zero, typeof(IShellItemImageFactory).GUID, out item) != 0 || item == null) return null;
            IntPtr hbm;
            int hr = item.GetImage(new SIZE { cx = px, cy = px }, 0x4 /* SIIGBF_ICONONLY */, out hbm);
            Marshal.ReleaseComObject(item);
            if (hr != 0 || hbm == IntPtr.Zero) return null;
            try {
                DIBSECTION dib;
                int got = GetDib(hbm, Marshal.SizeOf(typeof(DIBSECTION)), out dib);
                BITMAP bm = dib.dsBm;
                if (got == 0 || bm.bmBits == IntPtr.Zero || bm.bmBitsPixel != 32) return Image.FromHbitmap(hbm);
                // The pixels carry their own transparency, which Image.FromHbitmap would drop (a black square behind the icon).
                using (var view = new Bitmap(bm.bmWidth, bm.bmHeight, bm.bmWidthBytes, PixelFormat.Format32bppPArgb, bm.bmBits)) {
                    var copy = new Bitmap(view.Width, view.Height, PixelFormat.Format32bppPArgb);
                    using (var g = Graphics.FromImage(copy)) g.DrawImageUnscaled(view, 0, 0);
                    // A DIB's rows are stored last row first unless its height is written as negative: read as they
                    // lie in memory, such a picture is upside down.
                    if (got >= Marshal.SizeOf(typeof(DIBSECTION)) && dib.dsBmih.biHeight > 0) copy.RotateFlip(RotateFlipType.RotateNoneFlipY);
                    return copy;
                }
            } finally { DeleteObject(hbm); }
        } catch { return null; }
    }

    // The worker's shortcuts: "id<US>name<US>icon source", TAB between them. label is written in the wallpaper's corner.
    public static void SetShortcuts(string label, string spec) {
        int px = (int)Math.Round(48 * Dpi());
        var list = new List<Shortcut>();
        foreach (string entry in spec.Split('\t')) {
            string[] f = entry.Split('\u001f');
            if (f.Length < 2 || f[0].Length == 0) continue;
            list.Add(new Shortcut { Id = f[0], Name = f[1], Img = ShellIcon(f.Length > 2 ? f[2] : "", px) });
        }
        lock (deskLock) {
            foreach (var old in shortcuts) if (old.Img != null) old.Img.Dispose();
            shortcuts = list; deskLabel = label ?? ""; picked = -1;
            if (backdrop != null) { backdrop.Dispose(); backdrop = null; }
        }
        Kick();
    }

    static readonly Color[] BadgeColors = { Color.FromArgb(0x2f, 0x8f, 0x6b), Color.FromArgb(0x3b, 0x6f, 0xb6), Color.FromArgb(0x8a, 0x5c, 0xb8), Color.FromArgb(0xb8, 0x6a, 0x3b), Color.FromArgb(0xa8, 0x4a, 0x62) };

    // What is behind every window: the wallpaper, with the shortcuts on it. Kept until the shortcuts or the one picked
    // change, so a capture only copies it. Called with deskLock held.
    static Bitmap Backdrop(int w, int h) {
        if (backdrop != null && backdrop.Width == w && backdrop.Height == h) return backdrop;
        if (backdrop != null) backdrop.Dispose();
        var bmp = new Bitmap(w, h, PixelFormat.Format24bppRgb);
        float s = (float)Dpi();
        using (var g = Graphics.FromImage(bmp)) {
            g.SmoothingMode = SmoothingMode.AntiAlias;
            g.InterpolationMode = InterpolationMode.HighQualityBicubic;
            g.TextRenderingHint = System.Drawing.Text.TextRenderingHint.AntiAliasGridFit;
            // The wallpaper every Decurix desktop has (agent/decurix-wallpaper.svg: a black-to-red-to-yellow sweep, two
            // glows and the mark), drawn rather than loaded - GDI+ reads no SVG.
            float k = w / 1280f;
            using (var sweep = new LinearGradientBrush(new PointF(0, 0), new PointF(w, h), Color.Black, Color.White)) {
                sweep.InterpolationColors = new ColorBlend {
                    Colors = new[] { Color.FromArgb(0x05, 0x03, 0x02), Color.FromArgb(0x33, 0x05, 0x03), Color.FromArgb(0x7a, 0x0f, 0x07), Color.FromArgb(0xc2, 0x1f, 0x0c),
                                     Color.FromArgb(0xe8, 0x5f, 0x0f), Color.FromArgb(0xff, 0xb7, 0x03), Color.FromArgb(0xff, 0xe0, 0x66) },
                    Positions = new[] { 0f, 0.18f, 0.38f, 0.56f, 0.74f, 0.90f, 1f }
                };
                g.FillRectangle(sweep, 0, 0, w, h);
            }
            foreach (var glow in new[] { new { X = 154f, Y = -58f, R = 620f, C = Color.FromArgb(76, 0xff, 0xd6, 0x0a) }, new { X = 1344f, Y = 756f, R = 520f, C = Color.FromArgb(61, 0xff, 0x78, 0x14) } })
                using (var round = new GraphicsPath()) {
                    round.AddEllipse((glow.X - glow.R) * k, (glow.Y - glow.R) * k, 2 * glow.R * k, 2 * glow.R * k);
                    using (var light = new PathGradientBrush(round)) {
                        light.CenterColor = glow.C;
                        light.SurroundColors = new[] { Color.FromArgb(0, glow.C) };
                        g.FillPath(light, round);
                    }
                }
            Blossom(g, 970 * k, 470 * k, 24 * 11.5f * k, Color.FromArgb(14, 255, 255, 255), Color.FromArgb(18, 255, 255, 255));

            int cw = (int)(96 * s), ch = (int)(104 * s), icon = (int)Math.Round(48 * s), m = (int)(12 * s);
            if (barH == 0) LayoutBar();
            int rows = Math.Max(1, (h - barH - 2 * m) / ch);
            using (var f = new Font("Segoe UI", 12f * s, GraphicsUnit.Pixel))
            using (var white = new SolidBrush(Color.White))
            using (var shade = new SolidBrush(Color.FromArgb(170, 0, 0, 0)))
            using (var fmt = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Near, Trimming = StringTrimming.EllipsisCharacter }) {
                for (int i = 0; i < shortcuts.Count; i++) {
                    var sc = shortcuts[i];
                    sc.Box = new Rectangle(m + (i / rows) * cw, m + (i % rows) * ch, cw, ch);
                    if (i == picked) {
                        var box = Rectangle.Inflate(sc.Box, -(int)(3 * s), -(int)(3 * s));
                        using (var fill = new SolidBrush(Color.FromArgb(60, 255, 255, 255))) g.FillRectangle(fill, box);
                        using (var line = new Pen(Color.FromArgb(130, 255, 255, 255))) g.DrawRectangle(line, box);
                    }
                    var at = new Rectangle(sc.Box.X + (cw - icon) / 2, sc.Box.Y + (int)(10 * s), icon, icon);
                    if (sc.Img != null) g.DrawImage(sc.Img, at);
                    else {
                        using (var fill = new SolidBrush(BadgeColors[Math.Abs(sc.Name.GetHashCode() % BadgeColors.Length)])) g.FillEllipse(fill, at);
                        using (var big = new Font("Segoe UI", 24f * s, FontStyle.Bold, GraphicsUnit.Pixel))
                        using (var mid = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center })
                            g.DrawString(sc.Name.Length > 0 ? sc.Name.Substring(0, 1).ToUpper() : "?", big, white, at, mid);
                    }
                    var text = new RectangleF(sc.Box.X + 2, at.Bottom + 5 * s, cw - 4, sc.Box.Bottom - at.Bottom - 8 * s);
                    var under = text; under.Offset(1, 1);
                    g.DrawString(sc.Name, f, shade, under, fmt);
                    g.DrawString(sc.Name, f, white, text, fmt);
                }
            }
        }
        backdrop = bmp;
        return bmp;
    }

    // ---- The taskbar and the Start menu, drawn over the foot of the picture and laid out as Windows 11's are: a dark
    // bar with, in its middle, the Start button (the red tile of agent/decurix-start-icon.svg), a Search box and an icon
    // for each open window, and at the right the input language and the time over the date. Start and Search open one
    // panel - a search field over the worker's apps - and the clock opens a calendar. They are painted, not windows, so
    // they are in nobody's way but the viewer's: a press on them is the shell's and goes no further, and while the panel
    // is open the keyboard is its search field's.

    [DllImport("user32.dll")] static extern bool SystemParametersInfo(uint action, uint param, ref RECT r, uint ini);
    [DllImport("user32.dll", EntryPoint = "GetClassLongPtrW")] static extern IntPtr GetClassLongPtr(IntPtr h, int index);
    [DllImport("user32.dll")] static extern IntPtr GetKeyboardLayout(uint thread);

    class TaskButton { public IntPtr H; public bool Front; public string Title; public Rectangle Box; }
    // Open is a shortcut's id, or WebMark followed by the words to look up on the web.
    class MenuItem { public Rectangle Box; public string Open; }
    const char WebMark = '\u0001';
    const int HoverStart = -1, HoverNone = -2, HoverSearch = -3, HoverLang = -4, HoverClock = -5; // barHover, else a window button's index
    const int MaxQuery = 80;

    static int barH; static Rectangle barRect, startRect, searchRect, langRect, clockRect, menuRect, calRect, calPrev, calNext;
    static List<TaskButton> barButtons = new List<TaskButton>();
    static List<MenuItem> menuItems = new List<MenuItem>();
    static bool menuOpen, calOpen, shellDown; static int menuHover = -1, menuSel = -1, menuCols = 1, calMonth, barHover = HoverNone;
    static string query = "";
    static Bitmap barBmp, menuBmp, calBmp; static string barKey = "", menuKey = "", calKey = "";
    static string langTop, langBottom = "", langName = "";
    static readonly Dictionary<IntPtr, Bitmap> windowIcons = new Dictionary<IntPtr, Bitmap>();

    // Windows 11's dark theme: a near-black bar, lighter sheets above it, white text. The accent is Decurix's red.
    static readonly Color Panel = Color.FromArgb(0x20, 0x20, 0x20), Panel2 = Color.FromArgb(0x2d, 0x2d, 0x2d), Ink = Color.FromArgb(0xff, 0xff, 0xff),
                          Ink2 = Color.FromArgb(0xc5, 0xc5, 0xc5), Ink3 = Color.FromArgb(0x7d, 0x7d, 0x7d), Lit = Color.FromArgb(0x3d, 0x3d, 0x3d),
                          Sheet = Color.FromArgb(0x2b, 0x2b, 0x2b), Foot = Color.FromArgb(0x22, 0x22, 0x22), Field = Color.FromArgb(0x1c, 0x1c, 0x1c),
                          Glow = Color.FromArgb(22, 255, 255, 255), Red = Color.FromArgb(0xc2, 0x1f, 0x0c);

    // The Decurix mark: five petals and a centre, in a box of the given size.
    static void Blossom(Graphics g, float x, float y, float size, Color petals, Color centre) {
        float k = size / 24f;
        float[,] at = { { 12f, 7.25f }, { 16.52f, 10.53f }, { 14.79f, 15.84f }, { 9.21f, 15.84f }, { 7.48f, 10.53f } };
        using (var b = new SolidBrush(petals))
            for (int i = 0; i < 5; i++) g.FillEllipse(b, x + (at[i, 0] - 2.85f) * k, y + (at[i, 1] - 2.85f) * k, 5.7f * k, 5.7f * k);
        using (var b = new SolidBrush(centre)) g.FillEllipse(b, x + 9.8f * k, y + 9.8f * k, 4.4f * k, 4.4f * k);
    }

    static GraphicsPath Rounded(RectangleF r, float radius) {
        var p = new GraphicsPath(); float d = Math.Max(1f, Math.Min(radius * 2, Math.Min(r.Width, r.Height)));
        p.AddArc(r.X, r.Y, d, d, 180, 90); p.AddArc(r.Right - d, r.Y, d, d, 270, 90);
        p.AddArc(r.Right - d, r.Bottom - d, d, d, 0, 90); p.AddArc(r.X, r.Bottom - d, d, d, 90, 90);
        p.CloseFigure(); return p;
    }

    // A magnifying glass in a box of the given size: what says "search".
    static void Glass(Graphics g, float x, float y, float size, Color c) {
        float d = size * 0.66f;
        using (var pen = new Pen(c, Math.Max(1.4f, size / 9f)) { StartCap = LineCap.Round, EndCap = LineCap.Round }) {
            g.DrawEllipse(pen, x, y, d, d);
            g.DrawLine(pen, x + d * 0.86f, y + d * 0.86f, x + size, y + size);
        }
    }

    // A shortcut's picture, or its initial on a coloured disc when Windows has no picture of it.
    static void Emblem(Graphics g, Shortcut sc, RectangleF at) {
        if (sc.Img != null) { g.DrawImage(sc.Img, at); return; }
        using (var fill = new SolidBrush(BadgeColors[Math.Abs(sc.Name.GetHashCode() % BadgeColors.Length)])) g.FillEllipse(fill, at);
        using (var f = new Font("Segoe UI", at.Height * 0.5f, FontStyle.Bold, GraphicsUnit.Pixel)) using (var white = new SolidBrush(Color.White))
        using (var mid = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center })
            g.DrawString(sc.Name.Length > 0 ? sc.Name.Substring(0, 1).ToUpper() : "?", f, white, at, mid);
    }

    // Where the taskbar is. A maximised window fills this computer's work area, which leaves the height of the person's
    // own taskbar free at the foot of the screen: the worker's is drawn exactly there, so it covers nothing. With no
    // such gap (their taskbar hides itself, or is elsewhere) it has Windows 11's own height, over the foot of the picture.
    static void LayoutBar() {
        float s = (float)Dpi();
        var work = new RECT(); int gap = 0;
        try { if (SystemParametersInfo(0x30 /* SPI_GETWORKAREA */, 0, ref work, 0)) gap = screenH - work.B; } catch { }
        barH = gap >= (int)(30 * s) && gap <= (int)(80 * s) ? gap : (int)Math.Round(48 * s);
        barRect = new Rectangle(0, screenH - barH, screenW, barH);
        if (startRect.IsEmpty) startRect = new Rectangle(screenW / 2, barRect.Y, barH, barH);
    }

    // The small picture a window shows of itself: its own icon, else its program's.
    static Bitmap WindowIcon(IntPtr h, int px) {
        Bitmap have;
        if (windowIcons.TryGetValue(h, out have)) return have;
        if (windowIcons.Count > 64) windowIcons.Clear();
        Bitmap bmp = null;
        try {
            IntPtr icon = IntPtr.Zero, answer;
            foreach (int which in new[] { 1, 2, 0 }) // the big one first: it is drawn at 22 px and more
                if (icon == IntPtr.Zero && SendMessageTimeout(h, 0x7F /* WM_GETICON */, (IntPtr)which, IntPtr.Zero, 0x2, 100, out answer) != IntPtr.Zero) icon = answer;
            if (icon == IntPtr.Zero) icon = GetClassLongPtr(h, -14 /* GCLP_HICON */);
            if (icon == IntPtr.Zero) icon = GetClassLongPtr(h, -34 /* GCLP_HICONSM */);
            if (icon != IntPtr.Zero) using (var ic = Icon.FromHandle(icon)) bmp = ic.ToBitmap();
            else { int pid; GetWindowThreadProcessId(h, out pid); bmp = ShellIcon(Process.GetProcessById(pid).MainModule.FileName, px); }
        } catch { }
        windowIcons[h] = bmp;
        return bmp;
    }

    // The input language as Windows' taskbar writes it: "ENG" over "IN". It is the person's own - what they type
    // reaches the worker as the characters their keyboard made - so it is shown, not switched.
    static void ReadLanguage() {
        if (langTop != null) return;
        var ci = System.Globalization.CultureInfo.CurrentCulture;
        try { int id = (int)((long)GetKeyboardLayout(0) & 0xFFFF); if (id != 0) ci = new System.Globalization.CultureInfo(id); } catch { }
        try {
            langName = ci.DisplayName;
            langBottom = ci.IsNeutralCulture ? "" : new System.Globalization.RegionInfo(ci.LCID).TwoLetterISORegionName.ToUpperInvariant();
            langTop = ci.ThreeLetterISOLanguageName.ToUpperInvariant();
        } catch { langTop = "ENG"; }
    }

    // The worker's apps the search field's words find: names that begin with them first, then names with a word that
    // does, then names that hold them anywhere. Every app when nothing is typed.
    static List<Shortcut> Matches() {
        var found = new List<Shortcut>();
        string q = query.Trim().ToLowerInvariant();
        if (q.Length == 0) { found.AddRange(shortcuts); return found; }
        for (int pass = 0; pass < 3; pass++)
            foreach (var sc in shortcuts) {
                string name = sc.Name.ToLowerInvariant();
                int rank = name.StartsWith(q, StringComparison.Ordinal) ? 0 : (" " + name).IndexOf(" " + q, StringComparison.Ordinal) >= 0 ? 1
                         : name.IndexOf(q, StringComparison.Ordinal) >= 0 ? 2 : 3;
                if (rank == pass) found.Add(sc);
            }
        return found;
    }

    // What the open panel offers, in order: the apps found, and - when something is typed - looking it up on the web.
    // Worked out from the words alone, never from what was last drawn: Enter can follow a letter before the next picture.
    static List<string> Offers() {
        var offers = new List<string>();
        foreach (var sc in Matches()) offers.Add(sc.Id);
        if (query.Trim().Length > 0) offers.Add(WebMark + query.Trim());
        return offers;
    }

    static void Announce(string open) {
        if (string.IsNullOrEmpty(open)) return;
        if (open[0] == WebMark) Reply("EVENT web " + open.Substring(1)); else Reply("EVENT open " + open);
    }

    static void OpenMenu() { menuOpen = true; calOpen = false; query = ""; menuSel = -1; menuHover = -1; }

    // Draws the taskbar (and the Start panel or the calendar while one is open) over the picture. Each is kept as a
    // bitmap until what it shows changes, so a capture only copies it. Called with deskLock held.
    static void DrawShell(Graphics g) {
        if (barH == 0) LayoutBar();
        float s = (float)Dpi();
        ReadLanguage();

        var list = new List<IntPtr>();
        if (desktop != IntPtr.Zero) Enum(TaskWindow, list);
        var key = new StringBuilder();
        var titles = new List<string>(); var mins = new List<bool>();
        foreach (IntPtr h in list) {
            var title = new StringBuilder(200); GetWindowText(h, title, 200);
            titles.Add(title.ToString()); mins.Add(IsIconic(h));
            key.Append(h.ToInt64()).Append(mins[mins.Count - 1] ? 'm' : '-').Append(title).Append('\n');
        }
        // The time and the date as this computer writes them.
        string clock = DateTime.Now.ToString("t"), day = DateTime.Now.ToString("d");
        key.Append(clock).Append(day).Append(barHover).Append(menuOpen).Append(calOpen);

        if (barBmp == null || key.ToString() != barKey || barBmp.Width != barRect.Width) {
            barKey = key.ToString();
            if (barBmp != null) barBmp.Dispose();
            barBmp = new Bitmap(barRect.Width, barRect.Height, PixelFormat.Format24bppRgb);
            barButtons.Clear();
            using (var b = Graphics.FromImage(barBmp)) {
                b.SmoothingMode = SmoothingMode.AntiAlias;
                b.InterpolationMode = InterpolationMode.HighQualityBicubic;
                b.TextRenderingHint = System.Drawing.Text.TextRenderingHint.ClearTypeGridFit;
                b.Clear(Panel);
                using (var edge = new Pen(Lit)) b.DrawLine(edge, 0, 0, barRect.Width, 0);
                using (var hover = new SolidBrush(Glow)) using (var field = new SolidBrush(Panel2)) using (var red = new SolidBrush(Red))
                using (var ink = new SolidBrush(Ink)) using (var dim = new SolidBrush(Ink2)) using (var rim = new Pen(Lit)) {
                    float cell = barRect.Height, pad = 5 * s, tile = cell - 2 * pad;

                    // At the right: the language, then the time over the date.
                    int clockW = (int)(96 * s), langW = (int)(46 * s);
                    clockRect = new Rectangle(barRect.Width - clockW - (int)(6 * s), barRect.Y, clockW, barRect.Height);
                    langRect = new Rectangle(clockRect.X - langW - (int)(2 * s), barRect.Y, langW, barRect.Height);
                    foreach (var lit in new[] { new { On = barHover == HoverClock || calOpen, Box = clockRect }, new { On = barHover == HoverLang, Box = langRect } })
                        if (lit.On) using (var p = Rounded(new RectangleF(lit.Box.X, pad - 1 * s, lit.Box.Width, tile + 2 * s), 5 * s)) b.FillPath(hover, p);
                    using (var right = new StringFormat { Alignment = StringAlignment.Far, LineAlignment = StringAlignment.Center, FormatFlags = StringFormatFlags.NoWrap })
                    using (var centre = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center, FormatFlags = StringFormatFlags.NoWrap })
                    using (var small = new Font("Segoe UI", 12f * s, GraphicsUnit.Pixel)) {
                        float half = barRect.Height / 2f;
                        b.DrawString(clock, small, ink, new RectangleF(clockRect.X, 2 * s, clockRect.Width - 8 * s, half), right);
                        b.DrawString(day, small, ink, new RectangleF(clockRect.X, half - 2 * s, clockRect.Width - 8 * s, half), right);
                        if (langBottom.Length == 0) b.DrawString(langTop, small, ink, new RectangleF(langRect.X, 0, langRect.Width, barRect.Height), centre);
                        else {
                            b.DrawString(langTop, small, ink, new RectangleF(langRect.X, 2 * s, langRect.Width, half), centre);
                            b.DrawString(langBottom, small, ink, new RectangleF(langRect.X, half - 2 * s, langRect.Width, half), centre);
                        }
                    }

                    // In the middle: Start, the Search box, then an icon for each window.
                    float trayLeft = langRect.X - 8 * s;
                    bool wide = barRect.Width >= 900 * s;
                    float searchW = wide ? 190 * s : cell, gap = 6 * s;
                    float total = cell + searchW + gap + list.Count * cell;
                    float x = Math.Max(8 * s, Math.Min((barRect.Width - total) / 2f, trayLeft - total));
                    int icon = (int)Math.Round(24 * s);

                    startRect = new Rectangle((int)x, barRect.Y, (int)cell, barRect.Height);
                    var box = new RectangleF(x + pad, pad, tile, tile);
                    if (barHover == HoverStart || menuOpen) using (var p = Rounded(RectangleF.Inflate(box, 2 * s, 2 * s), 6 * s)) b.FillPath(hover, p);
                    using (var p = Rounded(RectangleF.Inflate(box, -2 * s, -2 * s), 6 * s)) b.FillPath(red, p);
                    float mark = (tile - 4 * s) * 0.82f;
                    Blossom(b, box.X + (tile - mark) / 2, box.Y + (tile - mark) / 2, mark, Color.FromArgb(200, 255, 255, 255), Color.White);
                    x += cell;

                    searchRect = new Rectangle((int)x, barRect.Y, (int)searchW, barRect.Height);
                    float glass = 13 * s;
                    if (wide) {
                        var pill = new RectangleF(x + 3 * s, pad + 1 * s, searchW - 6 * s, tile - 2 * s);
                        using (var p = Rounded(pill, pill.Height / 2)) { b.FillPath(field, p); if (barHover == HoverSearch) b.FillPath(hover, p); b.DrawPath(rim, p); }
                        Glass(b, pill.X + 13 * s, pill.Y + (pill.Height - glass) / 2, glass, Ink2);
                        using (var f = new Font("Segoe UI", 13f * s, GraphicsUnit.Pixel)) using (var mid = new StringFormat { LineAlignment = StringAlignment.Center })
                            b.DrawString("Search", f, dim, new RectangleF(pill.X + 34 * s, pill.Y, pill.Width - 40 * s, pill.Height), mid);
                    } else {
                        if (barHover == HoverSearch) using (var p = Rounded(new RectangleF(x + pad, pad, tile, tile), 6 * s)) b.FillPath(hover, p);
                        Glass(b, x + (cell - glass) / 2, (cell - glass) / 2, glass, Ink);
                    }
                    x += searchW + gap;

                    bool first = true;
                    for (int i = 0; i < list.Count && x + cell < trayLeft; i++, x += cell) {
                        box = new RectangleF(x + pad, pad, tile, tile);
                        bool isFront = first && !mins[i];
                        if (!mins[i]) first = false;
                        barButtons.Add(new TaskButton { H = list[i], Front = isFront, Title = titles[i], Box = new Rectangle((int)x, 0, (int)cell, barRect.Height) });
                        if (isFront || barHover == barButtons.Count - 1) using (var p = Rounded(box, 5 * s)) b.FillPath(hover, p);
                        // A pill under a window that is open, longer and in the accent under the one in front - how Windows says "running".
                        float line = isFront ? 16 * s : 6 * s;
                        using (var mark2 = new SolidBrush(isFront ? Red : Ink3)) using (var p = Rounded(new RectangleF(box.X + (tile - line) / 2, barRect.Height - 5 * s, line, 3 * s), 1.5f * s)) b.FillPath(mark2, p);
                        Bitmap pic = WindowIcon(list[i], icon);
                        var at = new RectangleF(box.X + (tile - icon) / 2, box.Y + (tile - icon) / 2 - 1 * s, icon, icon);
                        if (pic != null) b.DrawImage(pic, at);
                        else using (var p = Rounded(at, 5 * s)) b.FillPath(dim, p);
                    }
                }
            }
        }
        g.DrawImageUnscaled(barBmp, barRect.X, barRect.Y);

        // The name of what the pointer is on, above it: the buttons are icons only, and the tray's are abbreviations.
        string tipText = null; float tipAt = 0;
        if (!menuOpen && !calOpen) {
            if (barHover >= 0 && barHover < barButtons.Count) { tipText = barButtons[barHover].Title; tipAt = barButtons[barHover].Box.X + barButtons[barHover].Box.Width / 2f; }
            else if (barHover == HoverClock) { tipText = DateTime.Now.ToString("D"); tipAt = clockRect.X + clockRect.Width / 2f; }
            else if (barHover == HoverLang) { tipText = langName; tipAt = langRect.X + langRect.Width / 2f; }
        }
        if (!string.IsNullOrEmpty(tipText)) {
            using (var f = new Font("Segoe UI", 12f * s, GraphicsUnit.Pixel)) {
                string title = tipText.Length > 60 ? tipText.Substring(0, 57) + "..." : tipText;
                SizeF size = g.MeasureString(title, f);
                float tw = size.Width + 14 * s, th = size.Height + 8 * s;
                float tx = Math.Max(4 * s, Math.Min(barRect.Width - tw - 4 * s, tipAt - tw / 2));
                var tip = new RectangleF(tx, barRect.Y - th - 6 * s, tw, th);
                g.SmoothingMode = SmoothingMode.AntiAlias;
                g.TextRenderingHint = System.Drawing.Text.TextRenderingHint.AntiAliasGridFit;
                using (var p = Rounded(tip, 5 * s)) using (var fill = new SolidBrush(Color.FromArgb(245, Sheet))) using (var rim = new Pen(Lit)) { g.FillPath(fill, p); g.DrawPath(rim, p); }
                using (var ink = new SolidBrush(Ink)) g.DrawString(title, f, ink, tip.X + 7 * s, tip.Y + 4 * s);
            }
        }

        if (menuOpen) DrawStart(g, s);
        if (calOpen) DrawCalendar(g, s);
    }

    // The Start panel: a search field, then the worker's apps as a grid ("Pinned") - or, once something is typed, what
    // it finds, best match first, and the web - and the worker's name at the foot.
    static void DrawStart(Graphics g, float s) {
        var found = Matches();
        string words = query.Trim();
        bool searching = words.Length > 0;
        var mkey = query + "\n" + menuSel + ":" + menuHover + ":" + shortcuts.Count + ":" + (shortcuts.Count > 0 ? shortcuts[0].Id : "") + ":" + deskLabel + ":" + barRect.Y;
        if (menuBmp == null || mkey != menuKey) {
            menuKey = mkey;
            if (menuBmp != null) menuBmp.Dispose();
            int width = Math.Min((int)(600 * s), screenW - (int)(24 * s)), height = Math.Min((int)(640 * s), barRect.Y - (int)(24 * s));
            menuRect = new Rectangle((screenW - width) / 2, barRect.Y - (int)(12 * s) - height, width, height);
            menuBmp = new Bitmap(width, height, PixelFormat.Format32bppPArgb);
            menuItems.Clear();
            using (var b = Graphics.FromImage(menuBmp)) {
                b.SmoothingMode = SmoothingMode.AntiAlias;
                b.InterpolationMode = InterpolationMode.HighQualityBicubic;
                b.TextRenderingHint = System.Drawing.Text.TextRenderingHint.AntiAliasGridFit;
                int m = (int)(28 * s), foot = (int)(60 * s);
                var all = new RectangleF(0.5f, 0.5f, width - 1, height - 1);
                using (var p = Rounded(all, 9 * s)) using (var fill = new SolidBrush(Sheet)) using (var under = new SolidBrush(Foot)) using (var line = new Pen(Lit)) {
                    b.FillPath(fill, p);
                    b.SetClip(p);
                    b.FillRectangle(under, 0, height - foot, width, foot);
                    b.DrawLine(line, 0, height - foot, width, height - foot);
                    b.ResetClip();
                    b.DrawPath(line, p);
                }
                using (var ink = new SolidBrush(Ink)) using (var dim = new SolidBrush(Ink2)) using (var faint = new SolidBrush(Ink3))
                using (var hover = new SolidBrush(Glow)) using (var chosen = new SolidBrush(Lit)) using (var rim = new Pen(Lit))
                using (var f = new Font("Segoe UI", 14f * s, GraphicsUnit.Pixel))
                using (var small = new Font("Segoe UI", 12f * s, GraphicsUnit.Pixel))
                using (var bold = new Font("Segoe UI", 13.5f * s, FontStyle.Bold, GraphicsUnit.Pixel))
                using (var mid = new StringFormat { LineAlignment = StringAlignment.Center, Trimming = StringTrimming.EllipsisCharacter, FormatFlags = StringFormatFlags.NoWrap })
                using (var centred = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Near, Trimming = StringTrimming.EllipsisCharacter }) {
                    // The search field, with the keyboard: what is typed while the panel is open goes here.
                    var field = new RectangleF(m, 26 * s, width - 2 * m, 38 * s);
                    using (var p = Rounded(field, field.Height / 2)) using (var fill = new SolidBrush(Field)) using (var lit = new Pen(Red, 1.5f * s)) { b.FillPath(fill, p); b.DrawPath(searching ? lit : rim, p); }
                    float glass = 14 * s;
                    Glass(b, field.X + 15 * s, field.Y + (field.Height - glass) / 2, glass, Ink2);
                    var text = new RectangleF(field.X + 40 * s, field.Y, field.Width - 56 * s, field.Height);
                    float caret = text.X + 2 * s;
                    if (query.Length == 0) b.DrawString("Search for apps and the web", f, faint, text, mid);
                    else {
                        b.DrawString(query, f, ink, text, mid);
                        using (var fit = new StringFormat(StringFormat.GenericTypographic) { FormatFlags = StringFormatFlags.MeasureTrailingSpaces })
                            caret = Math.Min(text.Right, text.X + 3 * s + b.MeasureString(query, f, 4000, fit).Width);
                    }
                    using (var bar = new Pen(Ink, Math.Max(1f, s))) b.DrawLine(bar, caret, field.Y + 10 * s, caret, field.Bottom - 10 * s);

                    float top = field.Bottom + 22 * s, bottom = height - foot - 10 * s;
                    b.DrawString(searching ? (found.Count > 0 ? "Best match" : "Nothing here is called that") : "Pinned", bold, ink, m + 4 * s, top);
                    top += 32 * s;

                    if (!searching) {
                        const int cols = 6;
                        menuCols = cols;
                        float cw = (width - 2 * m) / (float)cols, ch = 90 * s, icon = 34 * s;
                        if (found.Count == 0) b.DrawString("No apps have been added to this worker yet.", f, dim, m + 4 * s, top + 4 * s);
                        for (int i = 0; i < found.Count; i++) {
                            float y = top + (i / cols) * ch;
                            if (y + ch > bottom) break;
                            var box = new Rectangle((int)(m + (i % cols) * cw), (int)y, (int)cw, (int)ch);
                            menuItems.Add(new MenuItem { Box = box, Open = found[i].Id });
                            if (i == menuSel || i == menuHover) using (var p = Rounded(RectangleF.Inflate(box, -3 * s, -3 * s), 6 * s)) b.FillPath(i == menuSel ? chosen : hover, p);
                            Emblem(b, found[i], new RectangleF(box.X + (cw - icon) / 2, box.Y + 12 * s, icon, icon));
                            b.DrawString(found[i].Name, small, ink, new RectangleF(box.X + 4 * s, box.Y + 12 * s + icon + 7 * s, cw - 8 * s, ch - icon - 24 * s), centred);
                        }
                    } else {
                        menuCols = 1;
                        float row = 54 * s, icon = 32 * s, y = top;
                        // Room is kept for the web's heading and row under the apps found.
                        for (int i = 0; i < found.Count && y + row <= bottom - row - 34 * s; i++, y += row) {
                            var box = new Rectangle(m, (int)y, width - 2 * m, (int)row);
                            menuItems.Add(new MenuItem { Box = box, Open = found[i].Id });
                            if (i == menuSel || i == menuHover) using (var p = Rounded(RectangleF.Inflate(box, 0, -2 * s), 6 * s)) b.FillPath(i == menuSel ? chosen : hover, p);
                            Emblem(b, found[i], new RectangleF(box.X + 12 * s, box.Y + (row - icon) / 2, icon, icon));
                            b.DrawString(found[i].Name, f, ink, new RectangleF(box.X + 56 * s, box.Y + 6 * s, box.Width - 66 * s, row / 2), mid);
                            b.DrawString("App", small, dim, new RectangleF(box.X + 56 * s, box.Y + row / 2 - 3 * s, box.Width - 66 * s, row / 2 - 4 * s), mid);
                        }
                        if (found.Count > 0) { b.DrawString("Search the web", bold, ink, m + 4 * s, y + 8 * s); y += 34 * s; }
                        int web = found.Count; // its place among what the panel offers (Offers), whatever fitted above it
                        var line = new Rectangle(m, (int)y, width - 2 * m, (int)row);
                        menuItems.Add(new MenuItem { Box = line, Open = WebMark + words });
                        if (web == menuSel || menuItems.Count - 1 == menuHover) using (var p = Rounded(RectangleF.Inflate(line, 0, -2 * s), 6 * s)) b.FillPath(web == menuSel ? chosen : hover, p);
                        var disc = new RectangleF(line.X + 12 * s, line.Y + (row - icon) / 2, icon, icon);
                        using (var fill = new SolidBrush(Lit)) b.FillEllipse(fill, disc);
                        Glass(b, disc.X + 9.5f * s, disc.Y + 9.5f * s, 13 * s, Ink);
                        b.DrawString(words, f, ink, new RectangleF(line.X + 56 * s, line.Y + 6 * s, line.Width - 66 * s, row / 2), mid);
                        b.DrawString("Look this up in the worker's browser", small, dim, new RectangleF(line.X + 56 * s, line.Y + row / 2 - 3 * s, line.Width - 66 * s, row / 2 - 4 * s), mid);
                    }

                    // Whose desktop this is, where Windows names the person signed in.
                    var face = new RectangleF(m + 2 * s, height - foot + (foot - 30 * s) / 2, 30 * s, 30 * s);
                    using (var fill = new SolidBrush(Red)) b.FillEllipse(fill, face);
                    using (var one = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center })
                        b.DrawString(deskLabel.Length > 0 ? deskLabel.Substring(0, 1).ToUpper() : "W", bold, ink, face, one);
                    b.DrawString(deskLabel.Length > 0 ? deskLabel : "Worker", f, ink, new RectangleF(face.Right + 12 * s, height - foot, width - face.Right - m - 12 * s, foot), mid);
                }
            }
        }
        g.DrawImage(menuBmp, menuRect.X, menuRect.Y, menuRect.Width, menuRect.Height);
    }

    // The calendar the clock opens: today's date in full, and a month that can be paged through.
    static void DrawCalendar(Graphics g, float s) {
        DateTime today = DateTime.Today, month = new DateTime(today.Year, today.Month, 1).AddMonths(calMonth);
        string ckey = today.ToString("yyyyMMdd") + ":" + calMonth + ":" + barRect.Y;
        if (calBmp == null || ckey != calKey) {
            calKey = ckey;
            if (calBmp != null) calBmp.Dispose();
            int m = (int)(14 * s), head = (int)(52 * s), bar = (int)(44 * s), names = (int)(28 * s), cell = (int)(40 * s);
            int width = 7 * cell + 2 * m, height = head + bar + names + 6 * cell + m;
            calRect = new Rectangle(screenW - width - (int)(12 * s), barRect.Y - (int)(12 * s) - height, width, height);
            calBmp = new Bitmap(width, height, PixelFormat.Format32bppPArgb);
            using (var b = Graphics.FromImage(calBmp)) {
                b.SmoothingMode = SmoothingMode.AntiAlias;
                b.TextRenderingHint = System.Drawing.Text.TextRenderingHint.AntiAliasGridFit;
                var all = new RectangleF(0.5f, 0.5f, width - 1, height - 1);
                using (var p = Rounded(all, 9 * s)) using (var fill = new SolidBrush(Sheet)) using (var line = new Pen(Lit)) { b.FillPath(fill, p); b.DrawPath(line, p); b.DrawLine(line, 0, head, width, head); }
                using (var ink = new SolidBrush(Ink)) using (var dim = new SolidBrush(Ink2)) using (var faint = new SolidBrush(Ink3)) using (var red = new SolidBrush(Red))
                using (var f = new Font("Segoe UI", 13f * s, GraphicsUnit.Pixel)) using (var bold = new Font("Segoe UI", 13.5f * s, FontStyle.Bold, GraphicsUnit.Pixel))
                using (var mid = new StringFormat { LineAlignment = StringAlignment.Center, FormatFlags = StringFormatFlags.NoWrap })
                using (var centre = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center })
                using (var pen = new Pen(Ink2, 1.6f * s) { StartCap = LineCap.Round, EndCap = LineCap.Round, LineJoin = LineJoin.Round }) {
                    b.DrawString(today.ToString("D"), f, ink, new RectangleF(m + 4 * s, 0, width - 2 * m, head), mid);
                    b.DrawString(month.ToString("Y"), bold, ink, new RectangleF(m + 4 * s, head, width - 2 * m, bar), mid);
                    // Up for the month before and down for the one after, as Windows has them.
                    int arrow = (int)(30 * s);
                    calNext = new Rectangle(width - m - arrow, head + (bar - arrow) / 2, arrow, arrow);
                    calPrev = new Rectangle(calNext.X - arrow - (int)(4 * s), calNext.Y, arrow, arrow);
                    foreach (var way in new[] { new { Box = calPrev, Up = true }, new { Box = calNext, Up = false } }) {
                        float cx = way.Box.X + arrow / 2f, cy = way.Box.Y + arrow / 2f, r = 4.5f * s, d = way.Up ? r / 2 : -r / 2;
                        b.DrawLines(pen, new[] { new PointF(cx - r, cy + d), new PointF(cx, cy - d), new PointF(cx + r, cy + d) });
                    }
                    var fmt = System.Globalization.CultureInfo.CurrentCulture.DateTimeFormat;
                    int firstDay = (int)fmt.FirstDayOfWeek;
                    for (int i = 0; i < 7; i++)
                        b.DrawString(fmt.ShortestDayNames[(firstDay + i) % 7], f, dim, new RectangleF(m + i * cell, head + bar, cell, names), centre);
                    DateTime from = month.AddDays(-(((int)month.DayOfWeek - firstDay + 7) % 7));
                    for (int i = 0; i < 42; i++) {
                        DateTime d = from.AddDays(i);
                        var box = new RectangleF(m + (i % 7) * cell, head + bar + names + (i / 7) * cell, cell, cell);
                        if (d == today) b.FillEllipse(red, RectangleF.Inflate(box, -4 * s, -4 * s));
                        b.DrawString(d.Day.ToString(), f, d == today ? ink : d.Month == month.Month ? ink : faint, box, centre);
                    }
                }
            }
        }
        g.DrawImage(calBmp, calRect.X, calRect.Y, calRect.Width, calRect.Height);
    }

    // A press on the taskbar, the Start panel or the calendar. "OK" when the press was the shell's: the Start button
    // opens and shuts the panel and the Search box opens it, an app or a row of the panel opens that, the clock opens and
    // shuts the calendar, a window's icon brings it to the front (or minimises the one already there), and a press
    // anywhere else while something is open only puts it away.
    static string ShellPress(POINT p, bool right) {
        string open = null; IntPtr raise = IntPtr.Zero, min = IntPtr.Zero; bool mine = false;
        lock (deskLock) {
            if (barH == 0) LayoutBar();
            if (menuOpen && menuRect.Contains(p.X, p.Y)) {
                for (int i = 0; i < menuItems.Count && !right; i++)
                    if (menuItems[i].Box.Contains(p.X - menuRect.X, p.Y - menuRect.Y)) open = menuItems[i].Open;
                if (open != null) menuOpen = false;
                mine = true;
            } else if (calOpen && calRect.Contains(p.X, p.Y)) {
                if (!right && calPrev.Contains(p.X - calRect.X, p.Y - calRect.Y)) calMonth--;
                if (!right && calNext.Contains(p.X - calRect.X, p.Y - calRect.Y)) calMonth++;
                mine = true;
            } else if (startRect.Contains(p.X, p.Y)) {
                if (!right) { if (menuOpen) menuOpen = false; else OpenMenu(); }
                mine = true;
            } else if (searchRect.Contains(p.X, p.Y)) {
                if (!right && !menuOpen) OpenMenu();
                mine = true;
            } else if (clockRect.Contains(p.X, p.Y)) {
                if (!right) { calOpen = !calOpen; calMonth = 0; menuOpen = false; }
                mine = true;
            } else {
                mine = menuOpen || calOpen;
                menuOpen = false; calOpen = false;
                if (barRect.Contains(p.X, p.Y)) {
                    mine = true;
                    foreach (var b in barButtons) {
                        if (right || !b.Box.Contains(p.X - barRect.X, p.Y - barRect.Y)) continue;
                        if (b.Front) min = b.H; else raise = b.H;
                    }
                }
            }
            if (mine) { menuHover = -1; shellDown = true; }
        }
        Announce(open);
        if (raise != IntPtr.Zero) Activate(raise);
        if (min != IntPtr.Zero) ShowWindowAsync(min, 6 /* SW_MINIMIZE */);
        return mine ? "OK" : null;
    }

    // The pointer over the taskbar, the open panel or the calendar lights what it is on; true when it is over any of
    // them, so the move goes to no window underneath.
    static bool ShellHover(POINT p) {
        lock (deskLock) {
            if (barH == 0) LayoutBar();
            int bar = HoverNone, item = -1;
            bool inMenu = menuOpen && menuRect.Contains(p.X, p.Y), inCal = calOpen && calRect.Contains(p.X, p.Y);
            if (inMenu) { for (int i = 0; i < menuItems.Count; i++) if (menuItems[i].Box.Contains(p.X - menuRect.X, p.Y - menuRect.Y)) item = i; }
            else if (inCal) { }
            else if (startRect.Contains(p.X, p.Y)) bar = HoverStart;
            else if (searchRect.Contains(p.X, p.Y)) bar = HoverSearch;
            else if (clockRect.Contains(p.X, p.Y)) bar = HoverClock;
            else if (langRect.Contains(p.X, p.Y)) bar = HoverLang;
            else if (barRect.Contains(p.X, p.Y)) { for (int i = 0; i < barButtons.Count; i++) if (barButtons[i].Box.Contains(p.X - barRect.X, p.Y - barRect.Y)) bar = i; }
            barHover = bar; menuHover = item;
            return inMenu || inCal || barRect.Contains(p.X, p.Y);
        }
    }

    // Opens the Start panel, or puts it away: what the Start key does.
    public static void ToggleStart() {
        lock (deskLock) { if (menuOpen) menuOpen = false; else OpenMenu(); }
        Kick();
    }

    // Whether what is typed now goes to the Start panel's search field and not to a window.
    public static bool ShellTyping() { lock (deskLock) return fixedScreen && menuOpen; }

    // A character typed while the Start panel is open is its search field's. True when the shell took it.
    static bool ShellChar(char ch) {
        lock (deskLock) {
            if (!fixedScreen || !menuOpen) return false;
            if (ch >= ' ' && ch != 127 && query.Length < MaxQuery) { query += ch; menuSel = query.Trim().Length > 0 ? 0 : -1; }
            return true;
        }
    }

    // A key pressed while the Start panel is open: Backspace edits the search, the arrows move through what is
    // offered, Enter opens the one chosen, Escape clears the search and then puts the panel away. Escape also puts the
    // calendar away. True when the shell took the key.
    static bool ShellKey(int vk) {
        string open = null;
        lock (deskLock) {
            if (!fixedScreen) return false;
            if (!menuOpen) { if (calOpen && vk == 27) { calOpen = false; return true; } return false; }
            var offers = Offers();
            int step = vk == 38 ? -menuCols : vk == 40 ? menuCols : vk == 37 ? -1 : vk == 39 ? 1 : 0;
            if (vk == 27) { if (query.Length > 0) { query = ""; menuSel = -1; } else menuOpen = false; }
            else if (vk == 8) { if (query.Length > 0) query = query.Substring(0, query.Length - 1); menuSel = query.Trim().Length > 0 ? 0 : -1; }
            else if (vk == 13) { if (menuSel >= 0 && menuSel < offers.Count) { open = offers[menuSel]; menuOpen = false; } }
            else if (step != 0 && offers.Count > 0) {
                if (menuSel < 0) menuSel = 0;
                else if (menuSel + step >= 0 && menuSel + step < offers.Count) menuSel += step;
            }
        }
        Announce(open);
        return true;
    }

    static bool OverShell(POINT p) {
        lock (deskLock) {
            if (barH == 0) LayoutBar();
            return barRect.Contains(p.X, p.Y) || (menuOpen && menuRect.Contains(p.X, p.Y)) || (calOpen && calRect.Contains(p.X, p.Y));
        }
    }

    // A press on the wallpaper: it picks the shortcut under it, and a second press there within a double-click opens it -
    // which is said to the control plane ("EVENT open <id>"), since what a shortcut starts is the control plane's to know.
    static void DesktopPress(POINT p) {
        string open = null;
        lock (deskLock) {
            Backdrop(screenW, screenH); // where each icon is is worked out when the wallpaper is drawn
            int hit = -1;
            for (int i = 0; i < shortcuts.Count; i++) if (shortcuts[i].Box.Contains(p.X, p.Y)) hit = i;
            long now = Stopwatch.GetTimestamp();
            if (hit >= 0 && hit == picked && (now - pickedAt) * 1000 / Stopwatch.Frequency <= GetDoubleClickTime()) { open = shortcuts[hit].Id; pickedAt = 0; }
            else pickedAt = now;
            if (hit != picked) { picked = hit; if (backdrop != null) { backdrop.Dispose(); backdrop = null; } }
        }
        if (open != null) Reply("EVENT open " + open);
    }

    // "window" where a window shows this point of the picture, "desktop" where only the wallpaper does.
    public static string Over(int x, int y) {
        EnsureGeometry();
        POINT p = ScreenPoint(x, y);
        return (!fixedScreen || !OverShell(p)) && WindowAt(p) != IntPtr.Zero ? "window" : "desktop";
    }

    // Brings a window to the front and gives it the keyboard, restoring it if it was minimised - what a click on a
    // window or on its taskbar button does. Windows does this only for real input, and only lets a thread of the same
    // desktop, joined to the window's input queue, do it instead.
    static void Activate(IntPtr top) {
        var th = new Thread(() => {
            if (!SetThreadDesktop(desktop)) return;
            int pid; uint owner = GetWindowThreadProcessId(top, out pid);
            uint me = GetCurrentThreadId();
            bool joined = AttachThreadInput(me, owner, true);
            try {
                if (IsIconic(top)) ShowWindowAsync(top, 9 /* SW_RESTORE */);
                // BringWindowToTop says yes and leaves another program's window where it was (measured); putting it
                // above everything and then back among ordinary windows does move it.
                bool always = (GetWindowLong(top, -20 /* GWL_EXSTYLE */) & 0x8 /* WS_EX_TOPMOST */) != 0;
                SetWindowPos(top, (IntPtr)(-1) /* HWND_TOPMOST */, 0, 0, 0, 0, 0x1 | 0x2 | 0x10 /* NOSIZE, NOMOVE, NOACTIVATE */);
                if (!always) SetWindowPos(top, (IntPtr)(-2) /* HWND_NOTOPMOST */, 0, 0, 0, 0, 0x1 | 0x2 | 0x10);
                SetForegroundWindow(top); SetActiveWindow(top);
            } finally { if (joined) AttachThreadInput(me, owner, false); }
        }) { IsBackground = true };
        th.Start(); th.Join(500); // an app that has stopped answering must not take the session with it
    }

    // What part of a window a point is on, in the window's own words (WM_NCHITTEST): 1 its contents, 2 its title bar,
    // 8/9/20 the minimise, maximise and close buttons, 10-17 its borders.
    static int HitTest(IntPtr top, POINT p) {
        double sc = Scale(top);
        int x = (int)Math.Round(p.X * sc), y = (int)Math.Round(p.Y * sc);
        IntPtr answer;
        if (SendMessageTimeout(top, 0x0084, IntPtr.Zero, (IntPtr)(((y & 0xFFFF) << 16) | (x & 0xFFFF)), 0x2, 200, out answer) == IntPtr.Zero) return 1;
        return (int)(long)answer;
    }

    static void Caption(IntPtr w, int hit) {
        int command = hit == 20 ? 0xF060 /* SC_CLOSE */ : hit == 8 ? 0xF020 /* SC_MINIMIZE */ : IsZoomed(w) ? 0xF120 /* SC_RESTORE */ : 0xF030 /* SC_MAXIMIZE */;
        PostMessage(w, 0x0112 /* WM_SYSCOMMAND */, (IntPtr)command, IntPtr.Zero);
    }

    static IntPtr dragWin, pressWin; static int dragHit, pressHit; static POINT dragFrom; static RECT dragRect;

    // The window manager's share of a mouse event on a worker's desktop. "OK" when it was all there was to do; null when
    // the event is for the window's own contents.
    static string Manage(string kind, POINT p, bool right, bool twice) {
        if (kind == "move") {
            if (dragWin == IntPtr.Zero) return captured == IntPtr.Zero && ShellHover(p) ? "OK" : null;
            int dx = p.X - dragFrom.X, dy = p.Y - dragFrom.Y;
            int l = dragRect.L, t = dragRect.T, r = dragRect.R, b = dragRect.B;
            bool left = dragHit == 10 || dragHit == 13 || dragHit == 16, rightEdge = dragHit == 11 || dragHit == 14 || dragHit == 17;
            bool topEdge = dragHit >= 12 && dragHit <= 14, bottom = dragHit >= 15 && dragHit <= 17;
            if (dragHit == 2) { l += dx; r += dx; t += dy; b += dy; }
            if (left) l = Math.Min(l + dx, r - 160);
            if (rightEdge) r = Math.Max(r + dx, l + 160);
            if (topEdge) t = Math.Min(t + dy, b - 80);
            if (bottom) b = Math.Max(b + dy, t + 80);
            SetWindowPos(dragWin, IntPtr.Zero, l, t, r - l, b - t, 0x4 | 0x10 | 0x4000 /* NOZORDER, NOACTIVATE, ASYNCWINDOWPOS */);
            return "OK";
        }
        if (kind == "up") {
            if (shellDown) { shellDown = false; return "OK"; }
            if (dragWin != IntPtr.Zero) { dragWin = IntPtr.Zero; return "OK"; }
            if (pressWin == IntPtr.Zero) return null;
            IntPtr w = pressWin; int hit = pressHit; pressWin = IntPtr.Zero;
            // A caption button acts when the press is let go on it, so a press can still be taken back by moving off.
            if (IsWindow(w) && WindowAt(p) == w && HitTest(w, p) == hit) Caption(w, hit);
            return "OK";
        }
        if (captured != IntPtr.Zero) return null;
        string shell = ShellPress(p, right);
        if (shell != null || right) return shell;
        IntPtr top = WindowAt(p);
        if (top == IntPtr.Zero) { DesktopPress(p); return "OK"; }
        var ws = AppWindows();
        if (ws.Count > 0 && ws[0].H != top) Activate(top);
        int at = HitTest(top, p);
        if (at == 2 || (at >= 10 && at <= 17)) {
            if (at == 2 && twice) Caption(top, 9);
            else if (!IsZoomed(top)) { dragWin = top; dragHit = at; dragFrom = p; GetWindowRect(top, out dragRect); }
            return "OK";
        }
        if (at == 8 || at == 9 || at == 20) { pressWin = top; pressHit = at; return "OK"; }
        return null;
    }

    static bool TaskWindow(IntPtr h, IntPtr l) {
        if (!IsWindowVisible(h) || GetWindow(h, 4 /* GW_OWNER */) != IntPtr.Zero) return true;
        if ((GetWindowLong(h, -20 /* GWL_EXSTYLE */) & 0x80 /* WS_EX_TOOLWINDOW */) != 0) return true;
        var cls = new StringBuilder(80); GetClassName(h, cls, 80);
        if (Array.IndexOf(SystemWindowClasses, cls.ToString()) >= 0) return true;
        int cloaked;
        if (DwmGetWindowAttribute(h, 14, out cloaked, 4) == 0 && cloaked != 0) return true;
        var title = new StringBuilder(200);
        if (GetWindowText(h, title, 200) == 0) return true;
        ((List<IntPtr>)GCHandle.FromIntPtr(l).Target).Add(h);
        return true;
    }

    // The windows a taskbar would have a button for, topmost first: "<handle><US><a|m|-><US><title>", TAB between them.
    // a is the one in front, m a minimised one.
    public static string Tasks() {
        var list = new List<IntPtr>();
        if (desktop != IntPtr.Zero) Enum(TaskWindow, list);
        var sb = new StringBuilder();
        bool front = true;
        foreach (IntPtr h in list) {
            var title = new StringBuilder(200); GetWindowText(h, title, 200);
            bool min = IsIconic(h);
            if (sb.Length > 0) sb.Append('\t');
            sb.Append(h.ToInt64()).Append('\u001f').Append(min ? "m" : front ? "a" : "-").Append('\u001f')
              .Append(title.ToString().Replace('\t', ' ').Replace('\u001f', ' ').Replace('\r', ' ').Replace('\n', ' '));
            if (!min) front = false;
        }
        return sb.ToString();
    }

    // What a taskbar button does to its window: raise (and restore), min, close. Only to a window that has a button.
    public static string WindowDo(string action, long handle) {
        IntPtr h = (IntPtr)handle;
        var list = new List<IntPtr>();
        if (desktop != IntPtr.Zero) Enum(TaskWindow, list);
        if (!list.Contains(h)) return "ERR that window is not there any more";
        switch (action) {
            case "raise": Activate(h); break;
            case "min": ShowWindowAsync(h, 6 /* SW_MINIMIZE */); break;
            case "close": PostMessage(h, 0x0010 /* WM_CLOSE */, IntPtr.Zero, IntPtr.Zero); break;
            default: return "ERR unknown action";
        }
        Kick();
        return "OK";
    }

    // Walks down to the innermost visible, enabled child under a screen point.
    static IntPtr Descend(IntPtr wnd, POINT screen) {
        for (int i = 0; i < 16; i++) {
            POINT c = screen; ScreenToClient(wnd, ref c);
            IntPtr child = ChildWindowFromPointEx(wnd, c, 0x7);
            if (child == IntPtr.Zero || child == wnd) return wnd;
            wnd = child;
        }
        return wnd;
    }

    // A position in the picture last shown, as a point on the private desktop.
    static POINT ScreenPoint(int x, int y) {
        lock (geo) return new POINT { X = frameL + (int)Math.Round(x / frameScale), Y = frameT + (int)Math.Round(y / frameScale) };
    }

    // The topmost of the app's windows that shows this point.
    static IntPtr WindowAt(POINT p) {
        foreach (var w in AppWindows())
            if (p.X >= w.V.L && p.X < w.V.R && p.Y >= w.V.T && p.Y < w.V.B) return w.H;
        return IntPtr.Zero;
    }

    static IntPtr ClientLParam(IntPtr target, POINT screen) {
        POINT c = screen; ScreenToClient(target, ref c);
        double tsc = Scale(target);
        c.X = (int)Math.Round(c.X * tsc); c.Y = (int)Math.Round(c.Y * tsc);
        return (IntPtr)(((c.Y & 0xFFFF) << 16) | (c.X & 0xFFFF));
    }

    static void SyncSend(IntPtr h, uint msg, IntPtr wp, IntPtr lp) {
        IntPtr ignored;
        SendMessageTimeout(h, msg, wp, lp, 0x2 /* SMTO_ABORTIFHUNG */, 300, out ignored);
    }

    // GetKeyState - which is how an app learns that Ctrl or Shift is held - only ever reflects real hardware input, never a
    // posted message. So for input that carries a modifier, act runs on a helper thread that has joined the app's input
    // queue on the private desktop and has set that queue's key-state table with the modifiers down; the messages are then
    // SENT (not posted) so the state is still in place while the app handles them. mods is any of c (Ctrl), s (Shift), a (Alt).
    // Returns "OK", or "OK unmodified" if Windows would not let a thread join the app's queue.
    static string WithModifiers(IntPtr target, string mods, Action act) {
        if (string.IsNullOrEmpty(mods)) { act(); return "OK"; }
        string result = "OK";
        var th = new System.Threading.Thread(() => {
            int pid; uint appThread = GetWindowThreadProcessId(target, out pid);
            uint me = GetCurrentThreadId();
            if (!SetThreadDesktop(desktop) || !AttachThreadInput(me, appThread, true)) { result = "OK unmodified"; act(); return; }
            try {
                var saved = new byte[256]; GetKeyboardState(saved);
                var keys = (byte[])saved.Clone();
                if (mods.Contains("c")) { keys[0x11] |= 0x80; keys[0xA2] |= 0x80; }
                if (mods.Contains("s")) { keys[0x10] |= 0x80; keys[0xA0] |= 0x80; }
                if (mods.Contains("a")) { keys[0x12] |= 0x80; keys[0xA4] |= 0x80; }
                SetKeyboardState(keys);
                act();
                SetKeyboardState(saved);
            } finally { AttachThreadInput(me, appThread, false); }
        });
        th.Start(); th.Join();
        return result;
    }

    // While a button is held, moves and the release go to the control that got the press, like mouse capture. mods is
    // any of c, s, a (Ctrl, Shift, Alt) held during this event.
    static IntPtr downWin; static long downAt; static POINT downPt;

    public static string Mouse(string kind, int x, int y, bool right, string mods) {
        EnsureGeometry();
        POINT screen = ScreenPoint(x, y);
        // A second press in the same place within the double-click time. Windows makes a double-click out of real input
        // only, so posted presses never became one: a word could not be selected, nor a file opened from a list.
        bool twice = false;
        if (kind == "down" && !right) {
            long now = Stopwatch.GetTimestamp();
            IntPtr under = WindowAt(screen);
            twice = under != IntPtr.Zero && under == downWin && (now - downAt) * 1000 / Stopwatch.Frequency <= GetDoubleClickTime()
                    && Math.Abs(screen.X - downPt.X) <= 4 && Math.Abs(screen.Y - downPt.Y) <= 4;
            downWin = twice ? IntPtr.Zero : under; downAt = now; downPt = screen;
        }
        if (fixedScreen) {
            string managed = Manage(kind, screen, right, twice);
            if (managed != null) return managed;
        }
        IntPtr target = captured;
        if (target == IntPtr.Zero) {
            IntPtr top = WindowAt(screen);
            if (top == IntPtr.Zero) return "OK"; // nothing of the app is there
            target = Descend(top, screen);
        }
        // Only a window that asked for double-clicks (CS_DBLCLKS) is sent one; any other takes a second press.
        uint leftDown = twice && (GetClassLong(target, -26 /* GCL_STYLE */) & 0x8) != 0 ? 0x0203u /* WM_LBUTTONDBLCLK */ : WM_LBUTTONDOWN;
        IntPtr lp = ClientLParam(target, screen);
        // The flags a mouse message carries for the modifiers that are down (MK_CONTROL, MK_SHIFT).
        int flags = (mods.Contains("c") ? 0x8 : 0) | (mods.Contains("s") ? 0x4 : 0);
        Action act = () => {
            switch (kind) {
                case "move": PostMessage(target, WM_MOUSEMOVE, (IntPtr)(flags | (captured != IntPtr.Zero ? (right ? 2 : 1) : 0)), lp); break;
                case "down":
                    PostMessage(target, WM_MOUSEMOVE, (IntPtr)flags, lp);
                    PostMessage(target, right ? WM_RBUTTONDOWN : leftDown, (IntPtr)(flags | (right ? 2 : 1)), lp);
                    break;
                case "up": PostMessage(target, right ? WM_RBUTTONUP : WM_LBUTTONUP, (IntPtr)flags, lp); break;
            }
            if (mods.Length > 0) System.Threading.Thread.Sleep(80); // keep the modifiers set until the app has read them
        };
        string result = WithModifiers(target, mods, act);
        if (kind == "down") { captured = target; lastTarget = target; }
        if (kind == "up") captured = IntPtr.Zero;
        return result;
    }

    public static string Wheel(int x, int y, int delta) {
        EnsureGeometry();
        POINT screen = ScreenPoint(x, y);
        IntPtr top = WindowAt(screen);
        if (top == IntPtr.Zero) return "OK";
        IntPtr target = Descend(top, screen);
        IntPtr wp = (IntPtr)(((delta * 120) << 16));
        IntPtr lp = (IntPtr)(((screen.Y & 0xFFFF) << 16) | (screen.X & 0xFFFF));
        PostMessage(target, WM_MOUSEWHEEL, wp, lp);
        return "OK";
    }

    // Keyboard input goes where the app itself has put keyboard focus (a Chromium address bar is not a window of its
    // own, so "what was clicked" is the wrong answer there) - on the topmost window, so a dialog gets its own keys.
    // If Windows will not say, fall back to what was clicked last.
    static IntPtr KeyTarget() {
        var ws = AppWindows();
        // The topmost window whose thread has a keyboard focus: a menu or dialog the app has open is above its main
        // window and shares its thread, so it gets its own keys.
        IntPtr focus = IntPtr.Zero;
        // Windows answers "where is the focus" only to a thread on the same desktop, and this process's own threads
        // are on the person's. Asked from one of those it always said "nowhere", so keys went to whatever was
        // clicked last - or, with nothing clicked yet, to the window's frame, which drops them. Found live on a
        // worker's screen: Notepad opened, its editor focused, and what a person typed went nowhere until they
        // clicked. So it is asked from a thread that has joined the private desktop.
        var ask = new System.Threading.Thread(() => {
            if (!SetThreadDesktop(desktop)) return;
            foreach (var w in ws) {
                int pid; uint tid = GetWindowThreadProcessId(w.H, out pid);
                var gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
                if (GetGUIThreadInfo(tid, ref gi) && gi.hwndFocus != IntPtr.Zero) { focus = gi.hwndFocus; return; }
            }
        });
        ask.Start(); ask.Join(1000);
        if (focus != IntPtr.Zero) return focus;
        // What was clicked last, only while it is still there: the last click may have been in an app since closed.
        if (lastTarget != IntPtr.Zero && IsWindow(lastTarget)) return lastTarget;
        return ws.Count > 0 ? ws[0].H : IntPtr.Zero;
    }

    // ---- Clipboard. The clipboard belongs to the window station, so it is the person's own: an app on the private desktop
    // copies into it and pastes out of it. Nothing here reads it except right after an explicit copy, and only if that copy
    // changed it; what was on it before is put back afterwards when it was plain text (or nothing).

    const uint CF_UNICODETEXT = 13;

    static bool OpenClipboardRetry() {
        for (int i = 0; i < 20; i++) { if (OpenClipboard(IntPtr.Zero)) return true; System.Threading.Thread.Sleep(10); }
        return false;
    }

    // The clipboard as plain text: null if it holds anything else, "" if it holds nothing. Unreadable counts as "else".
    static string ReadClipboardText(out bool plain) {
        plain = false;
        if (!OpenClipboardRetry()) return null;
        try {
            int formats = CountClipboardFormats();
            if (formats == 0) { plain = true; return ""; }
            if (!IsClipboardFormatAvailable(CF_UNICODETEXT)) return null;
            IntPtr h = GetClipboardData(CF_UNICODETEXT);
            if (h == IntPtr.Zero) return null;
            IntPtr p = GlobalLock(h);
            if (p == IntPtr.Zero) return null;
            try { string text = Marshal.PtrToStringUni(p); plain = formats <= 4; /* text plus the formats Windows derives from it */ return text; }
            finally { GlobalUnlock(h); }
        } finally { CloseClipboard(); }
    }

    static void WriteClipboardText(string text) {
        if (!OpenClipboardRetry()) return;
        try {
            EmptyClipboard();
            if (string.IsNullOrEmpty(text)) return;
            byte[] bytes = Encoding.Unicode.GetBytes(text + "\0");
            IntPtr mem = GlobalAlloc(0x2 /* GMEM_MOVEABLE */, (UIntPtr)bytes.Length);
            IntPtr dst = GlobalLock(mem);
            Marshal.Copy(bytes, 0, dst, bytes.Length);
            GlobalUnlock(mem);
            SetClipboardData(CF_UNICODETEXT, mem);
        } finally { CloseClipboard(); }
    }

    // Presses Ctrl+C (or Ctrl+X) in the app and returns the text it put on the clipboard: "CLIP <base64 utf-8>", or "CLIP"
    // when the clipboard did not change (nothing selected, or the app has its own idea of copy).
    public static string Copy(int vk) {
        bool plain;
        string before = ReadClipboardText(out plain);
        uint seq = GetClipboardSequenceNumber();
        string keyed = Key(vk, "c");
        if (!keyed.StartsWith("OK")) return keyed;
        for (int i = 0; i < 30 && GetClipboardSequenceNumber() == seq; i++) System.Threading.Thread.Sleep(20);
        if (GetClipboardSequenceNumber() == seq) return "CLIP";
        bool copiedPlain;
        string copied = ReadClipboardText(out copiedPlain);
        // The copy necessarily replaced the person's clipboard; give back what they had if it was only text (or empty).
        if (before != null && plain) WriteClipboardText(before);
        if (string.IsNullOrEmpty(copied)) return "CLIP";
        return "CLIP " + Convert.ToBase64String(Encoding.UTF8.GetBytes(copied));
    }

    public static string FocusInfo() {
        IntPtr t = KeyTarget();
        var cls = new StringBuilder(64); GetClassName(t, cls, 64);
        var pressed = new StringBuilder(64); if (lastTarget != IntPtr.Zero) GetClassName(lastTarget, pressed, 64);
        return t + " " + cls + (t == lastTarget ? " (last clicked)" : "") + "; last press: " + lastTarget + " " + pressed;
    }

    public static void TypeChar(char ch) {
        if (ShellChar(ch)) return;
        IntPtr t = KeyTarget();
        if (t != IntPtr.Zero) PostMessage(t, WM_CHAR, (IntPtr)ch, (IntPtr)1);
    }

    public static string Key(int vk, string mods) {
        if (ShellKey(vk)) return "OK";
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) return "ERR no window";
        // A keyboard turns these into characters through TranslateMessage; a direct message does not.
        bool asChar = (vk == 8 || vk == 9 || vk == 13 || vk == 27) && string.IsNullOrEmpty(mods.Replace("s", ""));
        if (string.IsNullOrEmpty(mods)) {
            PostMessage(t, WM_KEYDOWN, (IntPtr)vk, (IntPtr)1);
            if (asChar) PostMessage(t, WM_CHAR, (IntPtr)vk, (IntPtr)1);
            PostMessage(t, WM_KEYUP, (IntPtr)vk, (IntPtr)unchecked((int)0xC0000001));
            return "OK";
        }
        // Posted, not sent: a framework that handles shortcuts in its message loop (WinForms, MFC, most toolkits) only sees posted
        // messages. The modifier state has to stay set until the app has taken them off its queue, so hold it briefly.
        return WithModifiers(t, mods, () => {
            PostMessage(t, mods.Contains("a") ? 0x104u /* WM_SYSKEYDOWN */ : WM_KEYDOWN, (IntPtr)vk, (IntPtr)(mods.Contains("a") ? 0x20000001 : 1));
            if (asChar) PostMessage(t, WM_CHAR, (IntPtr)vk, (IntPtr)1);
            PostMessage(t, mods.Contains("a") ? 0x105u /* WM_SYSKEYUP */ : WM_KEYUP, (IntPtr)vk, (IntPtr)unchecked((int)(mods.Contains("a") ? 0xE0000001 : 0xC0000001)));
            System.Threading.Thread.Sleep(150);
        });
    }
}
'@

# Compiling the C# above takes seconds every time a helper starts, which is most of what starting an app used to cost. It is
# compiled once, to a DLL named for a hash of the source (so a changed source is never mistaken for an old build), and loaded
# from there afterwards. Two helpers starting together may both compile; each writes its own temporary file and the last
# move wins, which is harmless because the contents are identical.
if (-not ('Native' -as [type])) {
    $bridgeDir = Join-Path $env:LOCALAPPDATA 'Decurix\bridge'
    $hash = [BitConverter]::ToString([Security.Cryptography.SHA1]::Create().ComputeHash([Text.Encoding]::UTF8.GetBytes($NativeSource))).Replace('-', '').Substring(0, 12)
    $dll = Join-Path $bridgeDir "Native-$hash.dll"
    if (-not (Test-Path -LiteralPath $dll)) {
        try {
            New-Item -ItemType Directory -Force -Path $bridgeDir | Out-Null
            $tmp = "$dll.$PID.tmp"
            Add-Type -ReferencedAssemblies System.Drawing -TypeDefinition $NativeSource -OutputAssembly $tmp -OutputType Library
            Move-Item -LiteralPath $tmp -Destination $dll -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        } catch { }
    }
    if (-not ('Native' -as [type])) {
        if (Test-Path -LiteralPath $dll) { try { Add-Type -Path $dll } catch { } }
        if (-not ('Native' -as [type])) { Add-Type -ReferencedAssemblies System.Drawing -TypeDefinition $NativeSource }
    }
}

# Apps that hand a second launch to the copy already running would open on the person's own desktop, which is exactly what
# this bridge exists to avoid, so they get a profile of their own. That is every Chromium browser and every Electron app,
# recognised by the runtime files that sit beside their executable (a versioned Chrome keeps them one folder down, so the
# well-known browsers are also named). The profile is kept between sessions, per desktop and app, so a sign-in made in the
# hidden app is still there next time - and it is never the person's own profile, which the running copy has locked.
$SingleInstanceByName = @('chrome', 'msedge', 'brave', 'vivaldi', 'opera', 'chromium', 'code', 'cursor', 'slack', 'discord', 'teams', 'ms-teams')
# Browsers additionally skip their first-run and "make me your default" flows: on a private desktop nobody is there to answer
# them, and a modal one blocks the keyboard for the whole window.
$Browsers = @('chrome', 'msedge', 'brave', 'vivaldi', 'opera', 'chromium')
$ProfileRoot = Join-Path $env:LOCALAPPDATA 'Decurix\hidden-profiles'

function Test-SingleInstanceApp([string]$exe) {
    $base = [IO.Path]::GetFileNameWithoutExtension($exe).ToLower()
    if ($SingleInstanceByName -contains $base) { return $true }
    $dir = [IO.Path]::GetDirectoryName($exe)
    if (Test-Path -LiteralPath (Join-Path $dir 'icudtl.dat')) { return $true }
    # Squirrel-installed and newer self-updating apps keep the runtime in a versioned folder beside the launcher.
    foreach ($sub in Get-ChildItem -LiteralPath $dir -Directory -ErrorAction SilentlyContinue | Select-Object -First 30) {
        if (Test-Path -LiteralPath (Join-Path $sub.FullName 'icudtl.dat')) { return $true }
    }
    return $false
}

# Classic programs appear in the Start menu as shell:AppsFolder\{known-folder GUID}\relative\path.exe. The GUID is only
# a folder, so those can be run directly; anything else under AppsFolder is an app model ID (Store apps) and cannot.
$KnownFolders = @{
    '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}' = [Environment]::SystemDirectory
    '{D65231B0-B2F1-4857-A4CE-A8E7C6EA7D27}' = Join-Path $env:windir 'SysWOW64'
    '{6D809377-6AF0-444B-8957-A3773F02200E}' = $env:ProgramW6432
    '{7C5A40EF-A0FB-4BFC-874A-C0F2E0B9FA8E}' = ${env:ProgramFiles(x86)}
    '{F38BF404-1D43-42F2-9305-67DE0B28FC23}' = $env:windir
}

$UwpMessage = 'This is a Universal Windows (UWP) app, such as Calculator or Camera. Windows only runs those on the visible desktop and there is no way to hide one, so it cannot be shown from here. Full desktop programs (Notepad, Paint, Terminal, Chrome, VS Code and most others) can.'
$WindowsApps = Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps'

# A packaged app is either a UWP app (which needs the shell to activate it on the visible desktop) or a full-trust desktop
# program that merely ships in a package (Notepad, Paint, Terminal); the latter has an execution alias that CreateProcess
# runs like any other program, on whichever desktop it is asked to.
function Resolve-Packaged([string]$family, [string]$appId) {
    $pkg = Get-AppxPackage -Name $family.Split('_')[0] -ErrorAction SilentlyContinue | Where-Object { $_.PackageFamilyName -eq $family } | Select-Object -First 1
    if (-not $pkg) { return $null }
    [xml]$m = Get-Content -LiteralPath (Join-Path $pkg.InstallLocation 'AppxManifest.xml')
    $app = @($m.Package.Applications.Application) | Where-Object { $_.Id -eq $appId } | Select-Object -First 1
    if (-not $app) { return $null }
    if ($app.EntryPoint -ne 'Windows.FullTrustApplication') { return @{ Error = $UwpMessage } }
    $aliases = @($app.Extensions.Extension | Where-Object { $_.Category -eq 'windows.appExecutionAlias' } | ForEach-Object { $_.AppExecutionAlias.ExecutionAlias.Alias })
    foreach ($a in $aliases) {
        $path = Join-Path $WindowsApps $a
        if (Test-Path -LiteralPath $path) { return @{ Target = $path } }
    }
    return @{ Error = 'This packaged app has no command-line alias, so it can only be started from the Start menu, which runs it on the visible desktop.' }
}

# A classic program registered under an explicit app model ID (Chrome, Brave, Discord) shows up as shell:AppsFolder\<id>
# but is only a Start menu shortcut to an ordinary executable: find that shortcut by the name Windows lists it under.
function Resolve-StartMenuShortcut([string]$appModelId) {
    # Listed once per helper: a check asks about every app in the Start menu, and Get-StartApps is slow.
    if ($null -eq $script:StartApps) { $script:StartApps = @(Get-StartApps) }
    $entry = $script:StartApps | Where-Object { $_.AppID -eq $appModelId } | Select-Object -First 1
    if (-not $entry) { return $null }
    foreach ($root in @("$env:ProgramData\Microsoft\Windows\Start Menu\Programs", "$env:APPDATA\Microsoft\Windows\Start Menu\Programs")) {
        $found = Get-ChildItem -LiteralPath $root -Recurse -Filter ($entry.Name + '.lnk') -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($found) { return $found.FullName }
    }
    return $null
}

# Whether a program that exists is nevertheless one that cannot be run on a private desktop, as text for the person; "" if it can.
# Each of these was found to fail or to leak a window onto the visible desktop, so they are refused rather than offered.
function Get-CannotRunHidden([string]$target) {
    if (Test-Path -LiteralPath $target -PathType Container) { return 'This opens a folder, not a program.' }
    $leaf = [IO.Path]::GetFileName($target).ToLower()
    if ($leaf -eq 'wsl.exe' -or $leaf -eq 'wslg.exe') {
        return 'This is a Linux app running in WSL. Windows shows those itself, on the visible desktop, so they cannot be hidden.'
    }
    # These only pass their work on - to the shell, which shows it on the visible desktop, or to a script with no window - so
    # there is no app of their own to show.
    if (@('control.exe', 'explorer.exe', 'rundll32.exe', 'cscript.exe', 'wscript.exe', 'msiexec.exe') -contains $leaf) {
        return 'This only hands its work to Windows or a script host, which would show it on the visible desktop or not at all.'
    }
    $ext = [IO.Path]::GetExtension($target).ToLower()
    if ($ext -ne '.exe' -and $ext -ne '.com' -and $ext -ne '.bat' -and $ext -ne '.cmd') {
        return 'This is a document or a control-panel item, not a program that runs by itself.'
    }
    # A manifest that asks for administrator rights makes Windows show a prompt, which nobody can answer on a hidden desktop.
    if ($ext -eq '.exe') {
        try {
            if ((Get-Item -LiteralPath $target -ErrorAction Stop).Length -le 64MB) {
                $manifest = [Text.Encoding]::ASCII.GetString([IO.File]::ReadAllBytes($target))
                if ($manifest.Contains('requireAdministrator') -or $manifest.Contains('highestAvailable')) {
                    return 'This program asks Windows for administrator rights, and that prompt cannot be answered on a hidden desktop.'
                }
            }
        } catch { }
    }
    return ''
}

# What a target really is: the program, its arguments and folder, or why it cannot run hidden. This is the slow part of a launch
# (Get-StartApps, the Start menu, an app package's manifest, the program's own manifest), and it depends only on the target, so
# it is worked out once per helper: checking every app in the Start menu warms it, and the launch that follows is instant.
$ResolveCache = @{}

function Resolve-Program([string]$target, [string]$argLine) {
    $key = $target + "`t" + $argLine
    if ($ResolveCache.ContainsKey($key)) { return $ResolveCache[$key] }
    $r = Resolve-ProgramUncached $target $argLine
    $ResolveCache[$key] = $r
    return $r
}

function Resolve-ProgramUncached([string]$target, [string]$argLine) {
    if ($target -match '^shell:AppsFolder\\(\{[0-9A-Fa-f-]+\})\\(.+)$' -and $KnownFolders.ContainsKey($Matches[1].ToUpper())) {
        $target = Join-Path $KnownFolders[$Matches[1].ToUpper()] $Matches[2]
    }
    if ($target -match '^shell:AppsFolder\\([^\\!]+)!(.+)$') {
        $r = Resolve-Packaged $Matches[1] $Matches[2]
        if ($r) { if ($r.Error) { return $r }; $target = $r.Target }
    } elseif ($target -match '^shell:AppsFolder\\([^\\]+)$') {
        $lnk = Resolve-StartMenuShortcut $Matches[1]
        if ($lnk) { $target = $lnk }
    }
    if ($target -like 'shell:*' -or $target -like '*AppsFolder*') {
        return @{ Error = 'Windows starts this app through the shell, which only runs it on the visible desktop, so it cannot be hidden. Pick the program itself, or a shortcut to it, instead.' }
    }
    $workDir = ''
    if ($target -like '*.lnk') {
        $s = (New-Object -ComObject WScript.Shell).CreateShortcut($target)
        if (-not $s.TargetPath -or $s.TargetPath -like 'shell:*' -or $s.TargetPath -like '*AppsFolder*' -or $s.TargetPath -like '*explorer.exe') {
            return @{ Error = 'This shortcut opens a Store app or a folder, which Windows only shows on the visible desktop.' }
        }
        $target = $s.TargetPath; $workDir = $s.WorkingDirectory
        if (-not $argLine) { $argLine = $s.Arguments }
    }
    if (-not (Test-Path -LiteralPath $target)) { return @{ Error = "Not found on this computer: $target" } }
    $refusal = Get-CannotRunHidden $target
    if ($refusal) { return @{ Error = $refusal } }
    # Never inherit our own working directory: it can be a \\wsl.localhost UNC path, which Electron apps reject.
    if (-not $workDir) { $workDir = [IO.Path]::GetDirectoryName($target) }
    $base = [IO.Path]::GetFileNameWithoutExtension($target).ToLower()
    return @{ Target = $target; Args = $argLine; WorkDir = $workDir; Base = $base; OwnProfile = ((Test-SingleInstanceApp $target) -and ($argLine -notlike '*user-data-dir*')) }
}

# Chromium keeps a "the last session did not end cleanly" mark in its profile, and shows a Restore bubble that takes the
# keyboard for the whole window. A hidden app is ended by the control plane, sometimes without ceremony (a control plane that
# restarts, a helper that dies), so the mark is cleared before each start rather than trusting the last one to have ended well.
function Clear-CrashMark([string]$dir) {
    foreach ($f in @((Join-Path $dir 'Default\Preferences'), (Join-Path $dir 'Local State'))) {
        if (-not (Test-Path -LiteralPath $f)) { continue }
        try {
            $text = [IO.File]::ReadAllText($f)
            $fixed = $text -replace '"exit_type"\s*:\s*"(Crashed|SessionEnded)"', '"exit_type":"Normal"' -replace '"exited_cleanly"\s*:\s*false', '"exited_cleanly":true'
            if ($fixed -ne $text) { [IO.File]::WriteAllText($f, $fixed, (New-Object Text.UTF8Encoding($false))) }
        } catch { }
    }
}

# A browser on the private desktop needs its own user-data directory so it cannot hand the launch to the copy already
# open on the person's visible desktop. On the first launch only, initialize that private directory from the person's
# real profile. This keeps their chosen Chrome profile and signed-in account while preserving the private desktop's
# independent lifetime afterward. robocopy tolerates files held open by a running browser; caches and crash data are
# deliberately omitted, and a later launch never overwrites changes made in the private profile.
function Get-BrowserProfileSource([string]$base) {
    switch ($base) {
        'chrome'   { return (Join-Path $env:LOCALAPPDATA 'Google\Chrome\User Data') }
        'msedge'   { return (Join-Path $env:LOCALAPPDATA 'Microsoft\Edge\User Data') }
        'brave'    { return (Join-Path $env:LOCALAPPDATA 'BraveSoftware\Brave-Browser\User Data') }
        'vivaldi'  { return (Join-Path $env:LOCALAPPDATA 'Vivaldi\User Data') }
        'chromium' { return (Join-Path $env:LOCALAPPDATA 'Chromium\User Data') }
        default    { return '' }
    }
}

function Get-ActiveBrowserProfile([string]$dir) {
    $lsPath = Join-Path $dir 'Local State'
    if (-not (Test-Path -LiteralPath $lsPath)) { return '' }
    try {
        # FileShare.ReadWrite: the person's own browser is usually running and holds Local State open.
        $fs = [System.IO.File]::Open($lsPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        try { $content = (New-Object System.IO.StreamReader($fs, [Text.Encoding]::UTF8)).ReadToEnd() } finally { $fs.Close() }
        if (-not $content) { return '' }
        $json = ConvertFrom-Json $content -ErrorAction SilentlyContinue
        if (-not $json) { return '' }
        # Check profile.info_cache for any profile with a signed-in email
        if ($json.profile -and $json.profile.info_cache) {
            foreach ($prop in $json.profile.info_cache.PSObject.Properties) {
                $pData = $prop.Value
                if ($pData -and $pData.user_name) {
                    return $prop.Name
                }
            }
        }
        # Fall back to profile.last_used
        if ($json.profile -and $json.profile.last_used) {
            return $json.profile.last_used
        }
    } catch {}
    return ''
}

# What is never brought over: caches the browser rebuilds on its own, and components it downloads again. Chrome's
# Service Worker CacheStorage alone was 1.6 GB on the machine this was measured on, and with it the import took minutes.
$ProfileSkipDirs = @(
    'Cache', 'Code Cache', 'GPUCache', 'GrShaderCache', 'ShaderCache', 'DawnCache', 'DawnWebGPUCache', 'DawnGraphiteCache',
    'GraphiteDawnCache', 'GPUPersistentCache', 'Media Cache', 'CacheStorage', 'ScriptCache', 'Crashpad', 'BrowserMetrics',
    'DeferredBrowserMetrics', 'component_crx_cache', 'extensions_crx_cache', 'screen_ai', 'optimization_guide_model_store',
    'OnDeviceHeadSuggestModel', 'WasmTtsEngine', 'Safe Browsing', 'Shared Dictionary', 'Download Service',
    'segmentation_platform', 'Snapshots', 'SODA', 'SODALanguagePacks', 'hyphen-data'
)
$ProfileSkipFiles = @('Singleton*', '*lock*', '*.tmp', 'Lock*', 'Parent.lock', '*.pma')

function Initialize-PrivateBrowserProfile([string]$base, [string]$dir) {
    if (-not ($Browsers -contains $base)) { return }
    # A Decurix marker, rather than Chromium's Local State, lets profiles created by an older build receive this one-time
    # import too. Once imported, later launches never overwrite work or sign-ins added inside the private profile.
    $marker = Join-Path $dir '.decurix-host-profile-imported'
    if (Test-Path -LiteralPath $marker) { return }
    $source = Get-BrowserProfileSource $base
    if (-not $source -or -not (Test-Path -LiteralPath $source -PathType Container)) { return }
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $common = @('/XJ', '/R:0', '/W:0', '/MT:16', '/NFL', '/NDL', '/NJH', '/NJS', '/NP', '/XD') + $ProfileSkipDirs + @('/XF') + $ProfileSkipFiles
    # Only the profile the person uses (the signed-in one, else the last used) and the top-level files that go with it
    # (Local State holds the key its cookies and passwords are encrypted with): other profiles were most of the rest.
    $active = Get-ActiveBrowserProfile $source
    if ($active -and (Test-Path -LiteralPath (Join-Path $source $active) -PathType Container)) {
        & robocopy $source $dir '/LEV:1' @common | Out-Null
        $top = $LASTEXITCODE
        & robocopy (Join-Path $source $active) (Join-Path $dir $active) '/E' @common | Out-Null
        $code = [Math]::Max($top, $LASTEXITCODE)
    } else {
        & robocopy $source $dir '/E' @common | Out-Null
        $code = $LASTEXITCODE
    }
    # If robocopy skipped locked critical files like Local State, copy them with FileShare.ReadWrite fallback:
    $localStateDest = Join-Path $dir 'Local State'
    $localStateSrc = Join-Path $source 'Local State'
    if (-not (Test-Path -LiteralPath $localStateDest) -and (Test-Path -LiteralPath $localStateSrc)) {
        try {
            $inStream = [System.IO.File]::Open($localStateSrc, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
            $outStream = [System.IO.File]::Create($localStateDest)
            $inStream.CopyTo($outStream)
            $inStream.Close(); $outStream.Close()
        } catch {}
    }
    # robocopy's exit code is bits: 8 means some files could not be copied - the ones the person's running browser holds
    # locked (its cookie file, while it is open) - and only 16 is a failure of the copy itself. Treating 8 as failure left
    # the marker unwritten, so every start imported again, over what the private profile had changed since.
    if ($code -lt 16 -and (Test-Path -LiteralPath $dir)) { Set-Content -LiteralPath $marker -Value '1' -Encoding ASCII }
}

# The command line a resolved program is started with: its own profile for a browser or Electron app (per desktop), and the
# flags that keep a first-run or restore prompt from taking the keyboard.
function Complete-Launch($p) {
    $argLine = $p.Args
    if ($p.OwnProfile) {
        $dir = Join-Path $ProfileRoot "$DesktopName-$($p.Base)"
        Initialize-PrivateBrowserProfile $p.Base $dir
        $argLine = (($argLine + " --user-data-dir=`"$dir`"").Trim())
        if ($Browsers -contains $p.Base) {
            if ($argLine -notlike '*no-first-run*') { $argLine += ' --no-first-run --no-default-browser-check' }
            $argLine += ' --disable-session-crashed-bubble --hide-crash-restore-bubble'
            if ($argLine -notlike '*--profile-directory*') {
                $chosenProfile = Get-ActiveBrowserProfile $dir
                if ($chosenProfile) { $argLine += " --profile-directory=`"$chosenProfile`"" }
            }
        }
        Clear-CrashMark $dir
    }
    return @{ Target = $p.Target; Args = $argLine; WorkDir = $p.WorkDir }
}

function Resolve-Launch([string]$target, [string]$argLine) {
    $p = Resolve-Program $target $argLine
    if ($p.Error) { return $p }
    return Complete-Launch $p
}

# "<x> <y> [right] [+<mods>]", mods being any of c (Ctrl), s (Shift), a (Alt).
function Read-MouseArgs([string]$rest) {
    $p = $rest.Split(" ")
    $mods = ''
    foreach ($t in $p[2..($p.Count - 1)]) { if ($t -like '+*') { $mods = $t.Substring(1) } }
    return @{ X = [int]$p[0]; Y = [int]$p[1]; Right = ($p -contains 'right'); Mods = $mods }
}

# Everything said to Decurix goes through Native.Reply, which keeps an answer from landing in the middle of a picture.
function Reply([string]$text) { [Native]::Reply($text) }

[Native]::Init()
$stdin = [Console]::In
try {
    while (($line = $stdin.ReadLine()) -ne $null) {
        $verb, $rest = $line.Split([char[]]@(" ", "`t"), 2)
        try {
            switch ($verb) {
                "desktop" {
                    if ($rest -match '^[A-Za-z0-9_]{1,48}$') { $script:DesktopName = $rest; Reply "OK" } else { Reply "ERR invalid desktop name" }
                }
                "launch" {
                    $f = $rest.Split("`t")
                    $r = Resolve-Launch $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
                    if ($r.Error) { Reply "ERR $($r.Error)" }
                    else { Reply ([Native]::Launch($DesktopName, $r.Target, $r.Args, $r.WorkDir)) }
                }
                "check" {
                    $f = $rest.Split("`t")
                    $r = Resolve-Program $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
                    if ($r.Error) { Reply "ERR $($r.Error)" }
                    else { Reply ("OK`t" + $r.Target + "`t" + $r.Args + "`t" + $r.WorkDir + "`t" + [int]$r.OwnProfile + "`t" + $r.Base) }
                }
                "launchr" {
                    # What a check already worked out: nothing is looked up, so this is as fast as starting the program.
                    $f = $rest.Split("`t")
                    if ($f.Count -lt 5) { Reply "ERR malformed launchr" }
                    elseif (-not (Test-Path -LiteralPath $f[0] -PathType Leaf)) { Reply "ERR Not found on this computer: $($f[0])" }
                    else {
                        $l = Complete-Launch @{ Target = $f[0]; Args = $f[1]; WorkDir = $f[2]; OwnProfile = ($f[3] -eq '1'); Base = $f[4] }
                        Reply ([Native]::Launch($DesktopName, $l.Target, $l.Args, $l.WorkDir))
                    }
                }
                "open" {
                    $f = $rest.Split("`t")
                    $r = Resolve-Launch $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
                    if ($r.Error) { Reply "ERR $($r.Error)" }
                    else { Reply ([Native]::Open($DesktopName, $r.Target, $r.Args, $r.WorkDir)) }
                }
                "count" { Reply ("COUNT " + [Native]::WindowCount()) }
                "shortcuts" {
                    $f = $rest.Split("`t", 2)
                    [Native]::SetShortcuts($f[0], $(if ($f.Count -gt 1) { $f[1] } else { '' })); Reply "OK"
                }
                "tasks" { Reply ("TASKS " + [Native]::Tasks()) }
                "start" { [Native]::ToggleStart(); Reply "OK" }
                "shell" { Reply ("SHELL " + [int][Native]::ShellTyping()) }
                "win" { $p = $rest.Split(" "); Reply ([Native]::WindowDo($p[0], [long]$p[1])) }
                "over" { $p = $rest.Split(" "); Reply ("OVER " + [Native]::Over([int]$p[0], [int]$p[1])) }
                "screen" { [Native]::FixedScreen(); Reply "OK" }
                "viewers" { [Native]::SetViewers([int]$rest); Reply "OK" }
                "keyframe" { [Native]::KeyFrame(); Reply "OK" }
                "status" { Reply ("STATUS " + [Native]::Status()) }
                "focus" { Reply ("FOCUS " + [Native]::FocusInfo()) }
                "windows" { Reply ("WINDOWS " + [Native]::Windows()) }
                "move"  { $m = Read-MouseArgs $rest; Reply ([Native]::Mouse("move", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "down"  { $m = Read-MouseArgs $rest; Reply ([Native]::Mouse("down", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "up"    { $m = Read-MouseArgs $rest; Reply ([Native]::Mouse("up", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "click" { $m = Read-MouseArgs $rest; [Native]::Mouse("down", $m.X, $m.Y, $false, $m.Mods) | Out-Null; Start-Sleep -Milliseconds 30; Reply ([Native]::Mouse("up", $m.X, $m.Y, $false, $m.Mods)) }
                "wheel" { $p = $rest.Split(" "); Reply ([Native]::Wheel([int]$p[0], [int]$p[1], [int]$p[2])) }
                "type"  { foreach ($c in $rest.ToCharArray()) { [Native]::TypeChar($c) }; Reply "OK" }
                "copy"  { Reply ([Native]::Copy([int]$rest)) }
                "key"   { $p = $rest.Split(" "); Reply ([Native]::Key([int]$p[0], $(if ($p.Count -gt 1) { $p[1] } else { "" }))) }
                "quit"  { }
                default { Reply "ERR unknown command" }
            }
        } catch { Reply ("ERR " + $_.Exception.Message) }
        if ($verb -in @("move", "down", "up", "click", "wheel", "type", "key", "copy")) { [Native]::Kick() }
        if ($verb -eq "quit") { break }
    }
} finally {
    [Native]::Shutdown()
}
