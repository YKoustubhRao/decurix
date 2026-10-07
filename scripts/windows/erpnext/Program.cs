using System;
using System.Diagnostics;
using System.IO;
using System.Net;
using System.Threading;
using System.Windows.Forms;

// The existing ERPNext installation and volumes stay in Ubuntu. A live WSL
// process keeps that distro (and Docker) running after compose has returned.
static class ERPNextLauncher
{
    const string Address = "http://localhost:8000";
    static bool Ready()
    {
        try {
            var request = (HttpWebRequest)WebRequest.Create(Address + "/login");
            request.Proxy = null; request.Timeout = 3000;
            using (var response = (HttpWebResponse)request.GetResponse())
                return (int)response.StatusCode >= 200 && (int)response.StatusCode < 400;
        } catch { return false; }
    }

    [STAThread]
    static int Main(string[] args)
    {
        bool serverOnly = Array.IndexOf(args, "--server-only") >= 0;
        string folder = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile), "erpnext-demo");
        // The private host desktop has an isolated USERPROFILE. The installed
        // stack's location is saved beside this executable, not inferred there.
        string config = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "stack-path.txt");
        if (File.Exists(config)) folder = File.ReadAllText(config).Trim();
        try {
            if (!Ready()) {
                if (!File.Exists(Path.Combine(folder, "pwd.yml"))) throw new Exception("ERPNext's pwd.yml was not found in " + folder);
                string linux = "/mnt/" + char.ToLowerInvariant(folder[0]) + folder.Substring(2).Replace('\\', '/');
                string quoted = "'" + linux.Replace("'", "'\\''") + "'";
                var start = new ProcessStartInfo {
                    FileName = Path.Combine(Environment.SystemDirectory, "wsl.exe"),
                    Arguments = "-d Ubuntu -u root --exec sh -lc \"cd " + quoted + " && (service docker start >/dev/null 2>&1 || true) && docker compose -p erpnext -f pwd.yml up -d && exec sleep infinity\"",
                    UseShellExecute = false, CreateNoWindow = true, WindowStyle = ProcessWindowStyle.Hidden
                };
                var keeper = Process.Start(start);
                DateTime until = DateTime.UtcNow.AddMinutes(5);
                while (!Ready()) {
                    if (keeper == null || keeper.HasExited) throw new Exception("ERPNext startup exited. Check Docker in the Ubuntu WSL distribution.");
                    if (DateTime.UtcNow >= until) throw new Exception("ERPNext did not become ready within five minutes. Check its container logs.");
                    Thread.Sleep(1000);
                }
                keeper.Dispose(); // leaves the WSL keeper running
            }
            if (serverOnly) return 0;
            foreach (string browser in new[] {
                Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86), "Microsoft/Edge/Application/msedge.exe"),
                Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Microsoft/Edge/Application/msedge.exe"),
                Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Google/Chrome/Application/chrome.exe") }) {
                if (!File.Exists(browser)) continue;
                Process.Start(new ProcessStartInfo(browser, "--app=" + Address) { UseShellExecute = false });
                return 0;
            }
            throw new Exception("Install Microsoft Edge or Google Chrome to open ERPNext.");
        } catch (Exception e) {
            if (!serverOnly) MessageBox.Show(e.Message, "ERPNext could not start", MessageBoxButtons.OK, MessageBoxIcon.Error);
            return 1;
        }
    }
}
