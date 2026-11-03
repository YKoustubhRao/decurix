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
#   open <target> <args>     start one more app on the desktop a launch made (a worker's screen), resolved as launch does
#   openr <exe> <args> <dir> [sandbox <folder>]   start a control-plane-owned built-in, with no discovery
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
#   tree                     "TREE <fresh 0|1> <base64 JSON>": the app's controls read through UI Automation, in the
#                            picture's pixels ({w, h, title, nodes: [{role, name, box, value, password, checked, ...}]}),
#                            from the last read; fresh says that read began after the latest input. Starts the next read.
#   quit                     end the app and exit (also happens when stdin closes)
#
# Between answers the helper also says, on its own:
#   FRAME <W> <H> <x> <y> <w> <h> <whole 0|1> <n>   then n raw JPEG bytes: the picture is W x H and this JPEG is its
#                            rectangle x,y,w,h (the whole picture when <whole> is 1). Sent only when something changed.
#   EVENT closed             the app has no window left
#   EVENT open <id>          a shortcut on the wallpaper was double-clicked
# -Build and -Resolve are for the helper when it runs as a program of its own (see "This class as a program" below):
# -Build <path> compiles that program and ends; -Resolve <request> answers one question of it and ends.
# -DecurixInstance is the tag of the control plane that started this helper. The helper never reads it: it is there so
# that control plane's sweeps end only its own helpers, never another instance's (internal/httpapi/hostinstance_h.go).
param([Parameter(Mandatory = $true)][string]$DesktopName, [string]$Build = '', [string]$Resolve = '', [string]$DecurixInstance = '')

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

    public static string Launch(string desktopName, string exe, string args, string workDir) {
        ownDesktop = desktopName; // also used when this class is hosted by PowerShell
        desktop = CreateDesktop(desktopName, IntPtr.Zero, IntPtr.Zero, 0, 0x10000000, IntPtr.Zero);
        if (desktop == IntPtr.Zero) return "ERR CreateDesktop failed (" + Marshal.GetLastWin32Error() + ")";
        IntPtr process;
        // A worker's desktop is started for its agent, which has no window; a host app is started to be looked at.
        string said = Spawn(desktopName, exe, args, workDir, out process, fixedScreen);
        if (process != IntPtr.Zero) launched = process;
        return said;
    }

    // One more program on the desktop Launch already made: a person opening an app on a worker's screen. It is in the
    // same job, so it ends with the desktop, but it is not what the session was started for - Status still speaks of that.
    public static string Open(string desktopName, string exe, string args, string workDir) { return Open(desktopName, exe, args, workDir, null); }

    // sandbox is the one folder the program may work in (see Confined), or null for a program that runs as the person.
    public static string Open(string desktopName, string exe, string args, string workDir, string sandbox) {
        if (desktop == IntPtr.Zero) return "ERR this desktop has not been started";
        IntPtr process;
        string said = string.IsNullOrEmpty(sandbox) ? Spawn(desktopName, exe, args, workDir, out process, false)
                                                    : Confined(desktopName, exe, args, sandbox, out process);
        if (process != IntPtr.Zero) CloseHandle(process);
        Kick();
        return said;
    }

    // How many windows a person would see on the desktop: 0 is an empty screen.
    public static int WindowCount() { return AppWindows().Count; }

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

    // ---- A program confined to the worker. Everything else on a worker's screen runs as the person whose computer
    // this is: their PowerPoint, their browser. A terminal must not - a shell on the worker's screen that is the
    // person's own shell hands their whole computer to whoever can see that screen (it is reachable from a phone), and
    // to anything typed into it. So it is started in an AppContainer made for the worker: Windows' own sandbox, the one
    // browsers put their renderers in, which needs no administrator and no feature switched on. Measured on this
    // computer, for cmd and PowerShell 5.1: the person's profile can be neither read nor written ("Access is denied"),
    // the control plane on 127.0.0.1 cannot be reached (Windows refuses a container loopback), the only processes
    // seen are its own, the internet is there (the one capability given), and the folder it is given can be written.
    // Its home, temp and settings folders are the container's own.
    [StructLayout(LayoutKind.Sequential)] struct STARTUPINFOEX { public STARTUPINFO si; public IntPtr lpAttributeList; }
    [StructLayout(LayoutKind.Sequential)] struct SID_AND_ATTRIBUTES { public IntPtr Sid; public uint Attributes; }
    [StructLayout(LayoutKind.Sequential)] struct SECURITY_CAPABILITIES { public IntPtr AppContainerSid, Capabilities; public uint CapabilityCount, Reserved; }
    [StructLayout(LayoutKind.Sequential)] struct TRUSTEE { public IntPtr pMultipleTrustee; public int MultipleTrusteeOperation, TrusteeForm, TrusteeType; public IntPtr ptstrName; }
    [StructLayout(LayoutKind.Sequential)] struct EXPLICIT_ACCESS { public uint grfAccessPermissions; public int grfAccessMode; public uint grfInheritance; public TRUSTEE Trustee; }
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] static extern int CreateAppContainerProfile(string name, string display, string description, IntPtr capabilities, uint count, out IntPtr sid);
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] static extern int DeriveAppContainerSidFromAppContainerName(string name, out IntPtr sid);
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] static extern int GetAppContainerFolderPath(string sid, out IntPtr path);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode)] static extern bool ConvertSidToStringSid(IntPtr sid, out IntPtr text);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode)] static extern bool ConvertStringSidToSid(string text, out IntPtr sid);
    [DllImport("advapi32.dll")] static extern uint GetSecurityInfo(IntPtr handle, int type, uint info, IntPtr owner, IntPtr group, out IntPtr dacl, IntPtr sacl, out IntPtr descriptor);
    [DllImport("advapi32.dll")] static extern uint SetSecurityInfo(IntPtr handle, int type, uint info, IntPtr owner, IntPtr group, IntPtr dacl, IntPtr sacl);
    [DllImport("advapi32.dll")] static extern uint SetEntriesInAcl(int count, ref EXPLICIT_ACCESS entries, IntPtr old, out IntPtr made);
    [DllImport("kernel32.dll")] static extern IntPtr LocalFree(IntPtr mem);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool InitializeProcThreadAttributeList(IntPtr list, int count, int flags, ref IntPtr size);
    [DllImport("kernel32.dll", SetLastError = true)] static extern bool UpdateProcThreadAttribute(IntPtr list, uint flags, IntPtr attribute, IntPtr value, IntPtr size, IntPtr previous, IntPtr returned);
    [DllImport("kernel32.dll")] static extern void DeleteProcThreadAttributeList(IntPtr list);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true, EntryPoint = "CreateProcessW")] static extern bool CreateProcessEx(string app, string cmd, IntPtr pa, IntPtr ta, bool inh, int fl, IntPtr env, string dir, ref STARTUPINFOEX si, out PROCESS_INFORMATION pi);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] static extern uint GetLongPathName(string path, StringBuilder full, uint size);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern IntPtr OpenWindowStation(string name, bool inherit, uint access);
    [DllImport("user32.dll")] static extern bool CloseWindowStation(IntPtr station);

    // Lets the container use a window station or desktop: without it nothing in the container can have a window there.
    static void Admit(IntPtr handle, IntPtr sid, uint rights) {
        if (handle == IntPtr.Zero) return;
        IntPtr dacl, descriptor, made;
        if (GetSecurityInfo(handle, 7 /* SE_WINDOW_OBJECT */, 4 /* DACL */, IntPtr.Zero, IntPtr.Zero, out dacl, IntPtr.Zero, out descriptor) != 0) return;
        var entry = new EXPLICIT_ACCESS { grfAccessPermissions = rights, grfAccessMode = 1 /* GRANT_ACCESS */, Trustee = new TRUSTEE { TrusteeForm = 0 /* a SID */, TrusteeType = 5, ptstrName = sid } };
        if (SetEntriesInAcl(1, ref entry, dacl, out made) == 0) { SetSecurityInfo(handle, 7, 4, IntPtr.Zero, IntPtr.Zero, made, IntPtr.Zero); LocalFree(made); }
        LocalFree(descriptor);
    }

    // What the confined program is told about the computer: where Windows is and how to find its programs, and
    // nothing this helper was told - a worker's own settings and addresses are not a terminal's to read.
    static readonly string[] KeptVariables = { "SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "OS", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
        "PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL", "PROCESSOR_REVISION", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "CommonProgramFiles",
        "CommonProgramFiles(x86)", "CommonProgramW6432", "ProgramData", "ALLUSERSPROFILE", "PUBLIC", "COMPUTERNAME", "DriverData" };

    static string Confined(string desktopName, string exe, string args, string folder, out IntPtr process) {
        process = IntPtr.Zero;
        string name = ("Decurix.Worker." + desktopName).Replace('_', '-');
        if (name.Length > 64) name = name.Substring(0, 64);
        IntPtr sid;
        if (CreateAppContainerProfile(name, name, "A Decurix worker's own terminal", IntPtr.Zero, 0, out sid) != 0
            && DeriveAppContainerSidFromAppContainerName(name, out sid) != 0)
            return "ERR this computer could not make a sandbox for the worker's terminal";
        IntPtr text, path;
        if (!ConvertSidToStringSid(sid, out text)) return "ERR this computer could not make a sandbox for the worker's terminal";
        string sidText = Marshal.PtrToStringUni(text); LocalFree(text);
        string own = GetAppContainerFolderPath(sidText, out path) == 0 ? Marshal.PtrToStringUni(path) : null;
        if (string.IsNullOrEmpty(own)) return "ERR this computer could not make a sandbox for the worker's terminal";

        // The one folder of the person's disk it may work in. By its full name: a shortened one ("ADMINI~1") can only
        // be made sense of by reading the folders above it, which is exactly what the container may not do (measured:
        // PowerShell started in such a folder could not write a file into it).
        try {
            System.IO.Directory.CreateDirectory(folder);
            var full = new StringBuilder(1024);
            uint got = GetLongPathName(folder, full, (uint)full.Capacity);
            if (got > 0 && got < full.Capacity) folder = full.ToString();
            var who = new System.Security.Principal.SecurityIdentifier(sidText);
            var acl = System.IO.Directory.GetAccessControl(folder);
            bool admitted = false;
            foreach (System.Security.AccessControl.FileSystemAccessRule rule in acl.GetAccessRules(true, false, typeof(System.Security.Principal.SecurityIdentifier)))
                if (rule.IdentityReference.Equals(who)) admitted = true;
            if (!admitted) {
                acl.AddAccessRule(new System.Security.AccessControl.FileSystemAccessRule(who, System.Security.AccessControl.FileSystemRights.Modify,
                    System.Security.AccessControl.InheritanceFlags.ContainerInherit | System.Security.AccessControl.InheritanceFlags.ObjectInherit,
                    System.Security.AccessControl.PropagationFlags.None, System.Security.AccessControl.AccessControlType.Allow));
                System.IO.Directory.SetAccessControl(folder, acl);
            }
        } catch (Exception e) { return "ERR the worker's folder could not be opened to its terminal: " + e.Message; }

        // Its windows: on this worker's desktop, of this window station.
        Admit(desktop, sid, 0x000F01FF /* DESKTOP_ALL_ACCESS */);
        IntPtr station = OpenWindowStation("WinSta0", false, 0x00060000 /* READ_CONTROL | WRITE_DAC */);
        Admit(station, sid, 0x0002037F /* WINSTA_ALL_ACCESS less its own security */);
        if (station != IntPtr.Zero) CloseWindowStation(station);

        var vars = new List<string>();
        foreach (string keep in KeptVariables) { string v = Environment.GetEnvironmentVariable(keep); if (!string.IsNullOrEmpty(v)) vars.Add(keep + "=" + v); }
        string sys = Environment.SystemDirectory, win = Environment.GetEnvironmentVariable("SystemRoot") ?? @"C:\Windows";
        string temp = System.IO.Path.Combine(own, "Temp");
        vars.Add("Path=" + sys + ";" + win + ";" + System.IO.Path.Combine(sys, "Wbem") + ";" + System.IO.Path.Combine(sys, @"WindowsPowerShell\v1.0") + ";" + System.IO.Path.Combine(sys, "OpenSSH"));
        vars.Add("USERPROFILE=" + own); vars.Add("HOME=" + own); vars.Add("HOMEDRIVE=" + System.IO.Path.GetPathRoot(own).TrimEnd('\\')); vars.Add("HOMEPATH=" + own.Substring(2));
        vars.Add("APPDATA=" + own); vars.Add("LOCALAPPDATA=" + own); vars.Add("TEMP=" + temp); vars.Add("TMP=" + temp);
        vars.Add("USERNAME=worker"); vars.Add("DECURIX_SANDBOX=1");
        vars.Sort(StringComparer.OrdinalIgnoreCase);
        IntPtr env = Marshal.StringToHGlobalUni(string.Join("\0", vars) + "\0\0");

        IntPtr internet; ConvertStringSidToSid("S-1-15-3-1" /* internetClient: out to the internet, nothing else */, out internet);
        IntPtr capability = Marshal.AllocHGlobal(Marshal.SizeOf(typeof(SID_AND_ATTRIBUTES)));
        Marshal.StructureToPtr(new SID_AND_ATTRIBUTES { Sid = internet, Attributes = 4 /* SE_GROUP_ENABLED */ }, capability, false);
        var caps = new SECURITY_CAPABILITIES { AppContainerSid = sid, Capabilities = capability, CapabilityCount = 1 };
        IntPtr capsAt = Marshal.AllocHGlobal(Marshal.SizeOf(caps)); Marshal.StructureToPtr(caps, capsAt, false);
        IntPtr size = IntPtr.Zero; InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0, ref size);
        IntPtr list = Marshal.AllocHGlobal(size);
        try {
            if (!InitializeProcThreadAttributeList(list, 1, 0, ref size)
                || !UpdateProcThreadAttribute(list, 0, (IntPtr)0x00020009 /* PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES */, capsAt, (IntPtr)Marshal.SizeOf(caps), IntPtr.Zero, IntPtr.Zero))
                return "ERR this computer could not make a sandbox for the worker's terminal (" + Marshal.GetLastWin32Error() + ")";
            var six = new STARTUPINFOEX();
            six.si.cb = Marshal.SizeOf(six);
            six.si.lpDesktop = desktopName;
            six.si.lpTitle = System.IO.Path.GetFileNameWithoutExtension(exe);
            six.lpAttributeList = list;
            PROCESS_INFORMATION pi;
            string cmd = "\"" + exe + "\"" + (string.IsNullOrEmpty(args) ? "" : " " + args);
            int flags = 0x400 /* CREATE_UNICODE_ENVIRONMENT */ | 0x00080000 /* EXTENDED_STARTUPINFO_PRESENT */ | 0x10 /* CREATE_NEW_CONSOLE */;
            bool started = CreateProcessEx(exe, cmd, IntPtr.Zero, IntPtr.Zero, false, flags, env, folder, ref six, out pi);
            int err = Marshal.GetLastWin32Error();
            DeleteProcThreadAttributeList(list);
            if (!started) return "ERR the worker's terminal could not be started in its sandbox (" + err + ")";
            AssignProcessToJobObject(job, pi.hProcess);
            CloseHandle(pi.hThread);
            process = pi.hProcess;
            return "OK pid=" + pi.dwProcessId + " sandbox=" + sidText;
        } finally { Marshal.FreeHGlobal(list); Marshal.FreeHGlobal(capsAt); Marshal.FreeHGlobal(capability); Marshal.FreeHGlobal(env); LocalFree(internet); }
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
        bool started = CreateProcess(app, cmd, IntPtr.Zero, IntPtr.Zero, false, flags, env, string.IsNullOrEmpty(workDir) ? null : workDir, ref si, out pi);
        int err = Marshal.GetLastWin32Error();
        Marshal.FreeHGlobal(env);
        if (!started) return "ERR CreateProcess failed (" + err + ")";
        AssignProcessToJobObject(job, pi.hProcess);
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
            try {
                if (!Answer(verb, rest)) other(verb, rest);
            } catch (Exception e) {
                // What PowerShell throws around a .NET failure says less than the failure itself.
                while (e.InnerException != null && (e is System.Reflection.TargetInvocationException || e.GetType().Name.EndsWith("MethodInvocationException"))) e = e.InnerException;
                Reply("ERR " + e.Message);
            }
            if (verb == "move") Nudge();
            else if (verb == "down" || verb == "up" || verb == "click" || verb == "wheel" || verb == "type" || verb == "newline" || verb == "key" || verb == "copy" || verb == "choose" || verb == "press") { NoteInput(); Kick(); }
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
            case "openr":
                p = rest.Split('\t');
                if (p.Length < 3 || !System.IO.File.Exists(p[0])) { Reply("ERR built-in program not found"); return true; }
                if (p.Length > 3 && (p.Length != 5 || p[3] != "sandbox")) { Reply("ERR invalid built-in sandbox"); return true; }
                OpenBehind(new[] { p[0], p[1], p.Length > 3 ? p[3] : "", p.Length > 4 ? p[4] : "" },
                    new[] { "OK", p[0], p[1], p[2], "0", "builtin", "" });
                return true;
            case "move": case "down": case "up": Reply(Pointer(verb, rest, true)); return true;
            case "click": Pointer("down", rest, false); Thread.Sleep(30); Reply(Pointer("up", rest, false)); return true;
            case "wheel": p = rest.Split(' '); Reply(Wheel(Int(p[0]), Int(p[1]), Int(p[2]))); return true;
            case "type": foreach (char c in rest) TypeChar(c); Reply("OK"); return true;
            case "newline": TypeNewline(); Reply("OK"); return true;
            case "keyed": KeyedText = rest.Trim() == "1"; Reply("OK"); return true;
            case "key": p = rest.Split(' '); Reply(Key(Int(p[0]), p.Length > 1 ? p[1] : "")); return true;
            case "copy": Reply(Copy(Int(rest))); return true;
            case "count": Reply("COUNT " + WindowCount()); return true;
            case "shortcuts": p = rest.Split(new[] { '\t' }, 2); SetShortcuts(p[0], p.Length > 1 ? p[1] : ""); Reply("OK"); return true;
            case "tasks": Reply("TASKS " + Tasks()); return true;
            case "start": ToggleStart(); Reply("OK"); return true;
            case "shell": Reply("SHELL " + (ShellTyping() ? 1 : 0)); return true;
            case "win": p = rest.Split(' '); Reply(WindowDo(p[0], long.Parse(p[1]))); return true;
            case "over": p = rest.Split(' '); Reply("OVER " + Over(Int(p[0]), Int(p[1]))); return true;
            case "screen": FixedScreen(); Reply("OK"); return true;
            case "viewers": SetViewers(rest.Trim().Length == 0 ? 0 : Int(rest)); Reply("OK"); return true;
            case "keyframe": KeyFrame(); Reply("OK"); return true;
            case "thumb": Thumb(Int(rest)); return true;
            case "tree": Reply(Tree()); return true;
            case "setfocus": p = rest.Split(' '); FocusAt(Int(p[0]), Int(p[1])); Reply("OK"); return true;
            case "setvalue": p = rest.Split(new[] { ' ' }, 4); Reply(SetValueAt(Int(p[0]), Int(p[1]), p[2] == "add", p.Length > 3 ? p[3] : "")); return true;
            case "press": p = rest.Split(' '); Reply(Press(Int(p[0]), Int(p[1]))); return true;
            case "choose": p = rest.Split(new[] { ' ' }, 3); Reply(Choose(Int(p[0]), Int(p[1]), p.Length > 2 ? p[2] : "")); return true;
            case "status": Reply("STATUS " + Status()); return true;
            case "focus": Reply("FOCUS " + FocusInfo()); return true;
            case "windows": Reply("WINDOWS " + Windows()); return true;
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

    // ---- This class as a program of its own. PowerShell compiles it once (-Build) and Decurix then starts the program
    // itself for each worker's desktop: a PowerShell kept running to host this class was 58 MB of every worker's memory
    // for an interpreter with nothing left to interpret (measured: 76 MB private for the hosted helper, about a third
    // of that for the program). The script is still what knows how to find a program - a Start-menu entry, a shortcut,
    // a browser that needs a profile of its own - so for that, and only then, the program runs the script once
    // (-Resolve) and reads its answer. What a target turned out to be is remembered, so a program with nothing to
    // prepare (PowerPoint, a terminal) is found once and afterwards started with no PowerShell at all.
    static string ownDesktop = "", script = "";
    static readonly Dictionary<string, string[]> found = new Dictionary<string, string[]>();

    [STAThread]
    public static int Main(string[] args) {
        if (args.Length == 1 && args[0] == "--selftest") { Init(); Reply("OK"); return 0; }
        for (int i = 0; i + 1 < args.Length; i += 2) {
            if (args[i] == "-DesktopName") ownDesktop = args[i + 1];
            if (args[i] == "-Script") script = args[i + 1];
        }
        if (ownDesktop.Length == 0 || script.Length == 0) return 2;
        Init();
        try { Serve(Starting); } finally { Shutdown(); }
        return 0;
    }

    // One question put to the script: its answer, a line a message.
    static List<string> Ask(string verb, string rest) {
        var said = new List<string>();
        string request = Convert.ToBase64String(Encoding.UTF8.GetBytes(verb + "\t" + rest));
        var start = new ProcessStartInfo {
            FileName = System.IO.Path.Combine(Environment.SystemDirectory, @"WindowsPowerShell\v1.0\powershell.exe"),
            Arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File \"" + script + "\" -DesktopName " + ownDesktop + " -Resolve " + request,
            UseShellExecute = false, CreateNoWindow = true,
            // All three are its own: it must never be given this program's, which are the line protocol with Decurix.
            RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true
        };
        using (var ps = Process.Start(start)) {
            ps.StandardInput.Close();
            ps.ErrorDataReceived += (o, e) => { };
            ps.BeginErrorReadLine();
            string line;
            while ((line = ps.StandardOutput.ReadLine()) != null) {
                try { said.Add(Encoding.UTF8.GetString(Convert.FromBase64String(line.Trim()))); } catch (FormatException) { }
            }
            ps.WaitForExit();
        }
        if (said.Count == 0) said.Add("ERR the helper's script gave no answer");
        return said;
    }

    // What a target is: { "OK", program, arguments, folder, own profile (1/0), name, launcher } or { "ERR", why }.
    static string[] Found(string target, string args) {
        string key = target + "\t" + args;
        string[] r;
        lock (found) if (found.TryGetValue(key, out r)) return r;
        var said = Ask("check", key);
        string last = said[said.Count - 1];
        if (last.StartsWith("OK\t")) {
            r = new string[7];
            string[] f = last.Split('\t');
            for (int i = 0; i < 7; i++) r[i] = i < f.Length ? f[i] : "";
        } else r = new[] { "ERR", last.StartsWith("ERR ") ? last.Substring(4) : last };
        lock (found) found[key] = r;
        return r;
    }

    // Starts a program that has been found, after whatever it needs first: a browser's own profile, a web app's server.
    static readonly Dictionary<string, string[]> prepared = new Dictionary<string, string[]>();
    static string Run(bool open, string exe, string args, string dir, bool ownProfile, string name, string launcher) {
        string key = exe + "\t" + args + "\t" + dir + "\t" + name;
        string[] cached = null;
        // A browser's imported profile and arguments remain valid for this desktop.
        // Web app launchers still run their readiness check on every open.
        if (ownProfile && launcher.Length == 0) lock (prepared) prepared.TryGetValue(key, out cached);
        if (cached != null) { exe = cached[0]; args = cached[1]; dir = cached[2]; }
        else if (ownProfile || launcher.Length > 0) {
            bool ready = false;
            foreach (string line in Ask("complete", string.Join("\t", exe, args, dir, ownProfile ? "1" : "0", name, launcher))) {
                string[] f = line.Split('\t');
                if (f[0] == "PREP" && f.Length >= 3) {
                    string status = Prepare(ownDesktop, f[1], f[2]);
                    if (status.StartsWith("ERR ")) return status;
                }
                else if (f[0] == "RUN" && f.Length >= 4) { exe = f[1]; args = f[2]; dir = f[3]; ready = true; }
                else if (line.StartsWith("ERR ")) return line;
            }
            if (!ready) return "ERR the program could not be prepared";
            if (ownProfile && launcher.Length == 0) lock (prepared) prepared[key] = new[] { exe, args, dir };
        }
        return open ? Open(ownDesktop, exe, args, dir) : Launch(ownDesktop, exe, args, dir);
    }

    // Opening a program can take a while - finding what a Start-menu entry is (seconds), bringing a browser its
    // profile (a minute, the first time), waiting for a web app's own server (two) - and this loop is what every click
    // and key on the worker's screen goes through. Found live: opening an application froze the whole screen for half a
    // minute. So an open is answered at once with "WAIT <n>" and done on a thread of its own, one at a time; how it
    // ended is said when it has: "EVENT opened <n> <answer>".
    static int tickets;
    static readonly object opening = new object();

    static void OpenBehind(string[] f) { OpenBehind(f, null); }
    static void OpenBehind(string[] f, string[] resolved) {
        int ticket = Interlocked.Increment(ref tickets);
        Interlocked.Increment(ref opensGoing);
        Reply("WAIT " + ticket);
        var th = new Thread(() => {
            string said;
            try {
                lock (opening) {
                    string[] r = resolved ?? Found(f[0], f.Length > 1 ? f[1] : "");
                    if (r[0] == "ERR") said = "ERR " + r[1];
                    // "open <target> <arguments> sandbox <folder>": confined to that folder, and so nothing to prepare.
                    else if (f.Length > 3 && f[2] == "sandbox") said = Open(ownDesktop, r[1], r[2], r[3], f[3]);
                    else said = Run(true, r[1], r[2], r[3], r[4] == "1", r[5], r[6]);
                }
            } catch (Exception e) { said = "ERR " + e.Message; }
            Interlocked.Decrement(ref opensGoing);
            Reply("EVENT opened " + ticket + " " + said.Replace('\r', ' ').Replace('\n', ' '));
            Kick();
        }) { IsBackground = true };
        th.SetApartmentState(ApartmentState.STA);
        th.Start();
    }

    // The commands the script answers when it hosts this class ($Commands), answered through it here.
    static void Starting(string verb, string rest) {
        string[] f = rest.Split('\t');
        string[] r;
        switch (verb) {
            case "desktop":
                if (System.Text.RegularExpressions.Regex.IsMatch(rest, "^[A-Za-z0-9_]{1,48}$")) { ownDesktop = rest; Reply("OK"); }
                else Reply("ERR invalid desktop name");
                return;
            case "check":
                r = Found(f[0], f.Length > 1 ? f[1] : "");
                Reply(r[0] == "ERR" ? "ERR " + r[1] : "OK\t" + string.Join("\t", r, 1, 6));
                return;
            case "open": OpenBehind(f); return;
            case "launch":
                r = Found(f[0], f.Length > 1 ? f[1] : "");
                if (r[0] == "ERR") { Reply("ERR " + r[1]); return; }
                Reply(Run(false, r[1], r[2], r[3], r[4] == "1", r[5], r[6]));
                return;
            case "launchr":
                // What a check already worked out: nothing is looked up, so this is as fast as starting the program.
                if (f.Length < 5) { Reply("ERR malformed launchr"); return; }
                if (!System.IO.File.Exists(f[0])) { Reply("ERR Not found on this computer: " + f[0]); return; }
                // A web app's launcher still starts its server first (Run), as a launch through check does.
                Reply(Run(false, f[0], f[1], f[2], f[3] == "1", f[4], f.Length > 5 ? f[5] : ""));
                return;
            default: Reply("ERR unknown command"); return;
        }
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
    static Surface canvas, wallpaper;
    static Bitmap wallpaperOf;
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
        if (fixedScreen) lock (deskLock) {
            Bitmap drawn = Backdrop(pw, ph); // kept until the shortcuts change, and so is its copy here
            if (wallpaper == null || wallpaperOf != drawn || wallpaper.W != pw || wallpaper.H != ph) {
                if (wallpaper != null) wallpaper.Free();
                wallpaper = new Surface(pw, ph); wallpaperOf = drawn;
                using (var wg = Graphics.FromImage(wallpaper.View)) wg.DrawImageUnscaled(drawn, 0, 0);
            }
            BitBlt(canvas.Dc, 0, 0, pw, ph, wallpaper.Dc, 0, 0, Copy);
        }
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
            if (!fixedScreen) { g.Clear(Color.FromArgb(0x1b, 0x1b, 0x1b)); g.Flush(FlushIntention.Sync); }
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
                    bool lowest = i == ws.Count - 1 && !fixedScreen; // nothing is under the lowest window of an app's picture
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
                    if (fixedScreen && w.H == under) lock (deskLock) DrawCaptionFeedback(g, w.H, l, t, sc);
                }
            }
            if (fixedScreen) lock (deskLock) { DrawShell(g); DrawOpening(g, taskCount); DrawCaptionTip(g, under); }
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
    // screen, the last one sent, the wallpaper, every window: about 35 MB for a 1920x1080 screen with two apps) are
    // given back, and so is the memory this process is not using. The next look makes them again.
    [DllImport("kernel32.dll")] static extern IntPtr GetCurrentProcess();
    [DllImport("kernel32.dll")] static extern bool SetProcessWorkingSetSize(IntPtr process, IntPtr min, IntPtr max);
    static long lookedAt;

    static void Rest() {
        lock (composeLock) {
            if (canvas != null) { canvas.Free(); canvas = null; }
            if (wallpaper != null) { wallpaper.Free(); wallpaper = null; wallpaperOf = null; }
            if (small != null) { small.Free(); small = null; }
            foreach (var kept in printed.Values) kept.Free();
            printed.Clear(); printedAt.Clear();
            if (prev != IntPtr.Zero) { Marshal.FreeHGlobal(prev); prev = IntPtr.Zero; prevBytes = 0; }
            if (smallPrev != IntPtr.Zero) { Marshal.FreeHGlobal(smallPrev); smallPrev = IntPtr.Zero; }
            lock (deskLock) {
                if (backdrop != null) { backdrop.Dispose(); backdrop = null; }
                if (barBmp != null) { barBmp.Dispose(); barBmp = null; }
                if (menuBmp != null) { menuBmp.Dispose(); menuBmp = null; }
                if (calBmp != null) { calBmp.Dispose(); calBmp = null; }
                foreach (var icon in windowIcons.Values) if (icon != null) icon.Dispose();
                windowIcons.Clear();
            }
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
            if (openingName != null) delay = Math.Min(delay, 200);
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
        if (all.Count == 0 && !fixedScreen) return false;
        long best = 0;
        foreach (var w in all) {
            l = Math.Min(l, w.V.L); t = Math.Min(t, w.V.T); r = Math.Max(r, w.V.R); b = Math.Max(b, w.V.B);
            long a = (long)(w.V.R - w.V.L) * (w.V.B - w.V.T);
            if (a > best) { best = a; main = w.H; }
        }
        sc = Scale(main);
        if (fixedScreen) { l = 0; t = 0; r = screenW; b = screenH; sc = 1.0; }
        pw = (int)Math.Round((r - l) * sc); ph = (int)Math.Round((b - t) * sc);
        return pw > 0 && ph > 0 && pw <= 8192 && ph <= 8192;
    }
    static long inputSeq;
    static readonly object treeLock = new object();
    static string treeJson; static long treeSeq = -1; static bool treeWalking;
    static Thread uiaThread;
    static readonly System.Collections.Concurrent.BlockingCollection<Action> uiaWork = new System.Collections.Concurrent.BlockingCollection<Action>();
    const int MaxNodes = 900;

    public static void NoteInput() { Interlocked.Increment(ref inputSeq); }

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
    [DllImport("user32.dll")] static extern IntPtr GetAncestor(IntPtr h, uint flags);
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
    const int HoverStart = -1, HoverNone = -2, HoverSearch = -3, HoverLang = -4, HoverClock = -5, HoverPin = -100; // HoverPin - n is the n-th pinned app; barHover, else a window button's index
    const int MaxQuery = 80;

    static int barH; static List<Rectangle> pinRects = new List<Rectangle>(); static List<Shortcut> pinned = new List<Shortcut>(); static Rectangle barRect, startRect, searchRect, langRect, clockRect, menuRect, calRect, calPrev, calNext;
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
        if (windowIcons.Count > 64) {
            foreach (var icon in windowIcons.Values) if (icon != null) icon.Dispose();
            windowIcons.Clear();
        }
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
        if (open[0] == WebMark) Reply("EVENT web " + open.Substring(1)); else { Opening(open); Reply("EVENT open " + open); }
    }

    // "Opening PowerPoint..." over the taskbar, from the moment a shortcut is opened until its window is there. A
    // program takes seconds to show its first window, and a web app's server can take a minute to answer; with
    // nothing on the screen to say the click was taken, people click again - and get the app twice.
    static string openingName; static long openingAt; static int openingWindows, opensGoing;

    static void Opening(string id) {
        string name = null;
        lock (deskLock) foreach (var sc in shortcuts) if (sc.Id == id) name = sc.Name;
        if (name == null) return;
        var list = TaskWindows();
        openingWindows = list.Count; openingAt = Stopwatch.GetTimestamp(); openingName = name;
        Kick();
    }

    // Called with deskLock held, when the taskbar is drawn; windows is how many it has a button for.
    static void DrawOpening(Graphics g, int windows) {
        string name = openingName;
        if (name == null) return;
        double waited = (Stopwatch.GetTimestamp() - openingAt) / (double)Stopwatch.Frequency;
        if (opensGoing == 0 && ((windows > openingWindows && waited > 0.4) || waited > 25) || waited > 300) { openingName = null; return; }
        float s = (float)Dpi();
        using (var font = new Font("Segoe UI", 13f * s, GraphicsUnit.Pixel))
        using (var centre = new StringFormat { Alignment = StringAlignment.Center, LineAlignment = StringAlignment.Center, FormatFlags = StringFormatFlags.NoWrap }) {
            string text = "Opening " + name + "...";
            SizeF size = g.MeasureString(text, font);
            float w = size.Width + 30 * s, h = 34 * s;
            var box = new RectangleF((screenW - w) / 2f, barRect.Y - h - 14 * s, w, h);
            var smooth = g.SmoothingMode; g.SmoothingMode = SmoothingMode.AntiAlias;
            using (var shape = Rounded(box, 9 * s)) using (var fill = new SolidBrush(Color.FromArgb(238, 30, 30, 34))) using (var rim = new Pen(Color.FromArgb(90, 255, 255, 255))) { g.FillPath(fill, shape); g.DrawPath(rim, shape); }
            using (var ink = new SolidBrush(Color.White)) g.DrawString(text, font, ink, box, centre);
            g.SmoothingMode = smooth;
        }
    }

    static void OpenMenu() { menuOpen = true; calOpen = false; query = ""; menuSel = -1; menuHover = -1; }

    // Draws the taskbar (and the Start panel or the calendar while one is open) over the picture. Each is kept as a
    // bitmap until what it shows changes, so a capture only copies it. Called with deskLock held.
    static int taskCount;

    static void DrawShell(Graphics g) {
        if (barH == 0) LayoutBar();
        float s = (float)Dpi();
        ReadLanguage();

        var list = TaskWindows();
        taskCount = list.Count;
        var key = new StringBuilder();
        var titles = new List<string>(); var mins = new List<bool>();
        foreach (IntPtr h in list) {
            var title = new StringBuilder(200); GetWindowText(h, title, 200);
            titles.Add(title.ToString()); mins.Add(IsIconic(h));
            key.Append(h.ToInt64()).Append(mins[mins.Count - 1] ? 'm' : '-').Append(title).Append('\n');
        }
        // The time and the date as this computer writes them.
        string clock = DateTime.Now.ToString("t"), day = DateTime.Now.ToString("d");
        pinned = shortcuts.FindAll(sc => sc.Id == "files" || sc.Id == "terminal");
        key.Append(clock).Append(day).Append(barHover).Append(menuOpen).Append(calOpen).Append(pinned.Count);

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
                    float total = cell + searchW + gap + pinned.Count * cell + list.Count * cell;
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

                    // Pinned beside Search: the worker's Files and Terminal, drawn here (a press opens that shortcut).
                    pinRects.Clear();
                    for (int pi = 0; pi < pinned.Count; pi++, x += cell) {
                        pinRects.Add(new Rectangle((int)x, barRect.Y, (int)cell, barRect.Height));
                        box = new RectangleF(x + pad, pad, tile, tile);
                        if (barHover == HoverPin - pi) using (var p = Rounded(box, 5 * s)) b.FillPath(hover, p);
                        if (pinned[pi].Id == "files") {
                            float fw = 22 * s, fh = 16 * s, fx = box.X + (tile - fw) / 2, fy = box.Y + (tile - fh) / 2 + 1 * s;
                            using (var back = new SolidBrush(Color.FromArgb(0xd9, 0xa4, 0x2c))) using (var front = new SolidBrush(Color.FromArgb(0xf6, 0xc8, 0x4f))) {
                                using (var p = Rounded(new RectangleF(fx, fy - 3 * s, fw * 0.45f, 6 * s), 2 * s)) b.FillPath(back, p);
                                using (var p = Rounded(new RectangleF(fx, fy, fw, fh), 2.5f * s)) b.FillPath(back, p);
                                using (var p = Rounded(new RectangleF(fx, fy + 3 * s, fw, fh - 3 * s), 2.5f * s)) b.FillPath(front, p);
                            }
                        } else {
                            // A terminal: a dark window with a prompt.
                            float tw2 = 22 * s, th2 = 16 * s, tx = box.X + (tile - tw2) / 2, ty = box.Y + (tile - th2) / 2;
                            using (var p = Rounded(new RectangleF(tx, ty, tw2, th2), 3 * s)) { b.FillPath(field, p); b.DrawPath(rim, p); }
                            using (var pen = new Pen(Ink, 1.6f * s)) {
                                b.DrawLines(pen, new[] { new PointF(tx + 5 * s, ty + 4.5f * s), new PointF(tx + 9 * s, ty + 8 * s), new PointF(tx + 5 * s, ty + 11.5f * s) });
                                b.DrawLine(pen, tx + 11 * s, ty + 11.5f * s, tx + 17 * s, ty + 11.5f * s);
                            }
                        }
                    }

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
            else if (barHover <= HoverPin && HoverPin - barHover < pinRects.Count) { int n = HoverPin - barHover; tipText = pinned[n].Name; tipAt = pinRects[n].X + pinRects[n].Width / 2f; }
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
            } else if (PinAt(p) >= 0) {
                if (!right) { open = pinned[PinAt(p)].Id; menuOpen = false; calOpen = false; }
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
            else if (PinAt(p) >= 0) bar = HoverPin - PinAt(p);
            else if (clockRect.Contains(p.X, p.Y)) bar = HoverClock;
            else if (langRect.Contains(p.X, p.Y)) bar = HoverLang;
            else if (barRect.Contains(p.X, p.Y)) { for (int i = 0; i < barButtons.Count; i++) if (barButtons[i].Box.Contains(p.X - barRect.X, p.Y - barRect.Y)) bar = i; }
            barHover = bar; menuHover = item;
            return inMenu || inCal || barRect.Contains(p.X, p.Y);
        }
    }

    // Which pinned app a point is on, or -1. Called with deskLock held.
    static int PinAt(POINT p) {
        for (int i = 0; i < pinRects.Count && i < pinned.Count; i++) if (pinRects[i].Contains(p.X, p.Y)) return i;
        return -1;
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
        if (open != null) { Opening(open); Reply("EVENT open " + open); }
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
    //
    // A frame that calls its whole top strip "title bar" may still have the three buttons in it. Found live with
    // PowerPoint and Excel: Office draws its title bar in a child window, and its frame says "title bar" for all of
    // it - so a press on Close began a drag that went nowhere, and the window could not be closed, minimised or
    // maximised from its own buttons. The child knows its buttons, but only says so while the real pointer is over
    // the same spot of the screen (measured: the same point was "close" in one run and "contents" in the next, and
    // always right for a maximised window) - and the real pointer is the person's, on their own desktop. So when the
    // child does not say, the buttons are where Windows itself places them for that window.
    static int HitTest(IntPtr top, POINT p) {
        RECT button; bool enabled;
        int caption = CaptionAt(top, p, out button, out enabled);
        if (caption != 0) return enabled ? caption : 1;
        double sc = Scale(top);
        int at = Ask(top, p, sc);
        if (at != 2) return at == NoAnswer ? 1 : at;
        IntPtr inner = Descend(top, p);
        if (inner != top) {
            int said = Ask(inner, p, sc);
            if (said == 8 || said == 9 || said == 20) return said;
        }
        RECT w, area;
        var bar = new TITLEBARINFO(); bar.cbSize = Marshal.SizeOf(typeof(TITLEBARINFO));
        if (!GetWindowRect(top, out w) || !GetTitleBarInfo(top, ref bar)) return 2;
        if (DwmGetWindowAttribute(top, 5 /* DWMWA_CAPTION_BUTTON_BOUNDS */, out area, Marshal.SizeOf(typeof(RECT))) != 0 || area.R <= area.L) return 2;
        int x = p.X - w.L, y = p.Y - w.T;
        if (x < area.L || x >= area.R || y < area.T || y >= area.B) return 2;
        // Left to right: minimise, maximise, close - those of them the window has (0x8000: not shown).
        var shown = new List<int>();
        if ((bar.rgstate[2] & 0x8000) == 0) shown.Add(8);
        if ((bar.rgstate[3] & 0x8000) == 0) shown.Add(9);
        if ((bar.rgstate[5] & 0x8000) == 0) shown.Add(20);
        if (shown.Count == 0) return 2;
        return shown[Math.Min(shown.Count - 1, (x - area.L) * shown.Count / (area.R - area.L))];
    }

    [StructLayout(LayoutKind.Sequential)] struct TITLEBARINFO { public int cbSize; public RECT rcTitleBar; [MarshalAs(UnmanagedType.ByValArray, SizeConst = 6)] public uint[] rgstate; }
    [DllImport("user32.dll")] static extern bool GetTitleBarInfo(IntPtr h, ref TITLEBARINFO info);

    [StructLayout(LayoutKind.Sequential)] struct TITLEBARINFOEX {
        public int cbSize; public RECT rcTitleBar;
        [MarshalAs(UnmanagedType.ByValArray, SizeConst = 6)] public uint[] rgstate;
        [MarshalAs(UnmanagedType.ByValArray, SizeConst = 6)] public RECT[] rgrect;
    }
    static bool Contains(RECT r, POINT p) { return p.X >= r.L && p.X < r.R && p.Y >= r.T && p.Y < r.B; }

    // Windows supplies the actual button rectangles and availability. This is
    // shared by painting and clicking, including dialogs with no min/max buttons.
    static int CaptionAt(IntPtr top, POINT p, out RECT box, out bool enabled) {
        box = new RECT(); enabled = false;
        RECT window, area;
        if (!GetWindowRect(top, out window)) return 0;
        bool dwm = DwmGetWindowAttribute(top, 5, out area, Marshal.SizeOf(typeof(RECT))) == 0 && area.R > area.L && area.B > area.T;
        if (dwm) {
            area.L += window.L; area.R += window.L; area.T += window.T; area.B += window.T;
            if (!Contains(area, p)) return 0; // no cross-process query over the app's contents
        } else if (p.Y < window.T || p.Y > window.T + 64 * Dpi()) return 0;
        int size = Marshal.SizeOf(typeof(TITLEBARINFOEX));
        IntPtr data = Marshal.AllocHGlobal(size);
        var info = new TITLEBARINFOEX { cbSize = size, rgstate = new uint[6], rgrect = new RECT[6] };
        try {
            Marshal.StructureToPtr(info, data, false);
            IntPtr answer;
            if (SendMessageTimeout(top, 0x033F /* WM_GETTITLEBARINFOEX */, IntPtr.Zero, data, 0x2, 40, out answer) != IntPtr.Zero) {
                info = (TITLEBARINFOEX)Marshal.PtrToStructure(data, typeof(TITLEBARINFOEX));
                foreach (int index in new[] { 2, 3, 5 }) {
                    RECT r = info.rgrect[index];
                    if ((info.rgstate[index] & 0x18000) != 0 || r.R <= r.L || r.B <= r.T || !Contains(r, p)) continue;
                    box = r; enabled = (info.rgstate[index] & 1) == 0;
                    return index == 2 ? 8 : index == 3 ? 9 : 20;
                }
            } else return 0; // a hung app must not hold up pointer input
        } finally { Marshal.FreeHGlobal(data); }
        // Custom frames (Office) may expose only the group's DWM rectangle.
        var bar = new TITLEBARINFO(); bar.cbSize = Marshal.SizeOf(typeof(TITLEBARINFO));
        if (!dwm || !GetTitleBarInfo(top, ref bar)) return 0;
        var shown = new List<int>();
        foreach (int index in new[] { 2, 3, 5 }) if ((bar.rgstate[index] & 0x18000) == 0) shown.Add(index);
        if (shown.Count == 0) return 0;
        int slot = Math.Min(shown.Count - 1, (p.X - area.L) * shown.Count / (area.R - area.L));
        int child = shown[slot];
        box = area; box.L = area.L + (area.R - area.L) * slot / shown.Count; box.R = area.L + (area.R - area.L) * (slot + 1) / shown.Count;
        enabled = (bar.rgstate[child] & 1) == 0;
        return child == 2 ? 8 : child == 3 ? 9 : 20;
    }

    const int NoAnswer = int.MinValue;

    // One window's own answer to "what part of you is this point" (WM_NCHITTEST), or NoAnswer from one that is busy.
    static int Ask(IntPtr w, POINT p, double sc) {
        int x = (int)Math.Round(p.X * sc), y = (int)Math.Round(p.Y * sc);
        IntPtr answer;
        if (SendMessageTimeout(w, 0x0084, IntPtr.Zero, (IntPtr)(((y & 0xFFFF) << 16) | (x & 0xFFFF)), 0x2, 200, out answer) == IntPtr.Zero) return NoAnswer;
        return (int)(long)answer;
    }

    static void Caption(IntPtr w, int hit) {
        int command = hit == 20 ? 0xF060 /* SC_CLOSE */ : hit == 8 ? 0xF020 /* SC_MINIMIZE */ : IsZoomed(w) ? 0xF120 /* SC_RESTORE */ : 0xF030 /* SC_MAXIMIZE */;
        PostMessage(w, 0x0112 /* WM_SYSCOMMAND */, (IntPtr)command, IntPtr.Zero);
    }

    static IntPtr captionHoverWin; static int captionHoverHit; static RECT captionHoverBox;
    static long captionHoverSince;
    static void CaptionHover(POINT p) {
        IntPtr top = captured == IntPtr.Zero && dragWin == IntPtr.Zero && !OverShell(p) ? WindowAt(p) : IntPtr.Zero;
        RECT box = new RECT(); bool enabled = false;
        int hit = top == IntPtr.Zero ? 0 : CaptionAt(top, p, out box, out enabled);
        if (!enabled) hit = 0;
        IntPtr previous, current = hit != 0 ? top : IntPtr.Zero;
        bool changed;
        lock (deskLock) {
            previous = captionHoverWin;
            changed = previous != current || captionHoverHit != hit || captionHoverBox.L != box.L || captionHoverBox.T != box.T;
            if (changed) captionHoverSince = Stopwatch.GetTimestamp();
            captionHoverWin = current; captionHoverHit = hit; captionHoverBox = box;
        }
        if (changed) Kick(); // transitions repaint immediately, even after long idle
        if (previous != IntPtr.Zero && previous != current) PostMessage(previous, 0x02A2 /* WM_NCMOUSELEAVE */, IntPtr.Zero, IntPtr.Zero);
        if (top != IntPtr.Zero && hit > 1) {
            double dpi = Scale(top);
            int x = (int)Math.Round(p.X * dpi), y = (int)Math.Round(p.Y * dpi);
            PostMessage(top, 0x00A0 /* WM_NCMOUSEMOVE */, (IntPtr)hit, (IntPtr)(((y & 0xFFFF) << 16) | (x & 0xFFFF)));
        }
    }

    static void DrawCaptionFeedback(Graphics g, IntPtr window, int l, int t, double scale) {
        if (window != captionHoverWin || captionHoverHit == 0 || !IsWindow(window)) return;
        float s = (float)scale;
        var box = new RectangleF((captionHoverBox.L - l) * s, (captionHoverBox.T - t) * s,
            (captionHoverBox.R - captionHoverBox.L) * s, (captionHoverBox.B - captionHoverBox.T) * s);
        bool pressed = pressWin == window && pressHit == captionHoverHit;
        bool close = captionHoverHit == 20;
        // Neutral buttons retain the app's frame and glyph. Close is Windows red.
        using (var fill = new SolidBrush(close ? Color.FromArgb(pressed ? 196 : 232, 17, 35) : Color.FromArgb(pressed ? 64 : 32, 128, 128, 128))) g.FillRectangle(fill, box);
        if (close) {
            float cx = box.X + box.Width / 2, cy = box.Y + box.Height / 2, d = Math.Min(box.Width, box.Height) * .16f;
            using (var pen = new Pen(Color.White, Math.Max(1, s))) {
                g.DrawLine(pen, cx - d, cy - d, cx + d, cy + d);
                g.DrawLine(pen, cx + d, cy - d, cx - d, cy + d);
            }
        }
        g.Flush(FlushIntention.Sync);
    }

    static void DrawCaptionTip(Graphics g, IntPtr under) {
        if (captionHoverWin == IntPtr.Zero || captionHoverHit == 0 || pressWin != IntPtr.Zero || !IsWindow(captionHoverWin) || IsIconic(captionHoverWin)
            || OverShell(pointer) || under != captionHoverWin
            || Stopwatch.GetTimestamp() - captionHoverSince < Stopwatch.Frequency * 6 / 10) return;
        string text = captionHoverHit == 20 ? "Close" : captionHoverHit == 8 ? "Minimize" : IsZoomed(captionHoverWin) ? "Restore Down" : "Maximize";
        using (var font = new Font("Segoe UI", 12f * (float)Dpi(), FontStyle.Regular, GraphicsUnit.Pixel)) {
            SizeF size = g.MeasureString(text, font);
            var tip = new RectangleF(Math.Max(4, Math.Min(screenW - size.Width - 20, captionHoverBox.R - size.Width - 16)),
                Math.Min(screenH - barH - size.Height - 12, captionHoverBox.B + 8), size.Width + 16, size.Height + 10);
            using (var fill = new SolidBrush(Color.FromArgb(250, 250, 250))) g.FillRectangle(fill, tip);
            using (var rim = new Pen(Color.FromArgb(160, 160, 160))) g.DrawRectangle(rim, tip.X, tip.Y, tip.Width, tip.Height);
            using (var ink = new SolidBrush(Color.FromArgb(25, 25, 25))) g.DrawString(text, font, ink, tip.X + 8, tip.Y + 5);
        }
    }
    static IntPtr dragWin, pressWin; static int dragHit, pressHit; static POINT dragFrom; static RECT dragRect;
    // A press on a title bar that has a window of the app's own in it (Office's: the search box, Save, Undo), until it
    // is let go: moved, it drags the window; let go where it was pressed, it was a click on what is drawn there.
    static IntPtr barChild; static POINT barAt;

    static void ClickChild(IntPtr child, POINT p) {
        if (!IsWindow(child)) return;
        IntPtr lp = ClientLParam(child, p);
        PostMessage(child, WM_MOUSEMOVE, IntPtr.Zero, lp);
        PostMessage(child, WM_LBUTTONDOWN, (IntPtr)1, lp);
        PostMessage(child, WM_LBUTTONUP, IntPtr.Zero, lp);
    }

    // The window manager's share of a mouse event on a worker's desktop. "OK" when it was all there was to do; null when
    // the event is for the window's own contents.
    static string Manage(string kind, POINT p, bool right, bool twice) {
        if (kind == "move") {
            CaptionHover(p);
            if (barChild != IntPtr.Zero && (Math.Abs(p.X - barAt.X) > 4 || Math.Abs(p.Y - barAt.Y) > 4)) barChild = IntPtr.Zero;
            if (dragWin == IntPtr.Zero) return captured == IntPtr.Zero && (ShellHover(p) || captionHoverWin != IntPtr.Zero) ? "OK" : null;
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
            if (barChild != IntPtr.Zero) { ClickChild(barChild, barAt); barChild = IntPtr.Zero; dragWin = IntPtr.Zero; return "OK"; }
            if (dragWin != IntPtr.Zero) { dragWin = IntPtr.Zero; return "OK"; }
            if (pressWin == IntPtr.Zero) return null;
            IntPtr w = pressWin; int hit = pressHit; pressWin = IntPtr.Zero;
            // A caption button acts when the press is let go on it, so a press can still be taken back by moving off.
            if (IsWindow(w) && WindowAt(p) == w && HitTest(w, p) == hit) {
                lock (deskLock) { captionHoverWin = IntPtr.Zero; captionHoverHit = 0; }
                PostMessage(w, 0x02A2 /* WM_NCMOUSELEAVE */, IntPtr.Zero, IntPtr.Zero);
                Caption(w, hit);
            }
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
            barChild = IntPtr.Zero;
            if (at == 2 && twice) Caption(top, 9);
            else {
                if (!IsZoomed(top)) { dragWin = top; dragHit = at; dragFrom = p; GetWindowRect(top, out dragRect); }
                IntPtr inner = at == 2 ? Descend(top, p) : top;
                if (inner != top) { barChild = inner; barAt = p; }
            }
            return "OK";
        }
        if (at == 8 || at == 9 || at == 20) { pressWin = top; pressHit = at; return "OK"; }
        return null;
    }

    // The windows a taskbar has a button for, topmost first.
    static List<IntPtr> TaskWindows() {
        var list = new List<IntPtr>();
        if (desktop != IntPtr.Zero) Enum(TaskWindow, list);
        return list;
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
        var list = TaskWindows();
        var sb = new StringBuilder();
        bool front = true;
        lock (deskLock) {
            var gone = new List<IntPtr>();
            foreach (var handle in windowIcons.Keys) if (!list.Contains(handle)) gone.Add(handle);
            foreach (var handle in gone) { if (windowIcons[handle] != null) windowIcons[handle].Dispose(); windowIcons.Remove(handle); }
        }
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
        var list = TaskWindows();
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
        if (fixedScreen) {
            string managed = Manage(kind, screen, right, twice);
            if (managed != null) {
                if (kind == "move" && captured == IntPtr.Zero) ClientHover(IntPtr.Zero);
                return managed;
            }
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
        if (ShellChar(ch)) return;
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
        if (ShellKey(vk)) return "OK";
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
            System.Threading.Thread.Sleep(150);
        });
    }
}
'@

