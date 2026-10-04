// decurix-ui: what a native Windows worker sees and touches.
//
// On a Linux desktop the agent reads the screen with Xlib, the widgets with AT-SPI and acts with xdotool. A native
// worker's screen is a private Win32 desktop, where none of those exist, and where the usual Windows tools do not
// work either: SendInput and a screen capture both go to the desktop a person is looking at, not this one. So this
// program - started by the agent, and therefore on the worker's desktop - does the three jobs from inside:
//
//   pixels    every visible window printed into one picture the size of the screen (PrintWindow)
//   widgets   the controls of each window, through UI Automation (the accessibility layer every Windows has)
//   input     mouse and keyboard as posted window messages, and a control's own patterns where it has them
//
// It is spoken to over stdin/stdout, one JSON object per line each way (decurix_agent/computer/win.py). A reply is
// {"ok": true, ...} or {"ok": false, "error": "..."}; "grab" is followed by the picture's raw RGB bytes.
//
// Built with the C# 5 compiler that ships in every Windows (scripts/windows/build-electron.ps1), so the language is
// kept to what that compiler takes: no string interpolation, no ?. and no expression-bodied members.

using System;
using System.Collections;
using System.Collections.Generic;
using System.Drawing;
using System.Drawing.Imaging;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Threading;
using System.Web.Script.Serialization;
using System.Windows.Automation;

