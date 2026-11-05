package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// A file on the person's computer held open by an app one of our host-app helpers started is released by ending
// that app, and only that app: Windows' Restart Manager names every process holding the file, and a holder is ended
// only when one of its parents (a few levels up) is a PowerShell running decurix-winhost\bridge.ps1. The person's own
// Word, or anything else we did not start, is reported and left alone. Found live (2026-10-20, P1): a kept Word of a
// desktop still held Exceptions_memo_Oct2026.docx from a run stopped hours before, and the next run could not reset its
// inputs (WinError 32, the file in use).

// releaseScript is the PowerShell that lists the holders of path and ends those our helpers started. It prints one
// line per holder: "RELEASED <pid> <name>" or "HELD <pid> <name>" (not ours: left alone).
func releaseScript(path string) string {
	quoted := strings.ReplaceAll(path, "'", "''")
	return quietPreamble + `$code = @"
using System; using System.Collections.Generic; using System.Runtime.InteropServices;
public static class DecurixRm {
  [StructLayout(LayoutKind.Sequential)] struct UP { public int Pid; public System.Runtime.InteropServices.ComTypes.FILETIME Started; }
  [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] struct PI {
    public UP Process;
    [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 256)] public string App;
    [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 64)] public string Service;
    public int Type; public uint Status; public uint Session; [MarshalAs(UnmanagedType.Bool)] public bool Restartable;
  }
  [DllImport("rstrtmgr.dll", CharSet = CharSet.Unicode)] static extern int RmStartSession(out uint h, int f, string key);
  [DllImport("rstrtmgr.dll")] static extern int RmEndSession(uint h);
  [DllImport("rstrtmgr.dll", CharSet = CharSet.Unicode)] static extern int RmRegisterResources(uint h, uint n, string[] files, uint a, IntPtr b, uint c, string[] d);
  [DllImport("rstrtmgr.dll")] static extern int RmGetList(uint h, out uint needed, ref uint n, [In, Out] PI[] info, ref uint reasons);
  public static int[] Holders(string path) {
    uint h; if (RmStartSession(out h, 0, Guid.NewGuid().ToString()) != 0) return new int[0];
    try {
      if (RmRegisterResources(h, 1, new string[] { path }, 0, IntPtr.Zero, 0, null) != 0) return new int[0];
      uint needed = 0, n = 0, reasons = 0;
      RmGetList(h, out needed, ref n, null, ref reasons);
      if (needed == 0) return new int[0];
      PI[] info = new PI[needed]; n = needed;
      if (RmGetList(h, out needed, ref n, info, ref reasons) != 0) return new int[0];
      List<int> ids = new List<int>();
      for (int i = 0; i < n; i++) ids.Add(info[i].Process.Pid);
      return ids.ToArray();
    } finally { RmEndSession(h); }
  }
}
"@
Add-Type -TypeDefinition $code
foreach ($id in [DecurixRm]::Holders('` + quoted + `')) {
  $p = Get-CimInstance Win32_Process -Filter "ProcessId=$id"
  $cur = $p; $ours = $false
  for ($i = 0; $i -lt 6 -and $cur; $i++) {
    $cur = Get-CimInstance Win32_Process -Filter "ProcessId=$($cur.ParentProcessId)"
    if ($cur -and $cur.CommandLine -like '` + ownHelperMatch(hostInstanceTag()) + `') { $ours = $true; break }
  }
  if ($ours) { taskkill.exe /T /F /PID $id | Out-Null; 'RELEASED ' + $id + ' ' + $p.Name } else { 'HELD ' + $id + ' ' + $p.Name }
}
`
}

// releaseLines reads the script's own "RELEASED <pid> <name>" / "HELD <pid> <name>" lines into two lists.
func releaseLines(out string) (released, held []string) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[0] == "RELEASED" || f[0] == "HELD") {
			who := f[2] + " " + f[1]
			if f[0] == "RELEASED" {
				released = append(released, who)
			} else {
				held = append(held, who)
			}
		}
	}
	return released, held
}

// hostFileReleaseHandler ends the apps our host-app helpers started that hold a file of the person's computer named in
// the desktop's open task (POST ?desktop=&path=). It answers what it ended and what holds the file that is not ours.
func (s *Server) hostFileReleaseHandler(w http.ResponseWriter, r *http.Request) {
	asked, rel, ok := s.hostFileAsked(w, r, http.MethodPost)
	if !ok {
		return
	}
	// The same rule as reading it: not a path that resolves under a private folder or behind a link out of the drive.
	for _, drive := range hostCDrives() {
		if _, err := resolveHostFile(drive, rel); errors.Is(err, errHostFileRefused) {
			http.Error(w, refusedHostFile, http.StatusForbidden)
			return
		}
	}
	out, err := runOnHost(releaseScript(asked), 60*time.Second)
	if err != nil {
		log.Printf("host apps: could not release %s: %v", asked, err)
		jsonResponse(w, map[string]any{"path": asked, "released": []string{}, "held": []string{}, "unknown": true})
		return
	}
	released, held := releaseLines(out)
	if len(released) > 0 {
		log.Printf("host apps: released %s, held by our own app(s): %s", asked, strings.Join(released, ", "))
	}
	if released == nil {
		released = []string{}
	}
	if held == nil {
		held = []string{}
	}
	jsonResponse(w, map[string]any{"path": asked, "released": released, "held": held})
}