# What the C# above uses beyond the core: drawing, and UI Automation (its tree, and System.Windows.Rect from WindowsBase).
$NativeAssemblies = @('System.Drawing', 'UIAutomationClient', 'UIAutomationTypes', 'WindowsBase')

if ($Build) {
    try {
        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Build) | Out-Null
        $tmp = "$Build.$PID.tmp.exe" # named as a program: the compiler makes a library of anything else, whatever it is asked
        Add-Type -ReferencedAssemblies $NativeAssemblies -TypeDefinition $NativeSource -OutputAssembly $tmp -OutputType WindowsApplication
        Move-Item -LiteralPath $tmp -Destination $Build -Force
        exit 0
    } catch {
        [Console]::Error.WriteLine($_.Exception.Message)
        exit 1
    }
}

# What -Resolve says back, one line a message: as base64 of UTF-8, so a path keeps its letters whatever this console's
# code page is.
function Say([string]$text) { [Console]::Out.WriteLine([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($text))) }

# Compiling the C# above takes seconds every time a helper starts, which is most of what starting an app used to cost. It is
# compiled once, to a DLL named for a hash of the source (so a changed source is never mistaken for an old build), and loaded
# from there afterwards. Two helpers starting together may both compile; each writes its own temporary file and the last
# move wins, which is harmless because the contents are identical.
if (-not $Resolve -and -not ('Native' -as [type])) {
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
    if ($Resolve) { Say ("PREP`t" + $p.Launcher + "`t" + $u.AbsoluteUri) }
    else {
        $status = [Native]::Prepare($DesktopName, [string]$p.Launcher, [string]$u.AbsoluteUri)
        if ($status.StartsWith('ERR ')) { throw $status.Substring(4) }
    }
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

# A private profile an older build imported from the person's own browser (it carries an import marker) still holds
# their cookies, saved passwords and the key they are encrypted with, and launches kept using it after the import was
# made opt-in (B-19). Without the opt-in such a profile is deleted once, before the browser starts, so it starts empty
# like any other; the worker signs in on its own screen. With the opt-in it is left as it is.
function Remove-ImportedHostProfile([string]$dir) {
    if ($env:DECURIX_IMPORT_HOST_BROWSER_PROFILE -eq '1') { return }
    if (-not (Test-Path -LiteralPath $dir -PathType Container)) { return }
    $marks = @(Get-ChildItem -LiteralPath $dir -Force -Filter '.decurix-host-profile-imported-*' -ErrorAction SilentlyContinue)
    if ($marks.Count -eq 0) { return }
    Remove-Item -LiteralPath $dir -Recurse -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $dir) {
        # Not removable (held open): never launched with it.
        throw "the private browser profile imported from your own browser could not be removed: $dir"
    }
}