public static class Ui {
    // ---- Win32

    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
    [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
    [StructLayout(LayoutKind.Sequential)]
    struct GUITHREADINFO { public int cbSize, flags; public IntPtr hwndActive, hwndFocus, hwndCapture, hwndMenuOwner, hwndMoveSize, hwndCaret; public RECT rcCaret; }

    delegate bool EnumProc(IntPtr h, IntPtr l);

    [DllImport("user32.dll")] static extern bool SetProcessDPIAware();
    [DllImport("user32.dll")] static extern int GetSystemMetrics(int index);
    [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
    [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll")] static extern bool IsWindow(IntPtr h);
    [DllImport("user32.dll")] static extern bool IsIconic(IntPtr h);
    [DllImport("user32.dll")] static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out int pid);
    [DllImport("user32.dll")] static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")] static extern bool SetForegroundWindow(IntPtr h);
    [DllImport("user32.dll")] static extern bool BringWindowToTop(IntPtr h);
    [DllImport("user32.dll")] static extern bool ShowWindow(IntPtr h, int cmd);
    [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
    [DllImport("user32.dll")] static extern int GetWindowLong(IntPtr h, int index);
    [DllImport("user32.dll")] static extern bool PrintWindow(IntPtr h, IntPtr dc, uint f);
    [DllImport("user32.dll")] static extern bool PostMessage(IntPtr h, uint msg, IntPtr wp, IntPtr lp);
    [DllImport("user32.dll")] static extern IntPtr ChildWindowFromPointEx(IntPtr parent, POINT pt, uint flags);
    [DllImport("user32.dll")] static extern bool ScreenToClient(IntPtr h, ref POINT p);
    [DllImport("user32.dll")] static extern bool GetGUIThreadInfo(uint tid, ref GUITHREADINFO info);
    [DllImport("user32.dll")] static extern bool AttachThreadInput(uint from, uint to, bool attach);
    [DllImport("user32.dll")] static extern IntPtr SetActiveWindow(IntPtr h);
    [DllImport("user32.dll")] static extern IntPtr SetFocus(IntPtr h);
    [DllImport("user32.dll")] static extern bool GetKeyboardState(byte[] state);
    [DllImport("user32.dll")] static extern bool SetKeyboardState(byte[] state);
    [DllImport("user32.dll")] static extern uint MapVirtualKey(uint code, uint type);
    [DllImport("user32.dll")] static extern short VkKeyScan(char ch);
    [DllImport("kernel32.dll")] static extern uint GetCurrentThreadId();
    [DllImport("dwmapi.dll")] static extern int DwmGetWindowAttribute(IntPtr h, int attr, out RECT r, int size);
    [DllImport("dwmapi.dll")] static extern int DwmGetWindowAttribute(IntPtr h, int attr, out int v, int size);

    const uint WM_CLOSE = 0x0010, WM_KEYDOWN = 0x0100, WM_KEYUP = 0x0101, WM_CHAR = 0x0102, WM_SYSKEYDOWN = 0x0104,
               WM_SYSKEYUP = 0x0105, WM_MOUSEMOVE = 0x0200, WM_LBUTTONDOWN = 0x0201, WM_LBUTTONUP = 0x0202,
               WM_LBUTTONDBLCLK = 0x0203, WM_RBUTTONDOWN = 0x0204, WM_RBUTTONUP = 0x0205, WM_MOUSEWHEEL = 0x020A,
               WM_MOUSEHWHEEL = 0x020E;

    static readonly JavaScriptSerializer json = new JavaScriptSerializer();
    static Stream stdout;
    static int screenW, screenH;

    // Windows Windows itself puts in every process (IME and text-services helpers, the UAC input indicator); never an app's.
    static readonly string[] SystemClasses = {
        "UAC_InputIndicatorOverlayWnd", "UAC Input Indicator", "Touch Tooltip Window", "MSCTFIME UI", "IME", "Default IME",
        "CicLoaderWndClass", "GDI+ Hook Window Class", "tooltips_class32" };

    public static int Main(string[] args) {
        SetProcessDPIAware(); // positions are physical pixels everywhere: in pictures, in UI Automation and in messages
        screenW = GetSystemMetrics(0); screenH = GetSystemMetrics(1);
        json.MaxJsonLength = int.MaxValue;
        json.RecursionLimit = 64;
        stdout = Console.OpenStandardOutput();
        var stdin = new StreamReader(Console.OpenStandardInput(), new UTF8Encoding(false));

        string line;
        while ((line = stdin.ReadLine()) != null) {
            if (line.Trim().Length == 0) continue;
            byte[] tail = null;
            var reply = new Dictionary<string, object>();
            try {
                var req = json.Deserialize<Dictionary<string, object>>(line);
                string op = Str(req, "op");
                if (op == "quit") break;
                Dispatch(op, req, reply, out tail);
                reply["ok"] = true;
            } catch (Exception e) {
                reply.Clear();
                reply["ok"] = false;
                reply["error"] = e.GetType().Name + ": " + e.Message;
                tail = null;
            }
            byte[] head = Encoding.UTF8.GetBytes(json.Serialize(reply) + "\n");
            stdout.Write(head, 0, head.Length);
            if (tail != null) stdout.Write(tail, 0, tail.Length);
            stdout.Flush();
        }
        return 0;
    }

    static void Dispatch(string op, Dictionary<string, object> req, Dictionary<string, object> reply, out byte[] tail) {
        tail = null;
        switch (op) {
            case "ping": reply["screen"] = new[] { screenW, screenH }; break;
            case "windows": reply["windows"] = WindowList(); break;
            case "grab":
                tail = Grab();
                reply["width"] = screenW; reply["height"] = screenH; reply["bytes"] = tail.Length;
                break;
            case "elements": reply["windows"] = Elements(req); break;
            case "at": At(Int(req, "x", 0), Int(req, "y", 0), reply); break;
            case "front": Front(reply); break;
            case "invoke": reply["did"] = Invoke(Find(req)); break;
            case "focus": Find(req).SetFocus(); break;
            case "set_value": reply["set"] = SetValue(Find(req), Str(req, "text")); break;
            case "select": reply["chose"] = SelectOption(Find(req), Str(req, "option")); break;
            case "text": reply["text"] = DocumentText(Find(req), Int(req, "limit", 4000)); break;
            case "click": Click(Int(req, "x", 0), Int(req, "y", 0), Str(req, "button") == "right", Int(req, "count", 1)); break;
            case "move": Mouse(WM_MOUSEMOVE, Int(req, "x", 0), Int(req, "y", 0), 0, IntPtr.Zero); break;
            case "drag": Drag(Int(req, "x", 0), Int(req, "y", 0), Int(req, "x2", 0), Int(req, "y2", 0)); break;
            case "scroll": Scroll(Int(req, "x", 0), Int(req, "y", 0), Str(req, "direction"), Int(req, "amount", 3)); break;
            case "type": TypeText(Str(req, "text")); break;
            case "key": reply["sent"] = Key(Str(req, "combo")); break;
            case "viewport": reply["box"] = BrowserViewport(); break;
            case "activate": Activate(Hwnd(req)); break;
            case "focus_info": {
                // Where keys would go now, and why: for finding out, on a desktop nobody can look at.
                var ws = Visible();
                IntPtr t = KeyTarget();
                var cls = new StringBuilder(128); if (t != IntPtr.Zero) GetClassName(t, cls, 128);
                var gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
                int p = 0; uint tid = ws.Count > 0 ? GetWindowThreadProcessId(ws[0].H, out p) : 0;
                bool got = tid != 0 && GetGUIThreadInfo(tid, ref gi);
                reply["target"] = t.ToInt64(); reply["class"] = cls.ToString();
                reply["thread_focus"] = got ? gi.hwndFocus.ToInt64() : -1;
                reply["thread_active"] = got ? gi.hwndActive.ToInt64() : -1;
                reply["top"] = ws.Count > 0 ? ws[0].H.ToInt64() : 0;
                break;
            }
            case "close": PostMessage(Hwnd(req), WM_CLOSE, IntPtr.Zero, IntPtr.Zero); break;
            default: throw new ArgumentException("unknown op " + op);
        }
    }

    static string Str(Dictionary<string, object> d, string k) {
        object v; return d.TryGetValue(k, out v) && v != null ? Convert.ToString(v) : "";
    }
    static int Int(Dictionary<string, object> d, string k, int fallback) {
        object v; return d.TryGetValue(k, out v) && v != null ? Convert.ToInt32(v) : fallback;
    }
    static IntPtr Hwnd(Dictionary<string, object> d) {
        object v;
        if (!d.TryGetValue("hwnd", out v) || v == null) throw new ArgumentException("no window given");
        return new IntPtr(Convert.ToInt64(v));
    }

    // ---- Windows

    class Win { public IntPtr H; public RECT R, V; public string Title, Class; public int Pid; }

    static bool Cloaked(IntPtr h) {
        int cloaked;
        return DwmGetWindowAttribute(h, 14 /* DWMWA_CLOAKED */, out cloaked, 4) == 0 && cloaked != 0;
    }

    // Every visible top-level window on this desktop, topmost first: an app's main window, and each menu, drop-down
    // and dialog it has open, which are top-level windows of their own.
    static List<Win> Visible() {
        var list = new List<Win>();
        EnumWindows(delegate(IntPtr h, IntPtr l) {
            if (!IsWindowVisible(h) || IsIconic(h) || Cloaked(h)) return true;
            var cls = new StringBuilder(128); GetClassName(h, cls, 128);
            if (Array.IndexOf(SystemClasses, cls.ToString()) >= 0) return true;
            RECT r; GetWindowRect(h, out r);
            if (r.R - r.L < 8 || r.B - r.T < 8) return true;
            // What the window shows: Windows adds an invisible resize border that prints black.
            RECT v;
            if (DwmGetWindowAttribute(h, 9 /* DWMWA_EXTENDED_FRAME_BOUNDS */, out v, Marshal.SizeOf(typeof(RECT))) != 0 || v.R <= v.L || v.B <= v.T) v = r;
            var title = new StringBuilder(512); GetWindowText(h, title, 512);
            int pid; GetWindowThreadProcessId(h, out pid);
            list.Add(new Win { H = h, R = r, V = v, Title = title.ToString(), Class = cls.ToString(), Pid = pid });
            return true;
        }, IntPtr.Zero);
        return list;
    }

    // The windows a person would name: titled, and not a popup that belongs to another window (a menu, a tooltip).
    static bool IsApplicationWindow(Win w) {
        if (w.Title.Length == 0) return false;
        const int GWL_EXSTYLE = -20, WS_EX_TOOLWINDOW = 0x80;
        return (GetWindowLong(w.H, GWL_EXSTYLE) & WS_EX_TOOLWINDOW) == 0;
    }

    static IntPtr ActiveWindow(List<Win> all) {
        // The topmost app window is where input goes, and what a person would call the active one. Not Windows' own
        // "foreground window": on a private desktop nothing can set it (SetForegroundWindow answers yes and changes
        // nothing), so it stays on whichever app had it first - found live, with the browser raised over Notepad,
        // Ctrl+L meant for the browser was sent to Notepad's editor.
        foreach (var w in all) if (IsApplicationWindow(w)) return w.H;
        return IntPtr.Zero;
    }

    static object WindowList() {
        var all = Visible();
        IntPtr active = ActiveWindow(all);
        var result = new List<object>();
        // The contract is topmost last (screen.windows on Linux lists the stacking order bottom up).
        for (int i = all.Count - 1; i >= 0; i--) {
            var w = all[i];
            if (!IsApplicationWindow(w)) continue;
            result.Add(new Dictionary<string, object> {
                { "id", w.H.ToInt64() }, { "title", w.Title }, { "pid", w.Pid }, { "class", w.Class },
                { "app", ProcessName(w.Pid) },
                { "box", new[] { w.V.L, w.V.T, w.V.R - w.V.L, w.V.B - w.V.T } }, { "active", w.H == active } });
        }
        return result;
    }

    [DllImport("user32.dll")] static extern bool EnumChildWindows(IntPtr parent, EnumProc p, IntPtr l);

    // Where the topmost browser window draws its page, in screen pixels, or null when no browser is open. A Chromium
    // window keeps the page in a child window of its own, whose rectangle is exact - what the page can say about itself
    // (screenX, outerHeight - innerHeight) is in rounded CSS pixels and counts borders the page is not inside.
    static object BrowserViewport() {
        foreach (var w in Visible()) {
            if (w.Class != "Chrome_WidgetWin_1" || !IsApplicationWindow(w)) continue;
            IntPtr host = IntPtr.Zero;
            EnumChildWindows(w.H, delegate(IntPtr child, IntPtr l) {
                var cls = new StringBuilder(64); GetClassName(child, cls, 64);
                if (cls.ToString() != "Chrome_RenderWidgetHostHWND" || !IsWindowVisible(child)) return true;
                host = child;
                return false;
            }, IntPtr.Zero);
            if (host == IntPtr.Zero) continue;
            RECT r; GetWindowRect(host, out r);
            return new[] { r.L, r.T, r.R - r.L, r.B - r.T };
        }
        return null;
    }

    [DllImport("user32.dll")] static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);

    // Puts a window in front of the others on this desktop. BringWindowToTop and SetForegroundWindow answer yes here
    // and leave another program's window where it was (measured, in the control plane's helper and again here: a page
    // opened in the browser stayed behind a Notepad left open by an earlier task, and the worker typed into a page it
    // could not see). Windows gives the foreground only to the program that has the person's input, and on a private
    // desktop no program does. Making the window topmost and then ordinary again is not subject to that, and moves it.
    static void Raise(IntPtr h) {
        if (IsIconic(h)) ShowWindow(h, 9 /* SW_RESTORE */);
        bool always = (GetWindowLong(h, -20 /* GWL_EXSTYLE */) & 0x8 /* WS_EX_TOPMOST */) != 0;
        SetWindowPos(h, (IntPtr)(-1) /* HWND_TOPMOST */, 0, 0, 0, 0, 0x1 | 0x2 | 0x10 /* NOSIZE, NOMOVE, NOACTIVATE */);
        if (!always) SetWindowPos(h, (IntPtr)(-2) /* HWND_NOTOPMOST */, 0, 0, 0, 0, 0x1 | 0x2 | 0x10);
        BringWindowToTop(h);
        SetForegroundWindow(h);
    }

    static void Activate(IntPtr h) {
        if (!IsWindow(h)) throw new ArgumentException("that window is gone");
        Raise(h);
        GiveFocus(h);
    }

    // On a desktop a person is looking at, Windows gives a newly opened window the keyboard. On a private desktop
    // nothing does: a window that was just opened has no focus anywhere in it until something is clicked, and keys
    // sent to it go nowhere - found live: a person opened Notepad from the viewer, typed, and nothing was typed. So
    // a window with no focus is made its thread's active window and given the focus, from a thread that has joined
    // its input queue (the calls only work on the calling thread's own queue); the app then passes focus on to its
    // own control, as it does when Windows activates it. A window that already has a focus is left as it is.
    static void GiveFocus(IntPtr h) {
        int pid; uint appThread = GetWindowThreadProcessId(h, out pid);
        var gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
        if (GetGUIThreadInfo(appThread, ref gi) && gi.hwndFocus != IntPtr.Zero) return;
        var th = new Thread(delegate() {
            uint me = GetCurrentThreadId();
            if (!AttachThreadInput(me, appThread, true)) return;
            try { SetActiveWindow(h); SetFocus(h); }
            finally { AttachThreadInput(me, appThread, false); }
        });
        th.Start();
        th.Join(2000);

        // Most apps pass the focus on to the control a person would type into. Some leave it on the window's frame,
        // where keys do nothing - measured: Windows 11 Notepad reopening its last session (a new, empty Notepad
        // focuses its editor by itself). Then the first field in the window is given it, as that app does when a
        // person's click or Windows' own activation reaches it.
        gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
        if (GetGUIThreadInfo(appThread, ref gi) && gi.hwndFocus != IntPtr.Zero && gi.hwndFocus != h) return;
        try {
            var field = AutomationElement.FromHandle(h).FindFirst(TreeScope.Descendants, new AndCondition(
                new PropertyCondition(AutomationElement.IsKeyboardFocusableProperty, true),
                new PropertyCondition(AutomationElement.IsEnabledProperty, true),
                new OrCondition(
                    new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Document),
                    new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Edit))));
            if (field == null) return;
            // A field that is a window of its own takes the focus as a window: UI Automation's own SetFocus marked
            // Notepad's editor focused and left the keyboard on the frame.
            var child = new IntPtr(field.Current.NativeWindowHandle);
            if (child == IntPtr.Zero || child == h) { field.SetFocus(); return; }
            var focus = new Thread(delegate() {
                uint me = GetCurrentThreadId();
                if (!AttachThreadInput(me, appThread, true)) return;
                try { SetFocus(child); }
                finally { AttachThreadInput(me, appThread, false); }
            });
            focus.Start();
            focus.Join(2000);
        } catch (Exception) { } // a window with no field, or one that is gone: there is nothing to focus
    }

