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
        launched = pi.hProcess;
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
        if (!IsWindowVisible(h)) return true;
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
        if (ws.Count == 0) return null;
        LocateWindow();
        int l = int.MaxValue, t = int.MaxValue, r = int.MinValue, b = int.MinValue;
        foreach (var w in ws) { l = Math.Min(l, w.V.L); t = Math.Min(t, w.V.T); r = Math.Max(r, w.V.R); b = Math.Max(b, w.V.B); }
        double sc = Scale(mainWindow); // the main window's scale is the picture's, so a DPI-unaware app stays crisp
        int pw = (int)Math.Round((r - l) * sc), ph = (int)Math.Round((b - t) * sc);
        if (pw <= 0 || ph <= 0 || pw > 8192 || ph > 8192) return null;
        var bmp = new Bitmap(pw, ph, PixelFormat.Format24bppRgb);
        try {
            using (var g = Graphics.FromImage(bmp)) {
                g.Clear(Color.FromArgb(0x1b, 0x1b, 0x1b));
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
    public static string Mouse(string kind, int x, int y, bool right, string mods) {
        EnsureGeometry();
        POINT screen = ScreenPoint(x, y);
        IntPtr target = captured;
        if (target == IntPtr.Zero) {
            IntPtr top = WindowAt(screen);
            if (top == IntPtr.Zero) return "OK"; // nothing of the app is there
            target = Descend(top, screen);
        }
        IntPtr lp = ClientLParam(target, screen);
        // The flags a mouse message carries for the modifiers that are down (MK_CONTROL, MK_SHIFT).
        int flags = (mods.Contains("c") ? 0x8 : 0) | (mods.Contains("s") ? 0x4 : 0);
        Action act = () => {
            switch (kind) {
                case "move": PostMessage(target, WM_MOUSEMOVE, (IntPtr)(flags | (captured != IntPtr.Zero ? (right ? 2 : 1) : 0)), lp); break;
                case "down":
                    PostMessage(target, WM_MOUSEMOVE, (IntPtr)flags, lp);
                    PostMessage(target, right ? WM_RBUTTONDOWN : WM_LBUTTONDOWN, (IntPtr)(flags | (right ? 2 : 1)), lp);
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
        if (ws.Count > 0) {
            int pid; uint tid = GetWindowThreadProcessId(ws[0].H, out pid);
            var gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
            if (GetGUIThreadInfo(tid, ref gi) && gi.hwndFocus != IntPtr.Zero) return gi.hwndFocus;
            if (lastTarget != IntPtr.Zero) return lastTarget;
            return ws[0].H;
        }
        return lastTarget;
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
        return t + " " + cls + (t == lastTarget ? " (last clicked)" : " (focus)");
    }

    public static void TypeChar(char ch) {
        IntPtr t = KeyTarget();
        if (t != IntPtr.Zero) PostMessage(t, WM_CHAR, (IntPtr)ch, (IntPtr)1);
    }

    public static string Key(int vk, string mods) {
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
