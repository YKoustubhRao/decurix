// A worker's own Files window. Compiled once with Windows' .NET Framework;
// no PowerShell interpreter, Explorer process, or directory scans while idle.
using System;
using System.Collections.Generic;
using System.Drawing;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;
using System.Windows.Forms;

public class WorkerFiles : Form {
    public readonly ListView Entries = new ListView();
    public string CurrentPath { get; private set; }
    public string DisplayPath { get { return address.Text; } }
    readonly string root, work;
    readonly TextBox address = new TextBox();
    readonly Button back = new Button(), forward = new Button(), up = new Button(), refresh = new Button();
    readonly Stack<string> past = new Stack<string>(), future = new Stack<string>();
    readonly FileSystemWatcher watcher = new FileSystemWatcher();
    readonly Timer debounce = new Timer { Interval = 150 };
    readonly ImageList icons = new ImageList();
    readonly ToolTip tips = new ToolTip();
    readonly Label status = new Label();
    bool dirty;

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] struct SHFILEINFO {
        public IntPtr icon; public int index; public uint attrs;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 260)] public string display;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 80)] public string type;
    }
    [DllImport("shell32.dll", CharSet = CharSet.Unicode)] static extern IntPtr SHGetFileInfo(string path, uint attrs, ref SHFILEINFO info, uint size, uint flags);
    [DllImport("user32.dll")] static extern bool DestroyIcon(IntPtr icon);
    [DllImport("user32.dll")] static extern bool SetProcessDPIAware();

    [STAThread] public static void Main(string[] args) {
        if (args.Length < 1) return;
        SetProcessDPIAware(); Application.EnableVisualStyles();
        Application.SetCompatibleTextRenderingDefault(false);
        using (var window = new WorkerFiles(args[0], args.Length > 1 ? args[1] : Path.Combine(args[0], "work"))) Application.Run(window);
    }

    public WorkerFiles(string home, string start) {
        root = Path.GetFullPath(home).TrimEnd(Path.DirectorySeparatorChar);
        work = Path.Combine(root, "work"); CurrentPath = root;
        Text = "Files"; Font = new Font("Segoe UI", 9f); Width = 860; Height = 560;
        MinimumSize = new Size(600, 360); StartPosition = FormStartPosition.CenterScreen; KeyPreview = true;
        var toolbar = new TableLayoutPanel { Dock = DockStyle.Top, Height = 44, Padding = new Padding(8), ColumnCount = 5 };
        for (int i = 0; i < 4; i++) toolbar.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 64));
        toolbar.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        Configure(back, "Back", "Back (Alt+Left)"); Configure(forward, "Forward", "Forward (Alt+Right)");
        Configure(up, "Up", "Up (Alt+Up)"); Configure(refresh, "Refresh", "Refresh (F5)");
        address.Dock = DockStyle.Fill; address.Margin = new Padding(8, 2, 0, 0);
        address.AccessibleName = "Folder path";
        toolbar.Controls.Add(back, 0, 0); toolbar.Controls.Add(forward, 1, 0); toolbar.Controls.Add(up, 2, 0);
        toolbar.Controls.Add(refresh, 3, 0); toolbar.Controls.Add(address, 4, 0);
        Entries.Dock = DockStyle.Fill; Entries.View = View.Details; Entries.FullRowSelect = true;
        Entries.MultiSelect = false; Entries.HideSelection = false; Entries.AccessibleName = "Files and folders";
        Entries.Columns.Add("Name", 340); Entries.Columns.Add("Type", 120); Entries.Columns.Add("Size", 100); Entries.Columns.Add("Modified", 170);
        icons.ImageSize = new Size(16, 16); icons.ColorDepth = ColorDepth.Depth32Bit; Entries.SmallImageList = icons;
        status.Dock = DockStyle.Bottom; status.Height = 28; status.Padding = new Padding(10, 5, 0, 0);
        Controls.Add(Entries); Controls.Add(toolbar); Controls.Add(status);
        back.Click += delegate { GoBack(); }; forward.Click += delegate { GoForward(); };
        up.Click += delegate { GoUp(); }; refresh.Click += delegate { TryFolder(CurrentPath, false); };
        Entries.DoubleClick += delegate { OpenSelected(); };
        Entries.KeyDown += delegate(object sender, KeyEventArgs e) {
            if (e.KeyCode == Keys.Enter) { OpenSelected(); e.SuppressKeyPress = true; }
            if (e.KeyCode == Keys.Back) { GoUp(); e.SuppressKeyPress = true; }
        };
        address.KeyDown += delegate(object sender, KeyEventArgs e) {
            if (e.KeyCode != Keys.Enter) return;
            e.SuppressKeyPress = true;
            try { ShowFolder(ResolveAddress(address.Text), true); Entries.Focus(); }
            catch (Exception error) { MessageBox.Show(this, error.Message, "Folder unavailable", MessageBoxButtons.OK, MessageBoxIcon.Information); }
        };
        watcher.SynchronizingObject = this;
        watcher.NotifyFilter = NotifyFilters.FileName | NotifyFilters.DirectoryName | NotifyFilters.LastWrite | NotifyFilters.Size;
        watcher.Changed += delegate { dirty = true; }; watcher.Created += delegate { dirty = true; };
        watcher.Deleted += delegate { dirty = true; }; watcher.Renamed += delegate { dirty = true; }; watcher.Error += delegate { dirty = true; };
        debounce.Tick += delegate {
            if (!dirty) return;
            try { ShowFolder(CurrentPath, false); }
            catch { TryFolder(root, false); }
        };
        KeyDown += delegate(object sender, KeyEventArgs e) {
            if (e.Control && e.KeyCode == Keys.L) { address.Focus(); address.SelectAll(); }
            else if (e.Alt && e.KeyCode == Keys.Left) GoBack();
            else if (e.Alt && e.KeyCode == Keys.Right) GoForward();
            else if (e.Alt && e.KeyCode == Keys.Up) GoUp();
            else if (e.KeyCode == Keys.F5) TryFolder(CurrentPath, false);
            else return;
            e.SuppressKeyPress = true;
        };
        Shown += delegate { TryFolder(Directory.Exists(start) ? start : root, false); debounce.Start(); };
        FormClosed += delegate { debounce.Dispose(); watcher.Dispose(); icons.Dispose(); tips.Dispose(); };
    }

    void Configure(Button button, string text, string tip) {
        button.Text = text; button.Dock = DockStyle.Fill; button.Margin = new Padding(0, 0, 4, 0);
        tips.SetToolTip(button, tip);
    }
    bool Inside(string path) { return path.Equals(root, StringComparison.OrdinalIgnoreCase) || path.StartsWith(root + "\\", StringComparison.OrdinalIgnoreCase); }
    string ResolveAddress(string text) {
        text = text.Trim();
        if (text.Equals("Work:", StringComparison.OrdinalIgnoreCase) || text.StartsWith("Work:\\", StringComparison.OrdinalIgnoreCase) || text.StartsWith("Work:/", StringComparison.OrdinalIgnoreCase)) return Path.Combine(work, text.Substring(5).TrimStart('\\', '/'));
        if (text.Equals("Home", StringComparison.OrdinalIgnoreCase) || text.StartsWith("Home\\", StringComparison.OrdinalIgnoreCase) || text.StartsWith("Home/", StringComparison.OrdinalIgnoreCase)) return Path.Combine(root, text.Substring(4).TrimStart('\\', '/'));
        return Path.IsPathRooted(text) ? text : Path.Combine(CurrentPath, text);
    }
    void TryFolder(string folder, bool remember) {
        try { ShowFolder(folder, remember); }
        catch (IOException error) { status.Text = error.Message; }
        catch (UnauthorizedAccessException) { status.Text = "This folder is not accessible. Choose another folder."; }
    }
    void GoBack() { if (past.Count > 0) { future.Push(CurrentPath); TryFolder(past.Pop(), false); } }
    void GoForward() { if (future.Count > 0) { past.Push(CurrentPath); TryFolder(future.Pop(), false); } }
    void GoUp() { if (CurrentPath != root) TryFolder(Path.GetDirectoryName(CurrentPath), true); }
    void OpenSelected() {
        if (Entries.SelectedItems.Count == 0) return;
        string path = (string)Entries.SelectedItems[0].Tag;
        try {
            if (Directory.Exists(path)) ShowFolder(path, true);
            else System.Diagnostics.Process.Start(new System.Diagnostics.ProcessStartInfo(path) { UseShellExecute = true });
        } catch (Exception error) { MessageBox.Show(this, error.Message, "Could not open", MessageBoxButtons.OK, MessageBoxIcon.Information); }
    }
    string IconKey(FileSystemInfo entry) {
        bool directory = (entry.Attributes & FileAttributes.Directory) != 0;
        string key = directory ? "folder" : entry.Extension.ToLowerInvariant();
        if (icons.Images.ContainsKey(key)) return key;
        var info = new SHFILEINFO();
        // Extension icons need no disk reads; reuse one bitmap per file type.
        SHGetFileInfo(directory ? "folder" : "file" + key, directory ? 0x10u : 0x80u, ref info, (uint)Marshal.SizeOf(typeof(SHFILEINFO)), 0x100 | 0x1 | 0x10);
        if (info.icon != IntPtr.Zero) {
            try { using (var icon = System.Drawing.Icon.FromHandle(info.icon)) icons.Images.Add(key, icon); }
            finally { DestroyIcon(info.icon); }
        }
        return key;
    }
    static string SizeText(long size) {
        if (size >= 1073741824) return (size / 1073741824.0).ToString("N1") + " GB";
        if (size >= 1048576) return (size / 1048576.0).ToString("N1") + " MB";
        if (size >= 1024) return (size / 1024.0).ToString("N0") + " KB";
        return size + " B";
    }
    public void ShowFolder(string folder, bool remember) {
        string full = Path.GetFullPath(folder).TrimEnd('\\');
        if (!Inside(full)) throw new IOException("Choose a folder inside this worker Home or Work drive.");
        if (!Directory.Exists(full)) throw new DirectoryNotFoundException("That folder no longer exists.");
        var entries = new DirectoryInfo(full).GetFileSystemInfos().Where(e => full != root || e.Name != ".decurix")
            .OrderBy(e => (e.Attributes & FileAttributes.Directory) != 0 ? 0 : 1).ThenBy(e => e.Name, StringComparer.CurrentCultureIgnoreCase).ToArray();
        string selected = full == CurrentPath && Entries.SelectedItems.Count > 0 ? Entries.SelectedItems[0].Text : null;
        string top = full == CurrentPath && Entries.TopItem != null ? Entries.TopItem.Text : null;
        if (remember && full != CurrentPath) { past.Push(CurrentPath); future.Clear(); }
        CurrentPath = full;
        Entries.BeginUpdate(); Entries.Items.Clear();
        ListViewItem first = null;
        try { foreach (var entry in entries) {
            try {
            bool directory = (entry.Attributes & FileAttributes.Directory) != 0;
            var item = new ListViewItem(entry.Name, IconKey(entry)) { Tag = entry.FullName };
            item.SubItems.Add(directory ? "Folder" : (entry.Extension.TrimStart('.').ToUpperInvariant() + " file").Trim());
            item.SubItems.Add(directory ? "" : SizeText(((FileInfo)entry).Length)); item.SubItems.Add(entry.LastWriteTime.ToString("g"));
            Entries.Items.Add(item); if (entry.Name == selected) item.Selected = true; if (entry.Name == top) first = item;
            } catch (IOException) { } // a file can disappear while the directory is refreshed
            catch (UnauthorizedAccessException) { }
        } } finally { Entries.EndUpdate(); }
        if (first != null) Entries.TopItem = first;
        string relative = full.Substring(root.Length).TrimStart('\\');
        address.Text = full.Equals(work, StringComparison.OrdinalIgnoreCase) ? "Work:\\" : full.StartsWith(work + "\\", StringComparison.OrdinalIgnoreCase) ? "Work:\\" + full.Substring(work.Length + 1) : relative.Length == 0 ? "Home" : "Home\\" + relative;
        Text = "Files - " + address.Text; status.Text = Entries.Items.Count + (Entries.Items.Count == 1 ? " item" : " items");
        back.Enabled = past.Count > 0; forward.Enabled = future.Count > 0; up.Enabled = full != root;
        if (watcher.Path != full) { watcher.Path = full; watcher.EnableRaisingEvents = true; }
        dirty = false;
    }
}