    // ---- Pixels

    // The whole desktop as packed RGB rows: every visible window printed where it is, bottom to top, on the dark
    // ground an empty desktop has. PrintWindow, because nothing can be copied off a desktop that is not on a screen.
    static byte[] Grab() {
        var ws = Visible();
        using (var bmp = new Bitmap(screenW, screenH, PixelFormat.Format24bppRgb)) {
            using (var g = Graphics.FromImage(bmp)) {
                g.Clear(Color.FromArgb(0x1b, 0x1b, 0x1b));
                // A popup's shadow and rounded corners print black; over the window beneath they should show it.
                var seeThrough = new ImageAttributes();
                seeThrough.SetColorKey(Color.FromArgb(0, 0, 0), Color.FromArgb(4, 4, 4));
                for (int i = ws.Count - 1; i >= 0; i--) {
                    var w = ws[i];
                    int bw = w.R.R - w.R.L, bh = w.R.B - w.R.T;
                    if (bw <= 0 || bh <= 0 || bw > 8192 || bh > 8192) continue;
                    using (var wb = new Bitmap(bw, bh, PixelFormat.Format24bppRgb)) {
                        using (var wg = Graphics.FromImage(wb)) {
                            IntPtr dc = wg.GetHdc();
                            PrintWindow(w.H, dc, 2 /* PW_RENDERFULLCONTENT */);
                            wg.ReleaseHdc(dc);
                        }
                        var src = new Rectangle(w.V.L - w.R.L, w.V.T - w.R.T, w.V.R - w.V.L, w.V.B - w.V.T);
                        var dest = new Rectangle(w.V.L, w.V.T, src.Width, src.Height);
                        if (i == ws.Count - 1) g.DrawImage(wb, dest, src.X, src.Y, src.Width, src.Height, GraphicsUnit.Pixel);
                        else g.DrawImage(wb, dest, src.X, src.Y, src.Width, src.Height, GraphicsUnit.Pixel, seeThrough);
                    }
                }
            }
            var data = bmp.LockBits(new Rectangle(0, 0, screenW, screenH), ImageLockMode.ReadOnly, PixelFormat.Format24bppRgb);
            try {
                var rgb = new byte[screenW * screenH * 3];
                var row = new byte[data.Stride];
                for (int y = 0; y < screenH; y++) {
                    Marshal.Copy(IntPtr.Add(data.Scan0, y * data.Stride), row, 0, data.Stride);
                    int o = y * screenW * 3;
                    for (int x = 0; x < screenW * 3; x += 3) { // GDI keeps a pixel as B, G, R
                        rgb[o + x] = row[x + 2]; rgb[o + x + 1] = row[x + 1]; rgb[o + x + 2] = row[x];
                    }
                }
                return rgb;
            } finally { bmp.UnlockBits(data); }
        }
    }