function Initialize-PrivateBrowserProfile([string]$base, [string]$dir, [string]$requestedProfile) {
    # Not imported any more (review 2026-11-02): the copy carried the person's cookies, saved passwords and Local State
    # (the key they are encrypted with) into a browser a worker drives, and anything that steers the worker - a web
    # page, an email - could then act as the person on every site they are signed in to. The private profile starts
    # empty; the worker signs in on its own screen, asking the person when it needs to. The import below is kept only
    # for an explicit opt-in, DECURIX_IMPORT_HOST_BROWSER_PROFILE=1 in this computer's environment.
    if ($env:DECURIX_IMPORT_HOST_BROWSER_PROFILE -ne '1') { return }
    if (-not ($Browsers -contains $base)) { return }
    $source = Get-BrowserProfileSource $base
    if (-not $source -or -not (Test-Path -LiteralPath $source -PathType Container)) { return }
    # The profile to bring over is the one the launch will actually open. A profile shortcut ("Koustubh - Chrome") carries
    # --profile-directory, and that profile - not whichever one is auto-detected as active - is where the person's sign-ins
    # live. An older build copied only the auto-detected profile, so the shortcut's own profile was launched empty and the
    # person had to sign in again.
    $active = $requestedProfile
    if (-not $active -or -not (Test-Path -LiteralPath (Join-Path $source $active) -PathType Container)) {
        $active = Get-ActiveBrowserProfile $source
    }
    # One marker per profile folder (rather than one for the whole directory): a worker whose browser opens a second profile
    # imports that one too, and a worker left half-imported by the older build heals itself the next time its shortcut's own
    # profile is launched. Once imported, later launches never overwrite work or sign-ins added inside the private profile.
    $safe = if ($active) { $active -replace '[^A-Za-z0-9._-]', '_' } else { 'all' }
    $marker = Join-Path $dir ".decurix-host-profile-imported-$safe"
    if (Test-Path -LiteralPath $marker) { return }
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $common = @('/XJ', '/R:0', '/W:0', '/MT:16', '/NFL', '/NDL', '/NJH', '/NJS', '/NP', '/XD') + $ProfileSkipDirs + @('/XF') + $ProfileSkipFiles
    # Only the profile the person uses and the top-level files that go with it (Local State holds the key its cookies and
    # passwords are encrypted with): other profiles were most of the rest.
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
    Start-WebAppServer $p
    if ($p.OwnProfile) {
        $dir = Join-Path $ProfileRoot "$DesktopName-$($p.Base)"
        # The profile the shortcut asks to open is the one whose sign-ins must come across, so import that one.
        $requested = ''
        if ($Browsers -contains $p.Base) {
            $m = [regex]::Match($argLine, '(?i)--profile-directory=(?:"([^"]*)"|(\S+))')
            if ($m.Success) { $requested = $m.Groups[1].Value + $m.Groups[2].Value }
        }
        if ($Browsers -contains $p.Base) { Remove-ImportedHostProfile $dir }
        Initialize-PrivateBrowserProfile $p.Base $dir $requested
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
            if ($argLine -notlike '*--profile-directory*') {
                $chosenProfile = Get-ActiveBrowserProfile $dir
                if ($chosenProfile) { $argLine += " --profile-directory=`"$chosenProfile`"" }
            }
        }
        Clear-CrashMark $dir
    }
    return @{ Target = $p.Target; Args = $argLine; WorkDir = $p.WorkDir }
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

if ($Resolve) {
    try {
        $f = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Resolve)).Split("`t")
        switch ($f[0]) {
            'check' {
                Say (Format-Check (Resolve-Program $f[1] $(if ($f.Count -gt 2) { $f[2] } else { '' })))
            }
            'complete' {
                $l = Complete-Launch @{ Target = $f[1]; Args = $f[2]; WorkDir = $f[3]; OwnProfile = ($f[4] -eq '1'); Base = $f[5]; Launcher = $f[6] }
                Say ("RUN`t" + $l.Target + "`t" + $l.Args + "`t" + $l.WorkDir)
            }
            default { Say 'ERR unknown command' }
        }
    } catch { Say ("ERR " + $_.Exception.Message) }
    exit 0
}

# The commands that need the functions above; every other is answered by Native.Serve itself, without PowerShell.
$Commands = [Action[string, string]] {
    param([string]$verb, [string]$rest)
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
            Reply (Format-Check (Resolve-Program $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })))
        }
        "launchr" {
            # What a check already worked out: nothing is looked up, so this is as fast as starting the program.
            $f = $rest.Split("`t")
            if ($f.Count -lt 5) { Reply "ERR malformed launchr" }
            elseif (-not (Test-Path -LiteralPath $f[0] -PathType Leaf)) { Reply "ERR Not found on this computer: $($f[0])" }
            else {
                $l = Complete-Launch @{ Target = $f[0]; Args = $f[1]; WorkDir = $f[2]; OwnProfile = ($f[3] -eq '1'); Base = $f[4]; Launcher = $(if ($f.Count -gt 5) { $f[5] } else { '' }) }
                Reply ([Native]::Launch($DesktopName, $l.Target, $l.Args, $l.WorkDir))
            }
        }
        "pid" {
            # Which process this helper is on the host: the control plane ends exactly this tree when it stops it.
            Reply "OK $PID"
        }
        "open" {
            $f = $rest.Split("`t")
            $r = Resolve-Launch $f[0] $(if ($f.Count -gt 1) { $f[1] } else { '' })
            if ($r.Error) { Reply "ERR $($r.Error)" }
            elseif ($f.Count -gt 3 -and $f[2] -eq 'sandbox') { Reply ([Native]::Open($DesktopName, $r.Target, $r.Args, $r.WorkDir, $f[3])) }
            else { Reply ([Native]::Open($DesktopName, $r.Target, $r.Args, $r.WorkDir)) }
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
