# Decurix host GUI bridge (Windows). Runs ONE application on a private Win32 desktop object, so it has
# no window on the person's own desktop, and lets Decurix see and drive it over stdin/stdout.
# There is deliberately no network listener: only the process that started this script can reach the app.
#
# stdin, one command per line (fields separated by TAB where a line has more than one). Each command is answered by
# exactly one line on stdout, in order:
#   desktop <name>           name the private desktop this helper will create (before launch)
#   launch <target> <args>   start the app; "OK pid=<n>" or "ERR <reason>"
#   check <target> <args>    what launch would decide, without starting anything:
#                            "OK<TAB>exe<TAB>args<TAB>dir<TAB>own<TAB>base<TAB>launcher" (the program it resolved to, for
#                            launchr; launcher is the web-app launcher it stands in for, else empty) or "ERR <reason>"
#   launchr <exe> <args> <dir> <own 0|1> <base> [launcher]   launch what a check resolved, with nothing left to look up
#   viewers <n>              how many people are watching; the picture is only captured while n > 0
#   keyframe                 the next picture sent is a whole one
#   move|down|up <x> <y> [right] [+mods]   mouse, in frame pixels; mods is any of c s a (Ctrl, Shift, Alt) held during it
#   wheel <x> <y> <delta>    scroll (positive is up)
#   copy <vk>                press Ctrl+<vk> (C or X) and answer "CLIP <base64 utf-8 text>" if the clipboard changed
#   type <text>              characters, to whatever was clicked last
#   key <vk> [mods]          one virtual key (Enter 13, Backspace 8, ...), with modifiers held (c s a)
#   status                   what is going on, for saying why nothing showed up
#   tree                     "TREE <fresh 0|1> <base64 JSON>": the app's controls read through UI Automation, in the
#                            picture's pixels ({w, h, title, nodes: [{role, name, box, value, password, checked, ...}]}),
#                            from the last read; fresh says that read began after the latest input. Starts the next read.
#   dropprofiles <name>...   remove the private app profiles kept for these private desktop names (TAB-separated), for a
#                            VM deleted for good: "OK removed=<n> left=<n> locked=<n>" or "ERR <reason>"
#   quit                     end the app and exit (also happens when stdin closes)
#
# Between answers the helper also says, on its own:
#   FRAME <W> <H> <x> <y> <w> <h> <whole 0|1> <n>   then n raw JPEG bytes: the picture is W x H and this JPEG is its
#                            rectangle x,y,w,h (the whole picture when <whole> is 1). Sent only when something changed.
#   EVENT closed             the app has no window left
# -DecurixInstance is the tag of the control plane that started this helper. The helper never reads it: it is there so
# that control plane's sweeps end only its own helpers, never another instance's (internal/httpapi/hostinstance_h.go).
param([Parameter(Mandatory = $true)][string]$DesktopName, [string]$DecurixInstance = '')

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

    static IntPtr desktop, job, lastTarget, captured, launched;
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

    // ---- Mirror mode. The app is not started here, and nothing here ever ends it: it is one the person already has open on
    // their own desktop, and its window is shown and driven from the VM as it is - the same process, signed in as it is,
    // with nothing copied. Everything below that would otherwise act on "the app on this desktop" is limited to that one
    // window and the popups it owns, and Shutdown, which on a private desktop closes and terminates whatever is there, does
    // nothing at all (it would be the person's own programs). Keyboard modifiers are not carried either (WithModifiers):
    // that borrows the app's input queue, which here is the person's own keyboard.
    static bool attached;
    static string attachExe = "", attachTitle = "";
    static IntPtr attachMain;
    [DllImport("user32.dll")] static extern IntPtr GetThreadDesktop(uint tid);

    // The name of the program a process runs, lower case and without .exe; "" when it cannot be read.
    static string ImageBase(int pid) {
        IntPtr proc = OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid);
        if (proc == IntPtr.Zero) return "";
        try {
            var name = new StringBuilder(1024); int size = name.Capacity;
            if (!QueryFullProcessImageName(proc, 0, name, ref size)) return "";
            return System.IO.Path.GetFileNameWithoutExtension(name.ToString()).ToLowerInvariant();
        } finally { CloseHandle(proc); }
    }

    // The window to mirror: the topmost visible, not minimised, unowned top-level window of the program, whose title holds
    // the words asked for (any, when none were).
    static IntPtr FindMain() {
        IntPtr found = IntPtr.Zero;
        EnumProc each = (h, l) => {
            if (!IsWindowVisible(h) || IsIconic(h) || GetWindow(h, 4 /* GW_OWNER */) != IntPtr.Zero) return true;
            int pid; GetWindowThreadProcessId(h, out pid);
            if (ImageBase(pid) != attachExe) return true;
            var title = new StringBuilder(512); GetWindowText(h, title, 512);
            if (title.Length == 0) return true;
            if (attachTitle.Length > 0 && title.ToString().IndexOf(attachTitle, StringComparison.OrdinalIgnoreCase) < 0) return true;
            found = h;
            return false;
        };
        EnumDesktopWindows(desktop, each, IntPtr.Zero);
        return found;
    }

    // Whether a window is the mirrored one or a popup (a menu, a drop-down, a dialog) it owns. True for anything on a
    // private desktop, where there is nothing else.
    static bool Belongs(IntPtr h) {
        if (!attached) return true;
        if (IsPopup(h)) return true; // a window the VM's page opened (see WatchPopups)
        if (attachMain == IntPtr.Zero || !IsWindow(attachMain)) {
            if (borrowed) return false; // the window made for the VM is gone; every other one is the person's
            attachMain = FindMain();
        }
        if (attachMain == IntPtr.Zero) return false;
        if (h == attachMain) return true;
        IntPtr owner = GetWindow(h, 4 /* GW_OWNER */);
        for (int i = 0; i < 4 && owner != IntPtr.Zero; i++) {
            if (owner == attachMain || IsPopup(owner)) return true;
            owner = GetWindow(owner, 4);
        }
        return false;
    }

    // Mirrors the window of program exe (a bare name such as chrome) that the person has open on their own desktop.
    public static string Attach(string exe, string title) {
        if (exe == null || !System.Text.RegularExpressions.Regex.IsMatch(exe, @"^[A-Za-z0-9._-]{1,64}$")) return "ERR not a program name";
        if (desktop != IntPtr.Zero || attached) return "ERR this helper already has an app";
        attachExe = exe.ToLowerInvariant();
        if (attachExe.EndsWith(".exe")) attachExe = attachExe.Substring(0, attachExe.Length - 4);
        attachTitle = title == null ? "" : title.Trim();
        desktop = GetThreadDesktop(GetCurrentThreadId()); // this helper's own: the person's
        if (desktop == IntPtr.Zero) return "ERR no desktop";
        attached = true;
        attachMain = FindMain();
        if (attachMain == IntPtr.Zero) {
            attached = false; desktop = IntPtr.Zero;
            return "ERR " + attachExe + " has no window open on this computer right now. Open it, and do not minimise it, then try again.";
        }
        return "OK pid=0"; // no process of ours: nothing for the control plane to end
    }

    // ---- A borrowed window. A browser the person uses is ONE process per profile folder (Chrome allows no other), on their own
    // desktop, and cannot also run on a private desktop of ours: their own Chrome would then hand every window it opens to the
    // process on the hidden desktop, out of their sight (found live 2026-11-13: "I can't open Chrome from my device"). So the VM
    // is given one window of that very process: asked for the way any second launch asks (--new-window), found as the one window
    // that was not there before, and made invisible to the person - fully transparent, click-through, off the taskbar and
    // Alt-Tab, and above everything so nothing can cover it. That is also what keeps Chrome painting it: it stops drawing a
    // window that is off the screen or covered by another (measured 2026-11-13: off the screen stops, an alpha of 1/255 on screen
    // does not, covered or not). The window carries a window property, so a helper that is ended without ceremony leaves no
    // window nobody can find: the next one takes it over. Every other window of the browser is the person's and is never touched
    // (Belongs); the one window made here is closed when the session ends, and given back to the person if it will not close.
    const string WindowMark = "DecurixWorkerWindow";
    // Which profile a window is of (each VM app is its own profile: a shortcut's --profile-directory): a window left by a helper
    // that died is taken over only by a helper of the same profile. A hash of the launch's arguments, never 0.
    const string ProfileMark = "DecurixWorkerProfile";
    static int profileKey;
    static int ProfileKey(string args) {
        uint h = 2166136261;
        foreach (char c in (args ?? "").Trim().ToLowerInvariant()) { h ^= c; h *= 16777619; }
        return (int)(h | 1);
    }
    static bool borrowed;
    static readonly Dictionary<IntPtr, int> originalStyle = new Dictionary<IntPtr, int>();
    [DllImport("user32.dll")] static extern int SetWindowLong(IntPtr h, int index, int value);
    [DllImport("user32.dll")] static extern bool SetLayeredWindowAttributes(IntPtr h, uint key, byte alpha, uint flags);
    [DllImport("user32.dll")] static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
    [DllImport("user32.dll")] static extern bool ShowWindow(IntPtr h, int cmd);
    [DllImport("user32.dll")] static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")] static extern bool SetForegroundWindow(IntPtr h);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern bool SetProp(IntPtr h, string name, IntPtr data);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern IntPtr GetProp(IntPtr h, string name);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern IntPtr RemoveProp(IntPtr h, string name);

    // The visible, unowned top-level windows of the program that are browser windows: not a bubble or a tooltip - unless anySize,
    // one has to be a window's size, or minimised (a browser started minimised opens its next window minimised too).
    static List<IntPtr> BrowserWindows(bool anySize) {
        var list = new List<IntPtr>();
        EnumProc each = (h, l) => {
            if (!IsWindowVisible(h) || GetWindow(h, 4 /* GW_OWNER */) != IntPtr.Zero) return true;
            var cls = new StringBuilder(80); GetClassName(h, cls, 80);
            if (cls.ToString() != "Chrome_WidgetWin_1") return true;
            if (!anySize) { RECT r; GetWindowRect(h, out r); if ((r.R - r.L < 300 || r.B - r.T < 200) && !IsIconic(h)) return true; }
            int pid; GetWindowThreadProcessId(h, out pid);
            if (ImageBase(pid) == attachExe) list.Add(h);
            return true;
        };
        EnumDesktopWindows(desktop, each, IntPtr.Zero);
        return list;
    }

    static string TitleOf(IntPtr h) { var t = new StringBuilder(256); GetWindowText(h, t, 256); return t.ToString(); }

    // Whether the helper with this process id is still running. A mark is its process id: a window marked by one that has gone is the next
    // helper's to take over, and one marked by one that has not is another VM's (another profile's) and is never touched.
    static bool HelperAlive(int pid) {
        try {
            using (var q = System.Diagnostics.Process.GetProcessById(pid)) {
                if (q.HasExited) return false;
                using (var me = System.Diagnostics.Process.GetCurrentProcess()) return q.ProcessName.Equals(me.ProcessName, StringComparison.OrdinalIgnoreCase);
            }
        } catch { return false; }
    }
    static int MyPid() { using (var me = System.Diagnostics.Process.GetCurrentProcess()) return me.Id; }

    // The window a helper of this profile that was ended without ceremony left behind, if there is one (another profile's is left for a
    // helper of its own); and any popup a dead helper had adopted goes back to the person.
    static IntPtr FindMarked() {
        IntPtr found = IntPtr.Zero;
        foreach (IntPtr h in BrowserWindows(true)) {
            IntPtr main = GetProp(h, WindowMark);
            if (found == IntPtr.Zero && main != IntPtr.Zero && !HelperAlive((int)main) && (int)GetProp(h, ProfileMark) == profileKey) found = h;
            IntPtr pop = GetProp(h, PopupMark);
            if (pop != IntPtr.Zero && !HelperAlive((int)pop)) UnveilAsync(h);
        }
        return found;
    }

    // ---- Popups. A page can open a window of its own (a sign-in popup is one): it is another top-level window of the same browser, on the
    // person's own screen, and nothing in it says whose it is. It is the VM's when it appeared just after something was sent to the VM's
    // window and the person had touched no key and no mouse button for a second before: then nobody else could have opened it. Such a
    // window is veiled like the first one, shown with it and driven with it, and closed with it. When the person is working at the same
    // moment it is not told apart and is left as it is - the person's own window, on their own screen, which is what it was before.
    const string PopupMark = "DecurixWorkerPopup";
    static readonly Dictionary<IntPtr, bool> popups = new Dictionary<IntPtr, bool>();
    static int ourInputAt, personActiveAt;
    static IntPtr lastPersonTop;            // the person's own last foreground window, whatever program it is
    static System.Threading.Mutex borrowGate;
    static bool IsPopup(IntPtr h) { lock (popups) return popups.ContainsKey(h); }
    static List<IntPtr> LivePopups() {
        var list = new List<IntPtr>();
        lock (popups) {
            foreach (IntPtr h in new List<IntPtr>(popups.Keys)) { if (IsWindow(h)) list.Add(h); else popups.Remove(h); }
        }
        return list;
    }

    static void WatchPopups() {
        var known = new Dictionary<IntPtr, bool>();
        var firstSeen = new Dictionary<IntPtr, int>();
        var ours = new Dictionary<IntPtr, bool>();
        foreach (IntPtr h in BrowserWindows(true)) known[h] = true;
        IntPtr fore = IntPtr.Zero;
        while (borrowed && !stopCapture) {
            System.Threading.Thread.Sleep(20);
            try {
                for (int vk = 1; vk < 255; vk++) if ((GetAsyncKeyState(vk) & 0x8000) != 0) { personActiveAt = Environment.TickCount; break; }
                IntPtr top = GetForegroundWindow();
                var current = BrowserWindows(true);
                // A helper that is looking for its window is not to see that window taken for a popup.
                bool otherLooking = false;
                try { if (borrowGate != null) { if (borrowGate.WaitOne(0)) borrowGate.ReleaseMutex(); else otherLooking = true; } } catch { }
                foreach (IntPtr h in current) {
                    if (known.ContainsKey(h) || h == attachMain) continue;
                    int now = Environment.TickCount;
                    if (otherLooking) { known[h] = true; continue; }
                    if (!firstSeen.ContainsKey(h)) { firstSeen[h] = now; ours[h] = now - ourInputAt < 5000 && now - personActiveAt > 1000; }
                    if (!ours[h]) { known[h] = true; continue; }
                    RECT r; GetWindowRect(h, out r);
                    bool windowish = (GetWindowLong(h, -16 /* GWL_STYLE */) & 0xC00000 /* WS_CAPTION */) == 0xC00000 && r.R - r.L >= 200 && r.B - r.T >= 120
                                     && GetProp(h, WindowMark) == IntPtr.Zero;
                    if (windowish) {
                        known[h] = true;
                        lock (popups) popups[h] = true;
                        SetProp(h, PopupMark, (IntPtr)MyPid());
                        VeilAsync(h);
                        IntPtr back = fore, opened = h;
                        new System.Threading.Thread(() => { try { GiveBackFocus(back, opened); } catch { } }) { IsBackground = true }.Start();
                    } else if (now - firstSeen[h] > 1500) known[h] = true;
                }
                // The person's own foreground - never the VM's window, nor a window it is about to adopt (a popup is in front before it is judged).
                bool pending = top != IntPtr.Zero && ours.ContainsKey(top) && ours[top] && !known.ContainsKey(top);
                if (top != IntPtr.Zero && !Ours(top) && !pending) {
                    int tp, mp; GetWindowThreadProcessId(top, out tp); GetWindowThreadProcessId(attachMain, out mp);
                    if (tp == mp) personWindow = top;
                    lastPersonTop = top;
                } else if (Ours(top) && lendBack == IntPtr.Zero && !holdKeys && lastPersonTop != IntPtr.Zero && IsWindow(lastPersonTop)) {
                    // The VM's window became the foreground with no keystroke lent (the browser activated it, or a popup it opened): the person's
                    // keyboard is never left in a window they cannot see.
                    GiveBackFocus(lastPersonTop, IntPtr.Zero);
                }
                if (toldActive != IntPtr.Zero && Environment.TickCount - lastKeysAt > 2000 && lendBack == IntPtr.Zero) TakeBackActivation();
                if (lendBack != IntPtr.Zero && Monitor.TryEnter(lendLock)) {
                    try {
                        bool theirKey; lock (heldKeys) theirKey = heldKeys.Count > 0;
                        if (!Ours(top) || theirKey || Environment.TickCount - lastKeysAt > 1500) EndLend();
                    } finally { Monitor.Exit(lendLock); }
                }
                // What the person last had in front, other than the browser's own windows, is what the keyboard is given back to.
                if (top != IntPtr.Zero && !known.ContainsKey(top) && !IsPopup(top) && top != attachMain) fore = top;
                LivePopups();
            } catch { }
        }
    }

    // Makes a window invisible to the person while it stays on screen to the browser: layered at an alpha of 1/255, click-through,
    // a tool window (no taskbar button, not in Alt-Tab), topmost, and restored if something minimised it. Cheap to repeat.
    static void Veil(IntPtr h) {
        if (h == IntPtr.Zero || !IsWindow(h)) return;
        if (IsIconic(h)) ShowWindow(h, 4 /* SW_SHOWNOACTIVATE */); // a window of the person that is minimised is restored, without taking the keyboard
        int ex = GetWindowLong(h, -20 /* GWL_EXSTYLE */);
        // WS_EX_NOACTIVATE: a click the VM sends is not the browser's cue to take the person's foreground for it (measured 2026-11-13: with
        // the person in that browser, a posted click made the invisible window the foreground one, and their keys went into it), nor does
        // Windows give it the foreground when the person's own window closes. Its keyboard is lent to it instead (LendKeyboard).
        int want = ex | 0x80000 /* WS_EX_LAYERED */ | 0x20 /* WS_EX_TRANSPARENT */ | 0x80 /* WS_EX_TOOLWINDOW */ | 0x08000000 /* WS_EX_NOACTIVATE */;
        bool changed = want != ex;
        if (changed) { lock (originalStyle) originalStyle[h] = ex; SetWindowLong(h, -20, want); }
        SetLayeredWindowAttributes(h, 0, 1, 2 /* LWA_ALPHA */);
        // The taskbar learns that a window became a tool window only when it is shown again.
        if (changed) { ShowWindow(h, 0 /* SW_HIDE */); ShowWindow(h, 8 /* SW_SHOWNA */); }
        SetWindowPos(h, (IntPtr)(-1) /* HWND_TOPMOST */, 0, 0, 0, 0, 0x0001 | 0x0002 | 0x0010 /* NOSIZE | NOMOVE | NOACTIVATE */);
    }

    // Gives a window back to the person as an ordinary one of theirs.
    static void Unveil(IntPtr h) {
        if (h == IntPtr.Zero || !IsWindow(h)) return;
        RemoveProp(h, WindowMark); RemoveProp(h, ProfileMark); RemoveProp(h, PopupMark);
        lock (popups) popups.Remove(h);
        SetLayeredWindowAttributes(h, 0, 255, 2);
        int was; // what the window had before it was veiled; a window taken over from a dead helper has lost that, and gets back the plain style
        bool kept; lock (originalStyle) kept = originalStyle.TryGetValue(h, out was);
        SetWindowLong(h, -20, kept ? was : GetWindowLong(h, -20) & ~(0x80000 | 0x20 | 0x80 | 0x08000000));
        SetWindowPos(h, (IntPtr)(-2) /* HWND_NOTOPMOST */, 0, 0, 0, 0, 0x0001 | 0x0002 | 0x0010);
        ShowWindow(h, 0); ShowWindow(h, 8);
    }

    // Everything done to a window of another process (a style, a show state, a position) is a message sent to it and waits for its
    // thread to answer, and a browser that has just started - with the person's own profile and extensions, on a busy computer - does
    // not answer for a long time (found live 2026-11-13: a window sat visible and unveiled for over a minute while this thread
    // waited on it). So that work is queued on a thread of its own per window, one after another, and nothing waits on it unbounded.
    static readonly Dictionary<IntPtr, System.Threading.Thread> windowWork = new Dictionary<IntPtr, System.Threading.Thread>();
    static readonly Dictionary<IntPtr, RECT> homeOf = new Dictionary<IntPtr, RECT>();
    static void QueueWork(IntPtr h, Action work) {
        lock (windowWork) {
            System.Threading.Thread before; windowWork.TryGetValue(h, out before);
            var th = new System.Threading.Thread(() => {
                if (before != null) before.Join();
                try { work(); } catch { }
            }) { IsBackground = true };
            windowWork[h] = th;
            th.Start();
        }
    }
    // Waits for the work queued for a window, for at most ms; whether it is all done.
    static bool WorkDone(IntPtr h, int ms) {
        System.Threading.Thread th;
        lock (windowWork) { if (!windowWork.TryGetValue(h, out th)) return true; }
        return th.Join(ms);
    }

    // Veils a window that has just appeared, and is not to be seen by the person for a moment longer than it takes. The first thing
    // - a move off the screen, asked for without waiting (SWP_ASYNCWINDOWPOS) - takes it out of their sight at once, whatever its
    // thread is doing. Off the screen a browser stops painting, so that is only until the work queued behind it, which waits for the
    // browser as long as it must, makes the window invisible where it is and puts it back.
    static void VeilAsync(IntPtr h) {
        RECT home; GetWindowRect(h, out home);
        bool minimised = IsIconic(h);
        lock (windowWork) homeOf[h] = home;
        if (!minimised) SetWindowPos(h, IntPtr.Zero, -32000, -32000, 0, 0, 0x0001 | 0x0004 | 0x0010 | 0x4000 /* NOSIZE | NOZORDER | NOACTIVATE | ASYNCWINDOWPOS */);
        QueueWork(h, () => {
            Veil(h);
            if (!minimised) SetWindowPos(h, (IntPtr)(-1) /* HWND_TOPMOST */, home.L, home.T, 0, 0, 0x0001 | 0x0010);
        });
    }

    // Gives a window of the person's back to them, where it was, once its browser can be told.
    static void UnveilAsync(IntPtr h) {
        RECT home; bool known; lock (windowWork) known = homeOf.TryGetValue(h, out home);
        QueueWork(h, () => {
            Unveil(h);
            if (known) SetWindowPos(h, (IntPtr)(-2) /* HWND_NOTOPMOST */, home.L, home.T, 0, 0, 0x0001 | 0x0010);
        });
    }

    // A window opened for the VM takes the keyboard from whatever the person was typing in; hands it back. Windows lets only the
    // foreground thread do that, so this thread joins the input of the one that has it for the moment.
    static void GiveBackFocus(IntPtr was, IntPtr ours) {
        if (was == IntPtr.Zero || was == ours || !IsWindow(was)) return;
        IntPtr now = GetForegroundWindow();
        if (now == was) return;
        // A thread joins another's input only if it has a message queue of its own, and a worker thread has none until it asks for one
        // (measured 2026-11-13: from the watcher thread the foreground stayed on the VM's popup).
        MSG none; PeekMessage(out none, IntPtr.Zero, 0, 0, 0 /* PM_NOREMOVE */);
        int pid; uint holder = GetWindowThreadProcessId(now, out pid);
        uint me = GetCurrentThreadId();
        bool joined = holder != 0 && holder != me && AttachThreadInput(me, holder, true);
        SetForegroundWindow(was);
        if (joined) AttachThreadInput(me, holder, false);
    }

    // Started through the shell, which hands the new process none of this one's handles: this helper's standard input and output are
    // the line protocol with Decurix, and Chrome, asked to open a window in a running copy, says so on whatever it was given.
    static void Run(string target, string args, string workDir) {
        var psi = new System.Diagnostics.ProcessStartInfo(target, args);
        psi.UseShellExecute = true;
        if (!string.IsNullOrEmpty(workDir)) psi.WorkingDirectory = workDir;
        var p = System.Diagnostics.Process.Start(psi);
        if (p != null) p.Dispose();
    }

    // Asks the browser for a window of its own - one launch, whether it is running (the request is handed to the running process) or
    // not (it starts, and its first window is this one) - and finds it: the new window whose page says about:blank. Every new window is
    // veiled at once, so that none shows to the person for longer than it takes to look, and is judged by its page: the one asked for
    // is kept, and any other (one the person opened in the same moment, one a browser restores when it starts) is given back.
    // Nothing is started minimised: a browser opens its later windows the way its last one was, and the person's own would open
    // minimised too (found live 2026-11-13).
    static IntPtr NewWindow(string target, string args, string workDir, IntPtr was) {
        var before = new Dictionary<IntPtr, bool>();
        foreach (IntPtr h in BrowserWindows(true)) before[h] = true;
        Run(target, (args + " --new-window about:blank").Trim(), workDir);
        var seen = new Dictionary<IntPtr, int>();
        var rejected = new Dictionary<IntPtr, bool>();
        IntPtr ours = IntPtr.Zero;
        // The person's own browser, started cold with all its extensions on a busy computer, took over half a minute to show its first
        // window (found live 2026-11-13: it gave up at 30 s, and the window came a moment later).
        int end = Environment.TickCount + 120000;
        while (Environment.TickCount < end && ours == IntPtr.Zero) {
            foreach (IntPtr h in BrowserWindows(false)) {
                if (before.ContainsKey(h) || rejected.ContainsKey(h) || seen.ContainsKey(h)) continue;
                seen[h] = Environment.TickCount;
                VeilAsync(h);
                IntPtr newWin = h;
                new System.Threading.Thread(() => { try { GiveBackFocus(was, newWin); } catch { } }) { IsBackground = true }.Start();
            }
            foreach (IntPtr h in new List<IntPtr>(seen.Keys)) {
                if (!IsWindow(h)) { seen.Remove(h); continue; }
                string title = TitleOf(h);
                if (title.StartsWith("about:blank", StringComparison.OrdinalIgnoreCase)) { ours = h; break; }
                // A window that has not got to its page yet says nothing, only the browser's name, or "New Tab" (found live 2026-11-13: the
                // person's own profile shows that for a moment before about:blank, and the window was given back on sight, for good).
                // One that goes on saying anything else is a page of the person's.
                bool transient = title.Length == 0 || title.IndexOf(" - ", StringComparison.Ordinal) < 0 || title.StartsWith("New Tab", StringComparison.OrdinalIgnoreCase);
                int age = Environment.TickCount - seen[h];
                if ((!transient && age > 2500) || age > 15000) { rejected[h] = true; seen.Remove(h); UnveilAsync(h); }
            }
            if (ours == IntPtr.Zero) System.Threading.Thread.Sleep(4);
        }
        foreach (IntPtr h in seen.Keys) if (h != ours) UnveilAsync(h);
        return ours;
    }

    // Keeps the window invisible: a browser, or the person, can change what was set (Win+D minimises every window).
    static void KeepVeiled() {
        while (borrowed && !stopCapture) {
            System.Threading.Thread.Sleep(700);
            try { Veil(attachMain); foreach (IntPtr h in LivePopups()) Veil(h); } catch { }
        }
    }

    // Shows and drives one window of the browser the person uses (starting it, when it is not running), under their own profile and
    // signed in as they are. exe is a bare program name such as chrome.
    public static string Borrow(string exe, string target, string args, string workDir) {
        if (exe == null || !System.Text.RegularExpressions.Regex.IsMatch(exe, @"^[A-Za-z0-9._-]{1,64}$")) return "ERR not a program name";
        if (desktop != IntPtr.Zero || attached) return "ERR this helper already has an app";
        attachExe = exe.ToLowerInvariant();
        if (attachExe.EndsWith(".exe")) attachExe = attachExe.Substring(0, attachExe.Length - 4);
        attachTitle = "";
        desktop = GetThreadDesktop(GetCurrentThreadId()); // this helper's own: the person's
        if (desktop == IntPtr.Zero) return "ERR no desktop";
        attached = true; borrowed = true;
        IntPtr was = GetForegroundWindow();
        IntPtr mine = IntPtr.Zero; string why = ""; bool fresh = false;
        ourInputAt = Environment.TickCount - 100000;
        profileKey = ProfileKey(args);
        // Two helpers (two profiles) starting together must not take each other's about:blank window for their own: one looks at a time.
        if (borrowGate == null) borrowGate = new System.Threading.Mutex(false, "Local\\DecurixBorrowWindow");
        bool gated = false;
        try {
            try { gated = borrowGate.WaitOne(130000); } catch (System.Threading.AbandonedMutexException) { gated = true; }
            mine = FindMarked();
            if (mine == IntPtr.Zero) { mine = NewWindow(target, args, workDir, was); fresh = mine != IntPtr.Zero; }
        } catch (Exception e) { why = " (" + e.Message + ")"; }
        finally { if (gated) { try { borrowGate.ReleaseMutex(); } catch { } } }
        if (mine == IntPtr.Zero) {
            attached = false; borrowed = false; desktop = IntPtr.Zero;
            return "ERR The VM could not get a window of your " + attachExe + "." + why + " Try again in a moment.";
        }
        attachMain = mine;
        SetProp(mine, WindowMark, (IntPtr)MyPid()); SetProp(mine, ProfileMark, (IntPtr)profileKey);
        bool queued; lock (windowWork) queued = windowWork.ContainsKey(mine);
        if (!queued) QueueWork(mine, () => Veil(mine)); // a window taken over from a helper that died is veiled where it is
        WorkDone(mine, 60000); // the browser answers when it is ready; the VM is not told its window is there before that
        var keep = new System.Threading.Thread(KeepVeiled) { IsBackground = true };
        keep.Start();
        if (fresh) { try { OpenStartPage(mine); } catch { } }
        // A browser activates the window it opens: the person's foreground goes back to them (measured 2026-11-13: it stayed on the invisible
        // window, and their keys went into it). From here on the watcher keeps it so.
        if (was != IntPtr.Zero && !Ours(was) && IsWindow(was)) {
            lastPersonTop = was;
            if (Ours(GetForegroundWindow())) GiveBackFocus(was, IntPtr.Zero);
        }
        if (toldActive == IntPtr.Zero && !Ours(GetForegroundWindow())) PersonLastActive(mine); // the browser made its new window its last active one
        new System.Threading.Thread(WatchPopups) { IsBackground = true }.Start();
        return "OK pid=0"; // no process of ours: nothing for the control plane to end
    }

    // The window is asked for on a blank page because that is the one page that says the same in every language and is never one of
    // the person's, so it is how the window is told apart (NewWindow). What the VM is given is the browser's own new tab page - what
    // a window of the person's opens on, with their shortcuts and their account - reached the way a person reaches it: a click in the
    // address bar (found by its class, the same in every language), the address, Enter, all sent to this window only. A chrome://
    // address cannot be asked for on the command line (measured 2026-11-13: a window asked for with about:blank and chrome://newtab/
    // had the one tab), and keys sent to a window that was never clicked go nowhere - a browser's window has no focused control until
    // then (measured: Ctrl+L did nothing). Not getting there fails nothing: the page stays blank.
    static void OpenStartPage(IntPtr h) {
        var bar = System.Windows.Rect.Empty; var frame = System.Windows.Rect.Empty;
        var ask = new System.Threading.Thread(() => {
            try {
                var root = System.Windows.Automation.AutomationElement.FromHandle(h);
                var el = root.FindFirst(System.Windows.Automation.TreeScope.Descendants, new System.Windows.Automation.PropertyCondition(
                    System.Windows.Automation.AutomationElement.ClassNameProperty, "OmniboxViewViews"));
                if (el != null) { bar = el.Current.BoundingRectangle; frame = root.Current.BoundingRectangle; }
            } catch { }
        }) { IsBackground = true };
        ask.Start();
        if (!ask.Join(15000) || bar.IsEmpty || frame.IsEmpty || bar.Width < 20) return; // a browser busy starting answers late; never waited on unbounded
        // Where the bar is in the window, applied to where the window is: a browser reports places as of when it last laid itself out, and the
        // window was out of sight a moment ago (measured: the bar said -31794,-31936 with the window back at 25,25).
        RECT win; GetWindowRect(h, out win);
        var at = new POINT(); at.X = win.L + (int)(bar.X - frame.X + Math.Min(bar.Width / 2, 200)); at.Y = win.T + (int)(bar.Y - frame.Y + bar.Height / 2);
        IntPtr lp = ClientLParam(h, at);
        try { LendKeyboard(h); OpenStartPageKeys(h, lp); } finally { KeysDone(); }
        for (int i = 0; i < 50 && TitleOf(h).StartsWith("about:blank", StringComparison.OrdinalIgnoreCase); i++) System.Threading.Thread.Sleep(100);
    }
    static void OpenStartPageKeys(IntPtr h, IntPtr lp) {
        PostMessage(h, WM_MOUSEMOVE, IntPtr.Zero, lp);
        PostMessage(h, WM_LBUTTONDOWN, (IntPtr)1, lp);
        PostMessage(h, WM_LBUTTONUP, IntPtr.Zero, lp);
        System.Threading.Thread.Sleep(250);
        // What the address bar says is selected, so the address typed replaces it: a click selects it all only when the bar did not have the
        // keyboard already, and a window whose keyboard is lent to it has (measured: "about:blank" and the address typed came out mixed).
        WithModifiersShared(h, "c", () => {
            PostMessage(h, WM_KEYDOWN, (IntPtr)0x41 /* A */, (IntPtr)1);
            PostMessage(h, WM_KEYUP, (IntPtr)0x41, (IntPtr)unchecked((int)0xC0000001));
            System.Threading.Thread.Sleep(Hold(150));
        });
        foreach (char c in "chrome://newtab/") PostMessage(h, WM_CHAR, (IntPtr)c, (IntPtr)1);
        PostMessage(h, WM_KEYDOWN, (IntPtr)13, (IntPtr)1);
        PostMessage(h, WM_CHAR, (IntPtr)13, (IntPtr)1);
        PostMessage(h, WM_KEYUP, (IntPtr)13, (IntPtr)unchecked((int)0xC0000001));
    }

    // Ends the session's window: asked to close, the way a person would; given back to them, still open, if it will not (a
    // page asking "leave this site?" is waiting on an answer nobody can see).
    static void CloseBorrowed() {
        lock (lendLock) { if (lendBack != IntPtr.Zero || holdKeys) EndLend(); }
        borrowed = false;
        // The popups the VM's page opened go first, asked to close the way a person would, and are given back to the person if one will not.
        var ps = LivePopups();
        foreach (IntPtr q in ps) { WorkDone(q, 5000); PostMessage(q, 0x0010 /* WM_CLOSE */, IntPtr.Zero, IntPtr.Zero); }
        for (int i = 0; i < 20; i++) { bool any = false; foreach (IntPtr q in ps) if (IsWindow(q)) any = true; if (!any) break; System.Threading.Thread.Sleep(100); }
        foreach (IntPtr q in ps) if (IsWindow(q)) UnveilAsync(q);
        IntPtr h = attachMain;
        if (h == IntPtr.Zero || !IsWindow(h)) return;
        WorkDone(h, 20000);
        PostMessage(h, 0x0010 /* WM_CLOSE */, IntPtr.Zero, IntPtr.Zero);
        for (int i = 0; i < 30 && IsWindow(h); i++) System.Threading.Thread.Sleep(100);
        if (IsWindow(h)) UnveilAsync(h);
    }

    public static string Launch(string desktopName, string exe, string args, string workDir) {
        desktop = CreateDesktop(desktopName, IntPtr.Zero, IntPtr.Zero, 0, 0x10000000, IntPtr.Zero);
        if (desktop == IntPtr.Zero) return "ERR CreateDesktop failed (" + Marshal.GetLastWin32Error() + ")";
        IntPtr process;
        string said = Spawn(desktopName, exe, args, workDir, out process, false);
        if (process != IntPtr.Zero) launched = process;
        return said;
    }

    // Whether a program is a console one (cmd, PowerShell, a batch file), read from the file itself. Such a program makes
    // no window: it is given a console, and by default that is the console of whatever started it.
    static bool ConsoleProgram(string exe) {
        try {
            string ext = System.IO.Path.GetExtension(exe).ToLowerInvariant();
            if (ext == ".bat" || ext == ".cmd") return true;
            using (var f = new System.IO.FileStream(exe, System.IO.FileMode.Open, System.IO.FileAccess.Read, System.IO.FileShare.ReadWrite | System.IO.FileShare.Delete))
            using (var r = new System.IO.BinaryReader(f)) {
                if (f.Length < 0x100 || r.ReadUInt16() != 0x5A4D /* MZ */) return false;
                f.Position = 0x3C; int pe = r.ReadInt32();
                if (pe <= 0 || pe + 0x5E > f.Length) return false;
                f.Position = pe; if (r.ReadUInt32() != 0x4550 /* PE */) return false;
                f.Position = pe + 0x5C; return r.ReadUInt16() == 3; // IMAGE_SUBSYSTEM_WINDOWS_CUI
            }
        } catch { return false; }
    }

    // Starts a program on the private desktop, in the job. Nothing started here may share this helper's console or its
    // standard input and output: those are the line protocol with Decurix. Found live: a worker's Terminal (PowerShell
    // -NoExit) was given them, as Windows gives a console program its parent's - it had no window, read the commands meant
    // for this helper off the pipe and wrote its prompt in among the answers and pictures, and from then on every click
    // on that worker's screen was lost or answered with garbage. So a console program gets a console of its own: one
    // with no window for a worker's agent (windowless), otherwise the classic console window, asked for by name -
    // left to Windows it would be handed to the person's default terminal app, on their own screen.
    static string Spawn(string desktopName, string exe, string args, string workDir, out IntPtr process, bool windowless) {
        process = IntPtr.Zero;
        STARTUPINFO si = new STARTUPINFO();
        si.cb = Marshal.SizeOf(si);
        si.lpDesktop = desktopName;
        PROCESS_INFORMATION pi;
        string app = exe;
        string cmd = "\"" + exe + "\"" + (string.IsNullOrEmpty(args) ? "" : " " + args);
        int flags = 0x400 /* CREATE_UNICODE_ENVIRONMENT */;
        bool console = ConsoleProgram(exe);
        // A PowerShell told to hide its console (the worker's Files browser, which is a window of its own) has none shown.
        if (console && args != null && System.Text.RegularExpressions.Regex.IsMatch(args, @"(?i)(^|\s)-w(indowstyle)?\s+hidden(\s|$)")) windowless = true;
        if (console && windowless) flags |= 0x08000000 /* CREATE_NO_WINDOW: a new console, not ours */;
        else {
            if (console) {
                app = System.IO.Path.Combine(Environment.SystemDirectory, "conhost.exe");
                cmd = "\"" + app + "\" " + cmd;
            }
            si.dwFlags = 0x100 /* STARTF_USESTDHANDLES, and none given */;
        }
        IntPtr env = CleanEnvironment();
        // Started suspended, put in the job, and only then let run (B-27): a program that ran first could start children
        // outside the job before it was assigned, and those would outlive the helper. One that cannot be put in the job is
        // not started at all.
        flags |= 0x4 /* CREATE_SUSPENDED */;
        bool started = CreateProcess(app, cmd, IntPtr.Zero, IntPtr.Zero, false, flags, env, string.IsNullOrEmpty(workDir) ? null : workDir, ref si, out pi);
        int err = Marshal.GetLastWin32Error();
        Marshal.FreeHGlobal(env);
        if (!started) return "ERR CreateProcess failed (" + err + ")";
        if (!AssignProcessToJobObject(job, pi.hProcess)) {
            TerminateProcess(pi.hProcess, 0); CloseHandle(pi.hThread); CloseHandle(pi.hProcess);
            return "ERR the app could not be tied to its helper, so it was not started";
        }
        ResumeThread(pi.hThread);
        CloseHandle(pi.hThread);
        process = pi.hProcess;
        return "OK pid=" + pi.dwProcessId;
    }

    [StructLayout(LayoutKind.Sequential)] struct JOBPORT { public IntPtr Key, Port; }
    [DllImport("kernel32.dll")] static extern bool SetInformationJobObject(IntPtr j, int cls, ref JOBPORT info, int len);
    [DllImport("kernel32.dll")] static extern IntPtr CreateIoCompletionPort(IntPtr file, IntPtr existing, UIntPtr key, uint threads);
    [DllImport("kernel32.dll")] static extern bool GetQueuedCompletionStatus(IntPtr port, out uint code, out UIntPtr key, out IntPtr value, uint ms);
    [DllImport("kernel32.dll")] static extern bool TerminateJobObject(IntPtr j, uint code);
    [DllImport("kernel32.dll")] static extern uint ResumeThread(IntPtr t);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern bool QueryFullProcessImageName(IntPtr p, uint flags, StringBuilder name, ref int size);

    static readonly string[] BrowserPrograms = { "msedge", "chrome", "brave", "firefox", "opera", "vivaldi", "chromium", "iexplore" };

    // Runs a web app's launcher for what it does before it opens the page - starting the app's own server - and stops
    // it there: any browser it starts is ended the moment it is created, before it can hand the page to the copy the
    // person has open on their own screen. The page itself is then opened here, in a browser of the worker's own.
    // Returns once the launcher has ended, has started its browser, or has had its time.
    [DllImport("kernel32.dll")] static extern uint WaitForSingleObject(IntPtr handle, uint milliseconds);
    public static string RunLauncher(string desktopName, string exe, string workDir, int seconds) {
        IntPtr guard = CreateJobObject(IntPtr.Zero, null);
        IntPtr port = CreateIoCompletionPort((IntPtr)(-1), IntPtr.Zero, UIntPtr.Zero, 1);
        if (guard == IntPtr.Zero || port == IntPtr.Zero) return "ERR could not watch the launcher";
        try {
            var watch = new JOBPORT { Key = guard, Port = port };
            if (!SetInformationJobObject(guard, 7 /* JobObjectAssociateCompletionPortInformation */, ref watch, Marshal.SizeOf(watch))) return "ERR could not watch the launcher";
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = desktopName;
            PROCESS_INFORMATION pi;
            int flags = 0x400 | 0x4 /* CREATE_SUSPENDED: it is in the job before it runs */;
            if (ConsoleProgram(exe)) flags |= 0x08000000; else si.dwFlags = 0x100;
            IntPtr env = CleanEnvironment();
            bool started = CreateProcess(exe, "\"" + exe + "\"", IntPtr.Zero, IntPtr.Zero, false, flags, env, string.IsNullOrEmpty(workDir) ? null : workDir, ref si, out pi);
            int err = Marshal.GetLastWin32Error();
            Marshal.FreeHGlobal(env);
            if (!started) return "ERR CreateProcess failed (" + err + ")";
            AssignProcessToJobObject(job, pi.hProcess);
            if (!AssignProcessToJobObject(guard, pi.hProcess)) { TerminateProcess(pi.hProcess, 0); CloseHandle(pi.hThread); CloseHandle(pi.hProcess); return "ERR could not watch the launcher"; }
            ResumeThread(pi.hThread);
            CloseHandle(pi.hThread);
            long until = Stopwatch.GetTimestamp() + Stopwatch.Frequency * seconds;
            string ended = "timeout";
            while (Stopwatch.GetTimestamp() < until) {
                uint code; UIntPtr key; IntPtr value;
                if (!GetQueuedCompletionStatus(port, out code, out key, out value, 200)) {
                    // A launcher may leave a server/WSL keeper running. Once the
                    // launcher itself exits, keep those children alive.
                    if (WaitForSingleObject(pi.hProcess, 0) == 0) { ended = "exited"; break; }
                    continue;
                }
                if (code == 4 /* JOB_OBJECT_MSG_ACTIVE_PROCESS_ZERO */) { ended = "exited"; break; }
                if (code != 6 /* JOB_OBJECT_MSG_NEW_PROCESS */ || (int)value == pi.dwProcessId) continue;
                IntPtr child = OpenProcess(0x1001 /* TERMINATE, QUERY_LIMITED_INFORMATION */, false, (int)value);
                if (child == IntPtr.Zero) continue;
                var name = new StringBuilder(1024); int size = name.Capacity;
                bool browser = QueryFullProcessImageName(child, 0, name, ref size)
                    && Array.IndexOf(BrowserPrograms, System.IO.Path.GetFileNameWithoutExtension(name.ToString()).ToLowerInvariant()) >= 0;
                if (browser) TerminateProcess(child, 0);
                CloseHandle(child);
                if (browser) { ended = "browser"; break; }
            }
            // What it started and left running stays (a server it started is the point); a launcher still at it is ended.
            if (ended == "timeout") TerminateJobObject(guard, 0);
            CloseHandle(pi.hProcess);
            return "OK " + ended;
        } finally { CloseHandle(port); CloseHandle(guard); }
    }

    // ---- The command loop. One line in, one line out, for as long as Decurix keeps the pipe open. It runs here, in
    // compiled code, and hands PowerShell only the few commands that need its functions (finding and starting a
    // program): every move of the pointer on a worker's screen is a line, and read and dispatched by PowerShell each
    // cost a couple of milliseconds of interpreting (measured: about a tenth of a core while the pointer moved, on
    // top of looking at the screen). What is typed arrives as UTF-8, which is what Decurix writes.
    public static void Serve(Action<string, string> other) {
        var stdin = new System.IO.StreamReader(Console.OpenStandardInput(), new UTF8Encoding(false), false, 4096);
        string line;
        while ((line = stdin.ReadLine()) != null) {
            int cut = line.IndexOfAny(new[] { ' ', '\t' });
            string verb = cut < 0 ? line : line.Substring(0, cut), rest = cut < 0 ? "" : line.Substring(cut + 1);
            // Stamped before the input is sent, not after: a window it opens must find it already said.
            if (verb == "down" || verb == "up" || verb == "wheel" || verb == "type" || verb == "newline" || verb == "key" || verb == "copy" || verb == "choose" || verb == "press") ourInputAt = Environment.TickCount;
            try {
                if (!Answer(verb, rest)) other(verb, rest);
            } catch (Exception e) {
                // What PowerShell throws around a .NET failure says less than the failure itself.
                while (e.InnerException != null && (e is System.Reflection.TargetInvocationException || e.GetType().Name.EndsWith("MethodInvocationException"))) e = e.InnerException;
                Reply("ERR " + e.Message);
            }
            if (verb == "move") Nudge();
            else if (verb == "down" || verb == "up" || verb == "wheel" || verb == "type" || verb == "newline" || verb == "key" || verb == "copy" || verb == "choose" || verb == "press") { NoteInput(); Kick(); }
            if (verb == "quit") break;
        }
    }

    static int Int(string text) { return int.Parse(text.Trim(), System.Globalization.CultureInfo.InvariantCulture); }

    // "<x> <y> [right] [+<mods>]", mods being any of c (Ctrl), s (Shift), a (Alt).
    static string Pointer(string kind, string rest, bool rightToo) {
        string[] p = rest.Split(' ');
        string mods = ""; bool right = false;
        for (int i = 2; i < p.Length; i++) { if (p[i].StartsWith("+")) mods = p[i].Substring(1); if (p[i] == "right") right = true; }
        return Mouse(kind, Int(p[0]), Int(p[1]), right && rightToo, mods);
    }

    // Answers one command that needs nothing but this class. False for any other.
    static bool Answer(string verb, string rest) {
        string[] p;
        switch (verb) {
            case "move": case "down": case "up": Reply(Pointer(verb, rest, true)); return true;
            case "wheel": p = rest.Split(' '); Reply(Wheel(Int(p[0]), Int(p[1]), Int(p[2]))); return true;
            case "type": Keyboard(() => { foreach (char c in rest) TypeChar(c); return "OK"; }); return true;
            case "newline": Keyboard(() => { TypeNewline(); return "OK"; }); return true;
            case "keyed": KeyedText = rest.Trim() == "1"; Reply("OK"); return true;
            case "key": p = rest.Split(' '); Keyboard(() => Key(Int(p[0]), p.Length > 1 ? p[1] : "")); return true;
            case "copy": Keyboard(() => Copy(Int(rest))); return true;
            case "viewers": SetViewers(rest.Trim().Length == 0 ? 0 : Int(rest)); Reply("OK"); return true;
            case "keyframe": KeyFrame(); Reply("OK"); return true;
            case "thumb": Thumb(Int(rest)); return true;
            case "tree": Reply(Tree()); return true;
            case "setfocus": p = rest.Split(' '); FocusAt(Int(p[0]), Int(p[1])); Reply("OK"); return true;
            case "setvalue": p = rest.Split(new[] { ' ' }, 4); Reply(SetValueAt(Int(p[0]), Int(p[1]), p[2] == "add", p.Length > 3 ? p[3] : "")); return true;
            case "press": p = rest.Split(' '); Reply(Press(Int(p[0]), Int(p[1]))); return true;
            case "choose": p = rest.Split(new[] { ' ' }, 3); Reply(Choose(Int(p[0]), Int(p[1]), p.Length > 2 ? p[2] : "")); return true;
            case "status": Reply("STATUS " + Status()); return true;
            case "quit": return true;
        }
        return false;
    }

    // Starts a web app's own server through its launcher when nothing answers on its port, and gives it a moment.
    //
    // A port that answers is not yet an app that does. Found live with a self-hosted application: its front server listens within
    // seconds and says "502 Bad Gateway" for a minute more, while what is behind it starts - and the worker's browser
    // was opened on that. A gateway's own errors (502, 503, 504) and no answer at all mean "not yet", whatever the app
    // is; any other answer, an error of the app's own included, is the app's to show. So the page is waited for,
    // up to two minutes, and then opened whatever it says.
    public static string Prepare(string desktopName, string launcher, string address) {
        Uri url;
        if (!Uri.TryCreate(address, UriKind.Absolute, out url)) return "OK";
        if (!PortOpen(url.Port)) {
            string started = RunLauncher(desktopName, launcher, System.IO.Path.GetDirectoryName(launcher), 300);
            if (started.StartsWith("ERR ")) return started;
            for (int i = 0; i < 60 && !PortOpen(url.Port); i++) Thread.Sleep(250); // a server just started takes a moment to listen
        }
        long until = Stopwatch.GetTimestamp() + 120 * Stopwatch.Frequency;
        while (Stopwatch.GetTimestamp() < until) {
            int status = Status(url);
            if (status != 0 && status != 502 && status != 503 && status != 504) return "OK";
            Thread.Sleep(1000);
        }
        return "ERR The app's server did not become ready at " + address + ". Check its startup logs and try again.";
    }

    // What a page answers with: its HTTP status, or 0 when nothing answers.
    static int Status(Uri url) {
        try {
            var request = (System.Net.HttpWebRequest)System.Net.WebRequest.Create(url);
            request.Method = "GET"; request.AllowAutoRedirect = false; request.Timeout = 5000; request.ReadWriteTimeout = 5000;
            request.Proxy = null; // this computer's own address
            using (var response = (System.Net.HttpWebResponse)request.GetResponse()) return (int)response.StatusCode;
        } catch (System.Net.WebException e) {
            var response = e.Response as System.Net.HttpWebResponse;
            if (response == null) return 0;
            using (response) return (int)response.StatusCode;
        } catch { return 0; }
    }

    static bool PortOpen(int port) {
        try {
            using (var c = new System.Net.Sockets.TcpClient()) {
                var r = c.BeginConnect("127.0.0.1", port, null, null);
                if (!r.AsyncWaitHandle.WaitOne(300)) return false;
                c.EndConnect(r); return true;
            }
        } catch { return false; }
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
        // The app is the person's own and stays exactly as it is, apart from the one window made for the VM, which is closed.
        if (attached) { if (borrowed) CloseBorrowed(); return; }
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

    // "running", or the code the launched process exited with (0xC0000142 means it could not start on this desktop).
    public static string Status() {
        if (attached) return attachMain != IntPtr.Zero && IsWindow(attachMain) ? "running" : "exited 0x0";
        uint code;
        if (launched == IntPtr.Zero || !GetExitCodeProcess(launched, out code)) return "unknown";
        return code == 259 ? "running" : "exited 0x" + code.ToString("X");
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
        if (attached && !Belongs(h)) return true; // mirror mode: the person's other windows are not the app's
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

    // One picture of everything the app has on screen: every window, painted bottom to top at its own position, over
    // the smallest rectangle that holds them all. A menu or dialog is therefore visible, and clickable, like the rest.
    // Null when the app has no window.
    //
    // It is made with Windows' own drawing, into memory that is kept: the picture is one surface drawn over at every
    // look, each window is printed straight into a surface kept for it, and one is copied onto the other with BitBlt.
    // Through GDI+ bitmaps (as this was first written) every look allocated the whole screen and every window afresh,
    // copied each window twice more on its way in and out of a device context, and painted the wallpaper through the
    // resampler: measured on a 1920x1080 worker, 12-18 ms for the wallpaper alone and ~100 ms a look with Excel
    // maximised - ten pictures a second at best, and 15% of a core to find that nothing had changed. A window that
    // lies wholly behind another is not printed at all. Whoever calls this holds composeLock while it reads the picture.
    [DllImport("gdi32.dll")] static extern IntPtr CreateCompatibleDC(IntPtr dc);
    [DllImport("gdi32.dll")] static extern bool DeleteDC(IntPtr dc);
    [DllImport("gdi32.dll")] static extern IntPtr SelectObject(IntPtr dc, IntPtr obj);
    [DllImport("gdi32.dll")] static extern IntPtr CreateDIBSection(IntPtr dc, ref BITMAPINFOHEADER bi, uint usage, out IntPtr bits, IntPtr section, uint offset);
    [DllImport("gdi32.dll")] static extern bool BitBlt(IntPtr dst, int x, int y, int w, int h, IntPtr src, int sx, int sy, uint rop);
    [DllImport("gdi32.dll")] static extern bool GdiFlush();

    // A picture Windows can draw into: its memory (32 bits a pixel, top row first), a device context on it, and the same
    // memory as a GDI+ bitmap for the few things drawn or saved that way.
    class Surface {
        public IntPtr Dc, Bits; public int W, H; public Bitmap View;
        IntPtr bmp, old;
        public Surface(int w, int h) {
            W = w; H = h;
            var bi = new BITMAPINFOHEADER { biSize = 40, biWidth = w, biHeight = -h, biPlanes = 1, biBitCount = 32 };
            Dc = CreateCompatibleDC(IntPtr.Zero);
            bmp = CreateDIBSection(Dc, ref bi, 0, out Bits, IntPtr.Zero, 0);
            if (bmp == IntPtr.Zero) { DeleteDC(Dc); throw new InvalidOperationException("no memory for a picture of " + w + "x" + h); }
            old = SelectObject(Dc, bmp);
            View = new Bitmap(w, h, w * 4, PixelFormat.Format32bppRgb, Bits);
        }
        public void Free() { View.Dispose(); SelectObject(Dc, old); DeleteObject(bmp); DeleteDC(Dc); }
    }

    static readonly object composeLock = new object();
    static Surface canvas;
    static readonly Dictionary<IntPtr, Surface> printed = new Dictionary<IntPtr, Surface>();

    // A window nothing shows through: big, and neither layered nor click-through (a menu's shadow, a tooltip and an
    // overlay are small or layered, and what is under them is seen).
    static bool Hides(Win w) {
        if (w.V.R - w.V.L < 400 || w.V.B - w.V.T < 300) return false;
        return (GetWindowLong(w.H, -20 /* GWL_EXSTYLE */) & (0x80000 /* WS_EX_LAYERED */ | 0x20 /* WS_EX_TRANSPARENT */)) == 0;
    }

    // Printing a window is what a look costs: Windows takes 16-34 ms to hand over one big window's picture (measured:
    // Edge 1315x1000 in 16-24 ms, PowerPoint 1440x740 in 33 ms), whatever is done with it afterwards. So not every
    // window is printed at every look. The one in front, the one under the pointer, and anything small (a menu, a
    // tooltip, a dialog: cheap, and where things happen) always are; another big window is printed again when a
    // quarter of a second has passed since it last was, and otherwise drawn from the picture kept of it. everything
    // prints them all: a whole picture somebody asked for.
    static readonly Dictionary<IntPtr, long> printedAt = new Dictionary<IntPtr, long>();
    static POINT pointer; static bool pointerKnown;

    static Surface Compose(bool everything) {
        var all = AppWindows();
        // The main window's scale is the picture's, so a DPI-unaware app stays crisp.
        int l, t, pw, ph; double sc; IntPtr main;
        if (!Geometry(all, out l, out t, out pw, out ph, out sc, out main)) return null;

        // Topmost first: what is wholly inside a window that hides what is under it (less its rounded corners) is left out.
        var ws = new List<Win>(); var covers = new List<RECT>(); var solid = new HashSet<IntPtr>();
        foreach (var w in all) {
            bool hidden = false;
            foreach (var c in covers) if (w.V.L >= c.L && w.V.T >= c.T && w.V.R <= c.R && w.V.B <= c.B) { hidden = true; break; }
            if (hidden) continue;
            ws.Add(w);
            if (Hides(w)) { solid.Add(w.H); covers.Add(new RECT { L = w.V.L + 12, T = w.V.T + 12, R = w.V.R - 12, B = w.V.B - 12 }); }
        }

        if (canvas == null || canvas.W != pw || canvas.H != ph) {
            if (canvas != null) canvas.Free();
            canvas = new Surface(pw, ph);
        }
        const uint Copy = 0x00CC0020; // SRCCOPY
        var seen = new HashSet<IntPtr>();
        long look = Stopwatch.GetTimestamp();
        Interlocked.Exchange(ref lookedAt, look);
        IntPtr front = IntPtr.Zero, under = IntPtr.Zero;
        foreach (var w in ws) {
            if (front == IntPtr.Zero && solid.Contains(w.H)) front = w.H;
            if (under == IntPtr.Zero && pointerKnown && pointer.X >= w.V.L && pointer.X < w.V.R && pointer.Y >= w.V.T && pointer.Y < w.V.B) under = w.H;
        }
        using (var g = Graphics.FromHdc(canvas.Dc)) {
            g.InterpolationMode = InterpolationMode.HighQualityBilinear;
            g.Clear(Color.FromArgb(0x1b, 0x1b, 0x1b)); g.Flush(FlushIntention.Sync);
            // A popup's drop shadow and a window's rounded corners capture as black; over what is beneath them they
            // should show it.
            using (var seeThrough = new ImageAttributes()) {
                seeThrough.SetColorKey(Color.FromArgb(0, 0, 0), Color.FromArgb(4, 4, 4));
                int corner = (int)Math.Round(10 * Dpi()) + 2;
                for (int i = ws.Count - 1; i >= 0; i--) {
                    var w = ws[i];
                    int bw = (int)Math.Round((w.R.R - w.R.L) * w.Sc), bh = (int)Math.Round((w.R.B - w.R.T) * w.Sc);
                    if (bw <= 0 || bh <= 0) continue;
                    Surface wb; long printedLast;
                    bool fresh = !printed.TryGetValue(w.H, out wb) || wb.W != bw || wb.H != bh;
                    if (fresh) {
                        if (wb != null) wb.Free();
                        printed.Remove(w.H);
                        wb = new Surface(bw, bh);
                        printed[w.H] = wb;
                    }
                    seen.Add(w.H);
                    bool inUse = i == 0 || w.H == front || (pointerKnown && w.H == under) || (long)bw * bh < 250000;
                    if (fresh || everything || inUse || !printedAt.TryGetValue(w.H, out printedLast) || look - printedLast >= Stopwatch.Frequency / 4) {
                        PrintWindow(w.H, wb.Dc, 2 /* PW_RENDERFULLCONTENT */);
                        GdiFlush();
                        printedAt[w.H] = look;
                    }
                    int sx = Math.Max(0, (int)Math.Round((w.V.L - w.R.L) * w.Sc)), sy = Math.Max(0, (int)Math.Round((w.V.T - w.R.T) * w.Sc));
                    int sw = Math.Min(bw - sx, (int)Math.Round((w.V.R - w.V.L) * w.Sc)), sh = Math.Min(bh - sy, (int)Math.Round((w.V.B - w.V.T) * w.Sc));
                    if (sw <= 0 || sh <= 0) continue;
                    var dest = new Rectangle((int)Math.Round((w.V.L - l) * sc), (int)Math.Round((w.V.T - t) * sc),
                                             (int)Math.Round((w.V.R - w.V.L) * sc), (int)Math.Round((w.V.B - w.V.T) * sc));
                    bool lowest = i == ws.Count - 1; // nothing is under the lowest window of an app's picture
                    bool exact = dest.Width == sw && dest.Height == sh;
                    if (exact && (lowest || (solid.Contains(w.H) && sw > 4 * corner && sh > 4 * corner))) {
                        // A plain copy. Only the four corners can be see-through, so only they are judged pixel by pixel.
                        int c = lowest ? 0 : corner;
                        BitBlt(canvas.Dc, dest.X + c, dest.Y, sw - 2 * c, c, wb.Dc, sx + c, sy, Copy);
                        BitBlt(canvas.Dc, dest.X, dest.Y + c, sw, sh - 2 * c, wb.Dc, sx, sy + c, Copy);
                        BitBlt(canvas.Dc, dest.X + c, dest.Y + sh - c, sw - 2 * c, c, wb.Dc, sx + c, sy + sh - c, Copy);
                        if (c > 0) {
                            GdiFlush();
                            foreach (var at in new[] { new Point(0, 0), new Point(sw - c, 0), new Point(0, sh - c), new Point(sw - c, sh - c) })
                                g.DrawImage(wb.View, new Rectangle(dest.X + at.X, dest.Y + at.Y, c, c), sx + at.X, sy + at.Y, c, c, GraphicsUnit.Pixel, seeThrough);
                            g.Flush(FlushIntention.Sync);
                        }
                    } else {
                        // A menu, a tooltip, a layered window, or one drawn at another scale: through GDI+, as a whole.
                        if (lowest) g.DrawImage(wb.View, dest, sx, sy, sw, sh, GraphicsUnit.Pixel);
                        else g.DrawImage(wb.View, dest, sx, sy, sw, sh, GraphicsUnit.Pixel, seeThrough);
                        g.Flush(FlushIntention.Sync);
                    }
                }
            }
        }
        GdiFlush();
        // A window that has gone, or is not looked at any more, gives its memory back.
        if (printed.Count > seen.Count) {
            var gone = new List<IntPtr>();
            foreach (var h in printed.Keys) if (!seen.Contains(h)) gone.Add(h);
            foreach (var h in gone) { printed[h].Free(); printed.Remove(h); printedAt.Remove(h); }
        }
        lock (geo) { frameL = l; frameT = t; frameScale = sc; haveFrame = true; }
        return canvas;
    }

    // A click needs to know where the picture sat, so an input that arrives before the first capture takes one itself.
    static void EnsureGeometry() {
        bool have; lock (geo) have = haveFrame;
        if (have) return;
        lock (composeLock) Compose(false);
    }

    // ---- Capturing. A thread of its own looks at the app and says what changed, so nothing waits on it and a picture is
    // never asked for: it is sent when, and only when, it differs from the last one sent - and then only the part that differs.

    static volatile int viewers;
    static volatile bool stopCapture;
    static long requestedKeyframe, capturedKeyframe;
    static long boostUntil, lastChange; // Stopwatch ticks
    static readonly AutoResetEvent wake = new AutoResetEvent(false);
    static Thread capThread;
    static IntPtr prev; static int prevBytes, prevW, prevH;
    static bool everHadWindow, closedSent; static long noWindowSince;

    public static void SetViewers(int n) {
        int old = viewers; viewers = n;
        if (n > 0 && old == 0) Interlocked.Increment(ref requestedKeyframe);
        wake.Set();
    }

    public static void KeyFrame() { Interlocked.Increment(ref requestedKeyframe); wake.Set(); }

    // Something was just sent to the app: look at the screen again at once, and often for a moment, so its reaction shows quickly.
    public static void Kick() {
        Interlocked.Exchange(ref boostUntil, Stopwatch.GetTimestamp() + Stopwatch.Frequency * 7 / 10);
        Interlocked.Exchange(ref stillMoves, 0);
        wake.Set();
    }

    // The pointer moved. Most of a screen does not answer a pointer passing over it, and every move used to be looked
    // at like a click: sixty looks a second for as long as the pointer was moving (measured: 43% of a core, with not
    // one picture to send). So a move is looked at soon after, and while moves go on changing nothing, less and less
    // often; the first one that does change something (a button lighting up, a menu following the pointer) is looked
    // at like any other input again.
    static long stillMoves, moveAt;

    public static void Nudge() {
        Interlocked.Exchange(ref moveAt, Stopwatch.GetTimestamp());
        if (Interlocked.Read(ref stillMoves) < 3) wake.Set();
    }

    // Nobody has looked at this screen for a while - no viewer, no card on a dashboard: the pictures kept of it (the
    // screen, the last one sent, every window: about 35 MB for a 1920x1080 screen with two apps) are
    // given back, and so is the memory this process is not using. The next look makes them again.
    [DllImport("kernel32.dll")] static extern IntPtr GetCurrentProcess();
    [DllImport("kernel32.dll")] static extern bool SetProcessWorkingSetSize(IntPtr process, IntPtr min, IntPtr max);
    static long lookedAt;

    static void Rest() {
        lock (composeLock) {
            if (canvas != null) { canvas.Free(); canvas = null; }
            if (small != null) { small.Free(); small = null; }
            foreach (var kept in printed.Values) kept.Free();
            printed.Clear(); printedAt.Clear();
            if (prev != IntPtr.Zero) { Marshal.FreeHGlobal(prev); prev = IntPtr.Zero; prevBytes = 0; }
            if (smallPrev != IntPtr.Zero) { Marshal.FreeHGlobal(smallPrev); smallPrev = IntPtr.Zero; }
        }
        GC.Collect();
        SetProcessWorkingSetSize(GetCurrentProcess(), (IntPtr)(-1), (IntPtr)(-1));
    }

    static void CaptureLoop() {
        int delay = 50;
        long seenMove = 0;
        while (!stopCapture) {
            bool woken = wake.WaitOne(delay);
            if (stopCapture) break;
            if (desktop == IntPtr.Zero || viewers <= 0) {
                delay = 200;
                if (canvas != null && Stopwatch.GetTimestamp() - Interlocked.Read(ref lookedAt) > 20 * Stopwatch.Frequency) Rest();
                continue;
            }
            if (woken) Thread.Sleep(12); // whatever woke us was only just posted to the app; let it react before looking
            long move = Interlocked.Read(ref moveAt);
            bool afterMove = move != seenMove; seenMove = move;
            bool sent;
            try { sent = CaptureOnce(); } catch (Exception e) {
                if (Environment.GetEnvironmentVariable("DECURIX_SCREEN_TRACE") == "1") Reply("EVENT capture-error " + e.ToString().Replace('\r', ' ').Replace('\n', ' '));
                delay = 250; continue;
            }
            long now = Stopwatch.GetTimestamp();
            if (sent) { Interlocked.Exchange(ref lastChange, now); Interlocked.Exchange(ref stillMoves, 0); delay = 5; continue; }
            if (afterMove) Interlocked.Increment(ref stillMoves);
            if (now < Interlocked.Read(ref boostUntil)) { delay = 15; continue; }
            // Nothing is being done to the screen and nothing on it changed: it is looked at less and less often, down
            // to once a second. Any input, and any change found, brings it straight back.
            double idle = (now - Interlocked.Read(ref lastChange)) / (double)Stopwatch.Frequency;
            delay = idle < 0.6 ? 40 : idle < 4 ? 120 : idle < 20 ? 300 : idle < 60 ? 600 : 1000;
            // A pointer still moving over something that does not answer it is looked at a few times a second.
            if ((now - move) < Stopwatch.Frequency / 2) delay = Math.Min(delay, 150);
        }
    }

    // Looks at the app once and sends what changed. False when nothing did.
    static bool CaptureOnce() {
        lock (composeLock) return CaptureLocked();
    }

    static bool CaptureLocked() {
        // A boolean reset after drawing can erase a request that arrived while
        // drawing. Keep the generation actually captured, so reconnecting viewers
        // never wait indefinitely for an unchanged screen's whole frame.
        long generation = Interlocked.Read(ref requestedKeyframe);
        bool needKey = generation != capturedKeyframe;
        Surface pic = Compose(needKey);
        if (pic == null) {
            long now = Stopwatch.GetTimestamp();
            if (everHadWindow && !closedSent) {
                if (noWindowSince == 0) noWindowSince = now;
                else if (now - noWindowSince > 2 * Stopwatch.Frequency) { closedSent = true; Reply("EVENT closed"); }
            }
            return false;
        }
        noWindowSince = 0; everHadWindow = true; closedSent = false;
        int w = pic.W, h = pic.H, need = w * 4 * h;
        bool full = needKey || prev == IntPtr.Zero || prevW != w || prevH != h;
        Rectangle r = new Rectangle(0, 0, w, h);
        if (!full) {
            if (!Diff(pic.Bits, w, h, out r)) return false;
            // A change over most of the picture is cheaper and cleaner to send whole than as one big patch.
            if ((long)r.Width * r.Height * 100 > (long)w * h * 55) { full = true; r = new Rectangle(0, 0, w, h); }
        }
        if (prev == IntPtr.Zero || prevBytes != need) {
            if (prev != IntPtr.Zero) Marshal.FreeHGlobal(prev);
            prev = Marshal.AllocHGlobal(need); prevBytes = need;
        }
        CopyMemory(prev, pic.Bits, (UIntPtr)need);
        prevW = w; prevH = h;

        byte[] data;
        using (var ms = new System.IO.MemoryStream()) {
            if (full) pic.View.Save(ms, jpeg, jpegQuality);
            else using (var part = pic.View.Clone(r, PixelFormat.Format24bppRgb)) part.Save(ms, jpeg, jpegQuality);
            data = ms.ToArray();
        }
        byte[] head = Encoding.ASCII.GetBytes("FRAME " + w + " " + h + " " + r.X + " " + r.Y + " " + r.Width + " " + r.Height + " " + (full ? 1 : 0) + " " + data.Length + "\n");
        lock (outLock) { stdout.Write(head, 0, head.Length); stdout.Write(data, 0, data.Length); stdout.Flush(); }
        capturedKeyframe = generation;
        return true;
    }

    // The screen as a small picture, for a dashboard card: "THUMB <w> <h> <n>" and the JPEG, or "THUMB same" when it
    // shows what the last one of that size did. A card used to be given the whole screen at full size, which the
    // control plane then decoded, shrank and encoded again - every five seconds for every worker on the dashboard.
    [DllImport("gdi32.dll")] static extern bool StretchBlt(IntPtr dst, int x, int y, int w, int h, IntPtr src, int sx, int sy, int sw, int sh, uint rop);
    [DllImport("gdi32.dll")] static extern int SetStretchBltMode(IntPtr dc, int mode);
    [DllImport("gdi32.dll")] static extern bool SetBrushOrgEx(IntPtr dc, int x, int y, IntPtr old);
    static Surface small; static IntPtr smallPrev;

    // ---- What the app's windows hold, read through UI Automation ----
    // The picture alone is all a VM's agent would otherwise get: no control it can name, no field it can type into by
    // name, no password field it can tell from any other. Windows' own accessibility tree has them - the same one a
    // screen reader reads - and it reads on a private desktop as on the visible one (measured: Character Map, 341
    // elements with their labels), from a thread attached to that desktop. The viewer lays what it finds over the
    // picture as real elements, the way the phone viewer does with Android's tree.
    //
    // A read takes a few hundred milliseconds to seconds (every property is a call into the app), so it never holds up a
    // command: "tree" answers at once with the last read, and starts the next one on a thread of its own. Whether that
    // read started after the latest input is said with it, so the viewer can say "not yet" until the controls have
    // caught up with a click.

    // Where the last picture was cut from the screen, so a control's screen rectangle can be put on it.
    // The picture the app's windows make now, by the rule Compose cuts it by: its corner on the screen, its size and its
    // scale. Worked out afresh, not taken from the last picture: with nobody watching nothing is composed, and a window
    // that has grown since would put every control below its old edge outside the picture (found: Character Map's
    // advanced view). False when the app shows nothing.
    static bool Geometry(List<Win> all, out int l, out int t, out int pw, out int ph, out double sc, out IntPtr main) {
        l = int.MaxValue; t = int.MaxValue; pw = 0; ph = 0; sc = 1.0; main = IntPtr.Zero;
        int r = int.MinValue, b = int.MinValue;
        if (all.Count == 0) return false;
        long best = 0;
        foreach (var w in all) {
            l = Math.Min(l, w.V.L); t = Math.Min(t, w.V.T); r = Math.Max(r, w.V.R); b = Math.Max(b, w.V.B);
            long a = (long)(w.V.R - w.V.L) * (w.V.B - w.V.T);
            if (a > best) { best = a; main = w.H; }
        }
        sc = Scale(main);
        pw = (int)Math.Round((r - l) * sc); ph = (int)Math.Round((b - t) * sc);
        return pw > 0 && ph > 0 && pw <= 8192 && ph <= 8192;
    }
    static long inputSeq;
    static readonly object treeLock = new object();
    static string treeJson; static long treeSeq = -1; static bool treeWalking;
    static Thread uiaThread;
    static readonly System.Collections.Concurrent.BlockingCollection<Action> uiaWork = new System.Collections.Concurrent.BlockingCollection<Action>();
    const int MaxNodes = 900;

    public static void NoteInput() { Interlocked.Increment(ref inputSeq); ourInputAt = Environment.TickCount; }

    // "TREE <fresh 0|1> <base64 of the JSON>".
    public static string Tree() {
        lock (treeLock) {
            long now = Interlocked.Read(ref inputSeq);
            if (!treeWalking && desktop != IntPtr.Zero) {
                treeWalking = true;
                StartUia();
                uiaWork.Add(() => Walk(now));
            }
            string body = treeJson ?? "{\"w\":0,\"h\":0,\"nodes\":[],\"pending\":true}";
            bool fresh = treeJson != null && treeSeq == now;
            return "TREE " + (fresh ? "1" : "0") + " " + Convert.ToBase64String(Encoding.UTF8.GetBytes(body));
        }
    }

    static void StartUia() {
        if (uiaThread != null) return;
        uiaThread = new Thread(() => {
            // UI Automation reaches only the windows of the desktop its thread is on; a new thread has no windows, so it
            // may move.
            bool attached = SetThreadDesktop(desktop);
            foreach (var work in uiaWork.GetConsumingEnumerable()) {
                if (!attached) { attached = SetThreadDesktop(desktop); }
                work();
            }
        }) { IsBackground = true, Name = "uia" };
        uiaThread.SetApartmentState(ApartmentState.MTA);
        uiaThread.Start();
    }

    static void Walk(long seq) {
        string json;
        try { json = ReadControls(); }
        catch (Exception e) { json = "{\"w\":0,\"h\":0,\"nodes\":[],\"error\":" + Js(e.Message) + "}"; }
        lock (treeLock) { treeJson = json; treeSeq = seq; treeWalking = false; }
    }

    static System.Windows.Automation.CacheRequest treeCache;
    static System.Windows.Automation.CacheRequest TreeCache() {
        if (treeCache != null) return treeCache;
        var cr = new System.Windows.Automation.CacheRequest();
        cr.TreeScope = System.Windows.Automation.TreeScope.Element;
        cr.AutomationElementMode = System.Windows.Automation.AutomationElementMode.None;
        foreach (var p in new System.Windows.Automation.AutomationProperty[] {
            System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.AutomationElement.NameProperty,
            System.Windows.Automation.AutomationElement.BoundingRectangleProperty, System.Windows.Automation.AutomationElement.IsEnabledProperty,
            System.Windows.Automation.AutomationElement.IsPasswordProperty, System.Windows.Automation.AutomationElement.HasKeyboardFocusProperty,
            System.Windows.Automation.AutomationElement.AutomationIdProperty, System.Windows.Automation.AutomationElement.IsKeyboardFocusableProperty,
            System.Windows.Automation.AutomationElement.IsInvokePatternAvailableProperty,
            System.Windows.Automation.ValuePattern.ValueProperty, System.Windows.Automation.ValuePattern.IsReadOnlyProperty,
            System.Windows.Automation.TogglePattern.ToggleStateProperty,
            System.Windows.Automation.ExpandCollapsePattern.ExpandCollapseStateProperty,
            System.Windows.Automation.SelectionItemPattern.IsSelectedProperty }) cr.Add(p);
        return treeCache = cr;
    }

    static object Cached(System.Windows.Automation.AutomationElement e, System.Windows.Automation.AutomationProperty p) {
        try {
            object v = e.GetCachedPropertyValue(p, true);
            return v == System.Windows.Automation.AutomationElement.NotSupported ? null : v;
        } catch { return null; }
    }

    // The role a control is given in the viewer, by what UI Automation says it is; "" for what is only a container.
    static string RoleOf(System.Windows.Automation.ControlType ct, bool invokable, bool focusable, string name) {
        if (ct == System.Windows.Automation.ControlType.Button || ct == System.Windows.Automation.ControlType.SplitButton) return "button";
        if (ct == System.Windows.Automation.ControlType.Hyperlink) return "link";
        if (ct == System.Windows.Automation.ControlType.MenuItem) return "menuitem";
        if (ct == System.Windows.Automation.ControlType.TabItem) return "tab";
        if (ct == System.Windows.Automation.ControlType.ListItem) return "option";
        if (ct == System.Windows.Automation.ControlType.TreeItem) return "treeitem";
        if (ct == System.Windows.Automation.ControlType.DataItem) return "cell";
        if (ct == System.Windows.Automation.ControlType.Edit) return "textbox";
        if (ct == System.Windows.Automation.ControlType.Document) return focusable ? "textbox" : "text";
        if (ct == System.Windows.Automation.ControlType.CheckBox) return "checkbox";
        if (ct == System.Windows.Automation.ControlType.RadioButton) return "radio";
        if (ct == System.Windows.Automation.ControlType.ComboBox) return "combobox";
        if (ct == System.Windows.Automation.ControlType.Slider) return "slider";
        if (ct == System.Windows.Automation.ControlType.Spinner) return "spinbutton";
        if (ct == System.Windows.Automation.ControlType.Text) return "text";
        if (ct == System.Windows.Automation.ControlType.HeaderItem) return "columnheader";
        if (ct == System.Windows.Automation.ControlType.Image) return "image";
        // A control drawn by the app itself (Custom, a Pane) that can be pressed and says what it is.
        if ((invokable || focusable) && name.Length > 0 && (ct == System.Windows.Automation.ControlType.Custom || ct == System.Windows.Automation.ControlType.Pane || ct == System.Windows.Automation.ControlType.Group)) return "button";
        return "";
    }

    // Presses the named control at a point of the picture through UI Automation - Invoke, or Select for a tab, Toggle for
    // a check box, Expand for a menu - the way a screen reader presses it. A click posted as mouse messages does not reach
    // every control: Office's ribbon and its File page let it go by (found live, 2026-10-15: the File tab "clicked" and
    // nothing opened). Run on a thread of its own, bounded: a press that opens a modal dialog can hold Invoke until the
    // dialog closes, and that must hold neither this helper nor its reads of the app. "PRESSED <name>", "PENDING <name>"
    // (it is still being handled), or "ERR <why>" - then the caller clicks as before.
    public static string Press(int x, int y) {
        EnsureGeometry();
        POINT sp = ScreenPoint(x, y);
        string result = "ERR the app did not answer";
        string name = "";
        var started = new ManualResetEventSlim(false);
        var done = new ManualResetEventSlim(false);
        var th = new Thread(() => {
            try {
                if (!SetThreadDesktop(desktop)) { result = "ERR no desktop"; return; }
                // A window a dialog has disabled takes no press - a real click there does nothing. UI Automation's
                // Invoke would press it anyway (found live: the toolbar's Save behind an open Save As saved the
                // workbook as CSV), so it is refused, saying why.
                IntPtr top = WindowAt(sp);
                if (top != IntPtr.Zero && !IsWindowEnabled(top)) { result = "ERR BLOCKED behind an open dialog"; return; }
                var el = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(sp.X, sp.Y));
                var walker = System.Windows.Automation.TreeWalker.ControlViewWalker;
                for (int up = 0; el != null && up < 3; up++) {
                    object pat;
                    name = el.Current.Name ?? "";
                    var ct = el.Current.ControlType;
                    if (ct == System.Windows.Automation.ControlType.Edit || ct == System.Windows.Automation.ControlType.Document) { result = "ERR a field"; return; }
                    if (ct == System.Windows.Automation.ControlType.TabItem && el.TryGetCurrentPattern(System.Windows.Automation.SelectionItemPattern.Pattern, out pat)) {
                        started.Set(); ((System.Windows.Automation.SelectionItemPattern)pat).Select(); result = "PRESSED " + name; return;
                    }
                    if (el.TryGetCurrentPattern(System.Windows.Automation.InvokePattern.Pattern, out pat)) {
                        started.Set(); ((System.Windows.Automation.InvokePattern)pat).Invoke(); result = "PRESSED " + name; return;
                    }
                    if (el.TryGetCurrentPattern(System.Windows.Automation.TogglePattern.Pattern, out pat)) {
                        started.Set(); ((System.Windows.Automation.TogglePattern)pat).Toggle(); result = "PRESSED " + name; return;
                    }
                    if (el.TryGetCurrentPattern(System.Windows.Automation.ExpandCollapsePattern.Pattern, out pat)) {
                        started.Set(); ((System.Windows.Automation.ExpandCollapsePattern)pat).Expand(); result = "PRESSED " + name; return;
                    }
                    el = walker.GetParent(el);
                }
                result = "ERR nothing there can be pressed";
            } catch (Exception e) { result = "ERR " + e.Message; }
            finally { done.Set(); }
        }) { IsBackground = true };
        th.SetApartmentState(ApartmentState.MTA);
        th.Start();
        if (done.Wait(4000)) return result;
        return started.IsSet ? "PENDING " + name : "ERR the app did not answer";
    }

    // Gives the keyboard to the control at a point of the picture, through UI Automation on the thread attached to the
    // private desktop: a posted click does not move a dialog's focus. Waits for it (typing follows at once), at most 3 s.
    public static void FocusAt(int x, int y) {
        EnsureGeometry();
        POINT sp = ScreenPoint(x, y);
        // On a thread of its own, not the UI Automation queue: that queue also reads the app's whole tree, which for a
        // spreadsheet takes seconds, and the text typed next must not go out before the keyboard is in the field. Found
        // live (2026-10-16): an address typed into Excel's Name Box went to the sheet, the wait having given up.
        RunOnDesktop(() => {
            var el = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(sp.X, sp.Y));
            if (el != null && el.Current.IsKeyboardFocusable) el.SetFocus();
        }, 6000);
    }

    // Puts text into the single-line field at a point of the picture through its ValuePattern, gives it the keyboard, and
    // answers "SET <what it holds now>" or "ERR <why>" (the caller then types). Only an Edit or an editable ComboBox of
    // one line: a document or a tall field would be replaced whole. Typed key by key, text raced the app (a key overtook
    // the last characters - "D20" into Excel's Name Box went to D2 - and spaces were dropped or doubled).
    public static string SetValueAt(int x, int y, bool add, string text) {
        EnsureGeometry();
        POINT sp = ScreenPoint(x, y);
        string result = "ERR HELD? the app did not answer in time";
        RunOnDesktop(() => {
            try {
                IntPtr top = WindowAt(sp);
                if (top != IntPtr.Zero && !IsWindowEnabled(top)) { result = "ERR NOSET BLOCKED behind an open dialog"; return; }
                var el = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(sp.X, sp.Y));
                var walker = System.Windows.Automation.TreeWalker.ControlViewWalker;
                System.Windows.Automation.ValuePattern value = null;
                System.Windows.Automation.AutomationElement field = null;
                for (int up = 0; el != null && up < 3 && value == null; up++, el = walker.GetParent(el)) {
                    var ct = el.Current.ControlType;
                    if (ct == System.Windows.Automation.ControlType.Document) { result = "ERR NOSET a document"; return; }
                    if (ct != System.Windows.Automation.ControlType.Edit && ct != System.Windows.Automation.ControlType.ComboBox) continue;
                    object vp;
                    if (!el.TryGetCurrentPattern(System.Windows.Automation.ValuePattern.Pattern, out vp)) continue;
                    var v = (System.Windows.Automation.ValuePattern)vp;
                    if (v.Current.IsReadOnly) { result = "ERR NOSET read-only"; return; }
                    if (el.Current.IsPassword) { result = "ERR NOSET a password field"; return; }
                    var r = el.Current.BoundingRectangle;
                    if (!r.IsEmpty && r.Height > 60) { result = "ERR NOSET a field of several lines"; return; }
                    value = v; field = el;
                    // A dialog's combo box (Save As's "File name:" is one, holding an Edit of the same name) shows text set
                    // through UI Automation and reads it back, but the dialog does not use it: pressing Save in Word saved
                    // "Doc1" in Documents and ignored the path in the box, set on the Edit or on the combo (found live,
                    // 2026-10-17; Excel's dialog happened to take it). Typed keys are taken, so the caller types.
                    if (top != IntPtr.Zero) {
                        var cls = new StringBuilder(32); GetClassName(top, cls, 32);
                        bool inDialog = cls.ToString() == "#32770" || GetWindow(top, 4 /* GW_OWNER */) != IntPtr.Zero;
                        var parent = ct == System.Windows.Automation.ControlType.Edit ? walker.GetParent(el) : null;
                        bool inCombo = ct == System.Windows.Automation.ControlType.ComboBox
                            || (parent != null && parent.Current.ControlType == System.Windows.Automation.ControlType.ComboBox);
                        if (inDialog && inCombo) { result = "ERR NOSET a dialog's combo box takes typed keys, not a set value"; return; }
                    }
                }
                if (value == null) { result = "ERR NOSET no single-line field there"; return; }
                try { field.SetFocus(); } catch { }
                string now = add ? (value.Current.Value ?? "") + text : text;
                try { value.SetValue(now); }
                catch (Exception e) { result = "ERR NOSET " + e.Message; return; }
                // From here the field was set: an error now must not lead to typing the text again on top of it.
                string held = "";
                for (int tries = 0; tries < 8; tries++) {
                    Thread.Sleep(120);
                    held = value.Current.Value ?? "";
                    if (held == now) break;
                }
                result = held == now ? "SET " + held : "ERR HELD " + Cut(held, 120);
            } catch (Exception e) { result = "ERR HELD? " + e.Message; }
        }, 6000);
        return result;
    }

    // Runs a piece of UI Automation work on a new thread attached to the app's desktop, waiting up to timeout ms for it.
    static void RunOnDesktop(Action work, int timeout) {
        var done = new ManualResetEventSlim(false);
        var th = new Thread(() => {
            try { if (SetThreadDesktop(desktop)) work(); } catch { }
            finally { done.Set(); }
        }) { IsBackground = true, Name = "uia-now" };
        th.SetApartmentState(ApartmentState.MTA);
        th.Start();
        done.Wait(timeout);
    }

    // Picks, in the list control at a point of the picture, the one entry named by text, through UI Automation: keys and
    // clicks posted to a dialog do not move its keyboard focus, so a drop-down (Save As's "Save as type") cannot be chosen
    // from with them. The entry is the one named exactly so, else the only one starting with the text, else the only one
    // containing it; two that fit equally are not guessed between. Answers "CHOSE <name>" or "ERR <why>".
    public static string Choose(int x, int y, string text) {
        EnsureGeometry();
        POINT sp = ScreenPoint(x, y);
        string result = "ERR the app did not answer";
        // On a thread of its own, as FocusAt: not behind a tree read of the whole app.
        RunOnDesktop(() => {
            try { result = ChooseAt(sp, text.Trim()); }
            catch (Exception e) { result = "ERR " + e.Message; }
        }, 15000);
        return result;
    }

    static string ChooseAt(POINT sp, string text) {
        var el = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(sp.X, sp.Y));
        var walker = System.Windows.Automation.TreeWalker.ControlViewWalker;
        for (int up = 0; el != null && up < 4 && el.Current.ControlType != System.Windows.Automation.ControlType.ComboBox
                && el.Current.ControlType != System.Windows.Automation.ControlType.List; up++) el = walker.GetParent(el);
        if (el == null) return "ERR there is no list there";
        object ec;
        bool expanded = false;
        var before = new HashSet<IntPtr>();
        foreach (var w in AppWindows()) before.Add(w.H);
        IntPtr ownerTop = WindowAt(sp); // the window holding the drop-down itself
        if (el.TryGetCurrentPattern(System.Windows.Automation.ExpandCollapsePattern.Pattern, out ec)) {
            try { ((System.Windows.Automation.ExpandCollapsePattern)ec).Expand(); expanded = true; Thread.Sleep(300); } catch { }
        }
        try {
            var isItem = new System.Windows.Automation.OrCondition(
                new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.ControlType.ListItem),
                new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.ControlType.MenuItem),
                new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.ControlType.DataItem));
            var all = new List<System.Windows.Automation.AutomationElement>();
            foreach (System.Windows.Automation.AutomationElement it in el.FindAll(System.Windows.Automation.TreeScope.Descendants, isItem)) all.Add(it);
            if (all.Count == 0 && expanded) {
                // Office draws a drop-down's open list as a window of its own (Excel's Sort dialog, "Order"): its entries
                // are there, not under the drop-down - in a window that appeared when it opened. Only such windows are
                // read: the dialog's other lists ("Sort Field") are not this drop-down's entries.
                Thread.Sleep(300);
                OpenListEntries(isItem, hw => before.Contains(hw), all);
                // A list opened before this call (the worker clicked the drop-down first) is not a new window: then
                // any window of the app but the one holding the drop-down.
                if (all.Count == 0) OpenListEntries(isItem, hw => hw == ownerTop, all);
            }
            var items = all;
            var names = new List<string>();
            System.Windows.Automation.AutomationElement exact = null;
            var starts = new List<System.Windows.Automation.AutomationElement>();
            var holds = new List<System.Windows.Automation.AutomationElement>();
            foreach (System.Windows.Automation.AutomationElement it in items) {
                string n = (it.Current.Name ?? "").Trim();
                if (n.Length == 0) continue;
                names.Add(n);
                if (string.Equals(n, text, StringComparison.OrdinalIgnoreCase)) exact = it;
                else if (n.StartsWith(text, StringComparison.OrdinalIgnoreCase)) starts.Add(it);
                else if (n.IndexOf(text, StringComparison.OrdinalIgnoreCase) >= 0) holds.Add(it);
            }
            var pick = exact ?? (starts.Count == 1 ? starts[0] : starts.Count == 0 && holds.Count == 1 ? holds[0] : null);
            if (pick == null) {
                string offered = string.Join("; ", names.GetRange(0, Math.Min(30, names.Count)));
                return names.Count == 0 ? "ERR the list there has no entries"
                    : (starts.Count + holds.Count > 1 ? "ERR more than one entry fits \"" + text + "\": " : "ERR no entry is \"" + text + "\"; it offers: ") + offered;
            }
            string chosen = pick.Current.Name;
            // A real Windows combo box is told in its own messages: UI Automation's Select on its entries picked the
            // wrong one in Excel's Save As (asked for "Excel Workbook", it showed "Excel Add-in").
            IntPtr h = (IntPtr)el.Current.NativeWindowHandle;
            if (h != IntPtr.Zero && el.Current.ControlType == System.Windows.Automation.ControlType.ComboBox) {
                if (expanded) { try { ((System.Windows.Automation.ExpandCollapsePattern)ec).Collapse(); } catch { } expanded = false; }
                string why = SelectInCombo(h, chosen);
                if (why != null) return "ERR " + why;
                // What the drop-down shows now, as its owner draws it: a native list that took the choice is not proof
                // that the dialog did (found live: it showed one type and saved another).
                string shows = null;
                for (int tries = 0; tries < 8; tries++) {
                    shows = SelectedEntry(el);
                    if (shows == null || string.Equals(shows.Trim(), chosen.Trim(), StringComparison.OrdinalIgnoreCase)) break;
                    Thread.Sleep(200);
                }
                if (shows != null && !string.Equals(shows.Trim(), chosen.Trim(), StringComparison.OrdinalIgnoreCase))
                    return "ERR the drop-down still shows \"" + shows.Trim() + "\" - the choice did not take";
                return "CHOSE " + chosen;
            }
            object sel, inv;
            if (pick.TryGetCurrentPattern(System.Windows.Automation.SelectionItemPattern.Pattern, out sel))
                ((System.Windows.Automation.SelectionItemPattern)sel).Select();
            else if (pick.TryGetCurrentPattern(System.Windows.Automation.InvokePattern.Pattern, out inv))
                ((System.Windows.Automation.InvokePattern)inv).Invoke();
            else return "ERR its entries cannot be selected";
            return "CHOSE " + chosen;
        } finally {
            if (expanded) { try { ((System.Windows.Automation.ExpandCollapsePattern)ec).Collapse(); } catch { } }
        }
    }

    // Adds to all the shown entries of the topmost of the app's windows that has any, passing over those skip names:
    // the topmost window with entries is the open list.
    static void OpenListEntries(System.Windows.Automation.Condition isItem, Func<IntPtr, bool> skip, List<System.Windows.Automation.AutomationElement> all) {
        foreach (var w in AppWindows()) {
            if (skip(w.H)) continue;
            try {
                var root = System.Windows.Automation.AutomationElement.FromHandle(w.H);
                foreach (System.Windows.Automation.AutomationElement it in root.FindAll(System.Windows.Automation.TreeScope.Descendants, isItem)) {
                    var box = it.Current.BoundingRectangle;
                    if (!it.Current.IsOffscreen && !box.IsEmpty) all.Add(it);
                }
            } catch { }
            if (all.Count > 0) break;
        }
    }

    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, string lp, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll")] static extern IntPtr GetParent(IntPtr h);
    [DllImport("user32.dll")] static extern bool IsWindowEnabled(IntPtr h);
    [DllImport("user32.dll")] static extern int GetDlgCtrlID(IntPtr h);
    [StructLayout(LayoutKind.Sequential)] struct COMBOBOXINFO { public int cbSize; public RECT rcItem, rcButton; public int stateButton; public IntPtr hwndCombo, hwndItem, hwndList; }
    [DllImport("user32.dll")] static extern bool GetComboBoxInfo(IntPtr h, ref COMBOBOXINFO info);
    [DllImport("user32.dll", EntryPoint = "SendMessage")] static extern IntPtr SendMessageRect(IntPtr h, uint msg, IntPtr wp, out RECT lp);

    // Selects the entry named exactly so in a combo box, as its owner hears of a choice the person made: CB_SETCURSEL,
    // then CBN_SELCHANGE to the parent (CB_SETCURSEL alone tells nobody, and a Save As dialog would keep its old type).
    // Null when done, else why not.
    static string SelectInCombo(IntPtr h, string name) {
        const uint CB_FINDSTRINGEXACT = 0x0158, CB_SETCURSEL = 0x014E, CB_GETCURSEL = 0x0147, WM_COMMAND = 0x0111;
        IntPtr index, ignored;
        if (SendMessageTimeout(h, CB_FINDSTRINGEXACT, (IntPtr)(-1), name, 0x2, 1000, out index) == IntPtr.Zero) return "the list did not answer";
        if ((long)index < 0) return "the list has no entry \"" + name + "\"";
        // As a person does it: open the drop-down and click the entry in its list, so the combo box selects it, closes and
        // tells its owner in every way it listens for. Found live (2026-10-16): with CB_SETCURSEL and a CBN_SELCHANGE of our
        // own - and with arrow keys sent to it - Excel's Save As kept saving as CSV ("report.xlsx.csv"), the native list
        // showing "Excel Workbook" all along. A click on the entry in the open list is what its dialog takes in (checked:
        // the same dialog then saved a real workbook).
        var info = new COMBOBOXINFO(); info.cbSize = Marshal.SizeOf(typeof(COMBOBOXINFO));
        if (GetComboBoxInfo(h, ref info) && info.hwndList != IntPtr.Zero) {
            const uint CB_SHOWDROPDOWN = 0x014F, CB_GETDROPPEDSTATE = 0x0157, LB_SETTOPINDEX = 0x0197, LB_GETITEMRECT = 0x0198;
            SendMessageTimeout(h, CB_SHOWDROPDOWN, (IntPtr)1, IntPtr.Zero, 0x2, 1000, out ignored);
            Thread.Sleep(300);
            SendMessageTimeout(info.hwndList, LB_SETTOPINDEX, index, IntPtr.Zero, 0x2, 1000, out ignored);
            RECT item;
            if (SendMessageRect(info.hwndList, LB_GETITEMRECT, index, out item) != (IntPtr)(-1) && item.R > item.L) {
                int ix = (item.L + item.R) / 2, iy = (item.T + item.B) / 2;
                IntPtr at = (IntPtr)((iy << 16) | (ix & 0xFFFF));
                PostMessage(info.hwndList, 0x0200 /* WM_MOUSEMOVE */, IntPtr.Zero, at); Thread.Sleep(60);
                PostMessage(info.hwndList, 0x0201 /* WM_LBUTTONDOWN */, (IntPtr)1, at); Thread.Sleep(60);
                PostMessage(info.hwndList, 0x0202 /* WM_LBUTTONUP */, IntPtr.Zero, at); Thread.Sleep(400);
            }
            IntPtr open, picked;
            SendMessageTimeout(h, CB_GETDROPPEDSTATE, IntPtr.Zero, IntPtr.Zero, 0x2, 1000, out open);
            if (open != IntPtr.Zero) SendMessageTimeout(h, CB_SHOWDROPDOWN, IntPtr.Zero, IntPtr.Zero, 0x2, 1000, out ignored);
            SendMessageTimeout(h, CB_GETCURSEL, IntPtr.Zero, IntPtr.Zero, 0x2, 1000, out picked);
            if (picked == index && open == IntPtr.Zero) return null;
        }
        SendMessageTimeout(h, CB_SETCURSEL, index, IntPtr.Zero, 0x2, 1000, out ignored);
        IntPtr parent = GetParent(h);
        int id = GetDlgCtrlID(h);
        if (parent != IntPtr.Zero)
            SendMessageTimeout(parent, WM_COMMAND, (IntPtr)((1 /* CBN_SELCHANGE */ << 16) | (id & 0xFFFF)), h, 0x2, 2000, out ignored);
        IntPtr now;
        SendMessageTimeout(h, CB_GETCURSEL, IntPtr.Zero, IntPtr.Zero, 0x2, 1000, out now);
        return now == index ? null : "the list did not keep the choice";
    }

    // The entry selected in the drop-down whose box this is. What the tree walk returns is cached only (no live
    // element to ask), so the drop-down is found again at its middle.
    static string SelectedEntryAt(System.Windows.Rect r) {
        try {
            var el = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(r.X + r.Width / 2, r.Y + r.Height / 2));
            var walker = System.Windows.Automation.TreeWalker.ControlViewWalker;
            for (int up = 0; el != null && up < 3 && el.Current.ControlType != System.Windows.Automation.ControlType.ComboBox; up++) el = walker.GetParent(el);
            return el == null ? null : SelectedEntry(el);
        } catch { return null; }
    }

    // The name of the entry a list control has selected, read live (it is not in the cache), or null.
    static string SelectedEntry(System.Windows.Automation.AutomationElement e) {
        try {
            object p;
            if (!e.TryGetCurrentPattern(System.Windows.Automation.SelectionPattern.Pattern, out p)) return null;
            var picked = ((System.Windows.Automation.SelectionPattern)p).Current.GetSelection();
            return picked.Length > 0 ? picked[0].Current.Name : null;
        } catch { return null; }
    }

    static string ReadControls() {
        var wins = AppWindows(); // topmost first
        int l, t, fw, fh; double sc; IntPtr main;
        if (!Geometry(wins, out l, out t, out fw, out fh, out sc, out main)) return "{\"w\":0,\"h\":0,\"nodes\":[]}";
        var sb = new StringBuilder();
        sb.Append("{\"w\":").Append(fw).Append(",\"h\":").Append(fh);
        string title = "";
        if (main != IntPtr.Zero) { var tb = new StringBuilder(256); GetWindowText(main, tb, 256); title = tb.ToString(); }
        sb.Append(",\"title\":").Append(Js(title)).Append(",\"nodes\":[");
        int count = 0;
        var notOff = new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.IsOffscreenProperty, false);
        // Bottom window first: a dialog's controls come after the window it covers, as a page puts its dialog last.
        for (int i = wins.Count - 1; i >= 0 && count < MaxNodes; i--) {
            System.Windows.Automation.AutomationElementCollection all;
            int walkCells = 0;
            try {
                var root = System.Windows.Automation.AutomationElement.FromHandle(wins[i].H);
                using (TreeCache().Activate()) all = root.FindAll(System.Windows.Automation.TreeScope.Descendants, notOff);
            } catch { continue; }
            foreach (System.Windows.Automation.AutomationElement e in all) {
                if (count >= MaxNodes) break;
                var ctObj = Cached(e, System.Windows.Automation.AutomationElement.ControlTypeProperty) as System.Windows.Automation.ControlType;
                if (ctObj == null) continue;
                string name = (Cached(e, System.Windows.Automation.AutomationElement.NameProperty) as string ?? "").Trim();
                bool invokable = Cached(e, System.Windows.Automation.AutomationElement.IsInvokePatternAvailableProperty) as bool? ?? false;
                bool focusable = Cached(e, System.Windows.Automation.AutomationElement.IsKeyboardFocusableProperty) as bool? ?? false;
                string role = RoleOf(ctObj, invokable, focusable, name);
                if (role == "") continue;
                if (role == "cell") {
                    // A spreadsheet lists every cell it shows, empty or not: Excel's 700 empty ones filled the list to its
                    // limit, and its ribbon and dialogs went missing behind them (found live, 2026-10-16). An empty cell,
                    // and a row's or column's bare header ("T", "12"), says nothing; a cell that holds something stays.
                    string held = Cached(e, System.Windows.Automation.ValuePattern.ValueProperty) as string;
                    if (string.IsNullOrWhiteSpace(held) && (name.Length == 0 || IsCellAddress(name) || IsHeaderName(name))) continue;
                    walkCells++;
                }
                // A field that cannot be typed into is text: a file list's Name / Date modified / Size cells are read-only
                // edits, and listed as fields they invited typing - which in a file list renames the file (found live,
                // 2026-10-16: a path typed into a folder's "Name" cell, then Ctrl+A and Delete, sent the folder to the bin).
                if (role == "textbox" && ctObj == System.Windows.Automation.ControlType.Edit
                        && (Cached(e, System.Windows.Automation.ValuePattern.IsReadOnlyProperty) as bool? ?? false)) role = "text";
                // A browser's page is a Document too, and says it is read-only: the page, not a field. An editor's is not, and a
                // RichEdit field (Character Map's "Search for") is a Document that says nothing either way.
                if (ctObj == System.Windows.Automation.ControlType.Document && (Cached(e, System.Windows.Automation.ValuePattern.IsReadOnlyProperty) as bool? ?? false)) continue;
                // Edge's placeholder for an image with no description says nothing about the image.
                if (role == "image" && name.StartsWith("To get missing image descriptions")) continue;
                if (name.Length == 0 && role != "textbox" && role != "combobox" && role != "spinbutton" && role != "slider") {
                    // An unlabelled button is named by its id, as a screen reader falls back to it ("btnSave").
                    name = (Cached(e, System.Windows.Automation.AutomationElement.AutomationIdProperty) as string ?? "").Trim();
                    if (name.Length == 0 || role == "text" || role == "image") continue;
                }
                object rectObj = Cached(e, System.Windows.Automation.AutomationElement.BoundingRectangleProperty);
                if (!(rectObj is System.Windows.Rect)) continue;
                var r = (System.Windows.Rect)rectObj;
                if (r.IsEmpty || r.Width < 2 || r.Height < 2) continue;
                string box = BoxIn(r, wins, i, l, t, fw, fh, sc);
                if (box == null) continue;
                if (count > 0) sb.Append(',');
                sb.Append("{\"role\":\"").Append(role).Append("\",\"name\":").Append(Js(Cut(name, 200)));
                sb.Append(box);
                bool password = Cached(e, System.Windows.Automation.AutomationElement.IsPasswordProperty) as bool? ?? false;
                if (password) sb.Append(",\"password\":true");
                else {
                    string value = Cached(e, System.Windows.Automation.ValuePattern.ValueProperty) as string;
                    // A drop-down list that cannot be typed into (Save As's "Save as type") has no Value: what it
                    // shows is its selected entry. Found live (2026-10-15): read as empty, nobody could tell which
                    // file type Excel would save as.
                    if (string.IsNullOrEmpty(value) && role == "combobox") value = SelectedEntryAt(r);
                    if (!string.IsNullOrEmpty(value) && value != name) sb.Append(",\"value\":").Append(Js(Cut(value, 500)));
                }
                object toggle = Cached(e, System.Windows.Automation.TogglePattern.ToggleStateProperty);
                if (toggle is System.Windows.Automation.ToggleState) sb.Append(",\"checked\":").Append((System.Windows.Automation.ToggleState)toggle == System.Windows.Automation.ToggleState.On ? "true" : "false");
                object expand = Cached(e, System.Windows.Automation.ExpandCollapsePattern.ExpandCollapseStateProperty);
                if (expand is System.Windows.Automation.ExpandCollapseState && (System.Windows.Automation.ExpandCollapseState)expand != System.Windows.Automation.ExpandCollapseState.LeafNode)
                    sb.Append(",\"expanded\":").Append((System.Windows.Automation.ExpandCollapseState)expand == System.Windows.Automation.ExpandCollapseState.Expanded ? "true" : "false");
                if (Cached(e, System.Windows.Automation.SelectionItemPattern.IsSelectedProperty) as bool? ?? false) sb.Append(",\"selected\":true");
                if (!(Cached(e, System.Windows.Automation.AutomationElement.IsEnabledProperty) as bool? ?? true)) sb.Append(",\"disabled\":true");
                if (Cached(e, System.Windows.Automation.AutomationElement.HasKeyboardFocusProperty) as bool? ?? false) sb.Append(",\"focused\":true");
                sb.Append('}');
                count++;
            }
            // A spreadsheet's grid shows its cells to UI Automation only through its grid pattern: none of them is among
            // the descendants above. Found live (2026-10-16): in Excel the worker could not read a single cell - it typed,
            // undid and typed again, and could not tell whether its Total row was there.
            if (count < MaxNodes && walkCells == 0) count += GridCells(wins, i, l, t, fw, fh, sc, sb, count, MaxNodes - count);
        }
        sb.Append("]}");
        return sb.ToString();
    }

    const int GridRows = 30, GridCols = 12;

    // The cells a grid control shows from its top-left corner, those that hold something: role "cell", the cell's own
    // name (Excel's is its address) and what it holds, at its place on the screen. At most GridRows x GridCols are read.
    static int GridCells(List<Win> wins, int i, int l, int t, int fw, int fh, double sc, StringBuilder sb, int count, int room) {
        int added = 0;
        System.Windows.Automation.AutomationElementCollection grids;
        try {
            var root = System.Windows.Automation.AutomationElement.FromHandle(wins[i].H);
            var isGrid = new System.Windows.Automation.OrCondition(
                new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.ControlType.DataGrid),
                new System.Windows.Automation.PropertyCondition(System.Windows.Automation.AutomationElement.ControlTypeProperty, System.Windows.Automation.ControlType.Table));
            grids = root.FindAll(System.Windows.Automation.TreeScope.Descendants, isGrid);
        } catch { return 0; }
        foreach (System.Windows.Automation.AutomationElement g in grids) {
            try {
                object gp;
                if (!g.TryGetCurrentPattern(System.Windows.Automation.GridPattern.Pattern, out gp)) continue;
                var grid = (System.Windows.Automation.GridPattern)gp;
                var gr = g.Current.BoundingRectangle;
                if (gr.IsEmpty || gr.Width < 40 || gr.Height < 40) continue;
                // The first cell shown: the one just inside the grid's top-left corner, past any headers.
                int r0 = 0, c0 = 0;
                for (int probe = 4; probe <= 60; probe += 14) {
                    System.Windows.Automation.AutomationElement first = null;
                    try { first = System.Windows.Automation.AutomationElement.FromPoint(new System.Windows.Point(gr.X + probe, gr.Y + probe)); } catch { }
                    object ip;
                    if (first != null && first.TryGetCurrentPattern(System.Windows.Automation.GridItemPattern.Pattern, out ip)) {
                        var item = (System.Windows.Automation.GridItemPattern)ip;
                        r0 = item.Current.Row; c0 = item.Current.Column; break;
                    }
                }
                int rows = Math.Min(grid.Current.RowCount - r0, GridRows), cols = Math.Min(grid.Current.ColumnCount - c0, GridCols);
                for (int r = r0; r < r0 + rows && added < room; r++) {
                    bool rowShown = false;
                    for (int c = c0; c < c0 + cols && added < room; c++) {
                        System.Windows.Automation.AutomationElement cell;
                        try { cell = grid.GetItem(r, c); } catch { continue; }
                        if (cell == null) continue;
                        var cr = cell.Current.BoundingRectangle;
                        if (cr.IsEmpty || cr.X >= gr.Right || cr.Y >= gr.Bottom) continue;
                        rowShown = true;
                        // A row's or a column's header ("A", "1", "Select All") is not a cell of the sheet.
                        if (cell.Current.ControlType == System.Windows.Automation.ControlType.HeaderItem) continue;
                        string cname = (cell.Current.Name ?? "").Trim();
                        string cvalue = "";
                        object vp;
                        if (cell.TryGetCurrentPattern(System.Windows.Automation.ValuePattern.Pattern, out vp))
                            cvalue = ((System.Windows.Automation.ValuePattern)vp).Current.Value ?? "";
                        if (cvalue.Trim().Length == 0 && (cname.Length == 0 || IsCellAddress(cname))) continue;
                        string box = BoxIn(cr, wins, i, l, t, fw, fh, sc);
                        if (box == null) continue;
                        if (count + added > 0) sb.Append(',');
                        sb.Append("{\"role\":\"cell\",\"name\":").Append(Js(Cut(cname.Length > 0 ? cname : ColumnName(c) + (r + 1), 200)));
                        sb.Append(box);
                        if (cvalue.Length > 0 && cvalue != cname) sb.Append(",\"value\":").Append(Js(Cut(cvalue, 500)));
                        sb.Append('}');
                        added++;
                    }
                    if (!rowShown) break;
                }
            } catch { }
        }
        return added;
    }

    // The JSON "box" of a control of window i whose screen rectangle is r, in the pixels of the picture (corner l, t, size
    // fw x fh, scale sc), cut to the picture. Null under a window nearer the top (a dialog over its form: not something
    // that can be clicked now), and for what is too small to show.
    static string BoxIn(System.Windows.Rect r, List<Win> wins, int i, int l, int t, int fw, int fh, double sc) {
        double cx = r.X + r.Width / 2, cy = r.Y + r.Height / 2;
        for (int j = 0; j < i; j++) { var v = wins[j].V; if (cx >= v.L && cx < v.R && cy >= v.T && cy < v.B) return null; }
        int x = (int)Math.Round((r.X - l) * sc), y = (int)Math.Round((r.Y - t) * sc);
        int w = (int)Math.Round(r.Width * sc), h = (int)Math.Round(r.Height * sc);
        if (x < 0) { w += x; x = 0; } if (y < 0) { h += y; y = 0; }
        if (x + w > fw) w = fw - x; if (y + h > fh) h = fh - y;
        if (w < 2 || h < 2) return null;
        return new StringBuilder(",\"box\":[").Append(x).Append(',').Append(y).Append(',').Append(w).Append(',').Append(h).Append(']').ToString();
    }

    static bool IsHeaderName(string s) {
        return System.Text.RegularExpressions.Regex.IsMatch(s, @"^(?:[A-Z]{1,3}|[0-9]{1,7}|Select All)$");
    }

    static bool IsCellAddress(string s) {
        return System.Text.RegularExpressions.Regex.IsMatch(s, @"^\$?[A-Z]{1,3}\$?[0-9]{1,7}$");
    }

    static string ColumnName(int c) {
        string s = "";
        for (c++; c > 0; c = (c - 1) / 26) s = (char)('A' + (c - 1) % 26) + s;
        return s;
    }

    static string Cut(string s, int n) { return s.Length <= n ? s : s.Substring(0, n); }

    static string Js(string s) {
        var b = new StringBuilder("\"");
        foreach (char c in s) {
            if (c == '"' || c == '\\') b.Append('\\').Append(c);
            else if (c < 0x20) b.Append("\\u").Append(((int)c).ToString("x4"));
            else b.Append(c);
        }
        return b.Append('"').ToString();
    }

    public static void Thumb(int width) {
        lock (composeLock) {
            Surface pic = Compose(true);
            if (pic == null) { Reply("ERR nothing to show"); return; }
            int tw = Math.Min(Math.Max(width, 64), pic.W), th = Math.Max(1, (int)Math.Round(pic.H * (double)tw / pic.W));
            if (small == null || small.W != tw || small.H != th) {
                if (small != null) small.Free();
                small = new Surface(tw, th);
                if (smallPrev != IntPtr.Zero) { Marshal.FreeHGlobal(smallPrev); smallPrev = IntPtr.Zero; }
            }
            SetStretchBltMode(small.Dc, 4 /* HALFTONE: averaged, not picked */); SetBrushOrgEx(small.Dc, 0, 0, IntPtr.Zero);
            StretchBlt(small.Dc, 0, 0, tw, th, pic.Dc, 0, 0, pic.W, pic.H, 0x00CC0020);
            GdiFlush();
            int need = tw * 4 * th;
            if (smallPrev != IntPtr.Zero && memcmp(small.Bits, smallPrev, (UIntPtr)need) == 0) { Reply("THUMB same"); return; }
            if (smallPrev == IntPtr.Zero) smallPrev = Marshal.AllocHGlobal(need);
            CopyMemory(smallPrev, small.Bits, (UIntPtr)need);
            byte[] data;
            using (var ms = new System.IO.MemoryStream()) { small.View.Save(ms, jpeg, jpegQuality); data = ms.ToArray(); }
            byte[] head = Encoding.ASCII.GetBytes("THUMB " + tw + " " + th + " " + data.Length + "\n");
            lock (outLock) { stdout.Write(head, 0, head.Length); stdout.Write(data, 0, data.Length); stdout.Flush(); }
        }
    }

    // The rectangle of the picture that differs from the last one sent, grown to whole 16-pixel blocks so a JPEG patch
    // has no seam where it meets what is already on the viewer's screen. False if nothing differs.
    static bool Diff(IntPtr now, int w, int h, out Rectangle dirty) {
        int stride = w * 4, row = stride;
        dirty = Rectangle.Empty;
        int top = -1;
        for (int y = 0; y < h; y++)
            if (memcmp(IntPtr.Add(now, y * stride), IntPtr.Add(prev, y * stride), (UIntPtr)row) != 0) { top = y; break; }
        if (top < 0) return false;
        int bottom = top;
        for (int y = h - 1; y > top; y--)
            if (memcmp(IntPtr.Add(now, y * stride), IntPtr.Add(prev, y * stride), (UIntPtr)row) != 0) { bottom = y; break; }
        int chunks = (w + 15) / 16, minC = chunks, maxC = -1;
        for (int y = top; y <= bottom; y++) {
            IntPtr a = IntPtr.Add(now, y * stride), b = IntPtr.Add(prev, y * stride);
            for (int c = 0; c < minC; c++) {
                int off = c * 64, n = Math.Min(64, row - off);
                if (memcmp(IntPtr.Add(a, off), IntPtr.Add(b, off), (UIntPtr)n) != 0) { minC = c; break; }
            }
            for (int c = chunks - 1; c > maxC; c--) {
                int off = c * 64, n = Math.Min(64, row - off);
                if (memcmp(IntPtr.Add(a, off), IntPtr.Add(b, off), (UIntPtr)n) != 0) { maxC = c; break; }
            }
        }
        if (maxC < 0) return false;
        int x0 = minC * 16, x1 = Math.Min(w, (maxC + 1) * 16);
        int y0 = top & ~15, y1 = Math.Min(h, (bottom | 15) + 1);
        dirty = new Rectangle(x0, y0, x1 - x0, y1 - y0);
        return true;
    }

    // Window and drawing calls used above.
    [DllImport("user32.dll")] static extern bool IsIconic(IntPtr h);
    [DllImport("user32.dll")] static extern IntPtr GetAncestor(IntPtr h, uint flags);
    [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
    [DllImport("user32.dll")] static extern int GetWindowLong(IntPtr h, int index);
    [DllImport("user32.dll")] static extern uint GetClassLong(IntPtr h, int index);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] static extern uint GetDoubleClickTime();
    [DllImport("gdi32.dll")] static extern bool DeleteObject(IntPtr h);
    [StructLayout(LayoutKind.Sequential)] struct BITMAPINFOHEADER { public int biSize, biWidth, biHeight; public short biPlanes, biBitCount; public int biCompression, biSizeImage, biXPelsPerMeter, biYPelsPerMeter, biClrUsed, biClrImportant; }

    static double Dpi() { try { return GetDpiForSystem() / 96.0; } catch { return 1.0; } }

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

    // A mouse message's point, in the target's client area as this (DPI-aware) process sees it: physical pixels. Windows
    // itself rescales the point of a posted mouse message for a DPI-unaware window - measured at 125%: (269, 96) posted
    // arrived as (215, 77) - so it must not be scaled here too, or every click on such an app lands short of where it was
    // aimed (a field's click went to its label).
    static IntPtr ClientLParam(IntPtr target, POINT screen) {
        POINT c = screen; ScreenToClient(target, ref c);
        return (IntPtr)(((c.Y & 0xFFFF) << 16) | (c.X & 0xFFFF));
    }

    // GetKeyState - which is how an app learns that Ctrl or Shift is held - only ever reflects real hardware input, never a
    // posted message. So for input that carries a modifier, act runs on a helper thread that has joined the app's input
    // queue on the private desktop and has set that queue's key-state table with the modifiers down; the messages are then
    // SENT (not posted) so the state is still in place while the app handles them. mods is any of c (Ctrl), s (Shift), a (Alt).
    // Returns "OK", or "OK unmodified" if Windows would not let a thread join the app's queue.
    static string WithModifiers(IntPtr target, string mods, Action act) {
        if (string.IsNullOrEmpty(mods)) { act(); return "OK"; }
        // A window on the person's own desktop: its input queue is theirs too (see WithModifiersShared).
        if (attached) return WithModifiersShared(target, mods, act);
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

    // How long a keystroke's modifier state is held: as long as the app takes to read what was posted - on a private desktop, which
    // nobody else uses, with room to spare; in the person's own browser, as short as it can be.
    static int Hold(int ms) { return borrowed ? 40 : ms; }

    // ---- The keyboard of a borrowed window. A browser types into a window only while it holds the browser's activation: keys posted to
    // a window of it that is not active go nowhere (measured 2026-11-13 on a throwaway Chrome, another program in front: a click and
    // "p1" left the address bar empty). The window is never made the person's foreground for that - it is told, and only it is told,
    // that it is active (WM_NCACTIVATE, WM_ACTIVATE): with another program in front the foreground stays there and the text arrives
    // ("a2"); WM_SETFOCUS alone, or with WM_NCACTIVATE, is not enough. When the person's own foreground is a window of that same
    // browser, the browser answers that by taking the foreground for real (Windows lets the foreground program do so) - so it is lent:
    // only once the person has paused (no key for 400 ms), and given back when the VM has sent nothing for 1.5 s or the person acts -
    // a key of theirs is held by a keyboard hook, ends the lend at once and is played into their own window; a click of theirs takes
    // the foreground itself. Not on a timer after the VM's keys: the browser reads them when it gets to them, and a key it reads after it
    // is no longer active is lost (measured 2026-11-13: of an address, "chr" arrived; WaitForInputIdle did not wait). The told activation is taken
    // back two seconds after the VM's last key, and the person's last window of the browser is made its last active one again, or a
    // link the person opens from another program would open in the invisible window (the browser opens it in its last active one).
    static IntPtr lendBack;             // the person's foreground while the VM's window holds it for a keystroke; Zero otherwise
    static IntPtr toldActive;           // the window that was told it is active, the foreground left where it was; Zero when none
    static int lastKeysAt;
    static IntPtr personWindow;         // the person's own last foreground window of the browser (WatchPopups)
    static volatile bool holdKeys;
    static readonly List<KBDLLHOOKSTRUCT> heldKeys = new List<KBDLLHOOKSTRUCT>();

    static IntPtr SendQuick(IntPtr h, uint msg, IntPtr w, IntPtr l) {
        IntPtr r; SendMessageTimeout(h, msg, w, l, 0x2 /* SMTO_ABORTIFHUNG */, 1000, out r); return r;
    }
    static bool Ours(IntPtr h) { return h != IntPtr.Zero && (h == attachMain || IsPopup(h)); }

    static readonly object lendLock = new object();

    // Runs a keyboard command and replies; when the window is the person's own browser's, its keyboard is lent to it for the time it takes.
    static void Keyboard(Func<string> act) {
        if (!borrowed) { Reply(act()); return; }
        IntPtr root = GetAncestor(KeyTarget(), 2 /* GA_ROOT */);
        if (!Belongs(root)) root = attachMain;
        string said;
        lock (lendLock) { try { LendKeyboard(root); said = act(); } finally { KeysDone(); } }
        Reply(said);
    }

    static void LendKeyboard(IntPtr root) {
        lastKeysAt = Environment.TickCount;
        if (root == IntPtr.Zero || !IsWindow(root)) return;
        IntPtr fg = GetForegroundWindow();
        if (fg == root || toldActive == root) return;
        if (toldActive != IntPtr.Zero) TakeBackActivation();
        int pid; GetWindowThreadProcessId(root, out pid);
        int fpid = 0; if (fg != IntPtr.Zero) GetWindowThreadProcessId(fg, out fpid);
        if (fpid == pid) {
            // The browser will take the foreground: not while the person is typing, and their keys are held while it has it.
            for (int waited = 0; waited < 3000 && (Environment.TickCount - personActiveAt < 400 || PersonHoldsAKey()); waited += 20) Thread.Sleep(20);
            StartKeyHook();
            holdKeys = true;
        }
        SendQuick(root, 0x0086 /* WM_NCACTIVATE */, (IntPtr)1, IntPtr.Zero);
        SendQuick(root, 0x0006 /* WM_ACTIVATE */, (IntPtr)1 /* WA_ACTIVE */, IntPtr.Zero);
        IntPtr now = GetForegroundWindow();
        if (now != fg) lendBack = fg != IntPtr.Zero && !Ours(fg) ? fg : lastPersonTop; // taken for real: given back after the keys
        else { holdKeys = false; toldActive = root; }
    }

    static void KeysDone() { lastKeysAt = Environment.TickCount; }

    // Ends a lent foreground (WatchPopups, CloseBorrowed): back to the person's window if the VM's still has it, then their held keys.
    static void EndLend() {
        IntPtr back = lendBack; lendBack = IntPtr.Zero;
        if (back != IntPtr.Zero && IsWindow(back) && Ours(GetForegroundWindow())) GiveBackFocus(back, IntPtr.Zero);
        holdKeys = false;
        ReplayHeldKeys();
    }

    // Takes back an activation the VM's window was told of, and makes the person's own last window of the browser its last active one.
    static void TakeBackActivation() {
        IntPtr h = toldActive; toldActive = IntPtr.Zero;
        if (h == IntPtr.Zero || !IsWindow(h) || GetForegroundWindow() == h) return;
        SendQuick(h, 0x0006, IntPtr.Zero /* WA_INACTIVE */, IntPtr.Zero);
        SendQuick(h, 0x0086, IntPtr.Zero, IntPtr.Zero);
        PersonLastActive(h);
    }

    // Makes the person's own last window of the browser (the browser of window h) the one it last activated, as it was before the VM's
    // window was: a link opened from another program goes to the last active window (measured 2026-11-13: one opened after the VM's window
    // was activated opened in it, out of the person's sight; after this, in theirs). Told, not shown: the foreground does not move.
    static void PersonLastActive(IntPtr h) {
        IntPtr mine = personWindow, fg = GetForegroundWindow();
        // Not seen in front since this helper started: their topmost window of the browser is the one they used last.
        if (mine == IntPtr.Zero || !IsWindow(mine)) {
            mine = IntPtr.Zero;
            int bp; GetWindowThreadProcessId(h, out bp);
            foreach (IntPtr w in BrowserWindows(false)) { int wp; GetWindowThreadProcessId(w, out wp); if (wp == bp && !Ours(w)) { mine = w; break; } }
        }
        if (mine != IntPtr.Zero && IsWindow(mine) && mine != fg && !Ours(mine)) {
            SendQuick(mine, 0x0006, (IntPtr)1, IntPtr.Zero);
            if (GetForegroundWindow() == fg) SendQuick(mine, 0x0006, IntPtr.Zero, IntPtr.Zero);
            else if (fg != IntPtr.Zero) GiveBackFocus(fg, IntPtr.Zero); // the browser took the foreground for it: back to where the person was
        }
    }

    // The keys the person presses while their browser's foreground is lent: held, never passed to the VM's window, and played into theirs
    // once it is given back. Only real keys are held (a key this or any program sends is marked injected).
    [StructLayout(LayoutKind.Sequential)] public struct KBDLLHOOKSTRUCT { public uint vkCode, scanCode, flags, time; public IntPtr extra; }
    [StructLayout(LayoutKind.Sequential)] struct KEYBDINPUT { public ushort wVk, wScan; public uint dwFlags, time; public IntPtr extra; }
    [StructLayout(LayoutKind.Sequential)] struct MOUSEINPUT { public int dx, dy; public uint data, dwFlags, time; public IntPtr extra; }
    [StructLayout(LayoutKind.Explicit)] struct INPUTUNION { [FieldOffset(0)] public MOUSEINPUT mi; [FieldOffset(0)] public KEYBDINPUT ki; }
    [StructLayout(LayoutKind.Sequential)] struct INPUT { public uint type; public INPUTUNION u; }
    [StructLayout(LayoutKind.Sequential)] struct MSG { public IntPtr hwnd; public uint message; public IntPtr wParam, lParam; public uint time; public POINT pt; }
    delegate IntPtr LowLevelProc(int code, IntPtr w, IntPtr l);
    [DllImport("user32.dll")] static extern IntPtr SetWindowsHookEx(int id, LowLevelProc proc, IntPtr mod, uint thread);
    [DllImport("user32.dll")] static extern IntPtr CallNextHookEx(IntPtr h, int code, IntPtr w, IntPtr l);
    [DllImport("user32.dll")] static extern int GetMessage(out MSG m, IntPtr h, uint min, uint max);
    [DllImport("user32.dll")] static extern bool PeekMessage(out MSG m, IntPtr h, uint min, uint max, uint remove);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern IntPtr GetModuleHandle(string name);
    [DllImport("user32.dll")] static extern uint SendInput(uint n, INPUT[] inputs, int size);
    static LowLevelProc keyHookProc; // kept alive for as long as the hook is
    static Thread keyHookThread;
    static void StartKeyHook() {
        if (keyHookThread != null) return;
        var ready = new ManualResetEvent(false);
        keyHookThread = new Thread(() => {
            keyHookProc = (code, w, l) => {
                if (code >= 0 && holdKeys) {
                    var k = (KBDLLHOOKSTRUCT)Marshal.PtrToStructure(l, typeof(KBDLLHOOKSTRUCT));
                    if ((k.flags & 0x10 /* LLKHF_INJECTED */) == 0) { lock (heldKeys) heldKeys.Add(k); return (IntPtr)1; }
                }
                return CallNextHookEx(IntPtr.Zero, code, w, l);
            };
            SetWindowsHookEx(13 /* WH_KEYBOARD_LL */, keyHookProc, GetModuleHandle(null), 0);
            ready.Set();
            MSG m; while (GetMessage(out m, IntPtr.Zero, 0, 0) > 0) { }
        }) { IsBackground = true };
        keyHookThread.Start();
        ready.WaitOne(2000);
    }
    static void ReplayHeldKeys() {
        List<KBDLLHOOKSTRUCT> keys; lock (heldKeys) { keys = new List<KBDLLHOOKSTRUCT>(heldKeys); heldKeys.Clear(); }
        if (keys.Count == 0) return;
        var inputs = new INPUT[keys.Count];
        for (int i = 0; i < keys.Count; i++) {
            inputs[i].type = 1; // INPUT_KEYBOARD
            inputs[i].u.ki.wVk = (ushort)keys[i].vkCode;
            inputs[i].u.ki.wScan = (ushort)keys[i].scanCode;
            inputs[i].u.ki.dwFlags = ((keys[i].flags & 0x80) != 0 ? 2u /* KEYUP */ : 0u) | ((keys[i].flags & 0x1) != 0 ? 1u /* EXTENDEDKEY */ : 0u);
        }
        SendInput((uint)inputs.Length, inputs, Marshal.SizeOf(typeof(INPUT)));
    }

    [DllImport("user32.dll")] static extern short GetAsyncKeyState(int vk);
    // Whether the person is holding a key of their own keyboard down (the mouse buttons, 1 to 6, are not keys).
    static bool PersonHoldsAKey() {
        for (int vk = 8; vk < 255; vk++) if ((GetAsyncKeyState(vk) & 0x8000) != 0) return true;
        return false;
    }

    // The modifiers of a keystroke for a window of the person's own browser: this thread joins the window's input queue, sets that
    // queue's key state with the modifiers down for as long as the keystroke takes to be read, and puts it back. That queue is
    // also the one the person's own windows of that browser type into, so this waits (a few seconds at most) while they are holding
    // a key themselves, and holds the state for as short a time as it can (Hold). When the queue cannot be joined it says so: a
    // keystroke carried without its modifiers is a different one - Ctrl+A would be an "a" typed into the page.
    static string WithModifiersShared(IntPtr target, string mods, Action act) {
        string result = "OK";
        var th = new System.Threading.Thread(() => {
            int pid; uint appThread = GetWindowThreadProcessId(target, out pid);
            uint me = GetCurrentThreadId();
            for (int waited = 0; PersonHoldsAKey() && waited < 3000; waited += 20) System.Threading.Thread.Sleep(20);
            if (appThread == 0 || !AttachThreadInput(me, appThread, true)) { result = "ERR the modifier keys could not be carried to the window"; return; }
            try {
                var saved = new byte[256]; GetKeyboardState(saved);
                var keys = (byte[])saved.Clone();
                if (mods.Contains("c")) { keys[0x11] |= 0x80; keys[0xA2] |= 0x80; }
                if (mods.Contains("s")) { keys[0x10] |= 0x80; keys[0xA0] |= 0x80; }
                if (mods.Contains("a")) { keys[0x12] |= 0x80; keys[0xA4] |= 0x80; }
                SetKeyboardState(keys);
                try { act(); } finally { SetKeyboardState(saved); }
            } finally { AttachThreadInput(me, appThread, false); }
        });
        th.Start(); th.Join();
        return result;
    }

    // While a button is held, moves and the release go to the control that got the press, like mouse capture. mods is
    // any of c, s, a (Ctrl, Shift, Alt) held during this event.
    static IntPtr downWin, hoveredClient; static long downAt; static POINT downPt;
    static void ClientHover(IntPtr target) {
        if (target == hoveredClient) return;
        if (hoveredClient != IntPtr.Zero && IsWindow(hoveredClient)) PostMessage(hoveredClient, 0x02A3 /* WM_MOUSELEAVE */, IntPtr.Zero, IntPtr.Zero);
        hoveredClient = target;
    }

    public static string Mouse(string kind, int x, int y, bool right, string mods) {
        EnsureGeometry();
        POINT screen = ScreenPoint(x, y);
        pointer = screen; pointerKnown = true;
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
        IntPtr target = captured;
        if (target == IntPtr.Zero) {
            IntPtr top = WindowAt(screen);
            if (top == IntPtr.Zero) { if (kind == "move") ClientHover(IntPtr.Zero); return "OK"; }
            target = Descend(top, screen);
        }
        if (kind == "move" && captured == IntPtr.Zero) ClientHover(target);
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
            if (mods.Length > 0) System.Threading.Thread.Sleep(Hold(80)); // keep the modifiers set until the app has read them
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
        // A browser has one input queue for all its windows, so the focus it reports can be in a window of the person's: keys are
        // never sent there.
        if (attached && focus != IntPtr.Zero && !Belongs(GetAncestor(focus, 2 /* GA_ROOT */))) focus = IntPtr.Zero;
        if (focus != IntPtr.Zero) return focus;
        // What was clicked last, only while it is still there: the last click may have been in an app since closed.
        if (lastTarget != IntPtr.Zero && IsWindow(lastTarget) && (!attached || Belongs(GetAncestor(lastTarget, 2)))) return lastTarget;
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

    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern short VkKeyScan(char ch);
    [DllImport("user32.dll")] static extern uint MapVirtualKey(uint code, uint type);

    // KeyedText: the app takes typed text as keys, not characters (set by the control plane from how the app's controls
    // are read: one that draws its own screen). Off, a space and a line break are characters like any other.
    public static bool KeyedText = false;

    // TypeNewline is a line break inside typed text: the Enter character to an app that takes characters (a key going
    // down would be turned into a second one, after whatever is queued), the Enter key to one that takes keys.
    public static void TypeNewline() {
        if (KeyedText) { Key(13, ""); System.Threading.Thread.Sleep(60); return; }
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) return;
        PostMessage(t, WM_CHAR, (IntPtr)13, (IntPtr)1);
    }

    public static void TypeChar(char ch) {
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) return;
        var cls = new StringBuilder(32); GetClassName(t, cls, 32);
        if (cls.ToString() != "ConsoleWindowClass") {
            // A space is its key going down and up, as a keyboard sends it, and the app makes the character from that
            // itself: found live (2026-10-15), TallyPrime dropped every space posted as a bare character ("Northwind
            // Traders" saved as "NorthwindTraders"). Not the character as well: an app that translates its keys
            // (PowerPoint, Word) then had two, the second landing among the next letters ("Q3 2 026  Business"). The
            // pause lets the app make its character before the next letter is posted behind it. Only the space: a
            // letter's key going down would fire an app's one-letter shortcuts.
            if (ch == ' ' && KeyedText) {
                uint sp = MapVirtualKey(0x20, 0);
                PostMessage(t, WM_KEYDOWN, (IntPtr)0x20, (IntPtr)(1 | (sp << 16)));
                PostMessage(t, WM_KEYUP, (IntPtr)0x20, (IntPtr)(0xC0000001 | (sp << 16)));
                System.Threading.Thread.Sleep(60);
                return;
            }
            PostMessage(t, WM_CHAR, (IntPtr)ch, (IntPtr)1); return;
        }
        // A console takes a character as a key going down, and a second one like it with no key coming up in between
        // as that key still held: it counts a repeat. Typed into a terminal, every doubled letter came out three times
        // (measured: "bool" typed as "boool", ">>" as ">>>"). So each character is a key that goes down and comes up.
        int vk = VkKeyScan(ch) & 0xFF; if (vk == 0xFF) vk = 0;
        uint scan = vk == 0 ? 0 : MapVirtualKey((uint)vk, 0);
        PostMessage(t, WM_CHAR, (IntPtr)ch, (IntPtr)(1 | (scan << 16)));
        PostMessage(t, WM_KEYUP, (IntPtr)vk, (IntPtr)(0xC0000001 | (scan << 16)));
    }

    // Whether a window is a list of files and folders (a file dialog's or Explorer's): its own class, or anything inside
    // the shell's file view. Delete there sends what is selected to the Recycle Bin.
    static bool IsFileView(IntPtr h) {
        for (int up = 0; h != IntPtr.Zero && up < 6; up++, h = GetParent(h)) {
            var cls = new StringBuilder(64); GetClassName(h, cls, 64);
            string c = cls.ToString();
            if (c == "SysListView32" || c == "SysTreeView32" || c == "SHELLDLL_DefView" || c == "DUIViewWndClassName") return true;
        }
        return false;
    }

    public static string Key(int vk, string mods) {
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) return "ERR no window";
        // The Delete key never reaches a list of files: it would delete what is selected there, on the person's own computer.
        // Found live (2026-10-16): a worker's "replace this field's text" was Ctrl+A then Delete, the keyboard was on a
        // file dialog's list, and a folder of the person's went to the Recycle Bin.
        if (vk == 46 && IsFileView(t)) return "ERR refused: the Delete key would delete files in that list";
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
            System.Threading.Thread.Sleep(Hold(150));
        });
    }
}
'@