    // ---- Widgets

    const int MaxNodes = 4000;
    const int MaxDepth = 40;

    static CacheRequest TreeRequest() {
        var cr = new CacheRequest();
        cr.TreeScope = TreeScope.Element | TreeScope.Descendants;
        cr.TreeFilter = Automation.ControlViewCondition;
        cr.AutomationElementMode = AutomationElementMode.Full;
        foreach (var p in new AutomationProperty[] {
            AutomationElement.NameProperty, AutomationElement.ControlTypeProperty, AutomationElement.BoundingRectangleProperty,
            AutomationElement.IsEnabledProperty, AutomationElement.IsOffscreenProperty, AutomationElement.HasKeyboardFocusProperty,
            AutomationElement.IsPasswordProperty, AutomationElement.ClassNameProperty, AutomationElement.AutomationIdProperty,
            AutomationElement.IsKeyboardFocusableProperty, AutomationElement.LabeledByProperty, AutomationElement.HelpTextProperty,
            AutomationElement.FrameworkIdProperty,
            AutomationElement.IsInvokePatternAvailableProperty, AutomationElement.IsTogglePatternAvailableProperty,
            AutomationElement.IsSelectionItemPatternAvailableProperty, AutomationElement.IsExpandCollapsePatternAvailableProperty,
            AutomationElement.IsValuePatternAvailableProperty, AutomationElement.IsTextPatternAvailableProperty,
            AutomationElement.IsScrollPatternAvailableProperty,
            ValuePattern.ValueProperty, ValuePattern.IsReadOnlyProperty, TogglePattern.ToggleStateProperty,
            SelectionItemPattern.IsSelectedProperty, ExpandCollapsePattern.ExpandCollapseStateProperty })
            cr.Add(p);
        return cr;
    }

    static object Cached(AutomationElement e, AutomationProperty p) {
        object v = e.GetCachedPropertyValue(p, true);
        return v == AutomationElement.NotSupported ? null : v;
    }
    static bool Flag(AutomationElement e, AutomationProperty p) {
        object v = Cached(e, p); return v is bool && (bool)v;
    }

    // The controls of every application window that is not one of skip_pids (the browser's: its page is read over
    // DevTools, and its UI Automation tree is the whole page again, slowly). Each window is asked for its tree in one
    // request, with everything wanted about each control cached, so a window of hundreds of controls is one round
    // trip to the app rather than thousands.
    static object Elements(Dictionary<string, object> req) {
        var skip = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        object raw;
        if (req.TryGetValue("skip_apps", out raw) && raw is IEnumerable && !(raw is string))
            foreach (object p in (IEnumerable)raw) skip.Add(Convert.ToString(p));

        var all = Visible();
        var windows = new List<object>();
        var deadline = DateTime.UtcNow.AddSeconds(Int(req, "seconds", 8));
        var request = TreeRequest();

        foreach (var w in all) {
            string app = ProcessName(w.Pid);
            if (skip.Contains(app)) continue;
            // A menu or drop-down that is open is a window of its own with no title: it is read too, as part of the
            // app it belongs to, or the entries a person is choosing between would not be there.
            bool popup = !IsApplicationWindow(w);
            if (popup && w.Title.Length == 0 && w.Class != "#32768" && !w.Class.Contains("Popup") && !w.Class.Contains("DropDown") && !w.Class.Contains("ComboLBox")) continue;
            if (DateTime.UtcNow > deadline) break;

            var nodes = new List<object>();
            try {
                AutomationElement root;
                using (request.Activate()) root = AutomationElement.FromHandle(w.H);
                int count = 0;
                Walk(root, nodes, new List<string>(), 0, ref count, deadline);
            } catch (Exception e) {
                windows.Add(new Dictionary<string, object> {
                    { "hwnd", w.H.ToInt64() }, { "title", w.Title }, { "pid", w.Pid }, { "class", w.Class }, { "popup", popup },
                    { "app", app }, { "error", e.GetType().Name + ": " + e.Message }, { "nodes", nodes } });
                continue;
            }
            windows.Add(new Dictionary<string, object> {
                { "hwnd", w.H.ToInt64() }, { "title", w.Title }, { "pid", w.Pid }, { "class", w.Class }, { "popup", popup },
                { "app", app }, { "nodes", nodes } });
        }
        return windows;
    }

    static string ProcessName(int pid) {
        try { return System.Diagnostics.Process.GetProcessById(pid).ProcessName; } catch (Exception) { return ""; }
    }

