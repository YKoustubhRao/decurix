# Flowera host GUI bridge (Windows). Runs ONE application on a private Win32 desktop object, so it has
# no window on the person's own desktop, and lets Flowera see and drive it over stdin/stdout.
# There is deliberately no network listener: only the process that started this script can reach the app.
#
# stdin, one command per line (fields separated by TAB where a line has more than one):
#   launch <target> <args>   start the app; "OK pid=<n>" or "ERR <reason>"
#   frame                    "FRAME <w> <h> <base64 jpeg>" or "NOFRAME"
#   move|down|up <x> <y> [right] [+mods]   mouse, in frame pixels; mods is any of c s a (Ctrl, Shift, Alt) held during it
#   click <x> <y>            down + up
#   wheel <x> <y> <delta>    scroll (positive is up)
#   copy <vk>                press Ctrl+<vk> (C or X) and answer "CLIP <base64 utf-8 text>" if the clipboard changed
#   type <text>              characters, to whatever was clicked last
#   key <vk> [mods]          one virtual key (Enter 13, Backspace 8, ...), with modifiers held (c s a)
#   windows                  "WINDOWS pid:class:V|h:WxH ..." what exists on the private desktop
#   quit                     end the app and exit (also happens when stdin closes)
param([Parameter(Mandatory = $true)][string]$DesktopName)

Add-Type -ReferencedAssemblies System.Drawing -TypeDefinition @'
using System;
using System.Collections.Generic;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using System.Text;

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

    const uint WM_MOUSEMOVE = 0x0200, WM_LBUTTONDOWN = 0x0201, WM_LBUTTONUP = 0x0202,
               WM_RBUTTONDOWN = 0x0204, WM_RBUTTONUP = 0x0205, WM_MOUSEWHEEL = 0x020A,
               WM_CHAR = 0x0102, WM_KEYDOWN = 0x0100, WM_KEYUP = 0x0101;

    [DllImport("kernel32.dll")] static extern bool GetExitCodeProcess(IntPtr p, out uint code);

    static IntPtr desktop, job, mainWindow, lastTarget, captured, launched;
    // Where the last picture sat on the private desktop: its top-left corner (physical pixels) and its scale, so a click
    // in the picture that was shown is mapped back to the place it was drawn from even if windows have moved since.
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
    public static string Frame() {
        var ws = AppWindows();
        if (ws.Count == 0) return "NOFRAME";
        LocateWindow();
        int l = int.MaxValue, t = int.MaxValue, r = int.MinValue, b = int.MinValue;
        foreach (var w in ws) { l = Math.Min(l, w.V.L); t = Math.Min(t, w.V.T); r = Math.Max(r, w.V.R); b = Math.Max(b, w.V.B); }
        double sc = Scale(mainWindow); // the main window's scale is the picture's, so a DPI-unaware app stays crisp
        int pw = (int)Math.Round((r - l) * sc), ph = (int)Math.Round((b - t) * sc);
        if (pw <= 0 || ph <= 0 || pw > 8192 || ph > 8192) return "NOFRAME";
        using (var bmp = new Bitmap(pw, ph, PixelFormat.Format24bppRgb))
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
            frameL = l; frameT = t; frameScale = sc; haveFrame = true;
            using (var ms = new System.IO.MemoryStream()) {
                bmp.Save(ms, jpeg, jpegQuality);
                return "FRAME " + pw + " " + ph + " " + Convert.ToBase64String(ms.ToArray());
            }
        }
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
        return new POINT { X = frameL + (int)Math.Round(x / frameScale), Y = frameT + (int)Math.Round(y / frameScale) };
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
        if (!haveFrame) Frame();
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
        if (!haveFrame) Frame();
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

# Apps that hand a second launch to the copy already running would open on the person's own desktop, which is exactly what
# this bridge exists to avoid, so they get a profile of their own. That is every Chromium browser and every Electron app,
# recognised by the runtime files that sit beside their executable (a versioned Chrome keeps them one folder down, so the
# well-known browsers are also named). The profile is kept between sessions, per desktop and app, so a sign-in made in the
# hidden app is still there next time - and it is never the person's own profile, which the running copy has locked.
$SingleInstanceByName = @('chrome', 'msedge', 'brave', 'vivaldi', 'opera', 'chromium', 'code', 'cursor', 'slack', 'discord', 'teams', 'ms-teams')
# Browsers additionally skip their first-run and "make me your default" flows: on a private desktop nobody is there to answer
# them, and a modal one blocks the keyboard for the whole window.
$Browsers = @('chrome', 'msedge', 'brave', 'vivaldi', 'opera', 'chromium')
$ProfileRoot = Join-Path $env:LOCALAPPDATA 'Flowera\hidden-profiles'

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
    $entry = Get-StartApps | Where-Object { $_.AppID -eq $appModelId } | Select-Object -First 1
    if (-not $entry) { return $null }
    foreach ($root in @("$env:ProgramData\Microsoft\Windows\Start Menu\Programs", "$env:APPDATA\Microsoft\Windows\Start Menu\Programs")) {
        $found = Get-ChildItem -LiteralPath $root -Recurse -Filter ($entry.Name + '.lnk') -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($found) { return $found.FullName }
    }
    return $null
}