# What the C# above uses beyond the core: drawing, and UI Automation (its tree, and System.Windows.Rect from WindowsBase).
$NativeAssemblies = @('System.Drawing', 'UIAutomationClient', 'UIAutomationTypes', 'WindowsBase')

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
            Add-Type -ReferencedAssemblies $NativeAssemblies -TypeDefinition $NativeSource -OutputAssembly $tmp -OutputType Library
            Move-Item -LiteralPath $tmp -Destination $dll -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        } catch { }
    }
    if (-not ('Native' -as [type])) {
        if (Test-Path -LiteralPath $dll) { try { Add-Type -Path $dll } catch { } }
        if (-not ('Native' -as [type])) { Add-Type -ReferencedAssemblies $NativeAssemblies -TypeDefinition $NativeSource }
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

# A small launcher that only opens a web app in the person's browser (e.g. msedge --app=http://localhost:8000). Run
# on the private desktop it starts that browser on the person's own profile, which hands the page to the copy already open on
# the visible desktop - so nothing appears in the worker. Such a launcher is recognised by the text a managed or native
# program keeps in its file (the "--app=" flag and an http(s) address), and replaced by the same page opened in a browser of
# its own. Anything bigger or without both is an ordinary program.
#
# Such a launcher often does one more thing first: it starts the web app's own server when the page is on this computer
# and nothing answers there yet (starting its containers, say). Found live: with the launcher only replaced, the worker's
# app shortcut opened a browser on "localhost refused to connect". So when the page is local and not answering, the launcher
# is run for that part alone (Start-WebAppServer, Native.Prepare): any browser it goes on to start is ended as it is created.

# $p is a resolved program; Launcher is the launcher it stands in for, when it is a web app's.
function Start-WebAppServer($p) {
    if (-not $p.Launcher) { return }
    $m = [regex]::Match([string]$p.Args, '--app=(\S+)')
    if (-not $m.Success) { return }
    try { $u = [Uri]$m.Groups[1].Value } catch { return }
    if (@('localhost', '127.0.0.1', '[::1]') -notcontains $u.Host) { return }
    # The launcher is run by the process that holds the worker's desktop, so what it starts ends with the worker. That
    # process also waits for the page itself to answer, which a listening port does not promise.
    $status = [Native]::Prepare($DesktopName, [string]$p.Launcher, [string]$u.AbsoluteUri)
    if ($status.StartsWith('ERR ')) { throw $status.Substring(4) }
}

function Resolve-WebLauncher([string]$exe) {
    if ([IO.Path]::GetExtension($exe).ToLower() -ne '.exe') { return $null }
    try {
        if ((Get-Item -LiteralPath $exe -ErrorAction Stop).Length -gt 2MB) { return $null }
        $bytes = [IO.File]::ReadAllBytes($exe)
    } catch { return $null }
    # UTF-16 text can start at either byte of a pair, so both alignments are read.
    $even = $bytes.Length - ($bytes.Length % 2)
    $odd = $bytes.Length - 1 - (($bytes.Length - 1) % 2)
    $text = [Text.Encoding]::Unicode.GetString($bytes, 0, $even) + "`n" + [Text.Encoding]::Unicode.GetString($bytes, 1, $odd) + "`n" + [Text.Encoding]::ASCII.GetString($bytes)
    if ($text -notmatch '--app=') { return $null }
    $m = [regex]::Match($text, 'https?://[A-Za-z0-9._~:/?#@!$&''()*+,;=%-]+')
    if (-not $m.Success) { return $null }
    $url = $m.Value
    foreach ($b in @(
        @{ P = "${env:ProgramFiles(x86)}\Microsoft\Edge\Application\msedge.exe"; B = 'msedge' },
        @{ P = "$env:ProgramFiles\Microsoft\Edge\Application\msedge.exe"; B = 'msedge' },
        @{ P = "$env:ProgramFiles\Google\Chrome\Application\chrome.exe"; B = 'chrome' },
        @{ P = "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe"; B = 'chrome' })) {
        if (Test-Path -LiteralPath $b.P) { return @{ Target = $b.P; Args = "--app=$url"; Base = $b.B } }
    }
    return $null
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
    $web = Resolve-WebLauncher $target
    if ($web) { return @{ Target = $web.Target; Args = $web.Args; WorkDir = [IO.Path]::GetDirectoryName($web.Target); Base = $web.Base; OwnProfile = $true; Launcher = $target } }
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

# A private profile an older build copied from the person's own browser (it carries an import marker) holds a copy of their
# cookies and account data, which can never be opened there (see "In place" below) and only sits on the disk. It is deleted before
# the browser starts.
function Remove-ImportedHostProfile([string]$dir) {
    if (-not (Test-Path -LiteralPath $dir -PathType Container)) { return }
    $marks = @(Get-ChildItem -LiteralPath $dir -Force -Filter '.decurix-host-profile-imported-*' -ErrorAction SilentlyContinue)
    if ($marks.Count -eq 0) { return }
    Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $dir) {
        # Not removable (held open): never launched with it.
        throw "the private browser profile imported from your own browser could not be removed: $dir"
    }
}

# ---- In place. A browser's cookies and account tokens are encrypted ("v20", Chrome 127 and later) so that only Chrome running from
# the profile's own folder can open them: a copy anywhere else cannot, however it is made and however it is started. Found
# 2026-11-12 on Chrome 157: three Google accounts "Failed to decrypt token" on a copy made from the person's real files and started
# the ordinary way, none on the real folder; 3,510 cookies and the 3 tokens are all v20. So a browser given to a VM is not given a
# profile of its own: it is the person's own browser, from their own profile folder, signed in as they are, with their history, tabs,
# extensions and site logins.
#
# And it is not run anywhere of ours either. Chrome allows one process per profile folder, on one desktop: run on this helper's
# private desktop (the first version of this), it took the person's own Chrome from them - every launch of theirs was handed to
# the process on the hidden desktop and opened out of their sight (found live 2026-11-13). So the VM is given ONE WINDOW of the
# person's own running browser, made invisible to them (Native.Borrow), and their Chrome stays theirs, whenever they open it.

# Whether launching $p runs a browser from the person's own profile: a browser started plainly - not a web app's --app window
# (those keep a private profile of their own), not the stand-in for a web app's launcher, not given a profile folder.
function Test-RunsInPlace($p) {
    if (-not ($Browsers -contains $p.Base)) { return $false }
    if ($p.Launcher) { return $false }
    return ($p.Args -notmatch '(?i)(^|\s)--(app|user-data-dir)\b')
}

# Starts a completed launch, from Resolve-Launch or a check's answer: on the private desktop, or - for a browser that runs in place -
# as a window of the person's own browser (which is started if it is not running).
function Start-Resolved($l) {
    if (-not $l.InPlace) { return [Native]::Launch($DesktopName, $l.Target, $l.Args, $l.WorkDir) }
    return [Native]::Borrow($l.Base, $l.Target, $l.Args, $l.WorkDir)
}

# The command line a resolved program is started with: its own profile for an Electron app or a web app's browser (per desktop),
# and the flags that keep a first-run or restore prompt from taking the keyboard.
function Complete-Launch($p) {
    $argLine = $p.Args
    Start-WebAppServer $p
    if (Test-RunsInPlace $p) {
        # No profile folder of its own: it opens the person's (the shortcut's --profile-directory, else the one it used last).
        Remove-ImportedHostProfile (Join-Path $ProfileRoot "$DesktopName-$($p.Base)")
        # Only used when the browser is not running and is started here (otherwise the person's own process is asked for a window and
        # ignores these): no first-run or restore prompt taking the keyboard of a window nobody sees. Nothing else is changed about the
        # person's browser - not its accessibility, not its background mode.
        if ($argLine -notlike '*no-first-run*') { $argLine += ' --no-first-run --no-default-browser-check' }
        $argLine += ' --disable-session-crashed-bubble --hide-crash-restore-bubble'
        return @{ Target = $p.Target; Args = $argLine.Trim(); WorkDir = $p.WorkDir; Base = $p.Base; InPlace = $true }
    }
    if ($p.OwnProfile) {
        $dir = Join-Path $ProfileRoot "$DesktopName-$($p.Base)"
        if ($Browsers -contains $p.Base) { Remove-ImportedHostProfile $dir }
        $argLine = (($argLine + " --user-data-dir=`"$dir`"").Trim())
        # Chromium (a browser, an Electron app) builds the accessibility tree of what a page shows only once something
        # turns it on, and until then UI Automation sees its title bar and nothing in the page: the VM's agent, which
        # reads the app's controls through that tree (Native.Tree), would see a sign-in page as four buttons.
        if ($argLine -notlike '*force-renderer-accessibility*') { $argLine += ' --force-renderer-accessibility' }
        if ($Browsers -contains $p.Base) {
            if ($argLine -notlike '*no-first-run*') { $argLine += ' --no-first-run --no-default-browser-check' }
            $argLine += ' --disable-session-crashed-bubble --hide-crash-restore-bubble'
            # A worker's browser ends with its last window, and fetches nothing for itself. Measured on a worker whose
            # only page was a local web app: Edge downloaded 350 MB of its own components into the worker's profile
            # (component_crx_cache, ProvenanceData), and a browser left "running in the background" after its window is
            # closed is a few hundred MB that nobody can see to close.
            $argLine += ' --disable-background-mode --disable-component-update'
        }
        Clear-CrashMark $dir
    }
    return @{ Target = $p.Target; Args = $argLine; WorkDir = $p.WorkDir }
}

# Remove-TreeNoFollow deletes folder $dir and everything in it without ever entering a link: a junction or symbolic link
# inside is removed as a link, and what it points at is left alone (Remove-Item -Recurse in Windows PowerShell 5.1 walks
# into a junction and empties its target). Answers how many entries it had to leave - a file an app still holds open, an
# entry it may not delete - so 0 means the folder is gone. Never throws.
function Remove-TreeNoFollow([string]$dir) {
    $left = 0
    $link = [IO.FileAttributes]::ReparsePoint
    $readOnly = [IO.FileAttributes]::ReadOnly
    try { $entries = ([IO.DirectoryInfo]$dir).GetFileSystemInfos() } catch { return 1 }
    foreach ($e in $entries) {
        $isLink = ($e.Attributes -band $link) -ne 0
        $isDir = $e -is [IO.DirectoryInfo]
        if ($isDir -and -not $isLink) {
            $left += Remove-TreeNoFollow $e.FullName
            continue
        }
        try {
            if (-not $isLink -and ($e.Attributes -band $readOnly) -ne 0) { $e.Attributes = $e.Attributes -bxor $readOnly }
            if ($isDir) { [IO.Directory]::Delete($e.FullName, $false) } else { [IO.File]::Delete($e.FullName) }
        } catch { $left++ }
    }
    if ($left -eq 0) {
        try {
            $d = [IO.DirectoryInfo]$dir
            if (($d.Attributes -band $readOnly) -ne 0) { $d.Attributes = $d.Attributes -bxor $readOnly }
            [IO.Directory]::Delete($dir, $false)
        } catch { $left++ }
    }
    return $left
}

# Remove-DesktopProfiles is "dropprofiles": a VM deleted for good takes the private profiles its apps kept here with it
# (B-19) - a sign-in made in a hidden browser must not outlive the worker it was made for. A stop never asks: the profile
# is what keeps a worker signed in from one start to the next. $names are private desktop names (what "desktop" is told);
# an app's profile is "<name>-<app>" directly under $ProfileRoot (Complete-Launch), and a name never holds a '-', so
# "decurix_vm1" owns decurix_vm1-chrome and never decurix_vm10-chrome. Nothing else is touched: no folder that is not a
# direct child of the root, no root that is itself a link, and no link followed (a profile folder that is a junction has
# just the junction removed). Every name is checked before anything is removed. A file still held open (an app that has
# not quite ended) is left and counted, and the control plane asks again.
function Remove-DesktopProfiles([string[]]$names) {
    $wanted = New-Object 'System.Collections.Generic.HashSet[string]' ([StringComparer]::OrdinalIgnoreCase)
    foreach ($n in $names) {
        if ($n -cnotmatch '^decurix_[A-Za-z0-9_]{1,40}$') { return 'ERR invalid desktop name' }
        [void]$wanted.Add($n)
    }
    if ($wanted.Count -eq 0) { return 'ERR no desktop name' }
    $root = [IO.Path]::GetFullPath($ProfileRoot).TrimEnd('\')
    if (-not [IO.Directory]::Exists($root)) { return 'OK removed=0 left=0 locked=0' }
    if (([IO.File]::GetAttributes($root) -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        return 'ERR the profile folder is a link; nothing was removed'
    }
    $removed = 0; $left = 0; $locked = 0
    foreach ($d in ([IO.DirectoryInfo]$root).GetDirectories()) {
        $cut = $d.Name.IndexOf('-')
        if ($cut -lt 1 -or $cut -ge $d.Name.Length - 1) { continue }
        if (-not $wanted.Contains($d.Name.Substring(0, $cut))) { continue }
        $full = [IO.Path]::GetFullPath($d.FullName)
        if (-not [string]::Equals([IO.Path]::GetDirectoryName($full), $root, [StringComparison]::OrdinalIgnoreCase)) { continue }
        if (($d.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            # A link where a profile should be: the link goes, what it points at stays.
            try { [IO.Directory]::Delete($full, $false); $removed++ } catch { $left++; $locked++ }
            continue
        }
        $n = Remove-TreeNoFollow $full
        if ($n -eq 0) { $removed++ } else { $left++; $locked += $n }
    }
    return "OK removed=$removed left=$left locked=$locked"
}

# A check's answer: the program a target resolved to (for launchr), or why it cannot run hidden.
function Format-Check($r) {
    if ($r.Error) { return "ERR $($r.Error)" }
    return "OK`t" + $r.Target + "`t" + $r.Args + "`t" + $r.WorkDir + "`t" + [int]$r.OwnProfile + "`t" + $r.Base + "`t" + $r.Launcher
}

function Resolve-Launch([string]$target, [string]$argLine) {
    $p = Resolve-Program $target $argLine
    if ($p.Error) { return $p }
    return Complete-Launch $p
}

# Everything said to Decurix goes through Native.Reply, which keeps an answer from landing in the middle of a picture.
function Reply([string]$text) { [Native]::Reply($text) }

# The commands that need the functions above; every other is answered by Native.Serve itself, without PowerShell.
$Commands = [Action[string, string]] {
    param([string]$verb, [string]$rest)
    switch ($verb) {
        "desktop" {
            if ($rest -match '^[A-Za-z0-9_]{1,48}$') { $script:DesktopName = $rest; Reply "OK" } else { Reply "ERR invalid desktop name" }
        }
        "attach" {
            # Mirror mode: show and drive a window the person already has open (their own Chrome, say); starts nothing.
            $f = $rest.Split("`t")
            Reply ([Native]::Attach($f[0], $(if ($f.Count -gt 1) { $f[1] } else { '' })))
        }
        "launch" {
            $f = $rest.Split("`t")
            $r = Resolve-Launch $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
            if ($r.Error) { Reply "ERR $($r.Error)" }
            else { Reply (Start-Resolved $r) }
        }
        "check" {
            $f = $rest.Split("`t")
            Reply (Format-Check (Resolve-Program $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })))
        }
        "launchr" {
            # What a check already worked out: nothing is looked up, so this is as fast as starting the program.
            $f = $rest.Split("`t")
            if ($f.Count -lt 5) { Reply "ERR malformed launchr" }
            elseif (-not (Test-Path -LiteralPath $f[0] -PathType Leaf)) { Reply "ERR Not found on this computer: $($f[0])" }
            else {
                $l = Complete-Launch @{ Target = $f[0]; Args = $f[1]; WorkDir = $f[2]; OwnProfile = ($f[3] -eq '1'); Base = $f[4]; Launcher = $(if ($f.Count -gt 5) { $f[5] } else { '' }) }
                Reply (Start-Resolved $l)
            }
        }
        "pid" {
            # Which process this helper is on the host: the control plane ends exactly this tree when it stops it.
            Reply "OK $PID"
        }
        "dropprofiles" {
            # Asked of a helper with no app of its own, after the VM's apps were ended (Manager.DeleteDesktop).
            Reply (Remove-DesktopProfiles $rest.Split("`t"))
        }
        default { Reply "ERR unknown command" }
    }
}

[Native]::Init()
try {
    [Native]::Serve($Commands)
} finally {
    [Native]::Shutdown()
}