    [DllImport("kernel32.dll")] static extern IntPtr OpenProcess(uint access, bool inherit, int pid);
    [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)]
    static extern bool QueryFullProcessImageName(IntPtr process, uint flags, StringBuilder name, ref int size);

    // The program a window belongs to, by its full path - what says which app a person was working in. Asked with the
    // least right that answers it (Process.MainModule wants more, and fails for a program of another bitness).
    static string ProcessPath(int pid) {
        IntPtr h = OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid);
        if (h == IntPtr.Zero) return "";
        try {
            var name = new StringBuilder(1024); int size = name.Capacity;
            return QueryFullProcessImageName(h, 0, name, ref size) ? name.ToString() : "";
        } finally { CloseHandle(h); }
    }

    static readonly string[] BrowserApps = { "chrome", "msedge", "brave", "chromium" };

    static bool IsBrowser(string app) {
        return Array.IndexOf(BrowserApps, (app ?? "").ToLowerInvariant()) >= 0;
    }

    // The window of an app that a popup (a menu, a drop-down) belongs to: the topmost application window of the same
    // program. Null for a popup of a program with no other window.
    static Win OwnerOf(Win popup, List<Win> all) {
        foreach (var w in all) if (w.Pid == popup.Pid && w.H != popup.H && IsApplicationWindow(w)) return w;
        return null;
    }

    static Dictionary<string, object> Describe(Win w, List<Win> all, bool withNodes) {
        string app = ProcessName(w.Pid);
        bool popup = !IsApplicationWindow(w);
        var d = new Dictionary<string, object> {
            { "hwnd", w.H.ToInt64() }, { "title", w.Title }, { "pid", w.Pid }, { "class", w.Class }, { "popup", popup },
            { "app", app }, { "exe", ProcessPath(w.Pid) }, { "browser", IsBrowser(app) },
            { "box", new[] { w.V.L, w.V.T, w.V.R - w.V.L, w.V.B - w.V.T } } };
        if (popup) {
            var owner = OwnerOf(w, all);
            if (owner != null) d["owner"] = new Dictionary<string, object> { { "hwnd", owner.H.ToInt64() }, { "title", owner.Title } };
        }
        // A browser's own tree is its whole page again, slowly: its page is asked over DevTools instead.
        if (withNodes && !IsBrowser(app)) {
            var nodes = new List<object>();
            try {
                AutomationElement root;
                using (TreeRequest().Activate()) root = AutomationElement.FromHandle(w.H);
                int count = 0;
                Walk(root, nodes, new List<string>(), 0, ref count, DateTime.UtcNow.AddSeconds(3));
            } catch (Exception e) { d["error"] = e.GetType().Name + ": " + e.Message; }
            d["nodes"] = nodes;
        }
        return d;
    }

    // Where a browser window draws its page, or null.
    static int[] PageBox(IntPtr browser) {
        IntPtr host = IntPtr.Zero;
        EnumChildWindows(browser, delegate(IntPtr child, IntPtr l) {
            var cls = new StringBuilder(64); GetClassName(child, cls, 64);
            if (cls.ToString() != "Chrome_RenderWidgetHostHWND" || !IsWindowVisible(child)) return true;
            host = child;
            return false;
        }, IntPtr.Zero);
        if (host == IntPtr.Zero) return null;
        RECT r; GetWindowRect(host, out r);
        return new[] { r.L, r.T, r.R - r.L, r.B - r.T };
    }

    // What is under a point of the screen right now: the topmost window there - not one behind it, whose controls a
    // list of every window's controls would offer just the same - with that window's controls, so the one under the
    // point can be named. For a browser, whether the point is on its page (which DevTools then names) or on its own
    // bars. Asked the moment a person presses the mouse, before the press reaches the app.
    static void At(int x, int y, Dictionary<string, object> reply) {
        var all = Visible();
        Win top = null;
        foreach (var w in all)
            if (x >= w.V.L && x < w.V.R && y >= w.V.T && y < w.V.B) { top = w; break; }
        if (top == null) return;

        var d = Describe(top, all, true);
        if ((bool)d["browser"] && !(bool)d["popup"]) {
            int[] page = PageBox(top.H);
            if (page != null) {
                d["page_box"] = page;
                d["on_page"] = x >= page[0] && x < page[0] + page[2] && y >= page[1] && y < page[1] + page[3];
            }
        }
        reply["window"] = d;
    }

    // The window keys go to right now, with its controls (one of which says it has the keyboard): asked the moment a
    // person starts typing. A menu or drop-down the active window has open is above it, and is where keys belong.
    static void Front(Dictionary<string, object> reply) {
        var all = Visible();
        IntPtr active = ActiveWindow(all);
        Win front = null;
        foreach (var w in all) if (w.H == active) { front = w; break; }
        if (front == null) return;
        if (all.Count > 0 && all[0].H != front.H && all[0].Pid == front.Pid && !IsApplicationWindow(all[0])) front = all[0];
        reply["window"] = Describe(front, all, true);
    }

    static void Walk(AutomationElement e, List<object> nodes, List<string> containers, int depth, ref int count, DateTime deadline) {
        if (count++ > MaxNodes || depth > MaxDepth || DateTime.UtcNow > deadline) return;

        var type = Cached(e, AutomationElement.ControlTypeProperty) as ControlType;
        string role = type == null ? "" : type.ProgrammaticName.Replace("ControlType.", "");
        string name = (Cached(e, AutomationElement.NameProperty) as string ?? "").Trim();
        bool offscreen = Flag(e, AutomationElement.IsOffscreenProperty);

        if (depth > 0 && !offscreen) {
            var node = new Dictionary<string, object>();
            node["role"] = role;
            node["name"] = name;
            node["containers"] = containers.ToArray();
            node["class"] = Cached(e, AutomationElement.ClassNameProperty) as string ?? "";
            node["automation_id"] = Cached(e, AutomationElement.AutomationIdProperty) as string ?? "";
            node["help"] = Cached(e, AutomationElement.HelpTextProperty) as string ?? "";
            // Which toolkit drew it ("Win32", "WinForm", "WPF", "XAML", "Chrome", ...): it decides whether the control's
            // own patterns can be trusted on a desktop that is not on screen (win.py's default_action).
            node["framework"] = e.GetCachedPropertyValue(AutomationElement.FrameworkIdProperty, false) as string ?? "";

            var label = Cached(e, AutomationElement.LabeledByProperty) as AutomationElement;
            if (label != null) {
                try { node["label"] = (label.Current.Name ?? "").Trim(); } catch (Exception) { }
            }

            object rect = Cached(e, AutomationElement.BoundingRectangleProperty);
            if (rect is System.Windows.Rect) {
                var r = (System.Windows.Rect)rect;
                if (!r.IsEmpty && r.Width > 0 && r.Height > 0 && !double.IsInfinity(r.Width))
                    node["box"] = new[] { (int)Math.Round(r.X), (int)Math.Round(r.Y), (int)Math.Round(r.Width), (int)Math.Round(r.Height) };
            }

            bool password = Flag(e, AutomationElement.IsPasswordProperty);
            node["password"] = password;
            var patterns = new List<string>();
            if (Flag(e, AutomationElement.IsInvokePatternAvailableProperty)) patterns.Add("invoke");
            if (Flag(e, AutomationElement.IsTogglePatternAvailableProperty)) patterns.Add("toggle");
            if (Flag(e, AutomationElement.IsSelectionItemPatternAvailableProperty)) patterns.Add("select");
            if (Flag(e, AutomationElement.IsExpandCollapsePatternAvailableProperty)) patterns.Add("expand");
            if (Flag(e, AutomationElement.IsTextPatternAvailableProperty)) patterns.Add("text");
            if (Flag(e, AutomationElement.IsScrollPatternAvailableProperty)) patterns.Add("scroll");
            if (Flag(e, AutomationElement.IsValuePatternAvailableProperty)) {
                patterns.Add("value");
                // A password field's value is never read: the agent is told it is one, not what is in it.
                if (!password) node["value"] = Cached(e, ValuePattern.ValueProperty) as string ?? "";
                node["read_only"] = Flag(e, ValuePattern.IsReadOnlyProperty);
            }
            node["patterns"] = patterns.ToArray();

            // A drop-down says what is chosen through its selection, not a value. A classic one that draws its own
            // entries (a font list) has no selection to ask for either; its window still knows the chosen text.
            if (role == "ComboBox" && (!node.ContainsKey("value") || ((string)node["value"]).Length == 0)) {
                try {
                    object sp;
                    if (e.TryGetCurrentPattern(SelectionPattern.Pattern, out sp)) {
                        var chosen = ((SelectionPattern)sp).Current.GetSelection();
                        if (chosen.Length > 0) node["value"] = (chosen[0].Current.Name ?? "").Trim();
                    }
                } catch (Exception) { }
                if (!node.ContainsKey("value") || ((string)node["value"]).Length == 0) {
                    try {
                        var hwnd = new IntPtr(e.Current.NativeWindowHandle);
                        if (hwnd != IntPtr.Zero) node["value"] = WindowText(hwnd);
                    } catch (Exception) { }
                }
            }

            var states = new List<string>();
            if (Flag(e, AutomationElement.HasKeyboardFocusProperty)) states.Add("focused");
            if (patterns.Contains("toggle")) {
                object t = Cached(e, TogglePattern.ToggleStateProperty);
                if (t is ToggleState) states.Add((ToggleState)t == ToggleState.On ? "checked" : "unchecked");
            }
            if (patterns.Contains("select") && Flag(e, SelectionItemPattern.IsSelectedProperty)) states.Add("selected");
            if (patterns.Contains("expand")) {
                object x = Cached(e, ExpandCollapsePattern.ExpandCollapseStateProperty);
                if (x is ExpandCollapseState) {
                    var s = (ExpandCollapseState)x;
                    if (s == ExpandCollapseState.Expanded || s == ExpandCollapseState.PartiallyExpanded) states.Add("expanded");
                    else if (s == ExpandCollapseState.Collapsed) states.Add("collapsed");
                }
            }
            if (!Flag(e, AutomationElement.IsEnabledProperty)) states.Add("disabled");
            node["states"] = states.ToArray();
            node["focusable"] = Flag(e, AutomationElement.IsKeyboardFocusableProperty);

            try { node["rid"] = e.GetRuntimeId(); } catch (Exception) { node["rid"] = new int[0]; }
            nodes.Add(node);
        }

        // Named containers become part of their children's path, so the same "OK" in two groups is two controls.
        var inner = containers;
        if (depth > 0 && name.Length > 0 && (role == "Group" || role == "Pane" || role == "ToolBar" || role == "MenuBar" || role == "Menu"
                || role == "Tab" || role == "TabItem" || role == "List" || role == "Tree" || role == "Table" || role == "DataGrid"
                || role == "Window" || role == "StatusBar" || role == "Custom")) {
            inner = new List<string>(containers);
            inner.Add(role + "\t" + name);
        }

        AutomationElementCollection children;
        try { children = e.CachedChildren; } catch (Exception) { return; }
        foreach (AutomationElement child in children) Walk(child, nodes, inner, depth + 1, ref count, deadline);
    }

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, StringBuilder lp, uint flags, uint timeout, out IntPtr result);

    // A control's own text, asked of its window, without waiting on an app that is busy.
    static string WindowText(IntPtr h) {
        var text = new StringBuilder(512);
        IntPtr copied;
        if (SendMessageTimeout(h, 0x000D /* WM_GETTEXT */, (IntPtr)text.Capacity, text, 0x2 /* SMTO_ABORTIFHUNG */, 300, out copied) == IntPtr.Zero) return "";
        return text.ToString().Trim();
    }

    // The live control a listed one stands for: by the runtime id UI Automation gave it, within its window.
    static AutomationElement Find(Dictionary<string, object> req) {
        IntPtr h = Hwnd(req);
        if (!IsWindow(h)) throw new InvalidOperationException("its window is no longer open");
        object raw;
        if (!req.TryGetValue("rid", out raw) || !(raw is IEnumerable)) throw new ArgumentException("no control given");
        var ids = new List<int>();
        foreach (object p in (IEnumerable)raw) ids.Add(Convert.ToInt32(p));
        var root = AutomationElement.FromHandle(h);
        var found = root.FindFirst(TreeScope.Element | TreeScope.Descendants,
            new PropertyCondition(AutomationElement.RuntimeIdProperty, ids.ToArray()));
        if (found == null) throw new InvalidOperationException("it is no longer on the screen");
        return found;
    }

    // Does what a click on the control does, through the control itself, and says how. "" when it offers nothing.
    //
    // On a thread of its own, with a wait: Invoke on a button that opens a modal dialog does not return until that
    // dialog is closed, and waiting for it would hang the whole worker on its own click.
    static string Invoke(AutomationElement e) {
        string did = "";
        Exception failed = null;
        var th = new Thread(delegate() {
            try {
                object p;
                if (e.TryGetCurrentPattern(InvokePattern.Pattern, out p)) { did = "invoke"; ((InvokePattern)p).Invoke(); }
                else if (e.TryGetCurrentPattern(TogglePattern.Pattern, out p)) { did = "toggle"; ((TogglePattern)p).Toggle(); }
                else if (e.TryGetCurrentPattern(SelectionItemPattern.Pattern, out p)) { did = "select"; ((SelectionItemPattern)p).Select(); }
                else if (e.TryGetCurrentPattern(ExpandCollapsePattern.Pattern, out p)) {
                    did = "expand";
                    var x = (ExpandCollapsePattern)p;
                    if (x.Current.ExpandCollapseState == ExpandCollapseState.Collapsed) x.Expand(); else x.Collapse();
                }
            } catch (Exception ex) { failed = ex; }
        });
        th.IsBackground = true;
        th.Start();
        if (!th.Join(1500)) return did; // still inside the app's own modal loop: the click was delivered
        if (failed != null) throw failed;
        return did;
    }

    static bool SetValue(AutomationElement e, string text) {
        object p;
        if (!e.TryGetCurrentPattern(ValuePattern.Pattern, out p)) return false;
        var v = (ValuePattern)p;
        if (v.Current.IsReadOnly) return false;
        v.SetValue(text);
        return true;
    }

    static string DocumentText(AutomationElement e, int limit) {
        object p;
        if (e.TryGetCurrentPattern(TextPattern.Pattern, out p)) return ((TextPattern)p).DocumentRange.GetText(limit);
        if (e.TryGetCurrentPattern(ValuePattern.Pattern, out p)) return ((ValuePattern)p).Current.Value ?? "";
        return "";
    }

    // Picks an entry of a combo box or list by its label. The list is opened if it has to be for its entries to exist.
    static string SelectOption(AutomationElement e, string option) {
        string want = option.Trim();
        object p;

        // A classic combo box: its selection pattern changes what the box shows without telling the app, which goes on
        // using the old choice (measured: the font box read "Arial Black" and the grid stayed in Arial). The control's
        // own messages do it properly - choose the entry, then tell the parent it changed, as a click would.
        IntPtr native = IntPtr.Zero;
        try { native = new IntPtr(e.Current.NativeWindowHandle); } catch (Exception) { }
        if (native != IntPtr.Zero) {
            var cls = new StringBuilder(64); GetClassName(native, cls, 64);
            if (cls.ToString() == "ComboBox") return SelectClassicCombo(native, want);
        }

        ExpandCollapsePattern expander = null;
        if (e.TryGetCurrentPattern(ExpandCollapsePattern.Pattern, out p)) {
            expander = (ExpandCollapsePattern)p;
            try { if (expander.Current.ExpandCollapseState == ExpandCollapseState.Collapsed) expander.Expand(); } catch (Exception) { }
            Thread.Sleep(250);
        }

        var items = e.FindAll(TreeScope.Descendants, new OrCondition(
            new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.ListItem),
            new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.MenuItem),
            new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.TreeItem),
            new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.RadioButton)));
        var labels = new List<string>();
        AutomationElement hit = null;
        foreach (AutomationElement item in items) {
            string label = (item.Current.Name ?? "").Trim();
            if (label.Length == 0) continue;
            labels.Add(label);
            if (hit == null && string.Equals(label, want, StringComparison.OrdinalIgnoreCase)) hit = item;
        }

        if (hit == null) {
            if (expander != null) { try { expander.Collapse(); } catch (Exception) { } }
            if (labels.Count > 40) labels = labels.GetRange(0, 40);
            throw new InvalidOperationException("no option \"" + want + "\". Options: " + (labels.Count == 0 ? "none found" : string.Join(", ", labels.ToArray())));
        }

        string chosen = hit.Current.Name;
        if (hit.TryGetCurrentPattern(SelectionItemPattern.Pattern, out p)) ((SelectionItemPattern)p).Select();
        else if (hit.TryGetCurrentPattern(InvokePattern.Pattern, out p)) ((InvokePattern)p).Invoke();
        else throw new InvalidOperationException("the option \"" + chosen + "\" cannot be chosen");
        if (expander != null) { try { expander.Collapse(); } catch (Exception) { } }
        return chosen;
    }

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, string lp, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll")]
    static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, IntPtr lp, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll")] static extern IntPtr GetParent(IntPtr h);
    [DllImport("user32.dll")] static extern int GetDlgCtrlID(IntPtr h);

    static string SelectClassicCombo(IntPtr combo, string want) {
        const uint CB_GETCOUNT = 0x0146, CB_GETLBTEXT = 0x0148, CB_GETLBTEXTLEN = 0x0149, CB_SETCURSEL = 0x014E,
                   CB_FINDSTRINGEXACT = 0x0158, WM_COMMAND = 0x0111;
        const int CBN_SELCHANGE = 1, CBN_SELENDOK = 9;
        IntPtr r;

        if (SendMessageTimeout(combo, CB_FINDSTRINGEXACT, (IntPtr)(-1), want, 0x2, 2000, out r) == IntPtr.Zero)
            throw new InvalidOperationException("the list did not answer");
        int index = r.ToInt32();

        if (index < 0) {
            var labels = new List<string>();
            SendMessageTimeout(combo, CB_GETCOUNT, IntPtr.Zero, IntPtr.Zero, 0x2, 2000, out r);
            int count = Math.Min(r.ToInt32(), 40);
            for (int i = 0; i < count; i++) {
                SendMessageTimeout(combo, CB_GETLBTEXTLEN, (IntPtr)i, IntPtr.Zero, 0x2, 500, out r);
                var text = new StringBuilder(Math.Max(r.ToInt32(), 0) + 2);
                SendMessageTimeout(combo, CB_GETLBTEXT, (IntPtr)i, text, 0x2, 500, out r);
                if (text.Length > 0) labels.Add(text.ToString());
            }
            throw new InvalidOperationException("no option \"" + want + "\". Options: " + (labels.Count == 0 ? "none found" : string.Join(", ", labels.ToArray())));
        }

        SendMessageTimeout(combo, CB_SETCURSEL, (IntPtr)index, IntPtr.Zero, 0x2, 2000, out r);
        IntPtr parent = GetParent(combo);
        int id = GetDlgCtrlID(combo) & 0xFFFF;
        PostMessage(parent, WM_COMMAND, (IntPtr)((CBN_SELENDOK << 16) | id), combo);
        PostMessage(parent, WM_COMMAND, (IntPtr)((CBN_SELCHANGE << 16) | id), combo);
        return WindowText(combo);
    }

    // ---- Input. Posted, never sent and never SendInput: SendInput goes to the desktop a person is looking at, and
    // SendMessage does not return while the app shows a modal dialog in answer.

    static IntPtr lastTarget;

    static IntPtr WindowAt(POINT p) {
        foreach (var w in Visible())
            if (p.X >= w.V.L && p.X < w.V.R && p.Y >= w.V.T && p.Y < w.V.B) return w.H;
        return IntPtr.Zero;
    }

    // Down to the innermost visible, enabled child window under a screen point.
    static IntPtr Descend(IntPtr wnd, POINT screen) {
        for (int i = 0; i < 16; i++) {
            POINT c = screen; ScreenToClient(wnd, ref c);
            IntPtr child = ChildWindowFromPointEx(wnd, c, 0x7);
            if (child == IntPtr.Zero || child == wnd) return wnd;
            wnd = child;
        }
        return wnd;
    }

    static IntPtr LParam(IntPtr target, POINT screen) {
        POINT c = screen; ScreenToClient(target, ref c);
        return (IntPtr)(((c.Y & 0xFFFF) << 16) | (c.X & 0xFFFF));
    }

    static IntPtr Mouse(uint message, int x, int y, int wparam, IntPtr target) {
        var screen = new POINT { X = x, Y = y };
        if (target == IntPtr.Zero) {
            IntPtr top = WindowAt(screen);
            if (top == IntPtr.Zero) throw new InvalidOperationException("no window is at " + x + "," + y);
            target = Descend(top, screen);
        }
        PostMessage(target, message, (IntPtr)wparam, LParam(target, screen));
        return target;
    }

    static void Click(int x, int y, bool right, int count) {
        IntPtr top = WindowAt(new POINT { X = x, Y = y });
        if (top != IntPtr.Zero) Raise(top);
        IntPtr target = Mouse(WM_MOUSEMOVE, x, y, 0, IntPtr.Zero);
        lastTarget = target;
        uint down = right ? WM_RBUTTONDOWN : WM_LBUTTONDOWN, up = right ? WM_RBUTTONUP : WM_LBUTTONUP;
        int held = right ? 2 : 1;
        for (int i = 0; i < Math.Max(1, count); i++) {
            // The second press of a double click is its own message; an app that only counts presses gets both.
            Mouse(i == 1 && !right ? WM_LBUTTONDBLCLK : down, x, y, held, target);
            Thread.Sleep(30);
            Mouse(up, x, y, 0, target);
            if (i + 1 < count) Thread.Sleep(60);
        }
    }

    static void Drag(int x, int y, int x2, int y2) {
        IntPtr target = Mouse(WM_MOUSEMOVE, x, y, 0, IntPtr.Zero);
        lastTarget = target;
        Mouse(WM_LBUTTONDOWN, x, y, 1, target);
        // The pointer travels rather than jumps: a drop target that follows motion never sees a jump.
        for (int step = 1; step <= 8; step++) {
            Thread.Sleep(30);
            Mouse(WM_MOUSEMOVE, x + (x2 - x) * step / 8, y + (y2 - y) * step / 8, 1, target);
        }
        Thread.Sleep(30);
        Mouse(WM_LBUTTONUP, x2, y2, 0, target);
    }

    static void Scroll(int x, int y, string direction, int amount) {
        var screen = new POINT { X = x, Y = y };
        IntPtr top = WindowAt(screen);
        if (top == IntPtr.Zero) throw new InvalidOperationException("no window is at " + x + "," + y);
        IntPtr target = Descend(top, screen);
        bool horizontal = direction == "left" || direction == "right";
        int sign = (direction == "up" || direction == "right") ? 1 : -1;
        IntPtr lp = (IntPtr)(((y & 0xFFFF) << 16) | (x & 0xFFFF)); // a wheel message carries screen coordinates
        for (int i = 0; i < Math.Max(1, amount); i++) {
            PostMessage(target, horizontal ? WM_MOUSEHWHEEL : WM_MOUSEWHEEL, (IntPtr)((sign * 120) << 16), lp);
            Thread.Sleep(20);
        }
    }

    // Keys go where the app itself has put keyboard focus, in the topmost window - so a dialog gets its own keys, and
    // a browser's address bar, which is not a window of its own, still gets what is typed.
    static IntPtr KeyTarget() {
        var ws = Visible();
        if (ws.Count == 0) return lastTarget;
        IntPtr active = ActiveWindow(ws);
        if (active == IntPtr.Zero) active = ws[0].H;
        // A menu or drop-down the active window has open is above it, and is where keys belong.
        IntPtr top = ws[0].H;
        int activePid; uint activeThread = GetWindowThreadProcessId(active, out activePid);
        int topPid; uint topThread = GetWindowThreadProcessId(top, out topPid);
        uint tid = topPid == activePid ? topThread : activeThread;
        var gi = new GUITHREADINFO(); gi.cbSize = Marshal.SizeOf(gi);
        if (GetGUIThreadInfo(tid, ref gi) && gi.hwndFocus != IntPtr.Zero) return gi.hwndFocus;
        if (lastTarget != IntPtr.Zero && IsWindow(lastTarget)) return lastTarget;
        return topPid == activePid ? top : active;
    }

    static void TypeText(string text) {
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) throw new InvalidOperationException("nothing is open to type into");
        foreach (char ch in text) {
            if (ch == '\r') continue;
            if (ch == '\n') { PressKey(t, 13, ""); Thread.Sleep(15); t = KeyTarget(); continue; }
            PostMessage(t, WM_CHAR, (IntPtr)ch, (IntPtr)1);
            Thread.Sleep(4);
        }
    }

    static readonly Dictionary<string, int> KeyCodes = new Dictionary<string, int>(StringComparer.OrdinalIgnoreCase) {
        { "enter", 13 }, { "return", 13 }, { "esc", 27 }, { "escape", 27 }, { "tab", 9 }, { "space", 32 }, { "backspace", 8 },
        { "delete", 46 }, { "del", 46 }, { "insert", 45 }, { "up", 38 }, { "down", 40 }, { "left", 37 }, { "right", 39 },
        { "home", 36 }, { "end", 35 }, { "pageup", 33 }, { "pgup", 33 }, { "prior", 33 }, { "pagedown", 34 }, { "pgdn", 34 },
        { "next", 34 }, { "menu", 93 }, { "apps", 93 } };

    // "ctrl+s", "alt+F4", "Enter", "shift+tab": the keys of one combination, modifiers first. Returns what was sent.
    static string Key(string combo) {
        string mods = "";
        int vk = 0;
        foreach (string raw in combo.Split('+')) {
            string part = raw.Trim();
            if (part.Length == 0) continue;
            string lower = part.ToLowerInvariant();
            if (lower == "ctrl" || lower == "control" || lower == "cmd") mods += "c";
            else if (lower == "shift") mods += "s";
            else if (lower == "alt") mods += "a";
            else if (KeyCodes.ContainsKey(part)) vk = KeyCodes[part];
            else if (lower.Length >= 2 && lower[0] == 'f' && char.IsDigit(lower[1])) vk = 0x70 + int.Parse(lower.Substring(1)) - 1;
            else if (part.Length == 1) {
                short scan = VkKeyScan(part[0]);
                if (scan == -1) throw new ArgumentException("no key for \"" + part + "\"");
                vk = scan & 0xFF;
                if ((scan & 0x100) != 0 && !mods.Contains("s")) mods += "s";
            } else throw new ArgumentException("unknown key \"" + part + "\"");
        }
        if (vk == 0) throw new ArgumentException("no key given in \"" + combo + "\"");
        IntPtr t = KeyTarget();
        if (t == IntPtr.Zero) throw new InvalidOperationException("nothing is open to send a key to");
        PressKey(t, vk, mods);
        return mods + ":" + vk;
    }

    static void PressKey(IntPtr t, int vk, string mods) {
        uint scan = MapVirtualKey((uint)vk, 0);
        // A keyboard turns these into characters through TranslateMessage; a posted key does not.
        bool asChar = (vk == 8 || vk == 9 || vk == 13 || vk == 27) && mods.Replace("s", "").Length == 0;
        if (mods.Length == 0) {
            PostMessage(t, WM_KEYDOWN, (IntPtr)vk, (IntPtr)(1 | (scan << 16)));
            if (asChar) PostMessage(t, WM_CHAR, (IntPtr)vk, (IntPtr)1);
            PostMessage(t, WM_KEYUP, (IntPtr)vk, (IntPtr)unchecked((int)(0xC0000001 | (scan << 16))));
            return;
        }

        // GetKeyState - how an app learns Ctrl or Shift is held - reflects hardware input only, never a posted message.
        // So the keys are posted from a thread that has joined the app's input queue and set that queue's key state
        // with the modifiers down, and holds it until the app has taken the messages off its queue.
        var th = new Thread(delegate() {
            int pid; uint appThread = GetWindowThreadProcessId(t, out pid);
            uint me = GetCurrentThreadId();
            bool joined = AttachThreadInput(me, appThread, true);
            try {
                var saved = new byte[256];
                if (joined) {
                    GetKeyboardState(saved);
                    var keys = (byte[])saved.Clone();
                    if (mods.Contains("c")) { keys[0x11] |= 0x80; keys[0xA2] |= 0x80; }
                    if (mods.Contains("s")) { keys[0x10] |= 0x80; keys[0xA0] |= 0x80; }
                    if (mods.Contains("a")) { keys[0x12] |= 0x80; keys[0xA4] |= 0x80; }
                    SetKeyboardState(keys);
                }
                bool alt = mods.Contains("a");
                PostMessage(t, alt ? WM_SYSKEYDOWN : WM_KEYDOWN, (IntPtr)vk, (IntPtr)((alt ? 0x20000001 : 1) | (int)(scan << 16)));
                if (asChar) PostMessage(t, WM_CHAR, (IntPtr)vk, (IntPtr)1);
                PostMessage(t, alt ? WM_SYSKEYUP : WM_KEYUP, (IntPtr)vk, (IntPtr)unchecked((int)((alt ? 0xE0000001 : 0xC0000001) | (scan << 16))));
                Thread.Sleep(150);
                if (joined) SetKeyboardState(saved);
            } finally { if (joined) AttachThreadInput(me, appThread, false); }
        });
        th.Start(); th.Join();
    }
}