function Resolve-Launch([string]$target, [string]$argLine) {
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
    # Never inherit our own working directory: it can be a \\wsl.localhost UNC path, which Electron apps reject.
    if (-not $workDir) { $workDir = [IO.Path]::GetDirectoryName($target) }
    if ((Test-SingleInstanceApp $target) -and ($argLine -notlike '*user-data-dir*')) {
        $base = [IO.Path]::GetFileNameWithoutExtension($target).ToLower()
        $dir = Join-Path $ProfileRoot "$DesktopName-$base"
        $argLine = (($argLine + " --user-data-dir=`"$dir`"").Trim())
        if ($Browsers -contains $base -and $argLine -notlike '*no-first-run*') { $argLine += ' --no-first-run --no-default-browser-check' }
    }
    return @{ Target = $target; Args = $argLine; WorkDir = $workDir }
}

# "<x> <y> [right] [+<mods>]", mods being any of c (Ctrl), s (Shift), a (Alt).
function Read-MouseArgs([string]$rest) {
    $p = $rest.Split(" ")
    $mods = ''
    foreach ($t in $p[2..($p.Count - 1)]) { if ($t -like '+*') { $mods = $t.Substring(1) } }
    return @{ X = [int]$p[0]; Y = [int]$p[1]; Right = ($p -contains 'right'); Mods = $mods }
}

[Native]::Init()
$stdin = [Console]::In
$launched = $false
try {
    while (($line = $stdin.ReadLine()) -ne $null) {
        $verb, $rest = $line.Split([char[]]@(" ", "`t"), 2)
        try {
            switch ($verb) {
                "launch" {
                    $f = $rest.Split("`t")
                    $r = Resolve-Launch $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
                    if ($r.Error) { Write-Output "ERR $($r.Error)" }
                    else { Write-Output ([Native]::Launch($DesktopName, $r.Target, $r.Args, $r.WorkDir)); $launched = $true }
                }
                "frame" { Write-Output ([Native]::Frame()) }
                "status" { Write-Output ("STATUS " + [Native]::Status()) }
                "focus" { Write-Output ("FOCUS " + [Native]::FocusInfo()) }
                "windows" { Write-Output ("WINDOWS " + [Native]::Windows()) }
                "move"  { $m = Read-MouseArgs $rest; Write-Output ([Native]::Mouse("move", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "down"  { $m = Read-MouseArgs $rest; Write-Output ([Native]::Mouse("down", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "up"    { $m = Read-MouseArgs $rest; Write-Output ([Native]::Mouse("up", $m.X, $m.Y, $m.Right, $m.Mods)) }
                "click" { $m = Read-MouseArgs $rest; [Native]::Mouse("down", $m.X, $m.Y, $false, $m.Mods) | Out-Null; Start-Sleep -Milliseconds 30; Write-Output ([Native]::Mouse("up", $m.X, $m.Y, $false, $m.Mods)) }
                "wheel" { $p = $rest.Split(" "); Write-Output ([Native]::Wheel([int]$p[0], [int]$p[1], [int]$p[2])) }
                "type"  { foreach ($c in $rest.ToCharArray()) { [Native]::TypeChar($c) }; Write-Output "OK" }
                "copy"  { Write-Output ([Native]::Copy([int]$rest)) }
                "key"   { $p = $rest.Split(" "); Write-Output ([Native]::Key([int]$p[0], $(if ($p.Count -gt 1) { $p[1] } else { "" }))) }
                "quit"  { }
                default { Write-Output "ERR unknown command" }
            }
        } catch { Write-Output ("ERR " + $_.Exception.Message) }
        [Console]::Out.Flush()
        if ($verb -eq "quit") { break }
    }
} finally {
    [Native]::Shutdown()
}
